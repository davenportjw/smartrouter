provider "google" {
  project               = var.project_id
  region                = var.region
  user_project_override = true
  billing_project       = var.project_id
}

# Enable Necessary APIs
locals {
  services = [
    "run.googleapis.com",
    "firestore.googleapis.com",
    "secretmanager.googleapis.com",
    "iam.googleapis.com",
    "monitoring.googleapis.com",
    "identitytoolkit.googleapis.com",
    "aiplatform.googleapis.com",
    "generativelanguage.googleapis.com",
    "apikeys.googleapis.com",
  ]

  # Parse ALLOWED_EMAIL_DOMAINS to convert domains and specific emails to IAM members
  parsed_email_members = [
    for entry in split(",", var.allowed_email_domains) :
    length(split("@", trimspace(entry))) > 1 ? "user:${trimspace(entry)}" : "domain:${trimspace(entry)}"
    if trimspace(entry) != ""
  ]

  # Managed Secret Manager container IDs (secret versions are populated out-of-band via
  # `gcloud secrets versions add` / `./deploy.sh` so raw secret values never enter terraform.tfstate).
  managed_secret_ids = toset([
    "JEV_API_KEY",
    "FASTINO_API_KEY",
    "CLM_API_KEY",
    "BACKEND_SHARED_SECRET"
  ])

  mounted_secret_ids = toset([
    for s in var.enabled_secret_mounts : s
    if contains([
      "JEV_API_KEY",
      "FASTINO_API_KEY",
      "CLM_API_KEY",
      "BACKEND_SHARED_SECRET"
    ], s)
  ])
}

resource "google_project_service" "apis" {
  for_each = toset(local.services)
  service  = each.value

  disable_on_destroy = false
}

# Firestore Database Native Instance
resource "google_firestore_database" "database" {
  depends_on                        = [google_project_service.apis]
  name                              = "(default)"
  location_id                       = var.region
  type                              = "FIRESTORE_NATIVE"
  point_in_time_recovery_enablement = "POINT_IN_TIME_RECOVERY_ENABLED"

  # Prevent accidental deletion of Firestore database in production
  deletion_policy = "DELETE"
}

# Service Account for CLI/Developer OIDC Impersonation against Admin API
resource "google_service_account" "router_sa" {
  depends_on   = [google_project_service.apis]
  account_id   = "gemini-router-runner"
  display_name = "Gemini Smart Router Cloud Run Service Account"
}

# IAM: Vertex AI, Firestore, and Secret Manager bindings were removed from router_sa:
# no Cloud Run service runs as it any more (see service-accounts.tf). It is retained
# strictly as a zero-privilege OIDC caller identity for authorized CLI operators.

# Google Cloud Secret Manager: Managed Secret Containers for Decision Engines & Shared Auth
# Note: Secret versions are populated out-of-band via `gcloud secrets versions add` (see deploy.sh)
# so secret payloads are never persisted in plaintext inside terraform.tfstate.
resource "google_secret_manager_secret" "app_secrets" {
  for_each  = local.managed_secret_ids
  project   = var.project_id
  secret_id = each.key

  labels = {
    managed_by = "smartrouter-terraform"
    component  = "decision-engine"
  }

  replication {
    auto {}
  }

  depends_on = [google_project_service.apis]
}

# 1. BACKEND SERVICE (Gemini Router API and Administrative Configuration APIs)
resource "google_cloud_run_v2_service" "router_service" {
  depends_on = [
    google_project_service.apis,
    google_service_account.api_sa,
    google_secret_manager_secret.app_secrets,
    google_secret_manager_secret_iam_member.api_sa_secret_accessor
  ]

  name     = var.service_name
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.api_sa.email

    containers {
      image = "gcr.io/cloudrun/placeholder" # Updated dynamically in build/deploy script

      ports {
        container_port = 8080
      }

      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = var.project_id
      }

      env {
        name  = "GEMINI_LOCATION"
        value = var.region
      }

      # Admin API OIDC pinning (fail closed if either is empty); see service-accounts.tf.
      env {
        name  = "API_OIDC_AUDIENCES"
        value = local.api_oidc_audiences
      }

      env {
        name  = "API_ALLOWED_CALLERS"
        value = local.api_allowed_callers
      }

      dynamic "env" {
        for_each = local.api_sa_secrets
        content {
          name = env.value
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.app_secrets[env.value].secret_id
              version = "latest"
            }
          }
        }
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }
    }
  }

  lifecycle {
    ignore_changes = [
      client,
      client_version,
      template[0].containers[0].image,
      template[0].containers[0].env,
    ]
  }
}

