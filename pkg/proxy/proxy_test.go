package proxy

import (
	"context"
	"encoding/json"
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
	"geminirouter/pkg/dashboard"

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
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Pre-seed the cache with test client, app, and API key
	testClient := config.Client{
		ID:       "test-client",
		Name:     "Test Client",
		Tier:     "premium",
	}
	testApp := config.App{
		ID:       "app-test-client",
		ClientID: "test-client",
		Name:     "Test App",
		RPM:      100,
		TPM:      500000,
		Priority: "high",
	}
	testKeyStr := "gr_test_key_987654"
	testKey := config.APIKey{
		KeyHash:  config.HashKey(testKeyStr),
		AppID:    "app-test-client",
		ClientID: "test-client",
		Status:   "active",
	}

	if err := store.SaveClient(ctx, testClient); err != nil {
		t.Fatalf("failed to save test client: %v", err)
	}
	if err := store.SaveApp(ctx, testApp); err != nil {
		t.Fatalf("failed to save test app: %v", err)
	}
	if err := store.SaveKey(ctx, testKey); err != nil {
		t.Fatalf("failed to save test key: %v", err)
	}

	// Clear default pre-seeded headers
	_ = store.DeleteHeader(ctx, "header-1")

	var lastInterceptedPath string
	var lastInterceptedAuth string
	var lastInterceptedHost string
	var lastInterceptedKeyParam string
	var lastInterceptedKeyHeader string

	// Local mock backend target server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastInterceptedPath = r.URL.Path
		lastInterceptedAuth = r.Header.Get("Authorization")
		lastInterceptedHost = r.Host
		lastInterceptedKeyParam = r.URL.Query().Get("key")
		lastInterceptedKeyHeader = r.Header.Get("x-goog-api-key")

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
	}))
	defer backend.Close()

	targetURL, _ := url.Parse(backend.URL)
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}

	rp.TokenSource = &mockTokenSource{}
	rp.Target = targetURL
	
	originalDirector := rp.Proxy.Director
	rp.Proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.Host = targetURL.Host
	}

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
			lastInterceptedPath = ""
			lastInterceptedAuth = ""
			lastInterceptedHost = ""
			lastInterceptedKeyParam = ""
			lastInterceptedKeyHeader = ""

			reqURL := tt.inputPath
			if tt.apiKey != "" {
				reqURL += "?key=" + tt.apiKey
			}
			req := httptest.NewRequest(tt.method, reqURL, nil)
			if tt.apiKey != "" {
				req.Header.Set("x-goog-api-key", tt.apiKey)
			}

			rr := httptest.NewRecorder()
			rp.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
			}

			// Validate output path translation
			if lastInterceptedPath != tt.expectedPath {
				t.Errorf("expected path %q, got %q", tt.expectedPath, lastInterceptedPath)
			}

			// Validate OAuth2 Bearer Token injection
			if tt.expectAuth {
				if lastInterceptedAuth != "Bearer mock-gcp-token" {
					t.Errorf("expected bearer token header, got %q", lastInterceptedAuth)
				}
			} else {
				if lastInterceptedAuth != "" {
					t.Errorf("did not expect authorization header, got %q", lastInterceptedAuth)
				}
			}

			// Validate API Key scrubbing
			if tt.expectKeyDel {
				if lastInterceptedKeyParam != "" {
					t.Errorf("expected API key to be removed from query parameters")
				}
				if lastInterceptedKeyHeader != "" {
					t.Errorf("expected API key to be removed from headers")
				}
			}

			// Validate host redirection
			if lastInterceptedHost != targetURL.Host {
				t.Errorf("expected Host header %q, got %q", targetURL.Host, lastInterceptedHost)
			}
		})
	}

	// Clean up test config DB file if created
	os.RemoveAll("data/local_db.json")
}

