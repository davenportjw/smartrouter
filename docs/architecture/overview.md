# 🗺️ System Architecture Overview

This document maps out the core architectural layers of the Smart Router: the App-Centric data model, the request execution lifecycle, and the sliding-window rate-limiting engine.

---

## 🗄️ 1. The App-Centric Data Hierarchy

The Smart Router models traffic, rate limits, and billing constraints around an **App-Centric Hierarchy** rather than binding configurations to a raw client or key:

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

### Core Boundaries
* **Client**: Represents the billing and subscription boundary (e.g. `premium`, `standard`, `free`).
* **App**: Represents the functional boundary (e.g. `mobile-chat`, `invoice-processing`). **RPM, TPM, and latency priority are mapped directly to the App level.** This ensures one runaway app context cannot starve another context owned by the same Client.
* **API Key / Service Account OIDC**: Access credentials bound strictly to a single App.

---

## 🔄 2. Request Execution Lifecycle

Every incoming HTTP request targeting standard Gemini or Vertex AI APIs executes through a sequential pipeline before hitting Google's upstream servers:

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
        Limiter-->>Proxy: Starved / Exhausted
        Proxy-->>ClientApp: HTTP 429 Too Many Requests
    end

    Proxy->>Store: Evaluate Routing Rules
    Store-->>Proxy: Match target (e.g. gemini-2.5-pro, location: us-central1)

    Proxy->>Vertex: Execute upstream request (with OAuth/OIDC Token)
    Vertex-->>Proxy: Return Payload + usage metrics
    
    Proxy->>Limiter: Commit actual Token usage (TPM) to sliding window
    Proxy-->>ClientApp: Return payload + routing audit headers
```

### Request Processing Pipeline Steps
1. **Credential Verification (`proxy.go`)**:
   - Hashes the query param key (`?key=...`) or extracts the Google OIDC bearer token.
   - Queries the active database store (Firestore or `/data/local_db.json`) to resolve the linked **App** and **Client**.
2. **Custom Header Enforcement**:
   - Loads all declarative `CustomHeader` definitions mapped to the resolved `App`.
   - Verifies matching rules (non-empty, regex, enum constraints).
3. **Rate Limit Evaluation (`limiter.go`)**:
   - Evaluates the App's sliding-window budget for Requests Per Minute (RPM) and estimated Tokens Per Minute (TPM).
4. **Upstream Route Resolution**:
   - Evaluates registered `RoutingRule` models sequentially.
   - Translates request targets (such as mapping `gemini-dynamic` or standard targets to specific regional upstream Vertex endpoints like `us-central1`).
5. **Request Dispatch & Upstream Auth**:
   - Strips local credentials and injects GCP Service Account credentials (OAuth2/OIDC token).
   - Relays request to the upstream Vertex AI or Gemini API gateway.
6. **Token Update & Auditing**:
   - Inspects upstream responses to extract token counts and updates the App's rate limit window.
   - Injects audit response headers (`X-Routed-Model`, `X-Client-Tier`, `X-App-ID`) to enable client transparency.

---

## ⏳ 3. Sliding-Window Rate Limiting

Rate limiting tracks usage per **App** in-memory. The engine uses a precise **Sliding Window Algorithm** backed by a background cleaner to purge expired entries:

### Sliding Window Mechanics
* **Requests Per Minute (RPM)**: Each request appends an in-memory timestamp to a slice of timestamps mapped to the App ID. The slice is filtered dynamically, keeping only timestamps within the last 60 seconds. If `len(timestamps) > limit.RPM`, the request is rejected with `429`.
* **Tokens Per Minute (TPM)**:
  - **Pre-Request Estimation**: Before dispatching, the proxy estimates token consumption based on request character length (`estimateTokensAndCost`). If this estimation would breach the remaining TPM budget, the request is deferred or rejected.
  - **Post-Request Commit**: Once the upstream response is received, the actual token consumption (returned in response metadata) is committed to the sliding window, overwriting the estimation.

### Garbage Collection
To prevent memory leaks, a background scheduler executes continuously in `scheduler.go`, sweeping inactive app sliding windows and releasing memory.

---

## 🌐 4. Decoupled Services Layout

The Smart Router is deployed as two separate, decoupled service components to achieve optimal isolation, security, and horizontal scaling:

```mermaid
graph LR
    User([Browser Client]) -->|Login / Dashboard UI| Frontend["Frontend Service (/frontend)"]
    APIClient([API Clients]) -->|Vertex AI / Gemini Proxy| Backend["Backend Service (/backend)"]
    Frontend -->|Admin REST APIs + OIDC Auth| Backend
```

### Decoupled Components

* **Backend Service (`/backend`)**:
  - Core Proxy handler representing the Vertex AI / Gemini routing engine.
  - Exposes a fully API-callable, authenticated administration REST API (`/api/*`) for configuration CRUD and model discovery.
  - Maintains the `ConfigStore` Firestore listeners and active cache layers.
* **Frontend Service (`/frontend`)**:
  - Serves all HTML Templ templates and dashboard pages.
  - Manages browser session cookies and user logins via Firebase Authentication.
  - Employs the REST-based `APIConfigStore` client to interact with the Backend Service's REST API.
  - Securely authenticates requests against the Backend API using **Google OIDC Service Account identity tokens** in production, or a local shared secret in development.

