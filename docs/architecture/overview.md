# 🗺️ System Architecture Overview

This document explains the core layers of the Smart Router: the data hierarchy, the request execution lifecycle, and the rate-limiting engine.

---

## 🗄️ 1. Data Hierarchy

The Smart Router configures limits and rules around an **App-Centric Hierarchy**:

```mermaid
erDiagram
    CLIENT ||--o{ APP : "owns"
    APP ||--o{ API_KEY : "binds"
    APP ||--o{ ROUTING_RULE : "configures"
    APP ||--o{ CUSTOM_HEADER : "requires"

    CLIENT {
        string id PK "Client ID"
        string name "Billing Name"
        string tier "premium | standard | free"
    }
    APP {
        string id PK "App ID"
        string client_id FK "Owner Client"
        string name "Application Context"
        int rpm "Max Requests Per Minute"
        int tpm "Max Tokens Per Minute"
        string priority "high | medium | low"
    }
    API_KEY {
        string key_hash PK "Hashed Credential"
        string app_id FK "Target Application"
        string status "active | revoked"
    }
```

* **Client**: Subscription tier boundary (`premium`, `standard`, `free`).
* **App**: Functional context boundary. **RPM, TPM, and latency priority are configured at the App level.** This prevents traffic spikes in one app from impacting other apps owned by the same Client.
* **API Key / Service Account OIDC**: Credentials bound to a single App.

---

## 🔄 2. Request Execution Lifecycle

Requests execute through the following pipeline:

```mermaid
sequenceDiagram
    autonumber
    actor ClientApp as Client Application
    participant Proxy as Router Proxy
    participant Store as Config Store
    participant Limiter as Rate Limiter
    participant Vertex as Upstream Vertex / Gemini

    ClientApp->>Proxy: POST /v1/models/gemini-dynamic:generateContent?key=abc
    
    Proxy->>Store: Resolve Key status & retrieve App details
    alt Key Revoked or Invalid
        Store-->>Proxy: Key not found / inactive
        Proxy-->>ClientApp: HTTP 401 Unauthorized
    end
    
    Proxy->>Store: Lookup required Custom Headers for App
    alt Required header missing or regex fails
        Store-->>Proxy: Validation failed
        Proxy-->>ClientApp: HTTP 400 Bad Request
    end

    Proxy->>Limiter: Evaluate Rate Limit (RPM, TPM)
    alt Limit Exceeded
        Limiter-->>Proxy: Exhausted
        Proxy-->>ClientApp: HTTP 429 Too Many Requests
    end

    Proxy->>Store: Evaluate Routing Rules
    Store-->>Proxy: Match target (e.g. gemini-2.5-pro, location: us-central1)

    Proxy->>Vertex: Execute upstream request (with OAuth/OIDC Token)
    Vertex-->>Proxy: Return Payload + usage metrics
    
    Proxy->>Limiter: Commit actual Token usage (TPM) to sliding window
    Proxy-->>ClientApp: Return payload + routing audit headers
```

### Pipeline Steps
1. **Credential Verification**: Hashes the API key or extracts the OIDC token to resolve the linked App and Client from the database (Firestore or `/data/local_db.json`).
2. **Custom Header Enforcement**: Loads and validates `CustomHeader` requirements (presence, regex, or enum).
3. **Rate Limit Evaluation**: Checks sliding-window limits for Requests Per Minute (RPM) and estimated Tokens Per Minute (TPM).
4. **Upstream Route Resolution**: Resolves `RoutingRule` configurations and translates model names to specific regional endpoints.
5. **Request Dispatch**: Injects GCP Service Account credentials and forwards the request upstream.
6. **Metrics Update**: Updates the sliding window with actual token usage and injects auditing response headers (`X-Routed-Model`, `X-Client-Tier`, `X-App-ID`).

---

## ⏳ 3. Token-Weight Rate Limiting (RPM & TPM)

Rate limiting is computed dynamically in-memory per App:
* **RPM**: Enforced per-second using a token bucket based on the configured RPM capacity.
* **TPM (Tokens Per Minute)**:
  * **Estimation**: Pre-request token weight is estimated efficiently on the hot path using a character heuristic (1 token ≈ 4 characters of request body).
  * **Commit / Correction**: Once the upstream request completes, the proxy intercepts the response, parses the exact `totalTokenCount` from the `usageMetadata` block, and dynamically corrects the limiter budget (refunding over-estimations or deducting under-estimations). Bypassed for streaming responses to avoid unmarshalling delay.
  * **Opt Out**: Applications can choose to completely opt out of TPM rate limits via the dashboard, relying entirely on standard RPM limiting.
* **Priority Buffering**: During transient rate limit spikes, High priority apps wait/queue up to **5s** and Medium priority apps wait up to **2s** for token availability; Low priority apps fail immediately.

---

## 🌐 4. Decoupled Services

The Smart Router is split into frontend and backend services:

```mermaid
graph LR
    User([Browser Client]) -->|Login / Dashboard UI| Frontend["Frontend Service (/frontend)"]
    APIClient([API Clients]) -->|Vertex AI / Gemini Proxy| Backend["Backend Service (/backend)"]
    Frontend -->|Admin REST APIs + OIDC Auth| Backend
```

* **Backend Service (`/backend`)**: Implements the API proxy, admin REST API (`/api/*`), Firestore listeners, and cache layers.
* **Frontend Service (`/frontend`)**: Renders the dashboard UI, manages session cookies via Firebase Authentication, and uses the REST API to communicate with the backend. In production, calls are secured via GCP Service Account OIDC tokens.
