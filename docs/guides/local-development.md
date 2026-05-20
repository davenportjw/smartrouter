# 💻 Local Development Guide

Spin up the Smart Router locally for rapid testing, component refactoring, and dynamic rule evaluation.

---

## ⚙️ Prerequisites

Ensure you have the following core tools installed on your local machine:
* **Go** (version 1.22 or higher)
* **templ** (Go HTML component compiler)
* **jq** (for fast shell parsing)

---

## 🛠️ Step-by-Step Local Execution

### 1. Download Dependencies
Install all Go modules:
```bash
go mod download
```

### 2. Compile HTML Components
The dashboard uses `templ` to generate type-safe Go HTML templates. You must compile them before running the server:
```bash
go run github.com/a-h/templ/cmd/templ generate
```
*(This generates matching `*_templ.go` files next to all `.templ` components inside `pkg/dashboard/templates/`.)*

### 3. Setup Local Environment Mocks
To work offline without connecting to live GCP Firestore DB, set **`LOCAL_DEV=true`**. This forces the config engine to load and write all configuration boundaries to a local JSON file: `/data/local_db.json`.

Create your local `.env` file:
```bash
cp .env.sample .env
```

Ensure the following keys are configured:
```ini
PORT=8080
LOCAL_DEV=true
GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
GEMINI_API_KEY="your-gemini-api-key"
```

### 4. Run the Decoupled Services

The Smart Router is split into separate services. To run them both locally under local dev orchestration:

```bash
./run_local.sh
```

This startup script automatically:
1. Compiles all Go HTML templates inside the UI codebase.
2. Boots the **Backend Proxy & REST API Service** on Port `8080` in the background.
3. Boots the **Frontend Dashboard Service** on Port `8081` in the background, targeting the backend at `http://localhost:8080`.
4. Configures a secure bypass auth token `BACKEND_SHARED_SECRET=local-dev-bypass-token-12345` to allow communication.

To explore the UI portal in your browser, navigate to:
```
http://localhost:8081/login
```

To directly query the API proxy or call the administrative APIs:
- **Vertex API Proxy**: `http://localhost:8080/v1/models/...`
- **Administrative REST APIs**: `http://localhost:8080/api/apps`, `http://localhost:8080/api/rules`, etc. (requires `Authorization: Bearer local-dev-bypass-token-12345` header).

---

## 🗂️ Managing the Local JSON Database

When `LOCAL_DEV=true` is active:
* All registered Clients, Apps, API Keys, custom headers, and routing rules are read from and written directly to `/data/local_db.json`.
* **Automatic Seeding**: If `/data/local_db.json` doesn't exist, the router automatically seeds a default premium client, a mobile chat app, and active development key hashes to let you start querying immediately.
* **Automatic Migrations**: The server contains schema migration code that seamlessly updates legacy local JSON schemas to support the latest App-Centric models without resetting your local database configurations.
