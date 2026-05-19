package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"testing"
	"time"

	"geminirouter/pkg/config"

	"golang.org/x/oauth2"
)

// mockTokenSource implements oauth2.TokenSource for offline testing
type mockTokenSource struct{}

func (m *mockTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{
		AccessToken: "mock-gcp-token",
		Expiry:      time.Now().Add(1 * time.Hour),
	}, nil
}

func TestRouterProxyCompatibility(t *testing.T) {
	// Setup temporary local dev database environment
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Pre-seed the cache with test client and API key
	testClient := config.Client{
		ID:       "test-client",
		Name:     "Test Client",
		Tier:     "premium",
		RPM:      100,
		TPM:      500000,
		Priority: "high",
	}
	testKeyStr := "gr_test_key_987654"
	testKey := config.APIKey{
		KeyHash:  config.HashKey(testKeyStr),
		ClientID: "test-client",
		Status:   "active",
	}

	if err := store.SaveClient(ctx, testClient); err != nil {
		t.Fatalf("failed to save test client: %v", err)
	}
	if err := store.SaveKey(ctx, testKey); err != nil {
		t.Fatalf("failed to save test key: %v", err)
	}

	// Initialize our RouterProxy
	targetURL, _ := url.Parse("https://us-central1-aiplatform.googleapis.com")
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}

	// Overwrite TokenSource and Target with mock values
	rp.TokenSource = &mockTokenSource{}
	rp.Target = targetURL

	tests := []struct {
		name         string
		method       string
		inputPath    string
		apiKey       string
		expectedPath string
		expectAuth   bool
		expectKeyDel bool
	}{
		{
			name:         "Standard Model request path translation",
			method:       "POST",
			inputPath:    "/v1/models/gemini-1.5-flash:generateContent",
			apiKey:       testKeyStr,
			expectedPath: "/v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-1.5-flash:generateContent",
			expectAuth:   true,
			expectKeyDel: true,
		},
		{
			name:         "Enterprise Reasoning Engine list path translation",
			method:       "GET",
			inputPath:    "/v1/reasoningEngines/",
			apiKey:       testKeyStr,
			expectedPath: "/v1/projects/test-project/locations/us-central1/reasoningEngines/",
			expectAuth:   true,
			expectKeyDel: true,
		},
		{
			name:         "Enterprise Reasoning Engine query path translation",
			method:       "POST",
			inputPath:    "/v1beta1/reasoningEngines/my-agent-123:query",
			apiKey:       testKeyStr,
			expectedPath: "/v1beta1/projects/test-project/locations/us-central1/reasoningEngines/my-agent-123:query",
			expectAuth:   true,
			expectKeyDel: true,
		},
		{
			name:         "Enterprise RAG Corpora create path translation",
			method:       "POST",
			inputPath:    "/v1/ragCorpora",
			apiKey:       testKeyStr,
			expectedPath: "/v1/projects/test-project/locations/us-central1/ragCorpora",
			expectAuth:   true,
			expectKeyDel: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reconstruct a mock HTTP request hitting the proxy
			reqURL := tt.inputPath
			if tt.apiKey != "" {
				reqURL += "?key=" + tt.apiKey
			}
			req := httptest.NewRequest(tt.method, reqURL, nil)
			if tt.apiKey != "" {
				req.Header.Set("x-goog-api-key", tt.apiKey)
			}

			// Use custom director
			rp.Proxy.Director(req)

			// Validate output path translation
			if req.URL.Path != tt.expectedPath {
				t.Errorf("expected path %q, got %q", tt.expectedPath, req.URL.Path)
			}

			// Validate OAuth2 Bearer Token injection
			authHeader := req.Header.Get("Authorization")
			if tt.expectAuth {
				if authHeader != "Bearer mock-gcp-token" {
					t.Errorf("expected bearer token header, got %q", authHeader)
				}
			} else {
				if authHeader != "" {
					t.Errorf("did not expect authorization header, got %q", authHeader)
				}
			}

			// Validate API Key scrubbing
			if tt.expectKeyDel {
				if req.URL.Query().Get("key") != "" {
					t.Errorf("expected API key to be removed from query parameters")
				}
				if req.Header.Get("x-goog-api-key") != "" {
					t.Errorf("expected API key to be removed from headers")
				}
			}

			// Validate host redirection
			if req.Host != targetURL.Host {
				t.Errorf("expected Host header %q, got %q", targetURL.Host, req.Host)
			}
		})
	}

	// Clean up test config DB file if created
	os.RemoveAll("data/local_db.json")
}

