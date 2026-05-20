# 🚀 Cloud Run Deployment Guide

The Smart Router runs on **Google Cloud Run**. Deployment is automated using `deploy.sh` and Terraform.

---

## 📋 Google Cloud Project Prerequisites

If deploying to a new Google Cloud project:

1. **Enable Billing**: Link your project to an active Billing Account.
2. **Configure OAuth Consent Screen**: Go to **APIs & Services > OAuth consent screen**, select user type, and fill out the required fields.
3. **Enable Google Sign-In**: Go to **Identity Platform > Providers**, add **Google** as a provider, and enable it.

---

## 🔐 Step 1: Google Cloud CLI Authentication

Authenticate and configure the CLI project context:

```bash
gcloud auth login
gcloud config set project your-gcp-project-id
gcloud auth application-default login
gcloud auth application-default set-quota-project your-gcp-project-id
```

---

## 🔑 Step 2: Set Authorized Emails and Domains (Optional)

To authorize your own email addresses or domains for dashboard login:
1. Open `.env`.
2. Set `ALLOWED_EMAIL_DOMAINS`:
   ```ini
   ALLOWED_EMAIL_DOMAINS="mycompany.com,operator@gmail.com"
   ```

---

## 🚀 Step 3: Run the Deployment

Run the deployment script:

```bash
chmod +x deploy.sh
./deploy.sh
```

---

## 🔍 Deployment Steps Executed by `deploy.sh`

1. **Environment Checks**: Validates `.env` file variables.
2. **Firebase Provisioning**: Programmatically checks and configures the Firebase Web App registration.
3. **Infrastructure Setup (Terraform)**:
   * Enables APIs (Cloud Run, Firestore, Secret Manager, Cloud Build, Identity Toolkit).
   * Provisions Firestore in Native mode.
   * Configures IAM Roles.
4. **Secrets Upload**: Saves the local `GEMINI_API_KEY` to Secret Manager.
5. **Compilation & Deployment**:
   * Compiles HTML templates with `templ`.
   * Triggers Cloud Build to deploy Backend and Frontend services to Cloud Run.
6. **Verification Tests**:
   * Runs `go run cmd/verify/main.go` against the backend to test functionality (requests routing, security boundary, and rules engine).
   * Cleans up test records from Firestore.
