# 💻 Local Development Guide

Spin up the Gemini Smart Router locally for rapid testing, component refactoring, and dynamic rule evaluation.

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

### 4. Run the Service
Start the Go web application:
```bash
go run main.go
```
* (Or run the helper startup script: `./run_local.sh`)

Navigate to the dashboard index in your browser:
```
http://localhost:8080/admin
```

---

## 🗂️ Managing the Local JSON Database

When `LOCAL_DEV=true` is active:
* All registered Clients, Apps, API Keys, custom headers, and routing rules are read from and written directly to `/data/local_db.json`.
* **Automatic Seeding**: If `/data/local_db.json` doesn't exist, the router automatically seeds a default premium client, a mobile chat app, and active development key hashes to let you start querying immediately.
* **Automatic Migrations**: The server contains schema migration code that seamlessly updates legacy local JSON schemas to support the latest App-Centric models without resetting your local database configurations.
