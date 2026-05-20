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

## 🔑 Step 2: Prepare the Environment File (`.env`)

1. Copy the sample environment template to `.env` if you haven't already:
   ```bash
   cp .env.sample .env
   ```

2. Open `.env` and configure the variables:
   * **Required User-Provided Variable**: Ensure `GOOGLE_CLOUD_PROJECT` is set to your GCP project ID.
   * **Firebase Web SDK Configurations**: **Leave them as the default placeholders!** During the deployment, `deploy.sh` will programmatically register your application with Firebase and automatically write the resolved credentials back to this `.env` file.
   * **Authorized Emails & Domains (Optional)**: Set `ALLOWED_EMAIL_DOMAINS` to restrict dashboard admin logins (defaults to `@google.com` and `@cloudadvocacyorg.joonix.net`):
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
2. **Firebase Provisioning**: Programmatically checks, links Firebase if necessary, registers the Web App, and updates `.env` with client SDK details.
3. **Infrastructure Setup (Terraform)**:
   * Enables APIs (Cloud Run, Firestore, Secret Manager, Cloud Build, Identity Toolkit).
   * Provisions Firestore in Native mode.
   * Configures IAM Roles.
4. **Upstream Authentication Setup**: Governed dynamically by Google Cloud Application Default Credentials (ADC), meaning no manual API keys are stored or uploaded to Secret Manager.
5. **Compilation & Deployment**:
   * Compiles HTML templates with `templ`.
   * Triggers Cloud Build to deploy Backend and Frontend services to Cloud Run.
6. **Verification Tests**:
   * Runs `go run cmd/verify/main.go` against the backend to test functionality (requests routing, security boundary, and rules engine).
   * Cleans up test records from Firestore.
