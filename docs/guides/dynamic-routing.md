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

To align with standard enterprise security architectures, Lower Latencies, and Data Residency requirements, the Smart Router dynamically regulates and steers traffic using the **most specific compatible regional boundary available**.

### Serving Location Compatibility & Coercion Rules
When a router is instantiated in a primary serving region (configured via `GEMINI_LOCATION`, e.g. `us-central1`), operators can route traffic to models residing in compatible locations. However, to maximize reliability and align routing with verified operational bounds, **the reverse proxy always routes to the model's verified operational location, avoiding unsafe downscaling to a specific region if not explicitly verified**:
1. **Specific regional locations** (e.g., `us-central1`) are Level 1 (Smallest).
2. **Multi-regions** (e.g., `us`, `eu`) are Level 2.
3. **Global baselines** (`global`) are Level 3 (Broadest).

For example:
* If the router is in `us-central1` (Level 1) and the target model has its verified/operational location at `us` (Level 2), the proxy routes to **`us`** (rather than downscaling to `us-central1` where it might be unavailable) to ensure the call succeeds.
* If the model is registered at `us-central1` (Level 1) and the router is in `us` (Level 2), they are compatible and the proxy routes to **`us-central1`** as it is supported by the model and is within the router's geographic serving boundary.
* If the model is pinned to `europe-west9` (Level 1) and the router is in `us` (Level 2), they are incompatible. The proxy preserves the model's location **`europe-west9`** to ensure routing succeeds.

Incompatible routes (e.g., routing from an EU router strictly to a model residing in a `us-` region) are filtered out and prevented, guaranteeing that regional boundaries are enforced.

### Exact Location Extraction & Active Queryability Verification
When refreshing custom models, endpoints, and publisher models via `/admin/models/refresh`:
- For **custom models and endpoints**, the Smart Router extracts the exact regional code from their GCP resource path (e.g., `projects/{project}/locations/us-central1/models/custom-weights` -> `us-central1`).
- For **publisher (foundation) models** (which do not embed a location in their path), the router dynamically resolves and validates the correct location on refresh. It tests all compatible candidate locations in order of specificity (e.g., local region `us-central1` -> parent multiregion `us` -> `global`).
- **Queryability & Verification Loop**: Every discovered model undergoes active queryability verification via Vertex AI content generation pings. The most specific candidate location that successfully passes verification is selected and saved as the model's operational location (correcting any outdated or misconfigured "local" locations to their proper multiregion/regional bounds, such as correcting `gemini-3.5-flash` to `us`).
- **Automatic Model Disabling**: If a model fails verification at all candidate locations (indicating it is no longer queryable, deprecated, or restricted in the target GCP environment), the Smart Router automatically disables the model by setting its state to inactive (`Active = false`) rather than routing traffic to a broken endpoint.

### Dynamic Host & Path Rewriting in the Proxy
When a request is dynamically routed to a model:
- The reverse proxy automatically intercepts and rewrites the request hostname using the resolved smallest compatible location:
  `{resolved_location}-aiplatform.googleapis.com`
- The location path component is dynamically updated to target the resolved location:
  `/v1/projects/{project}/locations/{resolved_location}/publishers/google/models/...`
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
