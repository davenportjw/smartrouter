# 🛡️ Upstream Model Compliance Policy

This policy enforces constraints on upstream Gemini model configurations.

---

## ⚠️ Gemini 2.5+ Baseline & Latest Model Series

> [!IMPORTANT]
> **COMPLIANCE RULE**: Do not use or configure upstream Gemini models older than the **Gemini 2.5** series or Gemma older than the **Gemma 4** series.
> Always utilize the latest Gemini 3.8+ models (e.g., `gemini-3.8-flash`) and Gemma 4 models across agent workflows.

Legacy model versions (e.g., `gemini-1.0-*`, `gemini-1.5-*`, `gemini-2.0-*`, `gemma-2-*`) are strictly prohibited.

---

## ✅ Authorized Upstream Gemini Models & Offered Locations

Use only the verified and authorized models in database rules, integration tests, or complexity targets:

### Gemini 4 Series & Gemini Decisions API
* **`gemini-4-argon`**: Frontier reasoning model for architecture design, multi-system synthesis, and deep algorithmic problems.
  * **Offered Locations**: `global`, `us`, `us-central1`
* **`gemini-decision-1`**: Dedicated single-forward-pass calibrated probability engine (`POST https://generativelanguage.googleapis.com/v1beta/decisions`) returning calibrated choice/score distributions with zero output tokens (`\$0.10` / 1M input tokens, `\$0.00` output).
  * **Offered Locations**: `global`

### Gemini 3.8 Series
* **`gemini-3.8-flash`**: High-performance flagship model offering ultra-low latency, multimodal understanding, and reasoning.
  * **Offered Locations**: `global`, `us`, `us-central1`, `europe-west1`
  * *Note*: In the 3.8 family, only `gemini-3.8-flash` is available. Fictional variants like `gemini-3.8-pro` or `gemini-3.8-flash-lite` do not exist and will fail validation.

### Gemini 3.5 Series
* **`gemini-3.5-flash`**: High-speed, high-efficiency model for broad generation tasks.
  * **Offered Locations**: `global`, `us`, `us-central1`, `europe-west9`, `europe-west1`
* **`gemini-3.5-flash-lite`**: Cost-optimized, ultra-fast model ideal as default for simple prompts and classification.
  * **Offered Locations**: `global`, `us`, `us-central1`

### Gemini 3.1 Series
* **`gemini-3.1-pro`**: Premier deep-reasoning model for complex coding, mathematical logic, and multi-step tasks.
  * **Offered Locations**: `global`, `us`, `us-central1`
* **`gemini-3.1-flash-lite`**: Lightweight high-throughput model.
  * **Offered Locations**: `global`, `us`, `us-central1`

### Gemini 2.5 Series (Baseline Production)
* **`gemini-2.5-flash`**: Standard generation baseline.
  * **Offered Locations**: `us-central1`, `us`, `global`
* **`gemini-2.5-pro`**: Complex reasoning baseline.
  * **Offered Locations**: `us-central1`, `us`, `global`
* **`gemini-2.5-flash-lite`**: High-efficiency baseline.
  * **Offered Locations**: `us-central1`, `us`, `global`

### Gemma 4 Series
* **`gemma-4-26b`**, **`gemma-4-31b`**, **`gemma-4-4b`**: Open models available for local cluster or endpoint serving.

---

## 🧪 Compliance in Development

### 1. Database Configurations
Ensure seeded rules in `/data/local_db.json` or Firestore target authorized models:
* **Non-Compliant**: `"target_model": "gemini-1.5-flash"`
* **Non-Compliant**: `"target_model": "gemini-3.8-pro"` (non-existent model)
* **Compliant**: `"target_model": "gemini-3.8-flash"`
* **Compliant**: `"target_model": "gemini-3.5-flash-lite"`

### 2. Integration Tests
Target only authorized models and verify their offered locations in tests:
```go
_ = store.SaveRule(ctx, config.RoutingRule{
    ID:             "rule-tdd-1",
    ModelPattern:   "gemini-dynamic",
    TargetModel:    "gemini-3.8-flash", // Compliant
    TargetLocation: "us-central1",
})
```
