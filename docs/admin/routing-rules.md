# 🌐 Routing & Complexity Rules

This guide explains how to configure routing rules and prompt-complexity threshold mappings.

---

## 🌐 Interception & Custom Routing Rules

Routing rules evaluate request variables (application ID, client tier, requested model, and headers) to rewrite target models or locations.

### Creating a Routing Rule
1. Go to `/admin/rules`.
2. Click **Add Routing Rule**.
3. Fill in the fields:
   * **Model Request Pattern**: The model name in the incoming request to intercept (e.g. `gemini-3.8-flash`, or `*`).
   * **Target Application Scope**: Bind the rule to a specific App, or select `Global (All Applications)`.
   * **Client Tier Eligibility**: Apply the rule to specific tiers (e.g. `Premium Tier Only`), or select `All Tiers`.
   * **Classifier & Task Match Conditions (Optional)**:
     * **Decision Engine (`decision_engine`)**: Require classification by a specific engine (`jev`, `fastino`, `contrastive-lm`, `llm`, `heuristics`), or `Any Decision Engine`.
     * **Complexity Tier (`complexity_tier`)**: Require a specific classified complexity bucket (`simple`, `medium`, `complex`).
     * **Minimum Confidence (`min_confidence`)**: Require the classifier confidence score to be $\ge$ a threshold (e.g., `0.75`).
     * **Agent Task Kind (`task_kind`)**: Require a specific active task context (e.g., `code_generation`, `debugging`, `security_audit`, `triage`).
   * **Header-Based Route Segmentation (Optional)**: Require an HTTP header and value pattern (e.g., matching header `X-Route-Priority` with `gold`).
   * **Routed Target Model**: The upstream model to route the request to (e.g., `gemini-3.1-pro`).
   * **Target Location (GCP)**: The upstream GCP region target (e.g., `us-central1`).
   * **Fallback Model (Optional)**: Fallback target if the primary model returns an error.
   * **Priority Weight (1-10)**: Rule match evaluation order (higher weight executes first).
4. Click **Create Routing Rule**.

### Editing a Routing Rule
1. Go to `/admin/rules`.
2. Click **Edit** next to the target rule row.
3. Modify any fields in the modal (such as model pattern, scopes, classifier conditions, target models, fallback target, or priority weights).
4. Click **Save Routing Rule**. The admin dashboard uses reactive HTMX redirection to save updates safely without page-reload race conditions and automatically refreshes the table.

### GCP Models & Endpoints Viewer
Go to `/admin/models` to access the unified **Model Registry & Control Plane**. The single pane groups discovered models by category (**Generative Models** and **Embedding Models**), letting you:
* **Enable or Disable** individual regional options or custom endpoints for router traffic.
* **Inspect Unit Cost (1M In / Out)**: View deterministic input and output token pricing per 1M tokens (including `gemini-4-argon` at \$2.00 / \$10.00, `gemini-3.8-flash` at \$0.15 / \$0.60, and `$0.00 (Local)` for `@local` cluster models).
* **Track live metrics**: View Request Count, Avg Latency, and Accum Spend in real time when a model is enabled (automatically matching both compound `model@location` IDs and stripped base model names from proxy logs).
* **Identify defunct or deprecated models** automatically marked as **Obsolete** and **Delete obsolete configurations** directly from the database.
* **Trigger Asynchronous Background Model Discovery**: Clicking **Refresh Discovered Models** (`POST /admin/models/refresh`) launches a non-blocking 4-stage background discovery & verification pipeline across local clusters and Vertex AI regions (`us-central1`, `us`, `global`) with bounded concurrency, displaying a live progress bar (`#model-refresh-banner`, `#model-refresh-progress`) that polls `GET /admin/models/refresh/status` until completion. You can safely navigate away or close the page at any time while discovery runs in the background.

---

## 🧠 Tuning Complexity-Based Dynamic Routing

Clients can request the virtual model **`gemini-dynamic`**. The proxy evaluates the prompt structure, features, session context, and agent task at runtime and maps it to a complexity bucket (`simple`, `medium`, or `complex`):

```
[Client Request] -> gemini-dynamic
                          │
              ┌───────────┴───────────┐
      Simple (Lite)    Medium (Flash)   Complex (Pro)
```

### Decision Engines
You can configure the decision engine per application in `/admin/complexity`:
* **`jev` (Jev Structured Decision Model)**: Fast microsecond rule-based logit scoring using extracted prompt features (code density, algorithmic/math signals, multi-step patterns, length), or remote `/v1/systemone` endpoint evaluation.
* **`fastino` (Fastino GLiClass / Pioneer Classifier)**: Sub-millisecond zero-shot sequence classification via a remote Fastino endpoint (`FastinoEndpoint`) or local zero-shot structural density fallback.
* **`contrastive-lm` / `clm` (Contrastive-LM Embedding Classifier)**: Microsecond 5D Euclidean contrastive distance classifier comparing prompt feature vectors against Simple, Medium, and Complex anchor profiles, or remote `/rank` endpoint evaluation (`CLMEndpoint`).
* **`llm` (LLM Semantic Classifier)**: Dispatches prompt classification to a lightweight foundation model (`gemini-3.5-flash-lite`) with optional custom system guidelines.
* **`heuristics` (Static Bounds)**: Character length and multimodal/tool boundary checks.