func TestRouterProxyCustomHeaders(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

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
	t.Setenv("LOCAL_DEV", "true")

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
	t.Setenv("LOCAL_DEV", "true")

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
	t.Setenv("LOCAL_DEV", "true")

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
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	t.Setenv("ROUTER_MAX_QUEUE_SIZE", "5")

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
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	t.Setenv("ROUTER_MAX_QUEUE_SIZE", "1")

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
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	t.Setenv("ROUTER_MAX_QUEUE_SIZE", "5")

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
	t.Setenv("LOCAL_DEV", "true")

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

func TestProxyComplexityRoutingHeuristics(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, _ := config.NewConfigStore(ctx, "test-project")

	// Seed Client and App with complexity heuristics enabled
	app := config.App{
		ID:       "app-complexity-heuristics",
		ClientID: "client-1",
		RPM:      100,
		TPM:      500000,
		Priority: "high",
		Complexity: config.ComplexityRouting{
			Enabled:                true,
			AlwaysOverride:         true,
			SimpleModel:            "gemini-2.5-flash-lite",
			MediumModel:            "gemini-2.5-flash",
			ComplexModel:           "gemini-2.5-pro",
			SimpleCharLimit:        10,
			MediumCharLimit:        25,
			ForceComplexMultimodal: true,
			ForceComplexTools:      true,
			UseLLMClassifier:       false,
		},
	}
	client1 := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{
		KeyHash:  config.HashKey("key-heuristics"),
		AppID:    "app-complexity-heuristics",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	var lastRoutedModel string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastRoutedModel = strings.Split(parts[1], ":")[0]
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "success"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	tests := []struct {
		name          string
		requestBody   string
		expectedModel string
	}{
		{
			name:          "Classifies as Simple: length <= 10 chars",
			requestBody:   `{"contents":[{"parts":[{"text":"hi"}]}]}`,
			expectedModel: "gemini-2.5-flash-lite",
		},
		{
			name:          "Classifies as Medium: length > 10 and <= 25 chars",
			requestBody:   `{"contents":[{"parts":[{"text":"hello world, this is md"}]}]}`,
			expectedModel: "gemini-2.5-flash",
		},
		{
			name:          "Classifies as Complex: length > 25 chars",
			requestBody:   `{"contents":[{"parts":[{"text":"explain quantum mechanics algorithms in high reasoning detail"}]}]}`,
			expectedModel: "gemini-2.5-pro",
		},
		{
			name:          "Bypasses to Complex: short prompt with multimodal image input",
			requestBody:   `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw..."}},{"text":"what is this"}]}]}`,
			expectedModel: "gemini-2.5-pro",
		},
		{
			name:          "Bypasses to Complex: short prompt with tool calling declarations",
			requestBody:   `{"contents":[{"parts":[{"text":"fetch MAU stats"}]}],"tools":[{"functionDeclarations":[]}]}`,
			expectedModel: "gemini-2.5-pro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			lastRoutedModel = ""
			mu.Unlock()

			req := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-heuristics", strings.NewReader(tt.requestBody))
			req.Header.Set("x-goog-api-key", "key-heuristics")
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			rp.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d. Response: %s", rr.Code, rr.Body.String())
			}

			mu.Lock()
			actualModel := lastRoutedModel
			mu.Unlock()

			if actualModel != tt.expectedModel {
				t.Errorf("expected routed model to be %q, got %q", tt.expectedModel, actualModel)
			}
		})
	}

	os.RemoveAll("data/local_db.json")
}

