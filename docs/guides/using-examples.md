# 🔌 Running the Client Integration Examples

This guide provides copy-pasteable, step-by-step runbooks for running the built-in client integration examples located in the `/examples` directory.

---

## 🔑 1. Static API Key Example (`examples/cloudrun-apikey/`)

This containerized service demonstrates how to hook standard client applications up to the Smart Router using a static, hashed header API key.

### Run the API Key Example
1. Navigate to the example directory:
   ```bash
   cd examples/cloudrun-apikey
   ```
2. Spin up the local docker-alternative service or run it natively:
   - Ensure the local Smart Router is running on `localhost:8080`.
   - Seed an active API key in the dashboard (e.g., `gr_key_dev_chat`).
3. Configure environment settings in the example app (or write to `.env`):
   ```ini
   ROUTER_URL="http://localhost:8080"
   ROUTER_API_KEY="gr_key_dev_chat"
   ```
4. Compile and run the Go client:
   ```bash
   go run main.go
   ```
5. The client will dispatch content-generation requests through the proxy and log the routed response.

---

## 🛡️ 2. Service Account IAM Example (`examples/cloudrun-serviceaccount/`)

Demonstrates secure **Zero-Key Authentication** for internal microservices running on Google Cloud. The client service leverages Google OIDC metadata tokens instead of a static key.

### Run the Service Account Client
1. Navigate to the directory:
   ```bash
   cd examples/cloudrun-serviceaccount
   ```
2. Set active context env variables:
   ```ini
   ROUTER_URL="https://your-smart-router-cloudrun-url.run.app"
   ```
3. Execute the client natively using Application Default Credentials:
   ```bash
   go run main.go
   ```
   *(The client program programmatically contacts the local Google Metadata Server, retrieves a secure OIDC Bearer ID Token, injects it into the authorization headers, and authenticates securely with the router proxy.)*

---

## 🧠 3. Dynamic Routing Examples (`examples/dynamic-routing/`)

Run interactive Python scripts demonstrating dynamic complexity-based routing and declarative header rules.

### Setup the Python Environment
1. Navigate to the directory:
   ```bash
   cd examples/dynamic-routing
   ```
2. Create and activate a virtual environment, then install dependencies:
   ```bash
   python3 -m venv venv
   source venv/bin/activate
   pip install -r requirements.txt
   ```
3. Ensure the local Smart Router is running with local database seeding:
   ```bash
   # From workspace root
   ./run_local.sh
   ```

### Scenario A: Run Complexity-Based Routing (`gemini-dynamic`)
This script sends prompts of varying character lengths targeting the virtual `gemini-dynamic` model. It intercepts the response headers to show how simple vs. complex queries are handled:
```bash
python3 dynamic_complexity_routing.py
```

**Expected Console Log Output**:
```
- Sending simple prompt: "Explain gravity in one sentence."
  -> HTTP 200
  -> X-Requested-Model: gemini-dynamic
  -> X-Routed-Model: gemini-2.5-flash-lite

- Sending complex reasoning prompt (large math logic)...
  -> HTTP 200
  -> X-Requested-Model: gemini-dynamic
  -> X-Routed-Model: gemini-2.5-pro
```

### Scenario B: Run Custom Rules-Based Routing
This script sends requests with custom headers to show how the proxy executes model upgrades dynamically:
```bash
python3 dynamic_rules_routing.py
```
*(Watch the console log to verify how injecting headers like `X-Route-Priority: gold` forces the router to upgrade the target model from standard flash to high-performing Pro endpoints dynamically.)*
