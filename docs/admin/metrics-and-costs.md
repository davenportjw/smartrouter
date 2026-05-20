# 📊 Metrics & Cost Analytics

The Smart Router includes tools to monitor traffic, response latency, error rates, and model cost spend.

---

## 📈 Traffic & Latency Metrics

Navigate to `/admin/metrics` on the dashboard. (In local dev mode, mock data is displayed).

### Metrics Tracked
1. **Throughput**: Total requests, broken down by Application ID.
2. **Latency (p50, p95, p99)**: Proxy response time in milliseconds.
3. **Error Rates**: Non-200 HTTP codes:
   * `400 Bad Request`: Header validation failures or invalid payloads.
   * `401 Unauthorized`: Invalid or revoked API keys.
   * `429 Too Many Requests`: Rate limit blocks.
   * `5xx Server Error`: Upstream Vertex AI failures.
4. **Model Distribution**: Percentage of requests routed to each model.

---

## 💰 Token Spend & Cost Analytics

Navigate to `/admin/costs` on the dashboard. Cost values are computed using actual input and output token counts based on the following pricing:

| Model | Input Price (per 1M tokens) | Output Price (per 1M tokens) |
| :--- | :--- | :--- |
| **`gemini-2.5-pro`** | $1.25 | $5.00 |
| **`gemini-2.5-flash`** | $0.075 | $0.30 |
| **`gemini-2.5-flash-lite`** | $0.0375 | $0.15 |
| **`text-embedding-004`** | $0.025 | $0.00 |
| **`multimodal-embedding-001`** | $0.025 | $0.00 |

### Analytics Views
1. **App Spend**: Financial spend broken down by Application ID.
2. **Client Tier**: Total spend mapped to Client subscription tiers (`premium`, `standard`, `free`).
3. **Routing Efficiency**: Calculated financial savings achieved by dynamic complexity-based routing (e.g., the price difference when a simple prompt is routed to `gemini-2.5-flash-lite` instead of a Pro model).