func TestProxyComplexityRoutingValidation(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, _ := config.NewConfigStore(ctx, "test-project")

	// Seed Client, App with complexity DISABLED, and active key
	app := config.App{
		ID:       "app-complexity-disabled",
		ClientID: "client-1",
		RPM:      100,
		TPM:      500000,
		Priority: "medium",
		Complexity: config.ComplexityRouting{
			Enabled: false, // explicitly disabled!
		},
	}
	client1 := config.Client{ID: "client-1", Tier: "standard"}
	key := config.APIKey{
		KeyHash:  config.HashKey("key-disabled"),
		AppID:    "app-complexity-disabled",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}

	// Request the virtual dynamic model 'gemini-dynamic'
	req := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-disabled", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	req.Header.Set("x-goog-api-key", "key-disabled")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	// Should fail with HTTP 400 Bad Request
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for disabled complexity routing, got %d. Response: %s", rr.Code, rr.Body.String())
	}

	var out struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("failed to parse error JSON response: %v", err)
	}

	if !strings.Contains(out.Error.Message, "dynamic complexity routing is not enabled") {
		t.Errorf("expected descriptive error message, got: %q", out.Error.Message)
	}
	if out.Error.Status != "INVALID_ARGUMENT" {
		t.Errorf("expected status code INVALID_ARGUMENT, got: %q", out.Error.Status)
	}

	os.RemoveAll("data/local_db.json")
}

func TestProxyComplexityRoutingLLMClassifier(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, _ := config.NewConfigStore(ctx, "test-project")

	// Seed Client, App with LLM Semantic Classification enabled
	app := config.App{
		ID:       "app-complexity-llm",
		ClientID: "client-1",
		RPM:      100,
		TPM:      500000,
		Priority: "high",
		Complexity: config.ComplexityRouting{
			Enabled:                true,
			AlwaysOverride:         true,
			SimpleModel:            "gemini-2.5-flash-lite",
			MediumModel:            "gemini-2.5-flash",
			ComplexModel:           "gemini-2.5-pro",
			UseLLMClassifier:       true,
			ClassifierModel:        "gemini-3.1-flash-lite",
			ForceComplexMultimodal: false,
			ForceComplexTools:      false,
		},
	}
	client1 := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{
		KeyHash:  config.HashKey("key-llm"),
		AppID:    "app-complexity-llm",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	var lastRoutedModel string
	var mu sync.Mutex

	// Mock backend handles:
	// 1. Classifier OIDC requests at models/gemini-2.5-flash-lite:generateContent
	// 2. Target routed queries at standard model endpoints
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		// Detect if it is the classifier call
		if strings.Contains(r.URL.Path, "/models/gemini-3.1-flash-lite:generateContent") {
			w.WriteHeader(http.StatusOK)
			// Return structured OIDC response text: {"complexity": "complex"}
			w.Write([]byte(`{
				"candidates": [
					{
						"content": {
							"parts": [
								{
									"text": "{\"complexity\": \"complex\"}"
								}
							]
						}
					}
				]
			}`))
			return
		}

		// Standard routed request
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastRoutedModel = strings.Split(parts[1], ":")[0]
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "success"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	// Redirect ALL proxy round-trips through the mock backend
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Make a query using dynamic routing. Prompt is short ("hi") which heuristically would be "simple",
	// but our LLM Semantic Classifier returns {"complexity": "complex"}!
	req := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-llm", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	req.Header.Set("x-goog-api-key", "key-llm")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Response: %s", rr.Code, rr.Body.String())
	}

	mu.Lock()
	routed := lastRoutedModel
	mu.Unlock()

	// Should be routed to gemini-2.5-pro (since classifier classified it as complex!)
	if routed != "gemini-2.5-pro" {
		t.Errorf("expected LLM routed target model to be 'gemini-2.5-pro', got %q", routed)
	}

	os.RemoveAll("data/local_db.json")
}

