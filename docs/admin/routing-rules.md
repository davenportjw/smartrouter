# 🌐 Routing & Complexity Rules

Configure dynamic upgrades, regional failover targets, fallback pathways, and semantic prompt-complexity thresholds mapped to the virtual `gemini-dynamic` model name.

---

## ⚙️ User Flow 4: Upgrades & Fallback Target Routing

Routing rules evaluate request variables (application ID, client tier, requested model, and incoming headers) to dynamically rewrite destination endpoints or target models in real time.

### Step 1: Open the Rules Console
* Navigate to `/admin/rules` on the dashboard.
* You will see the chronological sequence of active routing rules, their matching preconditions, and target configurations.

### Step 2: Add a Dynamic Routing Rule
1. Click **Create Routing Rule** in the top right corner.
2. Complete the configuration form:
   - **Requested Model Pattern**: The model name in the incoming request to intercept (e.g. `gemini-2.5-flash`, or `*` to match any model request).
   - **Application Boundary**: Bind this rule to a specific App context, or select `all` to make it a global router rule.
   - **Client Tier Filter**: Limit the rule to specific tiers (e.g. `premium`, `standard`), or select `all`.
   - **Header Match Condition**: (Optional) Require a specific HTTP header and value pattern to activate this rule (e.g., matching header `X-Route-Priority` with pattern `gold`).
   - **Target Routed Model**: The actual upstream physical model to route the request to (e.g., `gemini-2.5-pro`).
   - **Target Location / Region**: The upstream Google Cloud region target (e.g., `us-central1`).
   - **Fallback Model Target**: (Optional) If the upstream target returns an error or times out, fallback to this model immediately (e.g., `gemini-2.5-flash-lite`).
   - **Rule Priority Weight**: Integer weight (default `1`). High-weight matching rules execute first.
3. Click **Save Routing Rule**. The rule binds and takes effect dynamically.

### 🔍 GCP Vertex Models & Endpoints Viewer
To make configuring locations and custom models easy, open `/admin/models`:
* The dashboard queries Google Vertex AI Management APIs in real time.
* It displays all active **GCP Locations** for your project, **Custom Fine-Tuned Models**, and deployed **Endpoint Paths**.
* Operators can copy the exact model paths or endpoint strings and paste them directly into routing rules.

---

## 🧠 User Flow 5: Tuning Complexity-Based Dynamic Routing

Instead of selecting a specific physical model in client code, client applications can request the virtual model **`gemini-dynamic`**. 

The Smart Router inspects prompt attributes (character length, word count, tool declarations, or multimodal status) at runtime and dynamically routes the request to one of three complexity buckets: `simple`, `medium`, or `complex`.

```
[Client Request] -> gemini-dynamic
                          │
              ┌───────────┴───────────┐
      Simple (Lite)    Medium (Flash)   Complex (Pro)
```

### Step 1: Open the Complexity Configuration
* Navigate to `/admin/complexity` on the dashboard.
* You will see the active thresholds and model assignments for each complexity tier.

### Step 2: Fine-Tune Thresholds and Assignments
1. Click **Edit Complexity Settings** at the bottom of the table.
2. Tune the threshold boundaries:
   - **Simple Threshold (Characters)**: Queries with character counts below this threshold route to the **Simple Model** (default `gemini-2.5-flash-lite`).
   - **Medium Threshold (Characters)**: Queries with character counts above the Simple limit but below this value route to the **Medium Model** (default `gemini-2.5-flash`).
   - **Complex Threshold**: Queries exceeding the Medium limit, or containing images, videos, or tool call declarations, automatically upgrade to the **Complex Model** (default `gemini-2.5-pro`).
3. Adjust physical model mappings if necessary. (e.g., upgrading `complex` to a custom fine-tuned Vertex endpoint).
4. Click **Save Settings**. The threshold boundaries are updated instantly in the database.
