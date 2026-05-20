# 🚀 Cloud Run Deployment Manual

The Smart Router is designed for highly durable, containerized execution on **Google Cloud Run**. Deployment is fully automated using a single wrapper script and Terraform.

---

## 📋 New Google Cloud Project: Manual Pre-requisites

If you are deploying the Smart Router to a **brand new Google Cloud Project**, there are a few one-time manual steps you must perform in the Google Cloud Console first:

1. **Enable Billing (Mandatory)**:
   - Associate an active **Billing Account** with your Google Cloud project. The deployment provisions serverless containers (Cloud Run), Secret Manager secrets, a native Firestore Database, Cloud Build triggers, and Google Identity Platform, which are only accessible on billing-enabled projects.
2. **Configure OAuth Consent Screen (Mandatory for Google Sign-In)**:
   - Navigate to the [Google Cloud Console](https://console.cloud.google.com/).
   - Go to **APIs & Services > OAuth consent screen**.
   - Select a **User Type**:
     - **Internal** (Recommended if deploying for your organization/team, restricting login strictly to your domain members).
     - **External** (If you want to authorize any individual Google email address to access).
   - Fill out the required fields: **App name** (e.g. `Smart Router Admin`), **User support email**, and **Developer contact information**. Click **Save and Continue**.
3. **Enable Google Sign-In Provider in Identity Platform (Mandatory)**:
   - Search for **Identity Platform** in the GCP search bar (this is the underlying engine for Firebase Auth).
   - Go to **Providers** in the left-hand menu.
   - Click **Add Provider and select Google**.
   - Toggle the **Enabled** switch.
   - Select your project's **Support email** in the dropdown, and click **Save**.

---

## 🔐 Step 1: Google Cloud CLI Authentication

Before executing the deployment script, you must authenticate with Google Cloud and configure your CLI tool context:

```bash
# 1. Login to standard gcloud CLI
gcloud auth login

# 2. Configure active project context
gcloud config set project your-gcp-project-id

# 3. Authorize Application Default Credentials (ADC)
# (Enables Terraform and local setup scripts to act on your behalf)
gcloud auth application-default login
gcloud auth application-default set-quota-project your-gcp-project-id
```

---

## 🔑 Step 2: Set Authorized Domains & Specific Email Addresses (Optional)

By default, admin dashboard logins are restricted to email addresses ending in `@google.com` and `@cloudadvocacyorg.joonix.net` to prevent unauthorized console access.

To authorize your own domain suffixes or individual email addresses (e.g., a specific team email alongside whole company domains):
1. Open `.env` (copied from `.env.sample`).
2. Add or update the `ALLOWED_EMAIL_DOMAINS` variable with a comma-separated list of domains and/or specific emails:
   ```ini
   # Supports both whole domains (e.g. 'mycompany.com') and specific individual emails (e.g. 'operator@gmail.com') simultaneously:
   ALLOWED_EMAIL_DOMAINS="mycompany.com,operator@gmail.com,another-team.org"
   ```

---

## 🚀 Step 3: Run the Automated Deploy Pipeline

Execute the primary deployment wrapper script from the workspace root directory:

```bash
chmod +x deploy.sh
./deploy.sh
```

---

## 🔍 Behind the Scenes: What `deploy.sh` Executes

The automated script performs the following sequential phases:

### Phase 1: Environment Configuration Checks
* Checks for an active `.env` configuration file.
* Validates essential variables (`GOOGLE_CLOUD_PROJECT`, `GEMINI_API_KEY`).

### Phase 2: Programmatic Firebase Web SDK Provisioning
Instead of forcing manual Console registration, `deploy.sh` leverages your active Google authentication token to:
1. Query Google Firebase Management APIs to check if Firebase is linked to the project.
2. If missing, programmatically enables and links Firebase.
3. Scans for an existing registered Web App named `Smart Router Admin`. If missing, it programmatically registers it.
4. Extracts Web Client configuration parameters and automatically writes them to `.env` (e.g. `FIREBASE_API_KEY`, `FIREBASE_AUTH_DOMAIN`, etc.).

### Phase 3: Infrastructure Provisioning (Terraform)
Invokes Terraform inside `/terraform` to provision and configure cloud resources:
* **Enables APIs**: Cloud Run, Native Firestore, Secret Manager, Cloud Build, Identity Toolkit.
* **Native Firestore Database**: Provisions a Firestore DB instance in Native mode in your selected region.
* **IAM Roles**: Sets up execution service accounts with minimum permission scopes.

### Phase 4: Secrets Storage Security Upload
* Synchronizes your local development `GEMINI_API_KEY` to Google Secret Manager (`gemini-api-key`).
* Grants the Cloud Run execution service account access to this secret at runtime.

### Phase 5: Multi-Service Compilation & Deployment
1. Executes Go `templ generate` to compile the type-safe Go HTML templates.
2. Triggers a remote Google **Cloud Build** task targeting the monorepo root with the **Backend Dockerfile** (`./backend/Dockerfile`) to build and deploy the **Backend API & Proxy Service** (`smart-router-backend`).
3. Triggers a second **Cloud Build** task targeting the monorepo root with the **Frontend Dockerfile** (`./frontend/Dockerfile`) to build and deploy the **Frontend Portal Service** (`smart-router-frontend`), automatically linking it to the backend's dynamic URI and service name context.
4. Outputs both live Cloud Run Endpoint URLs:
   - **Backend API Endpoint**: The Vertex AI proxy entrypoint.
   - **Frontend Dashboard URL**: The web-based administration panel.

### Phase 6: Post-Deployment Verification Tests
Immediately after deployment, the pipeline automatically invokes the Go verification tool (`go run cmd/verify/main.go`) targeting the newly deployed **Backend API Service URL**:
* **Programmatic Seeding**: Connects directly to your native Cloud Firestore instance to safely seed a temporary test client, application profile, custom API key, and high-priority routing rule.
* **Integration Checks**: Makes active upstream REST queries through the newly deployed Cloud Run Backend URL to test key functionality:
  1. Standard generation requests (routing to `gemini-2.5-flash`).
  2. Rules-based routing (header overrides routing `gemini-1.5-pro` to `gemini-2.5-pro`).
  3. Security boundaries (unauthenticated requests returning `401 Unauthorized`).
* **Automatic Cleanup**: Thoroughly deletes all seeded test data from Firestore immediately upon completion, leaving the production environment in a clean state.
* **Pipeline Safety**: If any verification scenario fails, the deployment pipeline terminates with an exit code of `1`.