func TestProxyDynamicRoutingSuite(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, _ := config.NewConfigStore(ctx, "test-project")

	// 1. Seed Client and App with complexity heuristics enabled
	app := config.App{
		ID:       "app-dynamic-suite",
		ClientID: "client-dynamic",
		RPM:      100,
		TPM:      500000,
		Priority: "high",
		Complexity: config.ComplexityRouting{
			Enabled:                true,
			AlwaysOverride:         false,
			SimpleModel:            "gemini-2.5-flash-lite",
			MediumModel:            "gemini-2.5-flash",
			ComplexModel:           "gemini-2.5-pro",
			SimpleCharLimit:        10,
			MediumCharLimit:        100,
			ForceComplexMultimodal: true,
			ForceComplexTools:      true,
			UseLLMClassifier:       false,
		},
	}
	client := config.Client{ID: "client-dynamic", Tier: "premium"}
	key := config.APIKey{
		KeyHash:  config.HashKey("key-dynamic-suite"),
		AppID:    "app-dynamic-suite",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// 2. Seed a custom VIP dynamic routing rule
	_ = store.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-vip-suite",
		AppID:          "app-dynamic-suite",
		ModelPattern:   "gemini-1.5-pro",
		ClientTier:     "premium",
		HeaderName:     "X-Route-Priority",
		HeaderValue:    "gold",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		PriorityWeight: 2,
	})

	var lastRoutedModel string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastRoutedModel = strings.Split(parts[1], ":")[0]
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"This is a mock response."}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// Test Cases
	tests := []struct {
		name             string
		requestedModel   string
		requestBody      string
		customHeaders    map[string]string
		expectedRouted   string
		expectedResponse string
	}{
		{
			name:           "Complexity: simple prompt",
			requestedModel: "gemini-dynamic",
			requestBody:    `{"contents":[{"parts":[{"text":"Hi"}]}]}`,
			expectedRouted: "gemini-2.5-flash-lite",
		},
		{
			name:           "Complexity: medium prompt",
			requestedModel: "gemini-dynamic",
			requestBody:    `{"contents":[{"parts":[{"text":"This is a medium prompt designed to trigger medium tier model."}]}]}`,
			expectedRouted: "gemini-2.5-flash",
		},
		{
			name:           "Complexity: complex prompt",
			requestedModel: "gemini-dynamic",
			requestBody:    `{"contents":[{"parts":[{"text":"This is an exceptionally long prompt designed to trigger the complex tier model by crossing the character threshold configured."}]}]}`,
			expectedRouted: "gemini-2.5-pro",
		},
		{
			name:           "Rules-based: upgrade gemini-1.5-pro to gemini-2.5-pro via header",
			requestedModel: "gemini-1.5-pro",
			requestBody:    `{"contents":[{"parts":[{"text":"Hello VIP"}]}]}`,
			customHeaders:  map[string]string{"X-Route-Priority": "gold"},
			expectedRouted: "gemini-2.5-pro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			lastRoutedModel = ""
			mu.Unlock()

			req := httptest.NewRequest("POST", "/v1/models/"+tt.requestedModel+":generateContent?key=key-dynamic-suite", strings.NewReader(tt.requestBody))
			req.Header.Set("x-goog-api-key", "key-dynamic-suite")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Client-App-ID", "prod-app-main")

			for k, v := range tt.customHeaders {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			rp.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d. Response: %s", rr.Code, rr.Body.String())
			}

			// Assert response audit headers
			routedHeader := rr.Header().Get("X-Routed-Model")
			if routedHeader != tt.expectedRouted {
				t.Errorf("expected response header X-Routed-Model to be %q, got %q", tt.expectedRouted, routedHeader)
			}

			tierHeader := rr.Header().Get("X-Client-Tier")
			if tierHeader != "premium" {
				t.Errorf("expected response header X-Client-Tier to be 'premium', got %q", tierHeader)
			}

			appHeader := rr.Header().Get("X-App-ID")
			if appHeader != "app-dynamic-suite" {
				t.Errorf("expected response header X-App-ID to be 'app-dynamic-suite', got %q", appHeader)
			}

			mu.Lock()
			actualRouted := lastRoutedModel
			mu.Unlock()

			if actualRouted != tt.expectedRouted {
				t.Errorf("expected request to be routed to model %q, but mock backend received %q", tt.expectedRouted, actualRouted)
			}
		})
	}

	os.RemoveAll("data/local_db.json")
}

func TestProxyRegionalModelRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed App, Client, Key
	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 100, TPM: 10000}
	client := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}
	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Seed a regional custom model residing in "us" multiregion (proxy is in "us-central1")
	customModel := config.ModelConfig{
		ID:          "gemini-custom-us",
		DisplayName: "Tuned Model in US Multi-Region",
		Location:    "us",
		Type:        "custom",
		Active:      true,
	}
	_ = store.SaveModel(ctx, customModel)

	var receivedPath string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "success"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	
	// Instantiate proxy in us-central1
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	
	// Create and use hostCapturingRoundTripper
	capturer := &hostCapturingRoundTripper{Target: backendURL}
	rp.Proxy.Transport = capturer

	// Perform proxy request requesting gemini-custom-us
	req := httptest.NewRequest("POST", "/v1/models/gemini-custom-us:generateContent?key=key-1", strings.NewReader(`{}`))
	req.Header.Set("x-goog-api-key", "key-1")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected proxy response code 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	// Verify response audit headers
	routedHeader := rr.Header().Get("X-Routed-Model")
	if routedHeader != "gemini-custom-us" {
		t.Errorf("expected X-Routed-Model to be 'gemini-custom-us', got %q", routedHeader)
	}

	locationHeader := rr.Header().Get("X-Routed-Model-Location")
	if locationHeader != "us-central1" {
		t.Errorf("expected X-Routed-Model-Location to be 'us-central1', got %q", locationHeader)
	}

	mu.Lock()
	path := receivedPath
	mu.Unlock()

	// The request host should be rewritten to "us-central1-aiplatform.googleapis.com"
	expectedHost := "us-central1-aiplatform.googleapis.com"
	if capturer.CapturedHost != expectedHost {
		t.Errorf("expected rewritten host to be %q, got %q", expectedHost, capturer.CapturedHost)
	}

	// The path location should be rewritten to "us-central1"
	expectedPath := "/v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-custom-us:generateContent"
	if path != expectedPath {
		t.Errorf("expected rewritten path to be %q, got %q", expectedPath, path)
	}

	os.RemoveAll("data/local_db.json")
}

type hostCapturingRoundTripper struct {
	Target       *url.URL
	CapturedHost string
}

func (h *hostCapturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	h.CapturedHost = req.Host
	req.URL.Scheme = h.Target.Scheme
	req.URL.Host = h.Target.Host
	req.Host = h.Target.Host
	return http.DefaultTransport.RoundTrip(req)
}

type mockGCPRoundTripper struct {
	Target *url.URL
}

