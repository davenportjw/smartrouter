# 🌐 Routing & Complexity Rules

This guide explains how to configure routing rules and prompt-complexity threshold mappings.

---

## ⚙️ Upgrades & Fallback Target Routing

Routing rules evaluate request variables (application ID, client tier, requested model, and headers) to rewrite target models or locations.

### Creating a Routing Rule
1. Go to `/admin/rules`.
2. Click **Create Routing Rule**.
3. Fill in the fields:
   * **Requested Model Pattern**: The model name in the incoming request to intercept (e.g. `gemini-2.5-flash`, or `*`).
   * **Application**: Bind the rule to a specific App, or select `all`.
   * **Client Tier**: Apply the rule to specific tiers (e.g. `premium`), or select `all`.
   * **Header Match**: (Optional) Require an HTTP header and value pattern (e.g., matching header `X-Route-Priority` with `gold`).
   * **Target Model**: The upstream model to route the request to (e.g., `gemini-2.5-pro`).
   * **Target Location**: The upstream GCP region target (e.g., `us-central1`).
   * **Fallback Model**: (Optional) Fallback target if the primary model returns an error.
   * **Rule Priority**: High-weight matching rules execute first.
4. Click **Save Routing Rule**.

### GCP Models & Endpoints Viewer
Go to `/admin/models` to inspect deployed Vertex AI foundation models, locations, and custom fine-tuned endpoints in real time. 

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
