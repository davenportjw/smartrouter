# 🛡️ Upstream Model Compliance Policy

This policy enforces constraints on upstream Gemini model configurations.

---

## ⚠️ Gemini 2.5+ Requirement

> [!IMPORTANT]
> **COMPLIANCE RULE**: Do not use or configure upstream Gemini models older than the **Gemini 2.5** series.

Legacy model versions (e.g., `gemini-1.0-*`, `gemini-1.5-*`, `gemini-2.0-*`) are prohibited.

---

## ✅ Authorized Upstream Gemini Models

Use only the following authorized models in database rules, integration tests, or complexity targets:

* **`gemini-2.5-flash`**: Default choice for standard content generation.
* **`gemini-2.5-pro`**: Used for complex reasoning, programming, math, or large multimodal files.
* **`gemini-2.5-flash-lite`**: Used for low latency, cost-sensitive requests.
* **Subsequent 2.5+ & 3.x+ Series**: Models exceeding the 2.5 baseline are supported.
* **Preview Models**: Compliant versions ending in `-preview` are supported.

---

## 🧪 Compliance in Development

### 1. Database Configurations
Ensure seeded rules in `/data/local_db.json` or Firestore target authorized models:
* **Non-Compliant**: `"target_model": "gemini-1.5-flash"`
* **Compliant**: `"target_model": "gemini-2.5-flash-lite"`

### 2. Integration Tests
Target only authorized models in tests:
```go
_ = store.SaveRule(ctx, config.RoutingRule{
    ID:             "rule-tdd-1",
    ModelPattern:   "gemini-dynamic",
    TargetModel:    "gemini-2.5-flash", // Compliant
    TargetLocation: "us-central1",
})
```
