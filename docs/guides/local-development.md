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

Copy the environment template to `.env`:
```bash
cp .env.sample .env
```

Open `.env` and configure the variables. For local development:
* **`GOOGLE_CLOUD_PROJECT`**: Set to your GCP project ID.
* **`LOCAL_DEV`**: Typically set automatically by `./run_local.sh`, but you can set it to `true` here to run without connecting to a live Cloud Firestore instance (this stores data locally at `data/local_db.json`).
* **Firebase Configurations**: Set `FIREBASE_API_KEY` / `FIREBASE_AUTH_DOMAIN` / `FIREBASE_APP_ID` to your Firebase web app config and `ALLOWED_EMAIL_DOMAINS` to your allowlist. There is **no authentication bypass**, even with `LOCAL_DEV=true`: sign in with Google at `http://localhost:8081/login`; the frontend verifies the ID token and mints a Firebase session cookie using your Application Default Credentials (`gcloud auth application-default login`), which need permission to call Identity Toolkit (`roles/firebaseauth.admin`). `LOCAL_DEV` only relaxes cookie `Secure` and local data paths, and is ignored entirely on Cloud Run (`K_SERVICE` set).

Example `.env` for local development:
```ini
PORT=8080
GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
GEMINI_LOCATION="us-central1"
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