### Secret Key Management & Cloud Secret Manager
In `/admin/complexity`, the **Decision Engine Secret Keys & Cloud Secret Manager Setup** panel displays masked key statuses (`jev`, `fastino`, `contrastive-lm`) and never exposes raw credentials in HTML or JSON responses:
* **Runtime Vault**: Enter an API key in the **Configure Complexity Settings** modal or post to `/admin/complexity/keys` to store it in the in-memory secret vault without writing raw secrets to `local_db.json` or Firestore.
* **Environment & Cloud Run Secret Manager**: Set `JEV_API_KEY`, `FASTINO_API_KEY`, or `CLM_API_KEY` (or a custom `SecretRef` variable) via Cloud Run `--set-secrets` backed by Google Cloud Secret Manager.

### Customizing Complexity Settings & Task Mappings
1. Go to `/admin/complexity`.
2. Click **Configure Settings** next to the target application.
3. Choose the **Routing Engine** (`jev`, `fastino`, `contrastive-lm`, `llm`, or `heuristics`) and set the model allocations:
   * **Simple Model**: Target for quick lookups and greetings (default: `gemini-3.5-flash-lite`).
   * **Medium Model**: Target for standard summarization and content generation (default: `gemini-3.8-flash`).
   * **Complex Model**: Target for deep code generation, algorithmic reasoning, debugging, or multimodal/tool payloads (default: `gemini-3.1-pro`).
4. **Endpoints, Secret Reference & Confidence Threshold**: Optionally configure `JevEndpoint`, `FastinoEndpoint`, `CLMEndpoint`, `SecretRef`, and `MinConfidence` (escalating low-confidence classifications to a higher tier).
5. **Custom Tier Criteria (`SimpleCriteria`, `MediumCriteria`, `ComplexCriteria`)**: Provide comma-separated keywords to boost tier scores.
6. **Agent Task Overrides (`TaskMapping`)**: Optionally configure explicit per-task model overrides for `code` (`task_code`), `triage` (`task_triage`), and `summarization` (`task_summarization`).
7. **Tuning Semantic Criteria**: If **Use LLM Semantic Classifier** is active, specify **Additional Classification Guidelines** (e.g., `"always classify coding requests as complex"`).
8. Click **Save Settings**.

### 🔬 Smart Routing Inspector & Simulator
Directly inside `/admin/complexity`, use the **Smart Routing Inspector & Simulator** to test prompts in real time:
* Enter any test prompt, select an engine (`jev`, `fastino`, `contrastive-lm`, or `heuristics`), and optionally specify an **Agent Task Context** (`code_generation`, `debugging`, `triage`, `summarization`).
* Click **Analyze & Route Prompt** to inspect the resolved Target Model, Complexity Tier, Confidence score, Extracted Feature Signals, and Decision Rationale.

---

## 🔄 Pipeline Interoperation: Multi-Stage Smart Routing

The Smart Router evaluates routing in sequential stages:

1. **Stage 0: Per-Prompt Overrides**
   * Explicit HTTP headers (`X-Router-Model`, `X-Router-Complexity`, `X-Router-Engine`) immediately pin the model, tier, or decision engine for a single request.
2. **Stage 1: Multi-Turn Session Affinity**
   * Requests carrying `X-Session-ID` and `X-Session-Policy: lock` stay pinned to the model resolved at the start of the session across subsequent conversation turns.
3. **Stage 2: Agent Task Switching**
   * Autonomous agents passing `X-Agent-Task` (or `X-Task-Kind`) dynamically switch model tiers based on task requirements (e.g. `debugging` -> Complex, `summarization` -> Medium, `triage` -> Simple).
4. **Stage 3: Complexity Decision Engine (App-Scoped)**
   * Evaluates the prompt via the configured engine (`jev`, `fastino`, `contrastive-lm`, `llm`, or `heuristics`) to select the base model.
5. **Stage 4: Declarative & Classifier-Conditional Routing Rules (Cross-App / Global)**
   * Evaluates declarative and classifier match conditions (tier, application scope, custom headers, `decision_engine`, `complexity_tier`, `min_confidence`, `task_kind`) and performs final location resolution.

### 🚫 Complete Application Opt-Out
To completely bypass the routing pipeline for a particular application:
1. Go to `/admin/apps` and click **Edit** next to the target application.
2. Check **Opt Out of Dynamic Routing**.
3. Click **Save Changes**.
* This bypasses all dynamic stages and declarative rules for requests using that application's API keys, directly targeting the requested model.


