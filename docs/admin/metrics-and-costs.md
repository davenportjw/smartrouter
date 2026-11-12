# 📊 Metrics & Cost Analytics

The Smart Router includes tools to monitor traffic, response latency, error rates, and model cost spend.

---

## 📈 Traffic & Latency Metrics

Navigate to `/admin/metrics` on the dashboard. All metrics and cost analytics are queried directly from the proxy's structured telemetry logs (`StructuredLog` entries in Google Cloud Logging or `local_logs.jsonl` in `LOCAL_DEV` mode), scoped to the backend proxy service (`BACKEND_SERVICE_NAME`, defaulting to `gemini-smart-router`). No synthetic multipliers or mock data are generated; if no traffic has been recorded yet, the dashboard renders a clean empty state.

### Telemetry Coverage & Calculation
* **End-to-End Proxy Coverage**: Every proxy request path—including Vertex AI / Gemini upstream calls, direct node endpoints, pull-queue cluster dispatches, and early HTTP error rejections (`400`, `401`, `403`, `429`, `502`, `503`, `504`)—emits a `StructuredLog` entry with `status`, `latency_ms`, `bytes_sent`, and `model_routed`. Lightweight `/v1/models` metadata lookups are excluded so discovery polling does not skew inference latency.
* **True 24-Hour P95 Latency**: Top-level 24-hour P95 latency is computed as the true 95th percentile across all request latencies recorded in the 24-hour window (or volume-weighted across hourly series when parsing Cloud Monitoring time series), preventing single-request timeout hours from distorting the 24-hour P95 KPI. Hourly chart series plot true per-hour median (P50) and 95th percentile (P95) latencies.

### Metrics Tracked
1. **Throughput**: Total requests and peak hourly request rate over the last 24 hours.
2. **Latency (p50, p95)**: True per-hour P50/P95 chart series and overall 24-hour P95 proxy response time in milliseconds.
3. **Error Rates**: Percentage of `4xx` and `5xx` HTTP responses across all proxy paths:
   * `400 Bad Request`: Header validation failures or invalid payloads.
   * `401 Unauthorized` / `403 Forbidden`: Invalid, unauthorized, or revoked API keys.
   * `429 Too Many Requests`: Rate limit blocks.
   * `5xx Server Error`: Upstream Vertex AI, direct endpoint, or cluster queue timeouts/failures.
4. **Model Distribution & Average Latency**: Per-model request counts, average latency (`latency_ms`), and token spend on `/admin/models`.

---

## 💰 Token Spend & Cost Analytics

Navigate to `/admin/costs` (or inspect per-model unit rates and accumulated spend on `/admin/models`) on the dashboard. Both views share a single deterministic pricing tier table (`GetModelPricing`) per 1M input and output tokens:

| Model / Tier | Input Price (per 1M tokens) | Output Price (per 1M tokens) |
| :--- | :--- | :--- |
| **`gemini-4-argon` / `gemini-4-pro` / `gemini-4`** | \$2.00 | \$10.00 |
| **`gemini-3.8-flash`** | \$0.15 | \$0.60 |
| **`gemini-3.7-flash` / `gemini-3.6-flash` / `gemini-3.5-flash`** | \$0.10 | \$0.40 |
| **`gemini-3.5-flash-lite` / `gemini-3.1-flash-lite`** | \$0.05 | \$0.20 |
| **`gemini-3.1-pro` / `gemini-3-pro`** | \$1.50 | \$6.00 |
| **`gemini-2.5-pro`** | \$1.25 | \$5.00 |
| **`gemini-2.5-flash`** | \$0.075 | \$0.30 |
| **`gemini-2.5-flash-lite`** | \$0.0375 | \$0.15 |
| **`gemma-4-*` (`@local` / Local Cluster)** | \$0.00 (Local) | \$0.00 (Local) |
| **`gemma-4-*` (Cloud / MaaS)** | \$0.15 | \$0.60 |
| **`gemini-decision-1` (Gemini Decisions API)** | \$0.10 | \$0.00 (Zero output tokens) |
| **`text-embedding-*` / `multimodal-embedding-*`** | \$0.025 | \$0.00 |

### Analytics Views
1. **App Spend**: Financial spend broken down by Application ID.
2. **Client Tier**: Total spend mapped to Client subscription tiers (`premium`, `standard`, `free`).
3. **Routing Efficiency**: Calculated financial savings achieved by dynamic complexity-based routing (e.g., the price difference when a simple prompt is routed to `gemini-3.5-flash-lite` instead of a Pro model).
