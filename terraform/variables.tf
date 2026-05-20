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
  default     = "google.com,cloudadvocacyorg.joonix.net"
  description = "Comma-separated list of email domains authorized to sign in to the admin dashboard."
}
