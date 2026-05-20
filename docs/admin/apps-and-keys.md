# 📱 Apps & API Keys Management

This guide explains how to register applications and manage API keys.

---

## 🛠️ Registering an Application

You must register applications to isolate rate limits and determine queue routing priority.

### Steps
1. Go to `/admin/apps`.
2. Click **Create New Application**.
3. Fill in the fields:
   * **Application Name**: A description (e.g., `invoice-processor`).
   * **Owner Client**: Select the parent Client.
   * **RPM / TPM**: Configure the sliding-window rate limits.
   * **Routing Priority**: Choose `high`, `medium`, or `low`. High-priority applications get faster access slots when under queue load.
4. Click **Save Application**.

---

## 🔑 Managing API Keys

API keys authorize applications to send requests to the proxy.

### Generating a Key
1. Go to `/admin/keys`.
2. Click **Generate API Key**.
3. Select the target Application from the dropdown.
4. Click **Generate Key**.
5. **Save the key immediately**: The raw key (prefixed with `gr_key_`) is shown only once. The database stores only the SHA-256 hash of the key.

### Revoking a Key
1. Locate the key in the `/admin/keys` table.
2. Click **Revoke** next to the target key.
3. The key status changes to `revoked` in the database immediately. The proxy blocks subsequent requests using this key, returning a `401` code.
