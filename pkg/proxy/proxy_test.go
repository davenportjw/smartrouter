package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"geminirouter/pkg/config"

	"github.com/golang-jwt/jwt/v4"
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

type mockRoundTripper struct {
	Target *url.URL
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = m.Target.Scheme
	req.URL.Host = m.Target.Host
	req.Host = m.Target.Host
	return http.DefaultTransport.RoundTrip(req)
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

			// Populate the headers that ServeHTTP would normally set
			pathParts := strings.Split(req.URL.Path, "/")
			if len(pathParts) >= 3 && pathParts[2] == "models" {
				parts := strings.Split(req.URL.Path, "/models/")
				if len(parts) >= 2 {
					modelAndAction := parts[1]
					actionParts := strings.Split(modelAndAction, ":")
					requestedModel := actionParts[0]
					req.Header.Set("X-Requested-Model", requestedModel)
					req.Header.Set("X-Routed-Model", requestedModel)
				}
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

func TestRouterProxyHeaderRouting(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Pre-seed database with a test client and active API key
	testClient := config.Client{
		ID:       "test-client-rules",
		Name:     "Test Client with Rules",
		Tier:     "premium",
		RPM:      100,
		TPM:      10000,
		Priority: "high",
	}
	testKeyStr := "gr_key_rules_test_12345"
	testKey := config.APIKey{
		KeyHash:  config.HashKey(testKeyStr),
		ClientID: "test-client-rules",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, testClient)
	_ = store.SaveKey(ctx, testKey)

	// Clear default seeded headers and rules
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")
	
	// Pre-seed custom header schema
	_ = store.SaveHeader(ctx, config.CustomHeader{
		ID:          "h1",
		Name:        "X-Route-High-Priority",
		Required:    false,
		Validation:  "non-empty",
	})
	_ = store.SaveHeader(ctx, config.CustomHeader{
		ID:          "h2",
		Name:        "X-Route-Version",
		Required:    false,
		Validation:  "non-empty",
	})

	// Seed Dynamic Routing Rules
	rule1 := config.RoutingRule{
		ID:             "rule-exact-header",
		ModelPattern:   "gemini-1.5-flash",
		ClientTier:     "all",
		HeaderName:     "X-Route-High-Priority",
		HeaderValue:    "true",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		PriorityWeight: 2,
	}
	rule2 := config.RoutingRule{
		ID:             "rule-regex-header",
		ModelPattern:   "gemini-1.5-flash",
		ClientTier:     "all",
		HeaderName:     "X-Route-Version",
		HeaderValue:    "/^canary-[a-z]+$/",
		TargetModel:    "gemini-2.0-flash",
		TargetLocation: "us-central1",
		PriorityWeight: 3,
	}
	rule3 := config.RoutingRule{
		ID:             "rule-catchall",
		ModelPattern:   "*",
		ClientTier:     "all",
		TargetModel:    "gemini-1.5-flash",
		TargetLocation: "us-central1",
		PriorityWeight: 1,
	}

	_ = store.SaveRule(ctx, rule1)
	_ = store.SaveRule(ctx, rule2)
	_ = store.SaveRule(ctx, rule3)

	// Initialize a mock backend target
	lastRoutedModel := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract target model from the translated path
		// Format: /v1/projects/test-project/locations/us-central1/publishers/google/models/MODEL_NAME:action
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastRoutedModel = strings.Split(parts[1], ":")[0]
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
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	tests := []struct {
		name          string
		headers       map[string]string
		expectedModel string
	}{
		{
			name:          "Matches Rule 1: exact header route override",
			headers:       map[string]string{"X-Route-High-Priority": "true"},
			expectedModel: "gemini-2.5-pro",
		},
		{
			name:          "Matches Rule 2: regex header route override",
			headers:       map[string]string{"X-Route-Version": "canary-alpha"},
			expectedModel: "gemini-2.0-flash",
		},
		{
			name:          "Does not match rule 2 (invalid regex value), matches default",
			headers:       map[string]string{"X-Route-Version": "canary-1234"},
			expectedModel: "gemini-1.5-flash",
		},
		{
			name:          "No headers supplied, falls back to default rule",
			headers:       map[string]string{},
			expectedModel: "gemini-1.5-flash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lastRoutedModel = "" // reset
			req := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent?key="+testKeyStr, nil)
			req.Header.Set("x-goog-api-key", testKeyStr)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			rp.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
			}

			if lastRoutedModel != tt.expectedModel {
				t.Errorf("expected routed target model to be %q, but got %q", tt.expectedModel, lastRoutedModel)
			}
		})
	}

	os.RemoveAll("data/local_db.json")
}

