# 🧠 Dynamic Routing Configuration & Auditing

Dynamic routing optimizes Gemini API spend and reliability across two distinct axes: **Complexity-Based Routing** and **Declarative Rules-Based Routing**.

---

## 🚀 1. Complexity-Based Routing (`gemini-dynamic`)

Complexity-based routing lets client services target a virtual model name **`gemini-dynamic`** instead of hardcoding physical models:

```http
POST http://localhost:8080/v1/models/gemini-dynamic:generateContent?key=gr_key_abc
```

### How the Proxy Classifies Queries
The router automatically evaluates the prompt at runtime:
* **Simple queries** (e.g., below character limits, short text) -> Routed to `gemini-2.5-flash-lite` (fastest, cheapest).
* **Medium queries** (medium length text, standard prompts) -> Routed to `gemini-2.5-flash` (balanced performance/cost).
* **Complex queries** (large prompts, images, video files, or active tool/function declarations) -> Routed to `gemini-2.5-pro` (advanced reasoning).

### Setting Up Complexity Mappings
Operators define characters count thresholds dynamically in the dashboard (`/admin/complexity`) or seed them in the config DB:
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

Rules-based routing enforces overrides and conditional upgrades based on Client tier, Application ID, and custom HTTP headers.

### Seeding Routing Rules (`local_db.json` or Firestore)
Create dynamic overrides by registering rule JSON profiles:

#### Example: VIP Client Model Upgrades
If a `premium` client sends a request to `gemini-2.5-flash` and provides the header `X-Route-Priority: gold`, dynamically upgrade their target to `gemini-2.5-pro`:
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

---

## 🌐 3. Regional & Multiregional Model Compatibility Routing

To align with standard enterprise security architectures and lower latencies, the Smart Router dynamically regulates and steers traffic based on the serving location of your physical model deployments.

### Serving Location Compatibility Rules
When a router is instantiated in a primary serving region (configured via `GEMINI_LOCATION`), operators can only route traffic to models residing in:
1. **The Local Region itself**: e.g., `us-central1` (lowest latency, ideal for data sovereignty).
2. **The Parent Multiregion parent**: e.g., `us` if the router is deployed in a `us-` region, or `eu` if deployed in a `europe-` region.
3. **The Global Baseline**: `global` (for standard pre-seeded baseline foundation models).

Incompatible routes are filtered out and prevented at both the configuration and routing layers, guaranteeing that EU traffic stays within EU boundaries (e.g., routed from an EU router strictly to EU models).

### Dynamic Host & Path Rewriting in the Proxy
When a request is dynamically routed to a compatible model in a different serving region (for instance, routing to a custom tuned model in the parent `us` multiregion from a `us-central1` router):
- The reverse proxy automatically intercepts and rewrites the request hostname:
  `{model_location}-aiplatform.googleapis.com`
- The location path component is dynamically updated to target `modelLoc` instead of the default `rp.Location`:
  `/v1/projects/{project}/locations/{model_location}/publishers/google/models/...`
- The Google OAuth2 access token remains authenticated across all regional endpoints.

---

## 📊 4. Monitoring & Auditing Decisions Natively

The Smart Router injects custom audit headers on all proxied HTTP responses. Client applications should extract and log these headers to audit routing decisions in real time:

| HTTP Response Header | Description | Value Example |
| :--- | :--- | :--- |
| **`X-Requested-Model`** | The model name requested by the client. | `gemini-dynamic` |
| **`X-Routed-Model`** | The actual upstream physical model targeted. | `gemini-2.5-flash-lite` |
| **`X-Routed-Model-Location`** | The resolved serving location of the routed model. | `us` |
| **`X-Client-Tier`** | Resolved client subscription tier. | `premium` |
| **`X-App-ID`** | Resolved calling Application context ID. | `customer-facing-bot` |

### Example: Reading Headers with `curl`
```bash
curl -i -X POST "http://localhost:8080/v1/models/gemini-dynamic:generateContent?key=gr_key_abc" \
  -H "Content-Type: application/json" \
  -d '{"contents": [{"parts":[{"text": "Hello"}]}]}'
```

**Outbound Response Headers Output**:
```http
HTTP/1.1 200 OK
Content-Type: application/json
X-Requested-Model: gemini-dynamic
X-Routed-Model: gemini-2.5-flash-lite
X-Client-Tier: standard
X-App-ID: mobile-chat-app
```
