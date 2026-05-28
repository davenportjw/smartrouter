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
    "aiplatform.googleapis.com"
  ]

  # Parse ALLOWED_EMAIL_DOMAINS to convert domains and specific emails to IAM members
  parsed_email_members = [
    for entry in split(",", var.allowed_email_domains) :
    length(split("@", trimspace(entry))) > 1 ? "user:${trimspace(entry)}" : "domain:${trimspace(entry)}"
    if trimspace(entry) != ""
  ]
}

resource "google_project_service" "apis" {
  for_each = toset(local.services)
  service  = each.value

  disable_on_destroy = false
}

# Firestore Database Native Instance
resource "google_firestore_database" "database" {
  depends_on  = [google_project_service.apis]
  name        = "(default)"
  location_id = var.region
  type        = "FIRESTORE_NATIVE"

  # Prevent accidental deletion of Firestore database in production
  deletion_policy = "DELETE"
}

# Service Account for Cloud Run Services
resource "google_service_account" "router_sa" {
  depends_on   = [google_project_service.apis]
  account_id   = "gemini-router-runner"
  display_name = "Gemini Smart Router Cloud Run Service Account"
}

# IAM: Vertex AI User Access to invoke Gemini API models
resource "google_project_iam_member" "vertex_ai_access" {
  project = var.project_id
  role    = "roles/aiplatform.user"
  member  = "serviceAccount:${google_service_account.router_sa.email}"
}

# IAM: Firestore Access
resource "google_project_iam_member" "firestore_access" {
  project = var.project_id
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.router_sa.email}"
}

# IAM: Cloud Monitoring Viewer
resource "google_project_iam_member" "monitoring_access" {
  project = var.project_id
  role    = "roles/monitoring.viewer"
  member  = "serviceAccount:${google_service_account.router_sa.email}"
}

# IAM: Cloud Logging Viewer
resource "google_project_iam_member" "logging_access" {
  project = var.project_id
  role    = "roles/logging.viewer"
  member  = "serviceAccount:${google_service_account.router_sa.email}"
}

# IAM: Firebase Auth Admin Access for backend session validation
resource "google_project_iam_member" "firebase_auth_access" {
  project = var.project_id
  role    = "roles/firebaseauth.admin"
  member  = "serviceAccount:${google_service_account.router_sa.email}"
}

# 1. BACKEND SERVICE (Gemini Router API and Administrative Configuration APIs)
resource "google_cloud_run_v2_service" "router_service" {
  depends_on = [
    google_project_service.apis,
    google_service_account.router_sa
  ]

  name     = var.service_name
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.router_sa.email

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

      env {
        name  = "BACKEND_SHARED_SECRET"
        value = var.backend_shared_secret
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
    ]
  }
}

# Make Backend Cloud Run accessible only to our authorized service account (UI / Generator) and authorized email members
resource "google_cloud_run_v2_service_iam_member" "public_access" {
  for_each = toset(concat(
    ["serviceAccount:${google_service_account.router_sa.email}"],
    local.parsed_email_members
  ))

  location = google_cloud_run_v2_service.router_service.location
  name     = google_cloud_run_v2_service.router_service.name
  role     = "roles/run.invoker"
  member   = each.value
}

# 2. FRONTEND SERVICE (Dashboard Admin Portal)
resource "google_cloud_run_v2_service" "frontend_service" {
  depends_on = [
    google_project_service.apis,
    google_service_account.router_sa
  ]

  name     = "${var.service_name}-ui"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.router_sa.email

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
        name  = "BACKEND_SERVICE_NAME"
        value = var.service_name
      }

      env {
        name  = "GEMINI_LOCATION"
        value = var.region
      }

      env {
        name  = "BACKEND_SHARED_SECRET"
        value = var.backend_shared_secret
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

# Manage Authorized Domains for Google Identity Platform / Firebase Auth
resource "google_identity_platform_config" "auth_config" {
  project = var.project_id

  authorized_domains = [
    "localhost",
    "google.com",
    "cloudadvocacyorg.joonix.net",
    "gemini-smart-router-txgsracloq-uc.a.run.app",
    "gemini-smart-router-834476222725.us-central1.run.app",
    "${var.project_id}.firebaseapp.com",
    "${var.project_id}.web.app",
    replace(replace(google_cloud_run_v2_service.frontend_service.uri, "https://", ""), "/", ""),
    replace(replace(google_cloud_run_v2_service.router_service.uri, "https://", ""), "/", "")
  ]

  lifecycle {
    ignore_changes = [
      autodelete_anonymous_users,
      blocking_functions,
      client,
      mfa,
      monitoring,
      multi_tenant,
      quota,
      sign_in,
      sms_region_config
    ]
  }

  depends_on = [
    google_project_service.apis
  ]
}

# 3. TRAFFIC GENERATOR SERVICE (Targets the Backend Service endpoint for load simulation)
resource "google_cloud_run_v2_service" "generator_service" {
  depends_on = [
    google_project_service.apis,
    google_service_account.router_sa
  ]

  name     = "gemini-traffic-generator"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.router_sa.email

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
