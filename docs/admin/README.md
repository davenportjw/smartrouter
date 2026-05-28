# 🗂️ Administrative Dashboard Overview

The Smart Router dashboard is located at `/admin`. It is used to manage applications, configure keys, setup routing rules, and view metrics.

---

## 🔒 Google Sign-In Authentication

Dashboard access is secured via Firebase **Google Sign-In OIDC Authentication**.

### Authorized Emails and Domains
For safety and security, there are no default allowed domains or email addresses. You must explicitly configure the permitted domains and addresses using the `ALLOWED_EMAIL_DOMAINS` variable in your `.env` file before deployment:
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
| **[Client Organizations](./client-organizations.md)** | `/admin/clients` | Manage subscriber organizations and billing tiers. |
| **[Apps & API Keys](./apps-and-keys.md)** | `/admin/keys` & `/admin/apps` | Manage applications, client tiers, and API keys. |
| **[Custom Headers](./custom-headers.md)** | `/admin/headers` | Configure request header requirements. |
| **[Routing Rules](./routing-rules.md)** | `/admin/rules` | Set model overrides, locations, and fallbacks. |
| **[Complexity Tuning](./routing-rules.md#user-flow-5-tuning-complexity-based-dynamic-routing)** | `/admin/complexity` | Set character thresholds for the virtual `gemini-dynamic` model. |
| **[GCP Models Viewer](./routing-rules.md#user-flow-4-implementing-fallback-models-and-dynamic-target-upgrades)** | `/admin/models` | Inspect active GCP Vertex AI model endpoints and locations. |
| **[Metrics Dashboard](./metrics-and-costs.md)** | `/admin/metrics` | Track requests, error rates, and latency. |
| **[Cost Analytics](./metrics-and-costs.md#user-flow-7-spend-allocation-and-cost-tracking)** | `/admin/costs` | Monitor token counts and calculated cost savings. |
