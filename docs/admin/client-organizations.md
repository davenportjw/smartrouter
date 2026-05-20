# 🏢 Client Organizations Management

This guide explains how to register, edit, and manage Client Organizations (top-level tenant / subscriber accounts) in the Smart Router dashboard.

---

## 🛠️ Registering & Editing a Client Organization

Client Organizations represent subscription/billing boundaries (`premium`, `standard`, `free` tiers). You must register a client organization before you can allocate specific applications and API keys under it.

### Registering a Client Organization
1. Go to `/admin/clients`.
2. Click **Add Client Organization**.
3. Fill in the fields:
   * **Client ID (Slug)**: Unique identifier (e.g., `marketing-prod`).
   * **Organization Name**: Descriptive title (e.g., `Marketing Department`).
   * **Billing Subscription Tier**: Select the subscription tier (`free`, `standard`, or `premium`).
   * **Fallback Priority Profile**: Select the default fallback queueing priority (`low`, `medium`, or `high`).
   * **Fallback RPM / TPM**: Configure the default sliding-window fallback rate limits.
4. Click **Create Organization**.

### Editing a Client Organization
1. Go to `/admin/clients`.
2. Click **Edit** next to the target client organization row.
3. Modify the organization name, subscription tier, default fallback priority, or rate limits in the modal. The Client ID (Slug) is unique and cannot be changed.
4. Click **Save Changes**.

---

## 🗑️ Deleting a Client Organization

If an organization is no longer active, you can delete it directly.

1. Locate the client organization in the `/admin/clients` table.
2. Click **Delete** next to the target row.
3. Confirm the deletion in the browser prompt.
   > [!WARNING]
   > Deleting a Client Organization will orphan any applications and API keys bound to it. Ensure all dependent resources are migrated first.
