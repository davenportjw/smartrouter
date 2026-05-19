# 🧪 Test-Driven Development Feature Addition

This guide details the mandatory **Test-Driven Development (TDD)** workflow required to add features, validate routing schemas, or expand request boundaries in the Smart Router codebase.

---

## 🚀 The Mandatory TDD Loop

All code modifications affecting proxy handlers (`ServeHTTP`), rate limiting, or credential resolution must proceed through three sequential phases:

```
┌──────────────┐      ┌──────────────┐      ┌──────────────┐
│  1. RED      │ ───> │  2. GREEN    │ ───> │  3. REFACTOR │
│  Write Test  │      │  Implement   │      │  Clean Code  │
└──────────────┘      └──────────────┘      └──────────────┘
```

1. **RED**: Write a failing integration test in `pkg/proxy/proxy_test.go` that asserts the desired new behavior.
2. **GREEN**: Write the absolute minimum code inside `pkg/proxy/proxy.go` (or config packages) required to make the test pass.
3. **REFACTOR**: Clean up code styles, optimize allocations, and ensure `go test -v ./pkg/...` executes successfully without warnings.

---

## ⚠️ Integration Test Requirements

* **Unit Tests are Insufficient**: Testing an isolated helper method (e.g. a standalone JSON parser) does not meet our TDD baseline.
* **Reverse Proxy Coverage**: Every new feature must be tested by spinning up an in-memory proxy instance and executing requests through the actual `RouterProxy.ServeHTTP` loop.

---

## 💡 step-by-step TDD Integration Test Tutorial

Suppose you are adding a custom check that blocks any query containing the word `deprecated-blocklist-word`.

### Step 1: Write the Failing Test (Red)
Open `pkg/proxy/proxy_test.go` and append the new integration test:

```go
func TestTDD_PromptWordBlocklistFilter(t *testing.T) {
	// 1. Mock local development environment
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize local store: %v", err)
	}

	// 2. Seed mock credentials
	_ = store.SaveClient(ctx, config.Client{ID: "c1", Name: "Test Client", Tier: "standard"})
	_ = store.SaveApp(ctx, config.App{ID: "app-chat", ClientID: "c1", Name: "Chat Application"})
	_ = store.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("gr_dev_key"), AppID: "app-chat", Status: "active"})

	// 3. Boot up local backend server to capture clean request relays
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"response": "mocked upstream answer"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	// 4. Instantiate proxy instance linked to mock target
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Test Case A: Normal prompt should return 200 OK
	reqOK := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=gr_dev_key", 
		strings.NewReader(`{"contents":[{"parts":[{"text":"Summarize this text."}]}]}`))
	rrOK := httptest.NewRecorder()
	rp.ServeHTTP(rrOK, reqOK)
	if rrOK.Code != http.StatusOK {
		t.Errorf("expected status 200 for safe prompt, got %d", rrOK.Code)
	}

	// Test Case B: Prompt containing forbidden word should fail with 400 Bad Request
	reqBlocked := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=gr_dev_key", 
		strings.NewReader(`{"contents":[{"parts":[{"text":"This prompt has deprecated-blocklist-word"}]}]}`))
	rrBlocked := httptest.NewRecorder()
	rp.ServeHTTP(rrBlocked, reqBlocked)
	if rrBlocked.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for blocked prompt word, got %d", rrBlocked.Code)
	}
}
```

Run the test suite to verify it fails (RED):
```bash
go test -v ./pkg/proxy -run TestTDD_PromptWordBlocklistFilter
```

### Step 2: Implement Code Changes (Green)
Modify `pkg/proxy/proxy.go` inside the `ServeHTTP` handler to parse the payload and check for the blocked word:

```go
// Inside ServeHTTP after key resolution and before rate limiting:
bodyBytes, _ := io.ReadAll(r.Body)
r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // Reset reader

if bytes.Contains(bodyBytes, []byte("deprecated-blocklist-word")) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusBadRequest)
    w.Write([]byte(`{"error":{"code":400,"message":"Prompt contains forbidden word."}}`))
    return
}
```

Run the test suite again (GREEN):
```bash
go test -v ./pkg/proxy -run TestTDD_PromptWordBlocklistFilter
```

### Step 3: Refactor
Clean up the implementation (e.g., extracting the check to a reusable filter method or making it configurable) and run the entire package test suite:
```bash
go test -v ./pkg/...
```
