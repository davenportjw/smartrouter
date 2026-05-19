# 📊 Performance & Spend Auditing

Monitor system health, evaluate real-time throughput volume, assess response latency distributions, track error metrics, and audit financial token spend.

---

## 📈 User Flow 6: Auditing Real-Time Production Traffic

Use the Metrics dashboard to track proxy throughput, check response latency curves, and identify API errors before they impact downstream client services.

### Step 1: Open the Performance Monitor
* Navigate to `/admin/metrics` on the dashboard.
* The dashboard hooks directly into **Google Cloud Monitoring REST APIs** (in local development mode, it generates rich mock data to enable fast offline evaluation).

### Step 2: Evaluate Traffic Metrics
1. **Throughput Graph**: Inspect total active requests, and split volume by Application ID to locate high-demand services.
2. **Latency Curve (p50, p95, p99)**: Monitor proxy response latency (measured in milliseconds). Spikes in `p99` indicate upstream network bottlenecks or queuing delays.
3. **Error Spikes Table**: Track the frequency of non-200 responses. Locate where errors originate:
   - `400 Bad Request`: Client header validation failures or bad prompt payloads.
   - `401 Unauthorized`: Revoked or invalid API keys.
   - `429 Too Many Requests`: Application rate limits (RPM/TPM) actively throttling clients.
   - `5xx Server Error`: Upstream Vertex AI or network issues.
4. **Endpoint Distribution**: Check the percentage distribution of routed foundation models. Visualizes how smart routing optimizes models (e.g., routing 75% of calls to `gemini-2.5-flash-lite` and upgrading only 25% to `gemini-2.5-pro`).

---

## 💰 User Flow 7: Spend Allocation & Cost Tracking

The Cost Analytics page computes financial metrics based on input/output token metrics and active model pricing.

### Step 1: Open the Spend Analyzer
* Navigate to `/admin/costs` on the dashboard.

### Step 2: Evaluate Token Spend Savings
The dashboard extracts exact token usage from successfully completed proxy calls and maps them to our pricing tables:

| Model | Input Price (per 1M tokens) | Output Price (per 1M tokens) |
| :--- | :--- | :--- |
| **`gemini-2.5-pro`** | $1.25 | $5.00 |
| **`gemini-2.5-flash`** | $0.075 | $0.30 |
| **`gemini-2.5-flash-lite`** | $0.0375 | $0.15 |
| **`text-embedding-004`** | $0.025 | $0.00 |
| **`multimodal-embedding-001`** | $0.025 | $0.00 |

### Step 3: Track Financial Allocation
1. **App Spend Chart**: View total spend broken down by Application ID. Use this to charge back department budgets accurately.
2. **Client Tier Distribution**: Compare total spend across subscription tiers (`premium`, `standard`, `free`) to evaluate tier profitability.
3. **Routing Efficiency**: Track estimated cost savings achieved by dynamic routing (e.g., calculating the price difference when the router dynamically downgrades a simple query from `gemini-2.5-pro` to `gemini-2.5-flash-lite`).
