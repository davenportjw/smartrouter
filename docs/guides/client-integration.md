# 🔌 Client Integration Guide

This guide explains the request formats and headers needed to authenticate with the Smart Router.

The proxy supports two authentication methods: **Static API Key** and **Zero-Key IAM Service Account OIDC**.

---

## 🔑 1. Static API Key Authentication

### Request Requirements
* Send the custom router API key (obtained from `/admin/keys`) in the **`x-goog-api-key`** HTTP header, or as a query parameter `?key=...`.
* Target the standard Google Gemini path structure.

### HTTP Header Example
```bash
curl -X POST "http://localhost:8080/v1/models/gemini-2.5-flash:generateContent" \
  -H "x-goog-api-key: gr_key_your_custom_api_key" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{
      "parts": [{"text": "Summarize this text."}]
    }]
  }'
```

### HTTP Query Parameter Example (Compatible with Official SDKs)
```bash
curl -X POST "http://localhost:8080/v1/models/gemini-dynamic:generateContent?key=gr_key_your_custom_api_key" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{"parts": [{"text": "Hello"}]}]
  }'
```

---

## 🛡️ 2. Zero-Key IAM Service Account OIDC

OIDC tokens eliminate the need to distribute, rotate, or store static API secrets for applications running inside Google Cloud.

### Flow
1. The client microservice runs under a Google Service Account.
2. The client queries the local GCP Metadata Server to fetch an OIDC identity token with the Smart Router URL as the audience.
3. The client injects the OIDC token into the `Authorization: Bearer <token>` header.
4. The Smart Router verifies the token signature and maps the Service Account ID directly to its registered `App` context.

### Calling the Proxy from a GCP Environment
```bash
# 1. Retrieve OIDC Identity Token from local Metadata Server
OIDC_TOKEN=$(curl -H "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=https://your-smart-router.run.app")

# 2. Call the proxy with the OIDC token
curl -X POST "https://your-smart-router.run.app/v1/models/gemini-dynamic:generateContent" \
  -H "Authorization: Bearer $OIDC_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{
      "parts": [{"text": "Explain Zero-Key auth."}]
    }]
  }'
```