func TestRouterProxyAppCentricFlows(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed Client, App and Key
	testClient := config.Client{
		ID:   "client-1",
		Name: "Enterprise Client",
		Tier: "premium",
	}
	testApp := config.App{
		ID:       "app-1",
		ClientID: "client-1",
		Name:     "App One",
		RPM:      100,
		TPM:      40000,
		Priority: "high",
	}
	keyStr := "gr_app_key_123456"
	testKey := config.APIKey{
		KeyHash: config.HashKey(keyStr),
		AppID:   "app-1",
		Status:  "active",
	}

	_ = store.SaveClient(ctx, testClient)
	_ = store.SaveApp(ctx, testApp)
	_ = store.SaveKey(ctx, testKey)

	// Clear default seeded headers and rules
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Seed App-specific header: only required for app-1
	_ = store.SaveHeader(ctx, config.CustomHeader{
		ID:          "h-app-specific",
		AppID:       "app-1",
		Name:        "X-App-Secret",
		Required:    true,
		Validation:  "non-empty",
	})

	// Seed App-specific routing rule: only routes app-1 requests to gemini-2.5-pro
	_ = store.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-app-1-routing",
		AppID:          "app-1",
		ModelPattern:   "gemini-1.5-flash",
		ClientTier:     "all",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		PriorityWeight: 1,
	})

	// Initialize a mock backend target
	lastRoutedModel := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastRoutedModel = strings.Split(parts[1], ":")[0]
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
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Test Case 1: Request without App-specific required header should fail (400 Bad Request)
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent?key="+keyStr, nil)
	req1.Header.Set("x-goog-api-key", keyStr)
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing app header, got %d", rr1.Code)
	}

	// Test Case 2: Request with correct App-specific header should pass and apply App-specific routing
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent?key="+keyStr, nil)
	req2.Header.Set("x-goog-api-key", keyStr)
	req2.Header.Set("X-App-Secret", "app-one-super-token")
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("expected status 200 for valid request, got %d. Body: %s", rr2.Code, rr2.Body.String())
	}

	if lastRoutedModel != "gemini-2.5-pro" {
		t.Errorf("expected routed target model to be 'gemini-2.5-pro', but got %q", lastRoutedModel)
	}

	os.RemoveAll("data/local_db.json")
}

