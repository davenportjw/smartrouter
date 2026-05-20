# 🧪 Test-Driven Development (TDD) Guide

This guide explains the Test-Driven Development (TDD) workflow for adding features to the Smart Router.

---

## 🚀 The TDD Loop

All modifications affecting proxy handlers (`ServeHTTP`), rate limiting, or credential resolution should follow three steps:

```
1. RED (Write Test)  ───>  2. GREEN (Implement)  ───>  3. REFACTOR (Clean Code)
```

1. **RED**: Write a failing integration test in `backend/proxy/proxy_test.go` that asserts the new behavior.
2. **GREEN**: Write the minimum code inside `backend/proxy/proxy.go` required to make the test pass.
3. **REFACTOR**: Clean up code style, optimize allocations, and ensure all tests execute successfully.

---

## ⚠️ Test Requirements

* Testing an isolated helper method does not guarantee proxy behavior.
* Test new features by spinning up an in-memory proxy instance and executing requests through the `RouterProxy.ServeHTTP` loop.

---

## 💡 Step-by-Step TDD Example

Example: Adding a check that blocks any query containing the word `forbidden-word`.

### Step 1: Write the Failing Test (Red)
Append the test in `backend/proxy/proxy_test.go`:

```go
func TestTDD_PromptWordBlocklistFilter(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize local store: %v", err)
	}

	_ = store.SaveClient(ctx, config.Client{ID: "c1", Name: "Test Client", Tier: "standard"})
	_ = store.SaveApp(ctx, config.App{ID: "app-chat", ClientID: "c1", Name: "Chat Application"})
	_ = store.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("gr_dev_key"), AppID: "app-chat", Status: "active"})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"response": "mocked upstream answer"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Test Case A: Normal prompt returns 200 OK
	reqOK := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=gr_dev_key", 
		strings.NewReader(`{"contents":[{"parts":[{"text":"Summarize this text."}]}]}`))
	rrOK := httptest.NewRecorder()
	rp.ServeHTTP(rrOK, reqOK)
	if rrOK.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rrOK.Code)
	}

	// Test Case B: Blocked word prompt returns 400 Bad Request
	reqBlocked := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=gr_dev_key", 
		strings.NewReader(`{"contents":[{"parts":[{"text":"Contains forbidden-word"}]}]}`))
	rrBlocked := httptest.NewRecorder()
	rp.ServeHTTP(rrBlocked, reqBlocked)
	if rrBlocked.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rrBlocked.Code)
	}
}
```

Run the test to verify it fails:
```bash
go test -v ./backend/proxy -run TestTDD_PromptWordBlocklistFilter
```

### Step 2: Implement the Code (Green)
Modify `backend/proxy/proxy.go` to check for the blocked word:

```go
bodyBytes, _ := io.ReadAll(r.Body)
r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

if bytes.Contains(bodyBytes, []byte("forbidden-word")) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusBadRequest)
    w.Write([]byte(`{"error":{"code":400,"message":"Forbidden word."}}`))
    return
}
```

Run the test again to verify it passes:
```bash
go test -v ./backend/proxy -run TestTDD_PromptWordBlocklistFilter
```

### Step 3: Refactor
Clean up the code and run all package tests:
```bash
go test -v ./backend/proxy/... ./frontend/dashboard/...
```
