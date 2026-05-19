package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
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
			inputPath:    "/v1/models/gemini-2.5-flash:generateContent",
			apiKey:       testKeyStr,
			expectedPath: "/v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent",
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
			req := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key="+testKeyStr, nil)
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
		ModelPattern:   "gemini-2.5-flash-lite",
		ClientTier:     "all",
		HeaderName:     "X-Route-High-Priority",
		HeaderValue:    "true",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		PriorityWeight: 2,
	}
	rule2 := config.RoutingRule{
		ID:             "rule-regex-header",
		ModelPattern:   "gemini-2.5-flash-lite",
		ClientTier:     "all",
		HeaderName:     "X-Route-Version",
		HeaderValue:    "/^canary-[a-z]+$/",
		TargetModel:    "gemini-2.5-flash",
		TargetLocation: "us-central1",
		PriorityWeight: 3,
	}
	rule3 := config.RoutingRule{
		ID:             "rule-catchall",
		ModelPattern:   "*",
		ClientTier:     "all",
		TargetModel:    "gemini-2.5-flash",
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
			expectedModel: "gemini-2.5-flash",
		},
		{
			name:          "Does not match rule 2 (invalid regex value), matches default",
			headers:       map[string]string{"X-Route-Version": "canary-1234"},
			expectedModel: "gemini-2.5-flash",
		},
		{
			name:          "No headers supplied, falls back to default rule",
			headers:       map[string]string{},
			expectedModel: "gemini-2.5-flash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lastRoutedModel = "" // reset
			req := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash-lite:generateContent?key="+testKeyStr, nil)
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
		ModelPattern:   "gemini-2.5-flash",
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
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key="+keyStr, nil)
	req1.Header.Set("x-goog-api-key", keyStr)
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing app header, got %d", rr1.Code)
	}

	// Test Case 2: Request with correct App-specific header should pass and apply App-specific routing
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key="+keyStr, nil)
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
		ModelPattern:   "gemini-2.5-flash",
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
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent", nil)
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

	req2 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent", nil)
	req2.Header.Set("Authorization", "Bearer "+unregisteredTokenStr)
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for unregistered Service Account identity, got %d", rr2.Code)
	}

	// Test Case 3: Malformed JWT should fail with 401
	req3 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent", nil)
	req3.Header.Set("Authorization", "Bearer eyJ.invalid.jwt")
	rr3 := httptest.NewRecorder()
	rp.ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for malformed OIDC JWT, got %d", rr3.Code)
	}

	os.RemoveAll("data/local_db.json")
}

func TestRequestSchedulerOrdering(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	os.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	os.Setenv("ROUTER_MAX_QUEUE_SIZE", "5")
	defer func() {
		os.Unsetenv("LOCAL_DEV")
		os.Unsetenv("ROUTER_CONCURRENCY_LIMIT")
		os.Unsetenv("ROUTER_MAX_QUEUE_SIZE")
	}()

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Create high and low priority apps
	appLow := config.App{ID: "app-low", ClientID: "client-1", RPM: 10, TPM: 1000, Priority: "low"}
	appHigh := config.App{ID: "app-high", ClientID: "client-1", RPM: 10, TPM: 1000, Priority: "high"}
	
	client1 := config.Client{ID: "client-1", Tier: "premium"}

	keyLow := config.APIKey{KeyHash: config.HashKey("key-low"), AppID: "app-low", Status: "active"}
	keyHigh := config.APIKey{KeyHash: config.HashKey("key-high"), AppID: "app-high", Status: "active"}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, appLow)
	_ = store.SaveApp(ctx, appHigh)
	_ = store.SaveKey(ctx, keyLow)
	_ = store.SaveKey(ctx, keyHigh)
	_ = store.DeleteHeader(ctx, "header-1")

	// Channel to coordinate request execution
	proceedChan := make(chan struct{})
	var orderMu sync.Mutex
	var executionOrder []string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// We receive App ID in the header
		appID := r.Header.Get("X-App-ID")
		orderMu.Lock()
		executionOrder = append(executionOrder, appID)
		orderMu.Unlock()

		// Block until signaled to proceed
		<-proceedChan
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// 1. First request (Low Priority): should execute immediately and block the concurrency slot
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-low", nil)
	req1.Header.Set("x-goog-api-key", "key-low")
	rr1 := httptest.NewRecorder()

	go func() {
		rp.ServeHTTP(rr1, req1)
	}()

	// Give goroutine time to enter handler
	time.Sleep(100 * time.Millisecond)

	// 2. Second request (Low Priority): should enqueue
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-low", nil)
	req2.Header.Set("x-goog-api-key", "key-low")
	rr2 := httptest.NewRecorder()

	go func() {
		rp.ServeHTTP(rr2, req2)
	}()

	time.Sleep(100 * time.Millisecond)

	// 3. Third request (High Priority): should enqueue and jump ahead of the second request
	req3 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-high", nil)
	req3.Header.Set("x-goog-api-key", "key-high")
	rr3 := httptest.NewRecorder()

	go func() {
		rp.ServeHTTP(rr3, req3)
	}()

	time.Sleep(100 * time.Millisecond)

	// Release the concurrency slot one-by-one
	proceedChan <- struct{}{} // Release first request
	time.Sleep(100 * time.Millisecond)

	proceedChan <- struct{}{} // Release second dispatched request (which should be the high priority one!)
	time.Sleep(100 * time.Millisecond)

	proceedChan <- struct{}{} // Release final request
	time.Sleep(100 * time.Millisecond)

	orderMu.Lock()
	expectedOrder := []string{"app-low", "app-high", "app-low"}
	if len(executionOrder) != 3 {
		t.Fatalf("expected 3 executions, got %d: %v", len(executionOrder), executionOrder)
	}
	for i, v := range expectedOrder {
		if executionOrder[i] != v {
			t.Errorf("expected index %d to be %s, got %s", i, v, executionOrder[i])
		}
	}
	orderMu.Unlock()

	os.RemoveAll("data/local_db.json")
}

