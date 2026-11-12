# Dedicated, least-privilege service accounts (one per Cloud Run service).
#
# The three Cloud Run services previously shared gemini-router-runner. Each now runs as its own SA:
#   - smart-router-api-sa      -> gemini-smart-router        (backend: Vertex AI proxy + Firestore config store)
#   - smart-router-ui-sa       -> gemini-smart-router-ui     (admin UI: Firebase session cookies, Vertex model
#                                                             listing/verification, Cloud Logging reads)
#   - smart-router-traffic-sa  -> gemini-traffic-generator   (calls the backend with app API keys; no Google APIs)
#
# gemini-router-runner (google_service_account.router_sa in main.tf) is kept, not deleted: operators
# impersonate it to mint OIDC tokens for the admin API (cmd/verify), and a GKE Workload Identity binding
# still references it. It is listed in API_ALLOWED_CALLERS for that reason.
# These resources were first created with gcloud and then imported into state with `terraform import`.
# main.tf points each Cloud Run service at its SA and wires the backend
# API_OIDC_AUDIENCES / API_ALLOWED_CALLERS env vars from the locals below.

locals {
  api_sa_project_roles = toset([
    "roles/aiplatform.user",                   # Vertex AI generateContent proxying
    "roles/datastore.user",                    # Firestore config store
    "roles/serviceusage.serviceUsageConsumer", # Quota check for Gemini Decisions API & API Keys v2
    "roles/serviceusage.apiKeysViewer"         # ADC lookup of project Authorization Key restricted to generativelanguage.googleapis.com
  ])

  ui_sa_project_roles = toset([
    "roles/firebaseauth.admin",                # createSessionCookie / VerifySessionCookie
    "roles/aiplatform.user",                   # model/endpoint listing + model verification calls
    "roles/logging.viewer",                    # dashboard request logs (logging entries:list)
    "roles/serviceusage.serviceUsageConsumer", # Quota check for live Decisions inspect
    "roles/serviceusage.apiKeysViewer"         # ADC lookup of project Authorization Key restricted to generativelanguage.googleapis.com
  ])

  # Only secrets each service actually mounts (the backend may also mount decision-engine keys).
  api_sa_secrets = setunion(toset(["BACKEND_SHARED_SECRET"]), local.mounted_secret_ids)
  ui_sa_secrets  = toset(["BACKEND_SHARED_SECRET"])
}

resource "google_service_account" "api_sa" {
  depends_on   = [google_project_service.apis]
  account_id   = "smart-router-api-sa"
  display_name = "Smart Router backend API (Cloud Run)"
}

resource "google_service_account" "ui_sa" {
  depends_on   = [google_project_service.apis]
  account_id   = "smart-router-ui-sa"
  display_name = "Smart Router admin UI (Cloud Run)"
}

resource "google_service_account" "traffic_sa" {
  depends_on   = [google_project_service.apis]
  account_id   = "smart-router-traffic-sa"
  display_name = "Smart Router traffic generator (Cloud Run)"
}

resource "google_project_iam_member" "api_sa_roles" {
  for_each = local.api_sa_project_roles
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.api_sa.email}"
}

resource "google_project_iam_member" "ui_sa_roles" {
  for_each = local.ui_sa_project_roles
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.ui_sa.email}"
}

resource "google_secret_manager_secret_iam_member" "api_sa_secret_accessor" {
  for_each  = local.api_sa_secrets
  project   = var.project_id
  secret_id = google_secret_manager_secret.app_secrets[each.value].secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.api_sa.email}"
}

resource "google_secret_manager_secret_iam_member" "ui_sa_secret_accessor" {
  for_each  = local.ui_sa_secrets
  project   = var.project_id
  secret_id = google_secret_manager_secret.app_secrets[each.value].secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.ui_sa.email}"
}

# Backend run.invoker for the callers that mint OIDC tokens for it (the backend is also public via
# allUsers for API-key traffic; these keep working if that binding is ever removed).
resource "google_cloud_run_v2_service_iam_member" "backend_invokers" {
  for_each = {
    ui      = "serviceAccount:${google_service_account.ui_sa.email}"
    traffic = "serviceAccount:${google_service_account.traffic_sa.email}"
    runner  = "serviceAccount:${google_service_account.router_sa.email}"
  }
  location = google_cloud_run_v2_service.router_service.location
  name     = google_cloud_run_v2_service.router_service.name
  role     = "roles/run.invoker"
  member   = each.value
}

# Admin API OIDC pinning (backend env, wired in main.tf): token aud must be one of these URLs and the
# verified email claim must be one of these callers. The audience list cannot reference
# router_service itself (cycle), so it is the deterministic <service>-<project-number>.<region>.run.app
# URL plus any extra URLs (e.g. the hashed *.a.run.app URL) passed via var.api_oidc_extra_audiences.
data "google_project" "this" {
  project_id = var.project_id
}

variable "api_oidc_extra_audiences" {
  type        = string
  default     = ""
  description = "Comma-separated extra backend URLs accepted as OIDC audiences (e.g. the hashed https://<service>-<hash>-uc.a.run.app URL)."
}

locals {
  api_oidc_audiences = join(",", distinct(compact(concat(
    [for u in split(",", var.api_oidc_extra_audiences) : trimspace(u)],
    ["https://${var.service_name}-${data.google_project.this.number}.${var.region}.run.app"],
  ))))
  api_allowed_callers = join(",", [
    google_service_account.ui_sa.email,
    google_service_account.router_sa.email,
  ])
}

output "api_oidc_audiences" {
  value       = local.api_oidc_audiences
  description = "Value for the backend API_OIDC_AUDIENCES env var."
}

output "api_allowed_callers" {
  value       = local.api_allowed_callers
  description = "Value for the backend API_ALLOWED_CALLERS env var."
}
