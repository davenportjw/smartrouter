# 🌐 Routing & Complexity Rules

This guide explains how to configure routing rules and prompt-complexity threshold mappings.

---

## 🌐 Interception & Custom Routing Rules

Routing rules evaluate request variables (application ID, client tier, requested model, and headers) to rewrite target models or locations.

### Creating a Routing Rule
1. Go to `/admin/rules`.
2. Click **Add Routing Rule**.
3. Fill in the fields:
   * **Model Request Pattern**: The model name in the incoming request to intercept (e.g. `gemini-2.5-flash`, or `*`).
   * **Target Application Scope**: Bind the rule to a specific App, or select `Global (All Applications)`.
   * **Client Tier Eligibility**: Apply the rule to specific tiers (e.g. `Premium Tier Only`), or select `All Tiers`.
   * **Header-Based Route Segmentation (Optional)**: Require an HTTP header and value pattern (e.g., matching header `X-Route-Priority` with `gold`).
   * **Routed Target Model**: The upstream model to route the request to (e.g., `gemini-2.5-pro`).
   * **Target Location (GCP)**: The upstream GCP region target (e.g., `us-central1`).
   * **Fallback Model (Optional)**: Fallback target if the primary model returns an error.
   * **Priority Weight (1-10)**: Rule match evaluation order (higher weight executes first).
4. Click **Create Routing Rule**.

### Editing a Routing Rule
1. Go to `/admin/rules`.
2. Click **Edit** next to the target rule row.
3. Modify any fields in the modal (such as model pattern, scopes, target models, fallback target, or priority weights).
4. Click **Save Routing Rule**. The admin dashboard uses reactive HTMX redirection to save updates safely without page-reload race conditions and automatically refreshes the table.

### GCP Models & Endpoints Viewer
Go to `/admin/models` to access the unified **Model Registry & Control Plane**. The single pane groups discovered models by category (**Generative Models** and **Embedding Models**), letting you:
* **Enable or Disable** individual regional options or custom endpoints for router traffic.
* **Track live metrics** (Request Count, Avg Latency, and Accum Spend) in real time when a model is enabled.
* **Identify defunct or deprecated models** automatically marked as **Obsolete**.
* **Delete obsolete configurations** directly from the database.
* **Trigger dynamic discovery** using the **Refresh Discovered Models** action.

---

## 🧠 Tuning Complexity-Based Dynamic Routing

Clients can request the virtual model **`gemini-dynamic`**. The proxy evaluates the prompt size and attributes at runtime and maps it to a complexity bucket (`simple`, `medium`, or `complex`):

```
[Client Request] -> gemini-dynamic
                          │
              ┌───────────┴───────────┐
      Simple (Lite)    Medium (Flash)   Complex (Pro)
```

### Customizing Complexity Thresholds
1. Go to `/admin/complexity`.
2. Click **Edit Complexity Settings**.
3. Set the boundaries:
   * **Simple Limit (Characters)**: Prompts below this character limit route to the **Simple Model** (default: `gemini-2.5-flash-lite`).
   * **Medium Limit (Characters)**: Prompts above the Simple limit but below this count route to the **Medium Model** (default: `gemini-2.5-flash`).
   * **Complex Prompts**: Prompts exceeding the Medium limit, or containing tools, images, or video files, route to the **Complex Model** (default: `gemini-2.5-pro`).
4. Map each bucket to your choice of physical model or custom Vertex endpoint.
5. Click **Save Settings**.
