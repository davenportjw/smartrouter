# 🔌 Client Integration Runbook

This guide details the exact request schemas, curl commands, and header specs required to authenticate and call the Smart Router.

The proxy supports two authentication methods: **Static API Key** and **Zero-Key IAM Service Account OIDC**.

---

## 🔑 1. Method A: Static API Key Authentication

Perfect for traditional external web apps, mobile applications, or standalone worker scripts.

### Request Protocol Requirements
* Send your custom hashed router API key (obtained from `/admin/keys`) in the **`x-goog-api-key`** HTTP header, or as a standard query parameter `?key=...`.
* Target the standard Google Gemini path structure.

### HTTP Header Authentication Example
```bash
curl -X POST "http://localhost:8080/v1/models/gemini-2.5-flash:generateContent" \
  -H "x-goog-api-key: gr_key_your_custom_api_key" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{
      "parts": [{"text": "Summarize the importance of core architectural consistency."}]
    }]
  }'
```

### HTTP Query Parameter Authentication Example (Compatible with Official SDKs)
If you use official Google GenAI SDKs, simply configure the client's base endpoint to target your Smart Router service and supply the key parameter:
```bash
curl -X POST "http://localhost:8080/v1/models/gemini-dynamic:generateContent?key=gr_key_your_custom_api_key" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{"parts": [{"text": "Hello"}]}]
  }'
```

---

## 🛡️ 2. Method B: Zero-Key IAM Service Account OIDC

Designed for secure Cloud Native microservices running inside Google Cloud. Eliminates the need to distribute, rotate, or store static API secrets.

### How it Works
1. The client microservice runs under a Google Service Account.
2. The client queries the local GCP Metadata Server to fetch a secure **OIDC identity token** targeted for your Cloud Run smart router audience context.
3. The client injects the OIDC token into the standard HTTP `Authorization: Bearer <token>` header.
4. The Smart Router verifies the token signature natively and maps the Service Account ID directly to its registered `App` context.

### Calling the Proxy via `curl` from a GCP Environment
```bash
# 1. Retrieve OIDC Identity Token from local Metadata Server
OIDC_TOKEN=$(curl -H "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=https://your-smart-router.run.app")

# 2. Dispatch request targeting the proxy with zero static keys
curl -X POST "https://your-smart-router.run.app/v1/models/gemini-dynamic:generateContent" \
  -H "Authorization: Bearer $OIDC_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{
      "parts": [{"text": "Explain Zero-Key auth."}]
    }]
  }'
```