func TestRouterProxyGoogleIdentityAuth(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed Client and App representing the Service Account (where App.ID is the email)
	serviceAccountEmail := "my-service-account@test-project.iam.gserviceaccount.com"
	testClient := config.Client{
		ID:   "client-sa",
		Name: "IAM Client",
		Tier: "premium",
	}
	testApp := config.App{
		ID:       serviceAccountEmail,
		ClientID: "client-sa",
		Name:     "IAM Application",
		RPM:      120,
		TPM:      60000,
		Priority: "high",
	}

	_ = store.SaveClient(ctx, testClient)
	_ = store.SaveApp(ctx, testApp)

	// Clear default seeded headers and rules
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Seed custom routing rule for this Service Account App
	_ = store.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-sa-routing",
		AppID:          serviceAccountEmail,
		ModelPattern:   "gemini-1.5-flash",
		ClientTier:     "all",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		PriorityWeight: 1,
	})

	// Generate a mock Google JWT token containing the service account email claim
	claims := jwt.MapClaims{
		"email": serviceAccountEmail,
		"iss":   "https://accounts.google.com",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	mockTokenStr, err := token.SignedString([]byte("mock-signing-key"))
	if err != nil {
		t.Fatalf("failed to generate mock token: %v", err)
	}

	// Set up mock backend
	lastRoutedModel := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastRoutedModel = strings.Split(parts[1], ":")[0]
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
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Test Case 1: Standard request with Google OIDC Bearer token in Authorization header
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent", nil)
	req1.Header.Set("Authorization", "Bearer "+mockTokenStr)
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("expected status 200 for valid Google OIDC request, got %d. Body: %s", rr1.Code, rr1.Body.String())
	}

	if lastRoutedModel != "gemini-2.5-pro" {
		t.Errorf("expected routed target model to be 'gemini-2.5-pro' due to service account app routing rule, but got %q", lastRoutedModel)
	}

	// Test Case 2: Request with an unregistered Google identity should fail (401 Unauthorized)
	unregisteredClaims := jwt.MapClaims{
		"email": "unknown-sa@test-project.iam.gserviceaccount.com",
		"iss":   "https://accounts.google.com",
	}
	unregisteredToken := jwt.NewWithClaims(jwt.SigningMethodHS256, unregisteredClaims)
	unregisteredTokenStr, _ := unregisteredToken.SignedString([]byte("mock-signing-key"))

	req2 := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent", nil)
	req2.Header.Set("Authorization", "Bearer "+unregisteredTokenStr)
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for unregistered Service Account identity, got %d", rr2.Code)
	}

	// Test Case 3: Malformed JWT should fail with 401
	req3 := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent", nil)
	req3.Header.Set("Authorization", "Bearer eyJ.invalid.jwt")
	rr3 := httptest.NewRecorder()
	rp.ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for malformed OIDC JWT, got %d", rr3.Code)
	}

	os.RemoveAll("data/local_db.json")
}

// --- Regression Tests: Double-write protection in proxy ServeHTTP ---

// failingTokenSource simulates an OAuth2 token fetch failure.
type failingTokenSource struct{}

func (f *failingTokenSource) Token() (*oauth2.Token, error) {
	return nil, fmt.Errorf("simulated token source failure")
}

func TestServeHTTP_DirectorError_ReturnsCleanErrorResponse(t *testing.T) {
	// Regression test: When the Director encounters an error (e.g., token fetch failure),
	// the proxy must return a proper HTTP error response without forwarding to upstream.
	// Previously, the proxy would stream upstream data first, then attempt to write an
	// error response, causing double-writes and potential panics.

	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed minimal data: client, app, key
	testClient := config.Client{
		ID:       "client-dw",
		Name:     "Double-Write Test Client",
		Tier:     "standard",
		RPM:      100,
		TPM:      50000,
		Priority: "low",
	}
	testApp := config.App{
		ID:       "app-dw",
		ClientID: "client-dw",
		Name:     "Double-Write Test App",
		RPM:      100,
		TPM:      50000,
		Priority: "low",
	}
	testKeyStr := "gr_key_doublewrite_test_99"
	testKey := config.APIKey{
		KeyHash: config.HashKey(testKeyStr),
		AppID:   "app-dw",
		Status:  "active",
	}

	_ = store.SaveClient(ctx, testClient)
	_ = store.SaveApp(ctx, testApp)
	_ = store.SaveKey(ctx, testKey)

	// Clear seeded headers and rules to avoid interference
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Track whether the upstream was actually called
	upstreamCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates": [{"content": "hello"}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}

	// Inject the failing token source to trigger a Director error
	rp.TokenSource = &failingTokenSource{}
	rp.Target = backendURL
	// Wrap the mock transport in errorInterceptTransport so the Director's
	// X-Router-Error header is intercepted before hitting the upstream.
	rp.Proxy.Transport = &errorInterceptTransport{base: &mockRoundTripper{Target: backendURL}}

	req := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent?key="+testKeyStr, nil)
	req.Header.Set("x-goog-api-key", testKeyStr)
	rr := httptest.NewRecorder()

	// This must not panic
	rp.ServeHTTP(rr, req)

	// The response should be an error, not a 200 with mixed content
	if rr.Code == http.StatusOK {
		t.Errorf("expected an error status code when token fetch fails, got 200")
	}

	// The response should be a 500 internal server error
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rr.Code)
	}

	// The body must be valid JSON, not corrupted/mixed content
	body := rr.Body.String()
	var errResp map[string]interface{}
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Errorf("response body is not valid JSON: %q (parse error: %v)", body, err)
	}

	// The error response should mention credentials
	if !strings.Contains(body, "credentials") && !strings.Contains(body, "token") && !strings.Contains(body, "error") {
		t.Errorf("error response should contain meaningful error info, got: %q", body)
	}

	// The upstream should NOT have been called when the Director fails
	if upstreamCalled {
		t.Errorf("upstream backend was called despite Director error; request should have been aborted before proxying")
	}

	os.RemoveAll("data/local_db.json")
}

