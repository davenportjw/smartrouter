variable "project_id" {
  type        = string
  description = "The Google Cloud Project ID where resources will be provisioned."
}

variable "region" {
  type        = string
  default     = "us-central1"
  description = "The GCP region where the Cloud Run service will be deployed."
}

variable "service_name" {
  type        = string
  default     = "gemini-smart-router"
  description = "The name of the Cloud Run service."
}

# Firebase Client configs passed to container env
variable "firebase_api_key" {
  type        = string
  description = "Firebase Web Client API Key."
}

variable "firebase_auth_domain" {
  type        = string
  description = "Firebase Web Client Auth Domain."
}

variable "firebase_storage_bucket" {
  type        = string
  description = "Firebase Web Client Storage Bucket."
}

variable "firebase_messaging_sender_id" {
  type        = string
  description = "Firebase Web Client Messaging Sender ID."
}

variable "firebase_app_id" {
  type        = string
  description = "Firebase Web Client App ID."
}

variable "allowed_email_domains" {
  type        = string
  default     = ""
  description = "Comma-separated list of email domains or specific email addresses authorized to sign in to the admin dashboard."
}

variable "backend_shared_secret" {
  type        = string
  default     = ""
  sensitive   = true
  description = "The shared secret key for internal administrative backend REST API calls. Sourced from .env if not explicitly set."
}

variable "jev_api_key" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Optional API key for the JEV (Jigsaw Embedding Vector) classifier. Sourced from .env if not explicitly set."
}

variable "fastino_api_key" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Optional API key for the Fastino Pioneer GLiNER-2 classifier. Sourced from .env if not explicitly set."
}

variable "clm_api_key" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Optional API key for the Contrastive Language Model (CLM) classifier. Sourced from .env if not explicitly set."
}

variable "dotenv_file" {
  type        = string
  default     = "../.env"
  description = "Deprecated: Secret versions are populated out-of-band via gcloud secrets versions add to avoid storing plaintext secrets in Terraform state."
}

variable "enabled_secret_mounts" {
  type        = list(string)
  default     = []
  description = "Optional list of Secret Manager secret IDs (e.g. BACKEND_SHARED_SECRET, JEV_API_KEY, FASTINO_API_KEY, CLM_API_KEY) that have active versions and should be mounted by Terraform."
}

