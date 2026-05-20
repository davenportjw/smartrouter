# Skill: Gemini Smart Router Agent Development & Feature Addition

This skill equips agentic software engineers with the precise guidelines, model selection criteria, core design patterns, and Test-Driven Development (TDD) workflows required to add features or modify the Gemini Smart Router codebase.

---

## 1. Prerequisites

Before performing any modifications:
- Ensure you are in the workspace root (the directory containing `main.go` and `go.mod`).
- Go toolchain must be installed.
- You must run testing and verification commands using standard Go test utilities:
  ```bash
  go test -v ./pkg/...
  ```

---

## 2. Model Selection & Compliance Guidelines

To ensure optimal performance, advanced reasoning capabilities, and long-term support, this repository enforces strict model selection policies.

> [!IMPORTANT]
> **CRITICAL RULE**: You must NEVER use, route to, or configure a Gemini model version earlier than the **Gemini 2.5** series. Legacy models (such as `gemini-1.0-*`, `gemini-1.5-*`, and `gemini-2.0-*`) are deprecated for active production routing in this repository.

### Authorized Gemini Model List (In Development & Production)
- **`gemini-2.5-flash`**: Default choice for high-throughput, general-purpose content generation, low-latency, and cost-effective routing.
- **`gemini-2.5-pro`**: Recommended for complex multi-step reasoning, coding, mathematical analysis, and heavy payloads requiring advanced reasoning.
- **`gemini-2.5-flash-lite`**: Recommended for lightweight, ultra-low latency, and highly cost-sensitive tasks.
- **Subsequent 2.5+ Versions**: Future model series (e.g., Gemini 3.0, Gemini 3.5) are authorized as they exceed the 2.5 baseline.

---

## 3. Core Repository Design Patterns

To reduce agent worktime and maintain a clean architecture, you should reuse the following built-in patterns instead of implementing custom, ad-hoc logic:

### A. The App-Centric Architecture
We model traffic around a hierarchy: **Client -> App -> APIKey**.
- **Client**: The billing and tier boundary (e.g., `premium`, `standard`, `free`).
- **App**: The functional logical boundary (e.g., `mobile-app`, `billing-service`). RPM, TPM, and latency priority are mapped directly to individual Apps rather than a client as a whole.
- **APIKey / OIDC Service Account**: Access credentials mapped directly to a specific `App`.
- *How to use*: To access client and app constraints, query `rp.Store.LookupApp(appID)` and `rp.Store.LookupClient(clientID)`.

### B. Local JSON Database Mocking (`LOCAL_DEV`)
Rather than relying on live cloud services (Firestore) during local development and unit testing, the codebase uses a local database mock.
- Setting the environment variable `LOCAL_DEV=true` forces `pkg/config/config.go` to load `/data/local_db.json`.
- **Schema Auto-Migration**: The `initLocalDB()` method automatically migrates legacy client/key associations into explicit App models without resetting the developer's local database.
- *How to use*: In tests, always mock configuration by setting `os.Setenv("LOCAL_DEV", "true")` and utilizing `config.NewConfigStore`.

### C. Declarative Custom Header Verification
Avoid writing manual validation logic or parsing headers directly in the HTTP handler. Instead, use the declarative `CustomHeader` validation engine.
- A `CustomHeader` rule contains:
  - `Name`: Header key (e.g., `X-Client-App-ID`).
  - `Required`: Boolean indicating whether the header must be present.
  - `Validation`: Match mode (`non-empty`, `regex`, `enum`).
  - `ValuePattern`: Matching parameter (regex string, or comma-separated enum options).
- *How to use*: Define these constraints inside Firestore or `local_db.json`. The proxy handles verification dynamically:
  ```go
  // Example custom header definition in local_db.json
  {
    "id": "header-1",
    "name": "X-Client-App-ID",
    "required": true,
    "validation": "regex",
    "value_pattern": "^[a-zA-Z0-9-]+$"
  }
  ```

### D. Seeding & Mocking Upstream APIs in Tests
In unit tests, avoid making real network calls to the upstream Google Vertex AI API. 
- Overwrite `rp.TokenSource` with a `mockTokenSource` that yields static mock tokens.
- Override `rp.Proxy.Transport` or `rp.Target` using a local `httptest.NewServer` to capture routed requests and inspect outbound paths.

