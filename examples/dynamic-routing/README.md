# 🌐 Dynamic Routing in Gemini Smart Router

This directory contains runnable examples demonstrating how to leverage the **Gemini Smart Router's** powerful dynamic routing systems. 

Dynamic routing optimizes your multi-model workloads across two dimensions:
1. **Dynamic Complexity-Based Routing (`gemini-dynamic`)**: Inspects the prompt's semantic or syntactic characteristics at runtime to automatically choose the most cost-efficient model.
2. **Dynamic Rules-Based Routing**: Allows you to declaratively override target endpoints, fallback paths, or model selections based on client tier, application ID, and custom request headers.

---

## 🚀 Getting Started

### 1. Install Dependencies
First, set up your Python virtual environment and install requirements:
```bash
python3 -m venv venv
source venv/bin/activate
pip install -r requirements.txt
```

### 2. Ensure the Local Smart Router is Running
Start the Smart Router in dynamic local development mode:
```bash
./run_local.sh
```

---

## 🧠 Example 1: Dynamic Complexity Routing (`gemini-dynamic`)

Complexity-based routing maps incoming queries to different model tiers based on prompt attributes (such as character length, multimodal status, or tool declarations) or a rapid upstream LLM Semantic Classifier.

### How it Works
Instead of choosing a specific physical model like `gemini-2.5-flash-lite`, your application targets the virtual model name **`gemini-dynamic`**:
```python
url = "http://localhost:8080/v1/models/gemini-dynamic:generateContent"
```

The smart router intercepts this call, analyzes the contents, and maps it to your configured complexity models:
* **`simple`** -> Routes to `gemini-2.5-flash-lite` (ultra-low latency, cheap)
* **`medium`** -> Routes to `gemini-2.5-flash` (perfect balance)
* **`complex`** -> Routes to `gemini-2.5-pro` (heavy reasoning, code, logic)

### Run the Complexity Routing Example
```bash
python3 dynamic_complexity_routing.py
```

---

## ⚙️ Example 2: Dynamic Rules-Based Routing

Rules-based routing applies high-priority conditional rewrites on top of standard or complexity-mapped targets based on client tiers or custom request headers.

### How it Works
You can configure declarative `RoutingRules` in the Smart Router database (e.g. Firestore or `local_db.json`):
```json
{
  "id": "rule-priority-vip",
  "model_pattern": "gemini-1.5-pro",
  "client_tier": "premium",
  "header_name": "X-Route-Priority",
  "header_value": "gold",
  "target_model": "gemini-2.5-pro",
  "target_location": "us-central1",
  "priority_weight": 1
}
```

When a premium client submits a request targeting `gemini-1.5-pro` and provides the header `X-Route-Priority: gold`, the router dynamically upgrades the model target to `gemini-2.5-pro` at runtime!

### Run the Rules-Based Routing Example
```bash
python3 dynamic_rules_routing.py
```

---

## 📊 Auditing Decisions via Response Headers

The Smart Router automatically appends client-auditable response headers on all successfully proxied HTTP responses. Your client applications can read these to log and verify routing decisions in real-time:

| Header | Description |
| --- | --- |
| `X-Requested-Model` | The original model name requested by the client (e.g. `gemini-dynamic`). |
| `X-Routed-Model` | The actual upstream model target processed by the router (e.g. `gemini-2.5-pro`). |
| `X-Client-Tier` | The resolved tier of the customer based on the API key (e.g. `premium`). |
| `X-App-ID` | The resolved application ID executing the request. |
