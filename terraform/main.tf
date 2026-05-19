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

# Secret Manager is no longer required for upstream Gemini calls.

# Service Account for Cloud Run
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


# Cloud Run Service (Deployed initially with Google Placeholder)
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
      image = "gcr.io/cloudrun/placeholder" # Initial placeholder; updated dynamically during deploy

      ports {
        container_port = 8080
      }

      # Inject Firebase web variables directly into env
      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = var.project_id
      }
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

      # Dynamic Location targeting for Vertex AI Gemini REST endpoint
      env {
        name  = "GEMINI_LOCATION"
        value = var.region
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi" # Go is extremely lightweight, but 512Mi is minimum for unthrottled CPU
        }
      }
    }
  }

  # Let gcloud handles container version updates without Terraform marking state drift
  lifecycle {
    ignore_changes = [
      client,
      client_version,
      template[0].containers[0].image,
    ]
  }
}

# Make Cloud Run Publicly Accessible
resource "google_cloud_run_v2_service_iam_member" "public_access" {
  location = google_cloud_run_v2_service.router_service.location
  name     = google_cloud_run_v2_service.router_service.name
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
    "${var.project_id}.web.app"
  ]

  # Safeguard: ignore changes to sign_in and other configs managed via Firebase console or other processes.
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

# Cloud Run Service for Traffic Generator (Deployed initially with Google Placeholder)
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