## 4. Test-Driven Development (TDD) Workflow

Every new feature, routing rule modification, or custom header constraint MUST be developed using Test-Driven Development (TDD).

### The TDD Loop:
1. **Red**: Write a failing integration/unit test in `pkg/proxy/proxy_test.go` specifying the desired behavior.
2. **Green**: Add the minimum amount of code in `pkg/proxy/proxy.go` (or relevant config package) to satisfy the new test.
3. **Refactor**: Clean up code structure, ensuring all styles match and no code duplication occurs. Ensure `go test -v ./pkg/...` passes.

### ⚠️ The Integration Test Requirement
Every new feature or logic addition **MUST** include at least one end-to-end integration test (or multiple if required) within `pkg/proxy/proxy_test.go`.
- **Scope**: A simple unit test of a single isolated function (e.g., just testing `config.MatchRule` or `limiter.EvaluateLimit`) is **INSUFFICIENT**. 
- **Methodology**: An integration test must trigger the entire reverse proxy routing loop. It must:
  1. Set `LOCAL_DEV=true` and instantiate a mock database via `config.NewConfigStore`.
  2. Pre-seed the client, app, API key, and routing/header rules.
  3. Instantiate the `RouterProxy` with a local `httptest.NewServer` backend.
  4. Construct a standard incoming HTTP request (`httptest.NewRequest`) and send it through the proxy via `rp.ServeHTTP(w, req)`.
  5. Assert and verify the HTTP response status codes, response payloads, outbound headers, rate limits, or routed target paths.

---

## 5. Tangible TDD Examples

### Example 1: Adding a Custom Header Constraint
Suppose we want to add a new required header `X-Router-Source` with valid enum values `web,mobile` for a specific Application.

#### Step 1: Write the Failing Test (Red)
Open `pkg/proxy/proxy_test.go` and add a test block checking for custom header enforcement:

```go
func TestTDD_CustomHeaderEnumEnforcement(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config: %v", err)
	}

	// Seed mock Client, App and Key
	_ = store.SaveClient(ctx, config.Client{ID: "c1", Name: "C1", Tier: "premium"})
	_ = store.SaveApp(ctx, config.App{ID: "app-tdd", ClientID: "c1", Name: "TDD App"})
	_ = store.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("gr_tdd_key"), AppID: "app-tdd", Status: "active"})

	// Clear default seeded headers
	_ = store.DeleteHeader(ctx, "header-1")

	// Seed new header rule: X-Router-Source must be 'web' or 'mobile'
	_ = store.SaveHeader(ctx, config.CustomHeader{
		ID:           "h-tdd-1",
		AppID:        "app-tdd",
		Name:         "X-Router-Source",
		Required:     true,
		Validation:   "enum",
		ValuePattern: "web,mobile",
	})

	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}

	// Test Case A: Missing required header should return 400 Bad Request
	reqA := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=gr_tdd_key", nil)
	rrA := httptest.NewRecorder()
	rp.ServeHTTP(rrA, reqA)
	if rrA.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing header, got %d", rrA.Code)
	}

	// Test Case B: Invalid enum option should return 400 Bad Request
	reqB := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=gr_tdd_key", nil)
	reqB.Header.Set("X-Router-Source", "desktop")
	rrB := httptest.NewRecorder()
	rp.ServeHTTP(rrB, reqB)
	if rrB.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid enum value, got %d", rrB.Code)
	}

	// Test Case C: Correct header should return 200 OK (represented by upstream mock proxy error/success)
	// Note: Use a mock backend to redirect traffic in actual implementation tests.
}
```

#### Step 2: Implement Code Changes (Green)
The custom header validation engine in `pkg/proxy/proxy.go` already reads the cached database configuration dynamically! No custom Go code is required for declarative headers, which demonstrates the strength of the design system. If we were adding a new validation style (e.g., a `min-length` validation), we would update `pkg/proxy/proxy.go` in `ServeHTTP`:

```go
// Inside proxy.go -> ServeHTTP -> Custom Header Validation Loop
switch h.Validation {
case "min-length":
    minLen, _ := strconv.Atoi(h.ValuePattern)
    if len(val) < minLen {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusBadRequest)
        w.Write([]byte(fmt.Sprintf(`{"error": {"code": 400, "message": "Header %s must be at least %s characters"}}`, h.Name, h.ValuePattern)))
        return
    }
}
```

---

### Example 2: Adding a Dynamic Routing Rule based on App Priority

Suppose we want a rule where any Application with `app.Priority == "high"` requesting `gemini-2.5-flash` is dynamically routed to a high-performing fallback location or `gemini-2.5-pro` model.

#### Step 1: Write the TDD Test Case in `proxy_test.go`

```go
func TestTDD_PriorityBasedRouting(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config: %v", err)
	}

	// Seed Client and a High Priority App
	_ = store.SaveClient(ctx, config.Client{ID: "c-premium", Tier: "premium"})
	_ = store.SaveApp(ctx, config.App{ID: "app-high-priority", ClientID: "c-premium", Priority: "high"})
	_ = store.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key_high"), AppID: "app-high-priority", Status: "active"})

	// Seed dynamic rule: route high priority apps requesting gemini-2.5-flash to gemini-2.5-pro
	_ = store.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-priority-route",
		AppID:          "app-high-priority",
		ModelPattern:   "gemini-2.5-flash",
		ClientTier:     "all",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
	})

	// Setup mock target
	lastRoutedModel := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastRoutedModel = strings.Split(parts[1], ":")[0]
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Perform request
	req := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key_high", nil)
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if lastRoutedModel != "gemini-2.5-pro" {
		t.Errorf("expected model to be routed to 'gemini-2.5-pro', got %q", lastRoutedModel)
	}
}
```

#### Step 2: Implement matching in `MatchRule`
If dynamic matching based on headers and model pattern needs adjustment, we refine `MatchRule` inside `pkg/config/config.go`:

```go
// Inside config.go -> MatchRule
appMatch := rule.AppID == "" || rule.AppID == "all" || rule.AppID == appID
```
Since `appID` matches exactly, this resolves seamlessly to the targeted routing logic, ensuring modular, reliable, and easily maintainable feature delivery.

---

## 6. Documentation Synchronicity & Maintenance

To ensure that developer and operator runbooks never drift from actual system capabilities:

* **Mandatory Documentation Updates**: Whenever you add a new feature, refactor routing structures, introduce custom validation rules, or modify admin dashboard handlers, you **MUST** update the corresponding guide under `/docs` in the same commit.
* **Formatting & Writing Rules**:
  - **Humanized Tone**: Write in a highly terse, active, and direct voice. Avoid verbose AI/robot templates (e.g., no unnecessary introductory preambles like "In this section, we will explore..."). Go straight to the instructions.
  - **Tactical Examples**: Focus on practical copy-pasteable config examples, `curl` requests, and visual sequence/entity diagrams.
  - **Document Index**: Ensure any newly created files are properly mapped and linked in the main `/docs/README.md` index file.

---

## 7. Monorepo Layout & Architectural Boundaries

To avoid structural complexity, the codebase maintains a single Go module in a unified monorepo structure. You must preserve the following layout decisions:

### A. Root-Level Shared Packages (`/pkg`)
- The `/pkg/config` directory acts as the **single source of truth** for all shared models and validation helpers.
- Both `backend` and `frontend` compile-time dependencies pull from here.
- **Never copy or duplicate schemas** inside the sub-folders. If you need new configuration parameters, add them to `pkg/config/config.go` so they are instantly shared.

### B. Root-Level Commands (`/cmd`)
- Standard CLI binaries and post-deployment verification utilities reside under `/cmd`.
- For example, `cmd/verify` is used after deployment by `deploy.sh` to test integration against production endpoints.
- Do not bundle validation or testing scripts inside the runtime service containers (`/backend` or `/frontend`).

### C. Docker Build Context Strategy
- All Docker builds must execute from the repository root directory (`.`) using `gcloud builds submit` or local engine commands.
- The Dockerfiles (`backend/Dockerfile`, `frontend/Dockerfile`) copy the root `go.mod`/`go.sum` files and `/pkg` directory during the build stage before invoking `go build`.


