# 🗂️ Administrative Dashboard Overview

The Smart Router dashboard is located at `/admin`. It is used to manage applications, configure keys, setup routing rules, and view metrics.

---

## 🔒 Google Sign-In Authentication

Dashboard access is secured via Firebase **Google Sign-In OIDC Authentication**.

### Authorized Emails and Domains
By default, access is restricted to emails ending in `@google.com` and `@cloudadvocacyorg.joonix.net`. 

To configure authorized emails and/or domains:
1. Set `ALLOWED_EMAIL_DOMAINS` in `.env`:
   ```ini
   ALLOWED_EMAIL_DOMAINS="mycompany.com,operator@gmail.com"
   ```

### Logging In
1. Open `/admin` in your browser.
2. If unauthenticated, you are redirected to `/admin/login`.
3. Click **Sign In with Google** and authorize with your Google credentials.

---

## 🗺️ Dashboard Sections

| View | Endpoint | Purpose |
| :--- | :--- | :--- |
| **[Apps & API Keys](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/apps-and-keys.md)** | `/admin/keys` & `/admin/apps` | Manage applications, client tiers, and API keys. |
| **[Custom Headers](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/custom-headers.md)** | `/admin/headers` | Configure request header requirements. |
| **[Routing Rules](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/routing-rules.md)** | `/admin/rules` | Set model overrides, locations, and fallbacks. |
| **[Complexity Tuning](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/routing-rules.md#user-flow-5-tuning-complexity-based-dynamic-routing)** | `/admin/complexity` | Set character thresholds for the virtual `gemini-dynamic` model. |
| **[GCP Models Viewer](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/routing-rules.md#user-flow-4-implementing-fallback-models-and-dynamic-target-upgrades)** | `/admin/models` | Inspect active GCP Vertex AI model endpoints and locations. |
| **[Metrics Dashboard](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/metrics-and-costs.md)** | `/admin/metrics` | Track requests, error rates, and latency. |
| **[Cost Analytics](file:///Users/jasondavenport/GitHub/geminirouter/docs/admin/metrics-and-costs.md#user-flow-7-spend-allocation-and-cost-tracking)** | `/admin/costs` | Monitor token counts and calculated cost savings. |
