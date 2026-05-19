# 🛡️ Calling the Smart Router with Google Service Accounts (Zero-Key IAM)

This directory contains a ready-to-deploy Python microservice demonstrating how a client application authenticates with the **Gemini Smart Router** using a native Google Service Account identity (Zero-Key IAM).

---

## 🚀 How it Works

Instead of generating, storing, and rotating static API keys, this method relies on standard Google Cloud Identity and Access Management (IAM).

1. When running on Cloud Run, the client microservice has a bound runtime **Service Account Identity** (represented by an email address, e.g., `my-service-identity@my-gcp-project.iam.gserviceaccount.com`).
2. When making a request, the client automatically fetches an OpenID Connect (OIDC) **Google ID Token** bound specifically to the audience of the **Smart Router URL**.
3. The client sends the request passing this token in the `Authorization: Bearer <token>` header.
4. The Smart Router intercepts the token, verifies its authenticity with Google (validating signature, expiration, and audience), extracts the email address, and routes the traffic mapping it directly to your custom configured application priorities and limits.

```
[Cloud Run Client Service] 
       │
       │ 1. Retrieve OIDC ID Token from metadata server
       │    Audience: https://your-smart-router.a.run.app
       │ 2. Send HTTP POST to /v1/models/gemini-2.5-flash-lite:generateContent
       │    Headers: [ Authorization: Bearer eyJhbGci... ]
       ▼
[Gemini Smart Router]
       │
       │ 3. Validate token signature and audience
       │ 4. Extract service account email: 'my-service-identity@...'
       │ 5. Lookup App by Service Account email (ID / Slug)
       │ 6. Enforce App-specific rate limits and priorities
       ▼
[Upstream Gemini API / Vertex AI]
```

---

## 📋 Step-by-Step Guide

### 1. Determine Your Service Account Email
By default, when you deploy a service to Cloud Run, it runs using the **Compute Engine default service account** or a custom Service Account you specify.
- Default Service Account: `PROJECT_NUMBER-compute@developer.gserviceaccount.com`
- Custom Service Account: `your-service-name@YOUR_GCP_PROJECT_ID.iam.gserviceaccount.com` (Recommended)

Copy this email address.

---

### 2. Register the Service Account as an App in the Admin Panel
Configure the Smart Router to recognize and authorize this service account identity:

1. Navigate to your **Gemini Smart Router Admin Panel** (e.g., `https://your-router-url.a.run.app/admin`).
2. Select the **Clients** tab and ensure the client (e.g., billing or team boundary) is created.
3. Select the **Applications** tab:
   - Click **New Application**.
   - **IMPORTANT**: Set the **Application ID (Slug)** to the exact email address of the service account (e.g., `PROJECT_NUMBER-compute@developer.gserviceaccount.com`).
   - Assign a friendly name (e.g., `Production Service Account App`).
   - Bind it to the appropriate Client.
   - Set your desired maximum **RPM**, **TPM**, and **Queueing Priority**.
4. Click **Create Application**.

No API Keys are needed! The service account's secure identity is now authorized.

---

### 3. Build and Deploy to Cloud Run

To simplify setup and ensure secure isolation, we provide an automated deployment script `deploy.sh` that:
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
*Note: The script will prompt you to supply your Smart Router URL. You do not pass any API keys or secrets in environment variables! The runtime Google environment automatically handles authorization.*

---

### 4. Trigger the Client Service (Authenticated Request)

Because the client microservice is deployed securely without public access, standard unauthenticated requests will fail with a `401 Unauthorized` or `403 Forbidden` error. 

To trigger the microservice, you must pass a Google-issued OpenID Connect (OIDC) identity token representing your active `gcloud` developer identity (or any service account possessing the `roles/run.invoker` IAM role on the deployed service) in the `Authorization` header:

```bash
curl -X POST "https://gemini-sa-client-xxxx-uc.a.run.app/generate" \
  -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "What are the benefits of a microservices architecture over a monolith?"}'
```

#### Expected Success Response
The client service outputs the response text, including custom headers detailing the routed model and parent client tier:
```json
{
  "text": "A microservices architecture offers several key advantages over a monolithic design, including independent deployability, granular scalability, and technology flexibility...",
  "model_routed": "gemini-2.5-flash-lite",
  "client_tier": "premium",
  "latency_ms": "1150"
}
```
