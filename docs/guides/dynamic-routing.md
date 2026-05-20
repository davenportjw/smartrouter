# 🧠 Routing Configuration & Auditing

The Smart Router provides **Complexity-Based Routing** and **Declarative Rules-Based Routing** to manage Gemini API requests.

---

## 🚀 1. Complexity-Based Routing (`gemini-dynamic`)

Clients can target the virtual model **`gemini-dynamic`** instead of hardcoding model names:

```http
POST http://localhost:8080/v1/models/gemini-dynamic:generateContent?key=gr_key_abc
```

### Query Classification
The router evaluates prompt size at runtime to target a model:
* **Simple prompts** (short text) -> Routed to `gemini-2.5-flash-lite` (lowest cost).
* **Medium prompts** (medium length) -> Routed to `gemini-2.5-flash` (balanced).
* **Complex prompts** (large size, images, videos, or tools/functions) -> Routed to `gemini-2.5-pro` (highest reasoning).

### Configuration Thresholds
Define character count thresholds in the dashboard (`/admin/complexity`):
```json
{
  "simple_threshold": 500,
  "medium_threshold": 5000,
  "simple_model": "gemini-2.5-flash-lite",
  "medium_model": "gemini-2.5-flash",
  "complex_model": "gemini-2.5-pro"
}
```

---

## ⚙️ 2. Declarative Rules-Based Routing

Enforce overrides based on Client tier, Application ID, or custom HTTP headers.

### Routing Rules Example
If a `premium` client sends a request to `gemini-2.5-flash` with the header `X-Route-Priority: gold`, upgrade their target model to `gemini-2.5-pro`:
```json
{
  "id": "rule-premium-upgrade",
  "model_pattern": "gemini-2.5-flash",
  "app_id": "all",
  "client_tier": "premium",
  "header_name": "X-Route-Priority",
  "header_value": "gold",
  "target_model": "gemini-2.5-pro",
  "target_location": "us-central1",
  "fallback_model": "gemini-2.5-flash",
  "priority_weight": 10
}
```

---

## 🌐 3. Regional Routing Compatibility

The Smart Router steers traffic using regional levels:
1. **Specific regions** (e.g., `us-central1`) - Level 1 (Smallest)
2. **Multi-regions** (e.g., `us`, `eu`) - Level 2
3. **Global** (`global`) - Level 3 (Broadest)

* If the router is in `us-central1` and the model is at `us`, the proxy routes to `us`.
* If the model is in `us-central1` and the router is in `us`, the proxy routes to `us-central1`.
* Incompatible routes (e.g., EU router to a strictly US model) are blocked unless overridden.

### Verification & Model Disabling
Discovered models (via `/admin/models/refresh`) are checked via content generation pings. 
* The most specific verified location is saved as the model's operational location.
* Models failing verification at all candidate locations are set to `Active = false`.

### Host and Path Rewriting
* Intercepts and rewrites the request host to: `{location}-aiplatform.googleapis.com`
* Intercepts and rewrites the path to: `/v1/projects/{project}/locations/{location}/publishers/google/models/...`

---

## 📊 4. Auditing & Monitoring

The Smart Router adds custom audit headers on all proxied HTTP responses:

| HTTP Response Header | Description | Example |
| :--- | :--- | :--- |
| **`X-Requested-Model`** | Model requested by the client | `gemini-dynamic` |
| **`X-Routed-Model`** | Upstream model targeted | `gemini-2.5-flash-lite` |
| **`X-Routed-Model-Location`** | Resolved serving location | `us` |
| **`X-Client-Tier`** | Client subscription tier | `premium` |
| **`X-App-ID`** | Calling Application ID | `customer-facing-bot` |

### Example Response
```http
HTTP/1.1 200 OK
Content-Type: application/json
X-Requested-Model: gemini-dynamic
X-Routed-Model: gemini-2.5-flash-lite
X-Client-Tier: standard
X-App-ID: mobile-chat-app
```