func (m *mockGCPRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = m.Target.Scheme
	req.URL.Host = m.Target.Host
	req.Host = m.Target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestProxyModelDiscoveryAndRoutingWorkflow(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed initial App, Client, Key
	app := config.App{ID: "app-discovery", ClientID: "client-discovery", RPM: 100, TPM: 10000}
	client := config.Client{ID: "client-discovery", Tier: "premium"}
	key := config.APIKey{KeyHash: config.HashKey("key-discovery-test"), AppID: "app-discovery", Status: "active"}
	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Instantiate dashboard controller
	dash := dashboard.NewDashboardController(store, "test-project", "us-central1")

	// Set up mock local server to act as the GCP Vertex AI API
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/models") {
			w.Write([]byte(`{
				"models": [
					{
						"name": "projects/test-project/locations/us-central1/models/gemini-custom-us",
						"displayName": "Gemini US Custom Weights (Mock)",
						"versionId": "v1",
						"createTime": "2026-05-19T13:00:00Z",
						"baseModelSource": {
							"modelGardenSource": {
								"publicModelName": "gemini-2.5-flash"
							}
						}
					}
				]
			}`))
		} else if strings.Contains(r.URL.Path, "/endpoints") {
			w.Write([]byte(`{
				"endpoints": [
					{
						"name": "projects/test-project/locations/us-central1/endpoints/ep-custom-us",
						"displayName": "US Serving Endpoint (Mock)",
						"createTime": "2026-05-19T13:00:00Z",
						"deployedModels": [
							{
								"id": "deployed-1",
								"model": "projects/test-project/locations/us-central1/models/gemini-custom-us",
								"displayName": "Gemini US Custom Deployed",
								"createTime": "2026-05-19T13:00:00Z"
							}
						]
					}
				]
			}`))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	mockServerURL, _ := url.Parse(mockServer.URL)
	dash.HTTPClient = &http.Client{
		Transport: &mockGCPRoundTripper{Target: mockServerURL},
	}

	var receivedPath string
	var mu sync.Mutex
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "success"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	capturer := &hostCapturingRoundTripper{Target: backendURL}
	rp.Proxy.Transport = capturer

	// 1. Verify custom model (gemini-custom-us) and endpoint (ep-custom-us) are not registered/active
	targetModelID := "projects/test-project/locations/us-central1/models/gemini-custom-us"
	targetEndpointID := "projects/test-project/locations/us-central1/endpoints/ep-custom-us"

	_, ok := store.LookupActiveModel(targetModelID)
	if ok {
		t.Fatalf("gemini-custom-us should not be active before discovery")
	}
	_, ok = store.LookupActiveModel(targetEndpointID)
	if ok {
		t.Fatalf("ep-custom-us should not be active before discovery")
	}

	// 2. Perform a Refresh call via Dashboard Controller
	wRec := httptest.NewRecorder()
	reqRefresh := httptest.NewRequest("POST", "/admin/models/refresh", nil)
	dash.RefreshModels(wRec, reqRefresh)

	if wRec.Code != http.StatusSeeOther {
		t.Fatalf("expected status 303 redirect from RefreshModels, got %d", wRec.Code)
	}

	// 3. Verify models and endpoints were discovered and persisted
	models, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to fetch models: %v", err)
	}

	var foundModel, foundEndpoint *config.ModelConfig
	for _, m := range models {
		if m.ID == targetModelID {
			mCopy := m
			foundModel = &mCopy
		}
		if m.ID == targetEndpointID {
			mCopy := m
			foundEndpoint = &mCopy
		}
	}

	if foundModel == nil {
		t.Fatalf("expected to find discovered model %q", targetModelID)
	}
	if foundEndpoint == nil {
		t.Fatalf("expected to find discovered endpoint %q", targetEndpointID)
	}

	if foundModel.Active || foundEndpoint.Active {
		t.Fatalf("discovered items should not be active by default")
	}

	// 4. Toggle/Activate the custom model and endpoint
	wRecToggle1 := httptest.NewRecorder()
	form1 := url.Values{}
	form1.Add("model_id", targetModelID)
	form1.Add("active", "true")
	reqToggle1 := httptest.NewRequest("POST", "/admin/models/toggle", strings.NewReader(form1.Encode()))
	reqToggle1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dash.ToggleModel(wRecToggle1, reqToggle1)

	wRecToggle2 := httptest.NewRecorder()
	form2 := url.Values{}
	form2.Add("model_id", targetEndpointID)
	form2.Add("active", "true")
	reqToggle2 := httptest.NewRequest("POST", "/admin/models/toggle", strings.NewReader(form2.Encode()))
	reqToggle2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dash.ToggleModel(wRecToggle2, reqToggle2)

	// Verify active status
	activeModel, ok := store.LookupActiveModel(targetModelID)
	if !ok || !activeModel.Active {
		t.Fatalf("expected model %q to be active", targetModelID)
	}
	activeEndpoint, ok := store.LookupActiveModel(targetEndpointID)
	if !ok || !activeEndpoint.Active {
		t.Fatalf("expected endpoint %q to be active", targetEndpointID)
	}

	// 5. Test Proxy Path Rewriter for custom model path
	reqProxy1 := httptest.NewRequest("POST", "/v1/models/"+targetModelID+":generateContent?key=key-discovery-test", strings.NewReader(`{}`))
	reqProxy1.Header.Set("x-goog-api-key", "key-discovery-test")
	rrProxy1 := httptest.NewRecorder()
	rp.ServeHTTP(rrProxy1, reqProxy1)

	if rrProxy1.Code != http.StatusOK {
		t.Fatalf("expected proxy response code 200, got %d. Body: %s", rrProxy1.Code, rrProxy1.Body.String())
	}

	mu.Lock()
	path1 := receivedPath
	mu.Unlock()

	expectedPath1 := "/v1/" + targetModelID + ":generateContent"
	if path1 != expectedPath1 {
		t.Errorf("expected path rewrite for custom model to be %q, got %q", expectedPath1, path1)
	}

	// 6. Test Proxy Path Rewriter for custom endpoint path
	reqProxy2 := httptest.NewRequest("POST", "/v1/models/"+targetEndpointID+":generateContent?key=key-discovery-test", strings.NewReader(`{}`))
	reqProxy2.Header.Set("x-goog-api-key", "key-discovery-test")
	rrProxy2 := httptest.NewRecorder()
	rp.ServeHTTP(rrProxy2, reqProxy2)

	if rrProxy2.Code != http.StatusOK {
		t.Fatalf("expected proxy response code 200, got %d. Body: %s", rrProxy2.Code, rrProxy2.Body.String())
	}

	mu.Lock()
	path2 := receivedPath
	mu.Unlock()

	expectedPath2 := "/v1/" + targetEndpointID + ":generateContent"
	if path2 != expectedPath2 {
		t.Errorf("expected path rewrite for custom endpoint to be %q, got %q", expectedPath2, path2)
	}

	os.RemoveAll("data/local_db.json")
}

