# 📱 Apps & API Keys Management

This guide explains how to register, edit, and manage applications and API keys.

---

## 🛠️ Registering & Editing an Application

You must register applications to isolate rate limits and determine queue routing priority.

### Registering an App
1. Go to `/admin/apps`.
2. Click **Add Application**.
3. Fill in the fields:
   * **Application ID (Slug)**: Unique identifier (e.g. `invoice-processor-prod`).
   * **Application Name**: Descriptive title (e.g., `Invoice Processor Service`).
   * **Parent Client**: Select the owner Client organization.
   * **RPM / TPM**: Configure the sliding-window rate limits.
   * **Opt Out of TPM Rate Limiting**: Toggle this checkbox to completely bypass Tokens Per Minute (TPM) limits for this application and rely solely on RPM limits.
   * **Routing Priority**: Choose `low`, `medium`, or `high`.
4. Click **Create Application**.

### Editing an App
1. Go to `/admin/apps`.
2. Click **Edit** next to the target application row.
3. Modify the name, parent client, RPM/TPM rate limits, or queueing priority in the edit modal. The Application ID (Slug) is a unique identifier and cannot be changed.
4. Click **Save Changes**.

---

## 🔑 Managing API Keys

API keys authorize applications to send requests to the proxy.

### Generating a Key
1. Go to `/admin/keys`.
2. Click **Generate API Key**.
3. Select the target Application from the dropdown.
4. Click **Generate Key**.
5. **Save the key immediately**: The raw key (prefixed with `gr_key_`) is shown only once. The database stores only the SHA-256 hash of the key.

### Editing/Updating a Key
1. Go to `/admin/keys`.
2. Click **Edit** next to the target API Key row.
3. In the modal, you can dynamically re-bind the key to a different logical **Application** or change its access **Status** (toggle between `active` and `revoked`).
4. Click **Save Key Details**.

### Revoking a Key Directly
1. Locate the key in the `/admin/keys` table.
2. Click **Revoke** next to the target key.
3. The key status changes to `revoked` in the database immediately. The proxy blocks subsequent requests using this key, returning a `401` code.