# Make Backend Cloud Run publicly accessible as a hybrid API gateway, secured internally by API keys and shared secrets
resource "google_cloud_run_v2_service_iam_member" "public_access" {
  location = google_cloud_run_v2_service.router_service.location
  name     = google_cloud_run_v2_service.router_service.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# 2. FRONTEND SERVICE (Dashboard Admin Portal)
resource "google_cloud_run_v2_service" "frontend_service" {
  depends_on = [
    google_project_service.apis,
    google_service_account.ui_sa,
    google_secret_manager_secret.app_secrets,
    google_secret_manager_secret_iam_member.ui_sa_secret_accessor
  ]

  name     = "${var.service_name}-ui"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.ui_sa.email

    containers {
      image = "gcr.io/cloudrun/placeholder" # Updated dynamically in build/deploy script

      ports {
        container_port = 8081
      }

      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = var.project_id
      }

      env {
        name  = "BACKEND_API_URL"
        value = google_cloud_run_v2_service.router_service.uri
      }

      env {
        name  = "PROXY_PUBLIC_URL"
        value = google_cloud_run_v2_service.router_service.uri
      }

      env {
        name  = "BACKEND_SERVICE_NAME"
        value = var.service_name
      }

      env {
        name  = "GEMINI_LOCATION"
        value = var.region
      }

      # The UI only needs the shared secret (decision-engine keys are backend-only).
      dynamic "env" {
        for_each = local.ui_sa_secrets
        content {
          name = env.value
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.app_secrets[env.value].secret_id
              version = "latest"
            }
          }
        }
      }

      # Inject Firebase Web SDK credentials
      env {
        name  = "FIREBASE_API_KEY"
        value = var.firebase_api_key
      }
      env {
        name  = "FIREBASE_AUTH_DOMAIN"
        value = var.firebase_auth_domain
      }
      env {
        name  = "FIREBASE_PROJECT_ID"
        value = var.project_id
      }
      env {
        name  = "FIREBASE_STORAGE_BUCKET"
        value = var.firebase_storage_bucket
      }
      env {
        name  = "FIREBASE_MESSAGING_SENDER_ID"
        value = var.firebase_messaging_sender_id
      }
      env {
        name  = "FIREBASE_APP_ID"
        value = var.firebase_app_id
      }
      env {
        name  = "ALLOWED_EMAIL_DOMAINS"
        value = var.allowed_email_domains
      }
      env {
        name  = "ALLOWED_AUTH_DOMAINS"
        value = var.allowed_email_domains
      }
      env {
        name  = "ALLOWED_DOMAINS"
        value = var.allowed_email_domains
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }
    }
  }

  lifecycle {
    ignore_changes = [
      client,
      client_version,
      template[0].containers[0].image,
      template[0].containers[0].env,
    ]
  }
}

# Make Frontend UI service publicly accessible for admin users
resource "google_cloud_run_v2_service_iam_member" "frontend_public_access" {
  location = google_cloud_run_v2_service.frontend_service.location
  name     = google_cloud_run_v2_service.frontend_service.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# Sign-in redirect hosts for Google Identity Platform / Firebase Auth.
# The project's authorized-domains list is shared by every app in the GCP project, so this repo
# no longer manages the whole project-wide list (google_identity_platform_config).
# It adds only its own Cloud Run hosts on create and removes only those on destroy, one resource per
# service (scripts/authorized-domains.sh). Email domains from allowed_email_domains are NOT redirect
# hosts and are only used for the IAM/app allowlist above.
locals {
  sign_in_hosts = {
    frontend = [for u in google_cloud_run_v2_service.frontend_service.urls : trimsuffix(trimprefix(u, "https://"), "/")]
    router   = [for u in google_cloud_run_v2_service.router_service.urls : trimsuffix(trimprefix(u, "https://"), "/")]
  }
}

resource "terraform_data" "authorized_domains" {
  for_each = local.sign_in_hosts

  triggers_replace = {
    project = var.project_id
    hosts   = join(" ", sort(each.value))
  }

  provisioner "local-exec" {
    command = "${path.module}/scripts/authorized-domains.sh add ${self.triggers_replace.project} ${self.triggers_replace.hosts}"
  }

  provisioner "local-exec" {
    when       = destroy
    on_failure = continue
    command    = "[ -x ${path.module}/scripts/authorized-domains.sh ] && ${path.module}/scripts/authorized-domains.sh remove ${self.triggers_replace.project} ${self.triggers_replace.hosts}"
  }

  depends_on = [google_project_service.apis]
}

# 3. TRAFFIC GENERATOR SERVICE (Targets the Backend Service endpoint for load simulation)
resource "google_cloud_run_v2_service" "generator_service" {
  depends_on = [
    google_project_service.apis,
    google_service_account.traffic_sa
  ]

  name     = "gemini-traffic-generator"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.traffic_sa.email

    annotations = {
      "run.googleapis.com/cpu-throttling" = "false"
    }

    containers {
      image = "gcr.io/cloudrun/placeholder"

      ports {
        container_port = 8080
      }

      env {
        name  = "ROUTER_URL"
        value = google_cloud_run_v2_service.router_service.uri
      }

      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = var.project_id
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }
    }
  }

  lifecycle {
    ignore_changes = [
      client,
      client_version,
      template[0].containers[0].image,
      template[0].containers[0].env,
      # Set by `gcloud run deploy --source` (deploy.sh); not managed here.
      build_config,
    ]
  }
}

# Allow our authorized email members to impersonate the router service account for OIDC token generation
resource "google_service_account_iam_member" "token_creator" {
  for_each           = toset(local.parsed_email_members)
  service_account_id = google_service_account.router_sa.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = each.value
}
