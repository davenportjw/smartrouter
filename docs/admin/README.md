# 🗂️ Administrative Dashboard Overview

The Smart Router includes an elegant, compiled HTMX-powered administrative control panel accessible at `/admin`. Operators use this dashboard to manage applications, provision keys, configure declarative request routing, and audit throughput costs in real time.

---

## 🔒 Google Sign-In Authentication

To protect your proxy against unauthorized access, the dashboard enforces Firebase-backed **Google Sign-In OIDC Authentication**.

### Authenticating as an Administrator
1. Navigate to the base URL: `http://localhost:8080/admin` (or your live Cloud Run service URL).
2. If unauthenticated, the router redirects you to `/admin/login` showing the secure login screen.
3. Click **Sign In with Google**.
4. Authentic credentials are verified by Firebase. Once approved, Firebase yields an OIDC token which is committed to a secure HTTP-only session cookie in your browser.
5. You are redirected to the keys console (`/admin/keys`).

---

## 🗺️ Navigation Tab Map

The dashboard is divided into 7 dedicated operational tabs:

| Tab / View | Endpoint | Purpose |
| :--- | :--- | :--- |
| **[Apps & API Keys](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/apps-and-keys.md)** | `/admin/keys` & `/admin/apps` | Manage active application profiles, map client tiers, and issue static keys. |
| **[Custom Headers](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/custom-headers.md)** | `/admin/headers` | Enforce declarative parameters validation for incoming API requests. |
| **[Routing Rules](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/routing-rules.md)** | `/admin/rules` | Override target models, regional endpoints, weight priorities, and fallbacks. |
| **[Complexity Tuning](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/routing-rules.md#user-flow-5-tuning-complexity-based-dynamic-routing)** | `/admin/complexity` | Tweak character boundaries mapping virtual `gemini-dynamic` calls. |
| **[GCP Models Viewer](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/routing-rules.md#user-flow-4-implementing-fallback-models-and-dynamic-target-upgrades)** | `/admin/models` | Natively inspect active GCP Vertex endpoints, locations, and foundation model tiers. |
| **[Metrics Dashboard](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/metrics-and-costs.md)** | `/admin/metrics` | Track real-time throughput volume, error rates, and latency curves. |
| **[Cost Analytics](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/metrics-and-costs.md#user-flow-7-spend-allocation-and-cost-tracking)** | `/admin/costs` | Audit token counts, and calculate dynamic spend savings charts. |

---

## 🚀 Primary Operator Workflows

To execute standard management procedures, follow the specialized operational runbooks:
* **To onboard a new application context and provision API keys**: Refer to **[Apps & API Keys Runbook](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/apps-and-keys.md)**.
* **To require a specific token header from clients**: Refer to **[Custom Request Verification Guide](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/custom-headers.md)**.
* **To override upstream routing paths dynamically**: Refer to **[Routing & Complexity Rules Guide](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/routing-rules.md)**.
* **To evaluate token costs and system analytics**: Refer to **[Performance & Spend Auditing Guide](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/metrics-and-costs.md)**.