func TestExtractLocationFromResourceName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"valid custom model path", "projects/test-project/locations/us-central1/models/gemini-custom", "us-central1"},
		{"valid serving endpoint path", "projects/test-project/locations/europe-west9/endpoints/my-endpoint", "europe-west9"},
		{"valid multi-region location path", "projects/test-project/locations/us/models/model-a", "us"},
		{"invalid foundation model name", "gemini-2.5-flash", ""},
		{"invalid prefix", "locations/us-central1/models/gemini-custom", ""},
		{"empty path", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := config.ExtractLocationFromResourceName(tt.input)
			if actual != tt.expected {
				t.Errorf("expected location to be %q, got %q", tt.expected, actual)
			}
		})
	}
}

func TestProxySmallestCompatibleLocationRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed App, Client, Key
	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 100, TPM: 10000}
	client := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}
	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Seed foundation model residing in "us" multiregion (e.g., gemini-3.5-pro)
	foundationModel := config.ModelConfig{
		ID:          "gemini-3.5-pro",
		DisplayName: "Gemini 3.5 Pro",
		Location:    "us",
		Type:        "foundation",
		Active:      true,
	}
	_ = store.SaveModel(ctx, foundationModel)

	var receivedPath string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "success"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	// Scenario A: Router deployed in specific region "us-central1" (Level 1)
	// compatible with model's region "us" (Level 2), but "us-central1" is smaller/more specific!
	rp1, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp1.TokenSource = &mockTokenSource{}
	rp1.Target = backendURL
	capturer1 := &hostCapturingRoundTripper{Target: backendURL}
	rp1.Proxy.Transport = capturer1

	req1 := httptest.NewRequest("POST", "/v1/models/gemini-3.5-pro:generateContent?key=key-1", strings.NewReader(`{}`))
	req1.Header.Set("x-goog-api-key", "key-1")
	rr1 := httptest.NewRecorder()

	rp1.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("expected proxy response code 200, got %d", rr1.Code)
	}

	// Location should be downscaled/resolved to the smaller compatible location (us-central1)
	locationHeader1 := rr1.Header().Get("X-Routed-Model-Location")
	if locationHeader1 != "us-central1" {
		t.Errorf("expected X-Routed-Model-Location to be 'us-central1', got %q", locationHeader1)
	}

	expectedHost1 := "us-central1-aiplatform.googleapis.com"
	if capturer1.CapturedHost != expectedHost1 {
		t.Errorf("expected host to be %q, got %q", expectedHost1, capturer1.CapturedHost)
	}

	mu.Lock()
	path1 := receivedPath
	mu.Unlock()

	expectedPath1 := "/v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-3.5-pro:generateContent"
	if path1 != expectedPath1 {
		t.Errorf("expected Scenario A rewritten path to be %q, got %q", expectedPath1, path1)
	}

	// Scenario B: Router deployed in multi-region "us" (Level 2)
	rp2, _ := NewRouterProxy(store, "test-project", "us")
	rp2.TokenSource = &mockTokenSource{}
	rp2.Target = backendURL
	capturer2 := &hostCapturingRoundTripper{Target: backendURL}
	rp2.Proxy.Transport = capturer2

	req2 := httptest.NewRequest("POST", "/v1/models/gemini-3.5-pro:generateContent?key=key-1", strings.NewReader(`{}`))
	req2.Header.Set("x-goog-api-key", "key-1")
	rr2 := httptest.NewRecorder()

	rp2.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("expected proxy response code 200, got %d", rr2.Code)
	}

	// Location remains "us" because that is the smallest compatible location between both
	locationHeader2 := rr2.Header().Get("X-Routed-Model-Location")
	if locationHeader2 != "us" {
		t.Errorf("expected X-Routed-Model-Location to be 'us', got %q", locationHeader2)
	}

	expectedHost2 := "us-aiplatform.googleapis.com"
	if capturer2.CapturedHost != expectedHost2 {
		t.Errorf("expected host to be %q, got %q", expectedHost2, capturer2.CapturedHost)
	}

	mu.Lock()
	path2 := receivedPath
	mu.Unlock()

	expectedPath2 := "/v1/projects/test-project/locations/us/publishers/google/models/gemini-3.5-pro:generateContent"
	if path2 != expectedPath2 {
		t.Errorf("expected Scenario B rewritten path to be %q, got %q", expectedPath2, path2)
	}

	os.RemoveAll("data/local_db.json")
}

func TestProxyIncompatibleLocationRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := config.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed App, Client, Key
	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 100, TPM: 10000}
	client := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}
	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Seed foundation model residing in "europe-west9" region (router will be in "us")
	foundationModel := config.ModelConfig{
		ID:          "gemini-3.5-europe",
		DisplayName: "Gemini 3.5 Europe",
		Location:    "europe-west9",
		Type:        "foundation",
		Active:      true,
	}
	_ = store.SaveModel(ctx, foundationModel)

	var receivedPath string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "success"}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)

	// Instantiate proxy in "us-central1"
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	capturer := &hostCapturingRoundTripper{Target: backendURL}
	rp.Proxy.Transport = capturer

	req := httptest.NewRequest("POST", "/v1/models/gemini-3.5-europe:generateContent?key=key-1", strings.NewReader(`{}`))
	req.Header.Set("x-goog-api-key", "key-1")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected proxy response code 200, got %d", rr.Code)
	}

	// Must route to model's specific europe-west9 region because it is incompatible with router's us-central1 location
	locationHeader := rr.Header().Get("X-Routed-Model-Location")
	if locationHeader != "europe-west9" {
		t.Errorf("expected X-Routed-Model-Location to be 'europe-west9', got %q", locationHeader)
	}

	expectedHost := "europe-west9-aiplatform.googleapis.com"
	if capturer.CapturedHost != expectedHost {
		t.Errorf("expected host to be %q, got %q", expectedHost, capturer.CapturedHost)
	}

	mu.Lock()
	path := receivedPath
	mu.Unlock()

	expectedPath := "/v1/projects/test-project/locations/europe-west9/publishers/google/models/gemini-3.5-europe:generateContent"
	if path != expectedPath {
		t.Errorf("expected incompatible model rewritten path to be %q, got %q", expectedPath, path)
	}

	os.RemoveAll("data/local_db.json")
}