func TestRequestSchedulerQueueFull(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	os.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	os.Setenv("ROUTER_MAX_QUEUE_SIZE", "1")
	defer func() {
		os.Unsetenv("LOCAL_DEV")
		os.Unsetenv("ROUTER_CONCURRENCY_LIMIT")
		os.Unsetenv("ROUTER_MAX_QUEUE_SIZE")
	}()

	ctx := context.Background()
	store, _ := config.NewConfigStore(ctx, "test-project")

	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 10, TPM: 1000, Priority: "medium"}
	client1 := config.Client{ID: "client-1", Tier: "standard"}
	key := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")

	blockChan := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockChan
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Request 1: Consumes active slot
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", nil)
	req1.Header.Set("x-goog-api-key", "key-1")
	rr1 := httptest.NewRecorder()
	go func() { rp.ServeHTTP(rr1, req1) }()

	time.Sleep(100 * time.Millisecond)

	// Request 2: Fills the 1 queue slot
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", nil)
	req2.Header.Set("x-goog-api-key", "key-1")
	rr2 := httptest.NewRecorder()
	go func() { rp.ServeHTTP(rr2, req2) }()

	time.Sleep(100 * time.Millisecond)

	// Request 3: Should fail immediately with 429 (Queue Full)
	req3 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", nil)
	req3.Header.Set("x-goog-api-key", "key-1")
	rr3 := httptest.NewRecorder()
	rp.ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429 for full queue, got %d. Body: %s", rr3.Code, rr3.Body.String())
	}
	if !strings.Contains(rr3.Body.String(), "request queue is full") {
		t.Errorf("expected queue full error message, got: %s", rr3.Body.String())
	}

	close(blockChan)
	os.RemoveAll("data/local_db.json")
}

func TestRequestSchedulerClientDisconnect(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	os.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	os.Setenv("ROUTER_MAX_QUEUE_SIZE", "5")
	defer func() {
		os.Unsetenv("LOCAL_DEV")
		os.Unsetenv("ROUTER_CONCURRENCY_LIMIT")
		os.Unsetenv("ROUTER_MAX_QUEUE_SIZE")
	}()

	ctx := context.Background()
	store, _ := config.NewConfigStore(ctx, "test-project")

	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 10, TPM: 1000, Priority: "medium"}
	client1 := config.Client{ID: "client-1", Tier: "standard"}
	key := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")

	var executedCount int
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		executedCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Hold the slot
	_, err := rp.Scheduler.Enqueue(ctx, "medium", "standard")
	if err != nil {
		t.Fatalf("enqueue block failed: %v", err)
	}

	// Send Request 2 with cancellable context
	cancelCtx, cancel := context.WithCancel(ctx)
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", nil)
	req2 = req2.WithContext(cancelCtx)
	rr2 := httptest.NewRecorder()

	go func() {
		rp.ServeHTTP(rr2, req2)
	}()

	time.Sleep(100 * time.Millisecond)

	// Cancel Request 2
	cancel()
	time.Sleep(100 * time.Millisecond)

	// Release slot
	rp.Scheduler.Release()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if executedCount != 0 {
		t.Errorf("expected 0 executed requests in backend (since client disconnected while enqueued), got %d", executedCount)
	}
	mu.Unlock()

	os.RemoveAll("data/local_db.json")
}

func TestProxyUpstream429Retry(t *testing.T) {
	os.Setenv("LOCAL_DEV", "true")
	defer os.Unsetenv("LOCAL_DEV")

	ctx := context.Background()
	store, _ := config.NewConfigStore(ctx, "test-project")

	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 10, TPM: 1000, Priority: "medium"}
	client1 := config.Client{ID: "client-1", Tier: "standard"}
	key := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")

	attempts := 0
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		currentAttempt := attempts
		mu.Unlock()

		if currentAttempt < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "Too many requests upstream"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "success"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	req := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", strings.NewReader(`{"input": "test"}`))
	req.Header.Set("x-goog-api-key", "key-1")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected final status 200 after retries, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	mu.Lock()
	if attempts != 3 {
		t.Errorf("expected 3 total attempts upstream, got %d", attempts)
	}
	mu.Unlock()

	os.RemoveAll("data/local_db.json")
}





