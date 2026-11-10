# 📱 Apps & API Keys Management

This guide explains how to register, edit, and manage applications and API keys.

---

## 🛠️ Registering & Editing an Application

You must register applications to isolate rate limits and determine queue routing priority.

### Registering an App
1. Go to `/admin/apps`.
2. Click **+ Add Application** to open the right-hand contextual inspector pane (`#modal-container`) next to the applications table.
3. Fill in the fields:
   * **Application ID (Slug)**: Unique identifier (e.g. `invoice-processor-prod`).
   * **Application Name**: Descriptive title (e.g., `Invoice Processor Service`).
   * **Parent Client**: Select the owner Client organization.
   * **RPM / TPM**: Configure the sliding-window rate limits. If left blank or `0`, the application automatically inherits `FallbackRPM` and `FallbackTPM` from its parent Client Organization.
   * **Opt Out of TPM Rate Limiting**: Toggle this checkbox to completely bypass Tokens Per Minute (TPM) limits for this application and rely solely on RPM limits.
   * **Routing Priority**: Choose `low`, `medium`, or `high` (or leave blank to inherit the parent Client's `FallbackPriority`).
4. Click **Create Application**.

### Editing or Deleting an App
1. Go to `/admin/apps`.
2. Click **Edit** next to the target application row to load its settings into the right-hand contextual inspector pane (`#modal-container`) beside the table. The Application ID (Slug) is immutable.
3. Click **Save Changes**.
4. To remove an application, click **Delete** (`POST /admin/apps/delete`) on the application row.

---

## 🔑 Managing API Keys

API keys authorize applications to send requests to the proxy.

### Generating a Key
1. Go to `/admin/keys`.
2. Click **+ Generate API Key** to open the right-hand contextual inspector pane (`#modal-container`) next to the keys table.
3. Select the target Application from the dropdown.
4. Click **Generate Key**.
5. **Save the key immediately**: The raw key (prefixed with `gr_key_`) is shown only once. The database stores only the SHA-256 hash of the key.

### Editing/Updating a Key
1. Go to `/admin/keys`.
2. Click **Edit** next to the target API Key row.
3. In the right-hand contextual inspector pane (`#modal-container`), you can dynamically re-bind the key to a different logical **Application** or change its access **Status** (toggle between `active` and `revoked`).
4. Click **Save Key Details**.

### Revoking a Key Directly
1. Locate the key in the `/admin/keys` table.
2. Click **Revoke** next to the target key.
3. The key status changes to `revoked` in the database immediately. The proxy blocks subsequent requests using this key, returning a `401` code.
