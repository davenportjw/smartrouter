output "router_url" {
  value       = google_cloud_run_v2_service.router_service.uri
  description = "The public endpoint URL of the Gemini Smart Router service."
}

output "managed_secret_ids" {
  value       = [for k, s in google_secret_manager_secret.app_secrets : s.secret_id]
  description = "Google Cloud Secret Manager secret IDs provisioned for the Smart Router."
}