func TestRouterProxyCustomHeaders(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Pre-seed database with a test client and active API key
	testClient := config.Client{
		ID:       "test-client-headers",
		Name:     "Test Client with Headers",
		Tier:     "standard",
		RPM:      100,
		TPM:      10000,
		Priority: "medium",
	}
	testKeyStr := "gr_key_headers_test_12345"
	testKey := config.APIKey{
		KeyHash:  config.HashKey(testKeyStr),
		ClientID: "test-client-headers",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, testClient)
	_ = store.SaveKey(ctx, testKey)

	// Pre-seed custom header verification rules
	headersRule1 := config.CustomHeader{
		ID:           "rule-h1",
		Name:         "X-Required-Header",
		Description:  "A required header",
		Required:     true,
		Validation:   "non-empty",
		ValuePattern: "",
	}
	headersRule2 := config.CustomHeader{
		ID:           "rule-h2",
		Name:         "X-Version-Header",
		Description:  "Optional format regex header",
		Required:     false,
		Validation:   "regex",
		ValuePattern: "^v[0-9]+$",
	}
	headersRule3 := config.CustomHeader{
		ID:           "rule-h3",
		Name:         "X-Env-Header",
		Description:  "Required environment enum",
		Required:     true,
		Validation:   "enum",
		ValuePattern: "dev,staging,prod",
	}

	_ = store.DeleteHeader(ctx, "header-1")

	_ = store.SaveHeader(ctx, headersRule1)
	_ = store.SaveHeader(ctx, headersRule2)
	_ = store.SaveHeader(ctx, headersRule3)

	// Setup local mock backend target server
	backendReceivedAllHeaders := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if headers are received cleanly
		if r.Header.Get("X-Required-Header") == "value" && r.Header.Get("X-Env-Header") == "staging" && r.Header.Get("X-Version-Header") == "v2" {
			backendReceivedAllHeaders = true
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy = httputil.NewSingleHostReverseProxy(backendURL) // Direct httputil proxy to mock backend

	// Overwrite Director to perform same routing and verify they are passed
	originalDirector := rp.Proxy.Director
	rp.Proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// We do NOT scrub custom headers since Vertex accepts them!
	}

	tests := []struct {
		name           string
		requestHeaders map[string]string
		expectedStatus int
	}{
		{
			name:           "Missing required X-Required-Header",
			requestHeaders: map[string]string{"X-Env-Header": "prod"},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "Missing required X-Env-Header",
			requestHeaders: map[string]string{"X-Required-Header": "value"},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "Invalid regex validation on X-Version-Header",
			requestHeaders: map[string]string{"X-Required-Header": "value", "X-Env-Header": "dev", "X-Version-Header": "beta1"},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "Invalid enum validation on X-Env-Header",
			requestHeaders: map[string]string{"X-Required-Header": "value", "X-Env-Header": "local"},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "Valid custom headers, correct regex and enum",
			requestHeaders: map[string]string{"X-Required-Header": "value", "X-Env-Header": "staging", "X-Version-Header": "v2"},
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backendReceivedAllHeaders = false // reset
			req := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent?key="+testKeyStr, nil)
			req.Header.Set("x-goog-api-key", testKeyStr)
			for k, v := range tt.requestHeaders {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			rp.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d. Response: %s", tt.expectedStatus, rr.Code, rr.Body.String())
			}

			if tt.expectedStatus == http.StatusOK && !backendReceivedAllHeaders {
				t.Errorf("custom headers were not correctly forwarded to the backend")
			}
		})
	}

	os.RemoveAll("data/local_db.json")
}

