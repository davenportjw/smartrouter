# 📱 Apps & API Keys Administration

This guide contains step-by-step operational procedures for onboarding new application workloads and managing the lifecycle of access API keys.

---

## 🛠️ User Flow 1: Onboarding a New Application

Before client applications can route traffic through the proxy, you must register their profile boundary. This isolates rate limits and determines routing priority.

### Step 1: Open the Apps Console
* Navigate to: `/admin/apps` on your dashboard.
* You will see the list of all active Application contexts, their owner Clients, current RPM/TPM budgets, and routing Priority weights.

### Step 2: Add a New Application
1. Click **Create New Application** in the top right corner.
2. Fill in the registration modal fields:
   - **Application Name**: A recognizable name (e.g., `production-chatbot`).
   - **Owner Client**: Select the parent Client profile (determines billing tier context, e.g., `c1`).
   - **Requests Per Minute (RPM)**: Set the maximum request boundary (e.g., `60`).
   - **Tokens Per Minute (TPM)**: Set the maximum sliding token budget (e.g., `40000`).
   - **Routing Priority**: Choose `high`, `medium`, or `low`. High-priority apps secure fast-path routing slots when processing upstream queues.
3. Click **Save Application**. The app profile is registered dynamically.

---

## 🔑 User Flow 2: API Key Lifecycle Management

Once the application profile is onboarded, you must generate a cryptographically secure key credential.

### Step 1: Open the Keys Management Panel
* Navigate to: `/admin/keys` on the dashboard.
* This page displays all registered API keys, their status (`active` or `revoked`), and the application profile they are bound to.

### Step 2: Provision a New API Key
1. Click **Generate API Key** at the top right.
2. Select the target Application from the dropdown menu (e.g., `production-chatbot`).
3. Click **Generate Key**.
4. **CRITICAL SECURITY WARNING**: The raw API Key (prefixed with `gr_key_`) is shown **ONCE** inside a prominent success banner:
   > [!WARNING]
   > **Copy this key immediately and save it securely!** It will never be displayed again. The database only stores a cryptographically secure SHA-256 hash of the key.
5. Copy the key value and distribute it to your client application configuration.

### Step 3: Revoking a Compromised or Stale Key
If a client key is leaked or needs to be rotated:
1. Locate the key in the keys table on the `/admin/keys` tab.
2. Click the red **Revoke** button next to the target key hash.
3. The status updates to `revoked` instantly in the database. The proxy will block any incoming request using this key in real time, returning an `HTTP 401 Unauthorized` error.
