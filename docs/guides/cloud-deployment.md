# 🚀 Cloud Run Deployment Manual

The Smart Router is designed for highly durable, containerized execution on **Google Cloud Run**. Deployment is fully automated using a single wrapper script and Terraform.

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

## 🚀 Step 2: Run the Automated Deploy Pipeline

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

### Phase 5: Dashboard Compilation & Deployment
1. Executes Go `templ generate` to compile HTML components.
2. Triggers a remote Google **Cloud Build** task to containerize the Go proxy.
3. Deploys the container to a **Google Cloud Run** service.
4. Outputs your live Smart Router Endpoint URL!

### Phase 6: Post-Deployment Verification Tests
Immediately after deployment, the pipeline automatically invokes the Go verification tool (`go run cmd/verify/main.go`):
* **Programmatic Seeding**: Connects directly to your native Cloud Firestore instance to safely seed a temporary test client, application profile, custom API key, and high-priority routing rule.
* **Integration Checks**: Makes active upstream REST queries through the newly deployed Cloud Run service URL to test key functionality:
  1. Standard generation requests (routing to `gemini-2.5-flash`).
  2. Rules-based routing (header overrides routing `gemini-1.5-pro` to `gemini-2.5-pro`).
  3. Security boundaries (unauthenticated requests returning `401 Unauthorized`).
* **Automatic Cleanup**: Thoroughly deletes all seeded test data from Firestore immediately upon completion, leaving the production environment in a clean state.
* **Pipeline Safety**: If any verification scenario fails, the deployment pipeline terminates with an exit code of `1`.
