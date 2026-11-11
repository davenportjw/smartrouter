# 🗂️ Administrative Dashboard Overview

The Smart Router dashboard is located at `/admin`. It is used to manage applications, configure keys, setup routing rules, and view metrics.

---

## 🔒 Google Sign-In Authentication

Dashboard access is secured via Firebase **Google Sign-In OIDC Authentication**.

### Authorized Emails and Domains
For safety and security, there are no default or hardcoded allowed domains or email addresses in the repository. You must explicitly configure the permitted comma-separated domains and addresses using `ALLOWED_EMAIL_DOMAINS` (or `ALLOWED_AUTH_DOMAINS` / `ALLOWED_DOMAINS`) in your `.env` file or Cloud Run environment variables before deployment:
```ini
ALLOWED_EMAIL_DOMAINS="mycompany.com,corp.example.org,operator@gmail.com"
```
During deployment, `deploy.sh` and Terraform synchronize both:
1. **Admin Session Email Allowlist**: Enforced server-side in `/auth/session` and on every `/admin/*` request (`frontend/auth/auth.go`) against verified Firebase ID token / session cookie email claims.
2. **Firebase Identity Platform Authorized Domains (`authorizedDomains`)**: Ensures `localhost`, `<project-id>.firebaseapp.com`, `<project-id>.web.app`, the deployed Cloud Run service hostnames (`gemini-smart-router-ui` and `gemini-smart-router`), and configured domain entries are registered on Google Identity Toolkit (`admin/v2/projects/<project-id>/config`).

### Logging In
1. Open `/admin` in your browser.
2. If unauthenticated (or if a stale session cookie fails verification), the session cookie is cleared and you are redirected to `/login`.
3. Click **Sign In with Google** and authorize with your Google credentials.

### Session Model (Firebase Session Cookies)
The dashboard never mints its own sessions and holds no session signing secret.
1. **Login (`POST /auth/session`)**: the browser signs in with Google via the Firebase JS SDK (no client-side persistence) and posts the ID token (same-origin only). The server verifies it with the Firebase Admin SDK (RS256, issuer/audience for the project, expiry), then requires `email_verified == true`, `firebase.sign_in_provider == "google.com"`, a sign-in (`auth_time`) within the last 10 minutes, and an email on the allowlist. It then calls `auth.Client.SessionCookie` (Identity Toolkit `createSessionCookie`) for a **5-day** Google-signed session cookie, set as `session` with `HttpOnly; Secure; SameSite=Lax; Path=/`.
2. **Every `/admin/*` request**: `VerifySessionCookieAndCheckRevoked` validates the signature against Google's session-cookie certificates, the `https://session.firebase.google.com/<project-id>` issuer, audience, expiry, and that the account is not disabled or revoked (`accounts:lookup`). The verified-email, Google-provider and allowlist checks are re-applied, so removing an address from `ALLOWED_EMAIL_DOMAINS` takes effect immediately. Any failure clears the cookie and redirects to `/login` (fail closed).
3. **Identity headers are never trusted**: the actor comes only from the verified cookie (`auth.SessionFromContext`); headers such as `X-User-Email` / `X-User-Id` are ignored.
4. **Logout (`/logout`)** clears the cookie and revokes the user's Firebase refresh tokens, invalidating all of their sessions.
5. **Local development** has no bypass. `LOCAL_DEV=true` only drops the cookie `Secure` flag on `http://localhost` and is ignored whenever `K_SERVICE` is set (Cloud Run).

The frontend Cloud Run service account needs permission to create session cookies and look up accounts (for example `roles/firebaseauth.admin`).

### Firebase Web API Key Verification & Renewal
During deployment, `deploy.sh` probes the configured `FIREBASE_API_KEY` against Google Identity Toolkit (`accounts:createAuthUri`). If the key in `.env` is missing, placeholder, or expired (`auth/api-key-expired`), `deploy.sh` queries the GCP Firebase Management API (`webApps/*/config` with `x-goog-user-project`) and API Keys API to synchronize the active Firebase Web API key into `.env` and Cloud Run environment variables (`FIREBASE_API_KEY` and `FIREBASE_WEB_API_KEY`).

---

## 🎨 Crisp Slate & Cobalt 2–3 Panel Workspace

