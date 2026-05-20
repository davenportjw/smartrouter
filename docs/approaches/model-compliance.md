# 🛡️ Upstream Model Compliance Policy

To ensure optimal system performance, reasoning capabilities, and long-term maintenance, this repository enforces strict constraints on upstream Gemini model configurations.

---

## ⚠️ The Baseline Gemini 2.5+ Requirement

> [!IMPORTANT]
> **CRITICAL COMPLIANCE RULE**: You must NEVER use, route to, or configure an upstream Gemini model version older than the **Gemini 2.5** series.

Legacy model versions (such as `gemini-1.0-*`, `gemini-1.5-*`, and `gemini-2.0-*`) are deprecated. They are prohibited for active routing rule configurations in this codebase.

---

## ✅ Authorized Upstream Gemini Models

When seeding database rules, writing integration tests, or setting up complexity targets, use only the following authorized models:

* **`gemini-2.5-flash`**: Default choice for general-purpose, high-throughput content generation tasks. Offers the perfect balance of latency and cost.
* **`gemini-2.5-pro`**: Mandatory choice for heavy reasoning, complex programming analysis, math, or multimodal files requiring advanced reasoning.
* **`gemini-2.5-flash-lite`**: Optimized for ultra-low latency and high-frequency, cost-sensitive requests.
* **Subsequent 2.5+ & 3.x+ Series**: Future releases exceeding the 2.5 baseline (e.g. Gemini 3.0, Gemini 3.5) are fully compliant.
* **Preview Models**: Compliant model versions ending in `-preview` (with or without date suffixes, e.g. `gemini-3.1-pro-preview`, `gemini-2.5-flash-preview-09-2025`) are fully supported.

---

## 🧪 Ensuring Compliance in Development

### 1. Database Seeding / Config Files
Ensure all seeded rules inside `/data/local_db.json` or Firestore target authorized models.
* **Non-Compliant**:
  ```json
  "target_model": "gemini-1.5-flash"
  ```
* **Compliant**:
  ```json
  "target_model": "gemini-2.5-flash-lite"
  ```

### 2. Integration Tests
When seeding mock routing pathways inside `pkg/proxy/proxy_test.go`, target only authorized models to ensure test suite validity:
```go
// Seeding compliant dynamic rule inside test setup
_ = store.SaveRule(ctx, config.RoutingRule{
    ID:             "rule-tdd-1",
    ModelPattern:   "gemini-dynamic",
    TargetModel:    "gemini-2.5-flash", // Compliant
    TargetLocation: "us-central1",
})
```
