output "router_url" {
  value       = google_cloud_run_v2_service.router_service.uri
  description = "The public endpoint URL of the Gemini Smart Router service."
}
