# 🔌 Running Client Integration Examples

This guide explains how to run the client integration examples located in the `/examples` directory.

---

## 🔑 1. Static API Key Example (`examples/cloudrun-apikey/`)

Demonstrates how client applications connect to the Smart Router using an API key.

### Steps
1. Go to the directory:
   ```bash
   cd examples/cloudrun-apikey
   ```
2. Ensure the local Smart Router is running on `localhost:8080`.
3. Seed an API key (e.g., `gr_key_dev_chat`) in the dashboard.
4. Set the environment variables:
   ```ini
   ROUTER_URL="http://localhost:8080"
   ROUTER_API_KEY="gr_key_dev_chat"
   ```
5. Run the client:
   ```bash
   go run main.go
   ```

---

## 🛡️ 2. Service Account IAM Example (`examples/cloudrun-serviceaccount/`)

Demonstrates how to authenticate using Google Cloud OIDC Service Account tokens.

### Steps
1. Go to the directory:
   ```bash
   cd examples/cloudrun-serviceaccount
   ```
2. Set the target environment variable:
   ```ini
   ROUTER_URL="https://your-smart-router-cloudrun-url.run.app"
   ```
3. Run the client using Application Default Credentials:
   ```bash
   go run main.go
   ```

---

## 🧠 3. Dynamic Routing Examples (`examples/dynamic-routing/`)

Python scripts demonstrating dynamic complexity routing and declarative rules routing.

### Python Environment Setup
1. Go to the directory:
   ```bash
   cd examples/dynamic-routing
   ```
2. Create a virtual environment and install dependencies:
   ```bash
   python3 -m venv venv
   source venv/bin/activate
   pip install -r requirements.txt
   ```
3. Ensure the local Smart Router is running:
   ```bash
   ./run_local.sh
   ```

### Scenario A: Complexity-Based Routing (`gemini-dynamic`)
Sends prompts of varying lengths to the virtual `gemini-dynamic` model:
```bash
python3 dynamic_complexity_routing.py
```

### Scenario B: Custom Rules-Based Routing
Sends requests with custom headers to trigger model upgrades:
```bash
python3 dynamic_rules_routing.py
```