func TestServeHTTP_DirectorError_NoPanic(t *testing.T) {
	// Regression test: Verify that a Director error does not cause a panic
	// from double-writing to the ResponseWriter.

	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	testClient := config.Client{
		ID:       "client-np",
		Name:     "No-Panic Test Client",
		Tier:     "premium",
		RPM:      200,
		TPM:      100000,
		Priority: "high",
	}
	testApp := config.App{
		ID:       "app-np",
		ClientID: "client-np",
		Name:     "No-Panic Test App",
		RPM:      200,
		TPM:      100000,
		Priority: "high",
	}
	keyStr := "gr_key_nopanic_test_42"
	testKey := config.APIKey{
		KeyHash: config.HashKey(keyStr),
		AppID:   "app-np",
		Status:  "active",
	}

	_ = store.SaveClient(ctx, testClient)
	_ = store.SaveApp(ctx, testApp)
	_ = store.SaveKey(ctx, testKey)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "should not reach here"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}
	rp.TokenSource = &failingTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &errorInterceptTransport{base: &mockRoundTripper{Target: backendURL}}

	// Send multiple requests in sequence to stress the double-write path.
	// If the bug exists, one of these will panic.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent?key="+keyStr, nil)
		req.Header.Set("x-goog-api-key", keyStr)
		rr := httptest.NewRecorder()

		// Must not panic
		rp.ServeHTTP(rr, req)

		if rr.Code == http.StatusOK {
			t.Errorf("iteration %d: expected error status when token fetch fails, got 200", i)
		}
	}

	os.RemoveAll("data/local_db.json")
}

func TestServeHTTP_DirectorError_SingleWrite(t *testing.T) {
	// Regression test: The response must be written exactly once.
	// Verify that the Content-Length and body are consistent (no partial/double writes).

	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	testClient := config.Client{
		ID:       "client-sw",
		Name:     "Single-Write Test Client",
		Tier:     "standard",
		RPM:      100,
		TPM:      50000,
		Priority: "medium",
	}
	testApp := config.App{
		ID:       "app-sw",
		ClientID: "client-sw",
		Name:     "Single-Write Test App",
		RPM:      100,
		TPM:      50000,
		Priority: "medium",
	}
	keyStr := "gr_key_singlewrite_test_77"
	testKey := config.APIKey{
		KeyHash: config.HashKey(keyStr),
		AppID:   "app-sw",
		Status:  "active",
	}

	_ = store.SaveClient(ctx, testClient)
	_ = store.SaveApp(ctx, testApp)
	_ = store.SaveKey(ctx, testKey)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Backend that writes a large body to make double-write obvious
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "This is a large response that should never appear in the error case"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}
	rp.TokenSource = &failingTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &errorInterceptTransport{base: &mockRoundTripper{Target: backendURL}}

	req := httptest.NewRequest("POST", "/v1/models/gemini-1.5-flash:generateContent?key="+keyStr, nil)
	req.Header.Set("x-goog-api-key", keyStr)
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	body := rr.Body.String()

	// The body must NOT contain upstream response content mixed with error message
	if strings.Contains(body, "candidates") {
		t.Errorf("response body contains upstream content mixed with error; double-write detected: %q", body)
	}

	// Verify body is parseable as a single JSON object (not two concatenated objects)
	decoder := json.NewDecoder(strings.NewReader(body))
	var first json.RawMessage
	if err := decoder.Decode(&first); err != nil {
		t.Errorf("failed to parse first JSON object in body: %v", err)
	}
	var second json.RawMessage
	if decoder.Decode(&second) == nil {
		t.Errorf("response body contains multiple JSON objects (double-write): %q", body)
	}

	os.RemoveAll("data/local_db.json")
}
