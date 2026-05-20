# 🔑 Calling the Smart Router with an API Key on Cloud Run

This directory contains a ready-to-deploy Python microservice demonstrating how a client application authenticates and executes requests against the **Gemini Smart Router** using an API Key.

---

## 🚀 How it Works

The client application targets the cost-effective `gemini-2.5-flash-lite` model. It routes requests to the Smart Router endpoint, appending the `x-goog-api-key` header. The Smart Router intercept and processes the request:

```
[Client Application] 
       │
       │ 1. Send HTTP POST to /v1/models/gemini-2.5-flash-lite:generateContent
       │    Headers: [ x-goog-api-key: gr_abc...123 ]
       ▼
[Gemini Smart Router] 
       │
       │ 2. Validate Key status
       │ 3. Check Client Tier rate limits (RPM/TPM) & Priority queueing
       │ 4. Apply dynamic routing rule matches
       │ 5. Retrieve upstream key securely from GCP Secret Manager
       │ 6. Inject Google OIDC authentication
       ▼
[Upstream Gemini API / Vertex AI]
```

---

## 📋 Step-by-Step Guide

### 1. Define Client and App in Admin Dashboard
Before calling the router, you must configure access credentials in the router's Administrative Dashboard:

1. Navigate to your **Gemini Smart Router Admin Panel** (e.g., `https://your-router-url.a.run.app/admin`).
2. Select the **Clients** tab and click **Create Client** (or ensure a client already exists, e.g., `Premium Client` with Tier set to `premium`).
3. Select the **Applications** tab:
   - Click **New Application**.
   - Give the Application an ID (slug), such as `customer-facing-chatbot` (or `prod-app-main` for local testing).
   - Bind it to the Client created above.
   - Configure capacity restrictions: Set the desired maximum **RPM** (Requests Per Minute), **TPM** (Tokens Per Minute), and **Queueing Priority** (High, Medium, or Low).
4. Select the **Access Control** (Keys) tab:
   - Click **Generate API Key**.
   - Bind the key to your Application (`customer-facing-chatbot`).
   - **IMPORTANT**: Copy the generated key instantly. The router stores only cryptographic hashes of the key (`sha256`) for security, meaning you cannot retrieve this key again.

---

### 💻 Running and Testing Locally

Before deploying to Cloud Run, you can easily run and validate the API Key client service locally:

1. **Ensure the Smart Router is running locally**:
   ```bash
   ./run_local.sh
   ```
2. **Setup your Python environment**:
   ```bash
   python3 -m venv venv
   source venv/bin/activate
   pip install -r requirements.txt
   ```
3. **Configure environment variables**:
   Specify the local router port and your generated API Key (or the pre-seeded `gr_key_enterprise_123456789` key):
   ```bash
   export ROUTER_URL="http://localhost:8080"
   export ROUTER_API_KEY="gr_key_enterprise_123456789"
   # If your router has mandatory custom headers (e.g. X-Client-App-ID), specify it:
   export CLIENT_APP_ID="prod-app-main"
   ```
4. **Start the client microservice**:
   ```bash
   uvicorn main:app --host 127.0.0.1 --port 8082
   ```
5. **Submit a test prompt**:
   ```bash
   curl -X POST "http://localhost:8082/generate" \
     -H "Content-Type: application/json" \
     -d '{"prompt": "Explain the significance of the expansion of the universe in simple terms."}'
   ```

---

### 2. Build and Deploy to Cloud Run

To simplify setup and security, we provide an automated deployment script `deploy.sh` that:
1. Builds your client container image securely using Google Cloud Build.
2. Deploys it to Google Cloud Run.
3. Configures secure authorization by blocking unauthenticated access (`--no-allow-unauthenticated`).

Ensure your active local shell is authenticated with Google Cloud:
```bash
gcloud auth login
gcloud config set project YOUR_GCP_PROJECT_ID
```

Run the deployment script:
```bash
chmod +x deploy.sh
./deploy.sh
```
*Note: The script will prompt you to supply your Smart Router URL and your generated Smart Router API key.*

---

### 3. Trigger the Client Service (Authenticated Request)

Because the client microservice is deployed securely without public access, standard unauthenticated requests will fail with a `401 Unauthorized` or `403 Forbidden` error. 

To trigger the microservice, you must pass a Google-issued OpenID Connect (OIDC) identity token representing your active `gcloud` developer identity (or any service account possessing the `roles/run.invoker` IAM role on the deployed service) in the `Authorization` header:

```bash
curl -X POST "https://gemini-apikey-client-xxxx-uc.a.run.app/generate" \
  -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Explain the significance of the expansion of the universe in simple terms."}'
```

#### Expected Success Response
The client service returns the generated text along with custom auditing headers injected by the router:
```json
{
  "text": "The expansion of the universe means that space itself is stretching. Think of it like a balloon covered in dots. As you blow it up, every dot moves further away from every other dot...",
  "model_routed": "gemini-2.5-flash-lite",
  "client_tier": "premium",
  "latency_ms": "1230"
}
```

If your application exceeds its configured capacity (RPM or TPM), the router intercepts the request immediately, returns a `429 Too Many Requests` status code, and prevents upstream billing spikes.