The portal uses a distraction-free **Crisp Slate & Cobalt** 2–3 panel layout:
1. **Left Panel (Core Domain Navigation)**: Cool slate (`bg-slate-100 border-r border-slate-200`) navigation (`id="left-sidebar"`) grouped into **Tenancy & Access**, **Routing Intelligence**, **Infrastructure & Compute**, and **Observability & Docs** with `text-[11px]` uppercase section headers and clean, unnumbered `text-[13px] leading-5 rounded-r-md` navigation links (`border-l-2 border-blue-600` when active).
2. **Center Panel (Main Workspace)**: Cool alabaster (`#F8FAFC`) canvas with crisp white (`#FFFFFF`) `rounded-lg` cards, a model-agnostic top header bar featuring a compact live **Proxy Endpoint Pill** (`Proxy: <PROXY_PUBLIC_URL>`) with one-click copy-to-clipboard, and reactive HTMX forms with inline error alerts (`Operation Failed:`).
   - **Master-Detail Split View & Contextual Inspector Pane (`#modal-container`)**: Instead of multi-card grids or viewport-blocking pop-up modals, configuration tables and actionable registries across the portal (`/admin/keys`, `/admin/clients`, `/admin/apps`, `/admin/headers`, `/admin/rules`, and `/admin/complexity`) use a responsive flex-row Master-Detail split (`lg:flex-row lg:items-start gap-6`). Clicking **Edit**, **Configure**, or a creation action button (**+ Add ...**) opens a sticky right-hand contextual inspector pane (`id="modal-container"`, `lg:w-[400px]`–`lg:w-[440px]`) directly beside the active resource table while keeping the table visible. Likewise, **Decision Engine Providers & Cluster Setup** on `/admin/complexity` (`#decision-providers-master-detail`) Synchronizes its inline inspector (`#provider-detail-inspector`) when selecting a compact row.
   - **Restrained Badge & Typography Standard**: Colored pills and badges are reserved strictly for live semantic state indicators (`Enabled` / `Disabled`, `Active` / `Inactive`, `Healthy` / `Stale` / `Warning`, `Obsolete`, required enforcement warnings, and the top-bar `Proxy: <PROXY_PUBLIC_URL>` intent pill). Ordinary table cells (model IDs, locations, priorities, billing tiers, auth locations, queue TTLs, and latency numbers) render as crisp slate text (`text-slate-700`) or clean monospace tokens (`font-mono text-xs`) without decorative card-header pills.
   - **Flush Documentation Secondary Sidebar (`#docs-sidebar`)**: On `/admin/docs`, the documentation navigation sidebar (`id="docs-sidebar"`) sits directly adjacent and flush next to `#left-sidebar` with identical `bg-slate-100 border-r border-slate-200` surface styling, `text-[11px]` category headers, and `text-[13px] leading-5 rounded-r-md` active/inactive link states, paired with a full-height scrollable `#docs-content` markdown reader (`0.9375rem` body, `1.625rem` H1, `1.25rem` H2).
3. **Right Panel (Contextual Docs & Quick Reference Drawer)**: Expandable drawer (`id="context-drawer"`) that starts closed (`hidden`) by default across all views and expands from the right via the top-right **Inspector** drawer handle (`id="context-drawer-toggle"`), providing live route dry-run simulation, `curl` snippets dynamically wired to `PROXY_PUBLIC_URL`, header references, and linkable documentation.

---

## 🗺️ Dashboard Sections

| View | Endpoint | Purpose |
| :--- | :--- | :--- |
| **[Client Organizations](./client-organizations.md)** | `/admin/clients` | Manage subscriber organizations, fallback quotas, and billing tiers. |
| **[Apps & API Keys](./apps-and-keys.md)** | `/admin/apps` & `/admin/keys` | Manage applications (with automatic client quota inheritance) and API keys. |
| **[Custom Headers](./custom-headers.md)** | `/admin/headers` | Configure declarative request header requirements. |
| **[Routing Rules](./routing-rules.md)** | `/admin/rules` | Set model overrides, locations, and fallbacks. |
| **[Complexity Tuning](./routing-rules.md#tuning-complexity-based-dynamic-routing)** | `/admin/complexity` | Configure `gemini-dynamic` engines, tier models, and agent task overrides. |
| **[GCP Models Viewer](./routing-rules.md#gcp-models--endpoints-viewer)** | `/admin/models` | Inspect unit pricing, toggle active GCP Vertex AI & local cluster models, and run async background model discovery with live progress status. |
| **[Local & Provider Clusters](./local-clusters.md)** | `/admin/clusters` | Manage provider clusters, cloud failover models, direct endpoints, and active local runner associations. |
| **[Request Queue](./local-clusters.md#4-unified-priority-queue--live-telemetry-adminqueue)** | `/admin/queue` | Inspect live priority queues, in-flight local cluster jobs, and overloaded model cooldowns. |
| **[Metrics Dashboard](./metrics-and-costs.md)** | `/admin/metrics` | Track live requests, error rates, and latency percentiles. |
| **[Cost Analytics](./metrics-and-costs.md#token-spend--cost-analytics)** | `/admin/costs` | Monitor live token counts and calculated cost savings. |
| **Documentation Reader** | `/admin/docs` | Browse repository markdown guides in-portal with flush secondary sidebar navigation (`#docs-sidebar`). |


