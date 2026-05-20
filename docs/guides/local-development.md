# 💻 Local Development Guide

This guide explains how to run the Smart Router locally for development and testing.

---

## ⚙️ Prerequisites

* **Go** (version 1.22 or higher)
* **templ** (Go HTML component compiler)
* **jq**

---

## 🛠️ Local Execution Steps

### 1. Download Go Dependencies
```bash
go mod download
```

### 2. Compile HTML Components
The dashboard uses `templ` to compile the HTML templates:
```bash
go run github.com/a-h/templ/cmd/templ generate
```

### 3. Configure Local Environment
Copy the environment template:
```bash
cp .env.sample .env
```

Set `LOCAL_DEV=true` in `.env` to run without connecting to live GCP Firestore. This uses a local JSON database stored at `/data/local_db.json`.

Ensure the following are set in `.env`:
```ini
PORT=8080
LOCAL_DEV=true
GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
GEMINI_API_KEY="your-gemini-api-key"
```

### 4. Start Services
To run both the backend and frontend services locally:
```bash
./run_local.sh
```

* **Backend Proxy & API**: Running on `http://localhost:8080`
* **Frontend Dashboard Portal**: Running on `http://localhost:8081/login`

---

## 🗂️ Local Database (`local_db.json`)

When `LOCAL_DEV=true`:
* All Clients, Apps, Keys, custom headers, and routing rules are saved to `/data/local_db.json`.
* If the file is not found, the router seeds default mock data (premium client, chat app, and development keys) so you can query the proxy immediately.
