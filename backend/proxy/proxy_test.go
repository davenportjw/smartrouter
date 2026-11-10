package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"fmt"
	store "geminirouter/backend/config"
	"geminirouter/frontend/dashboard"
	"geminirouter/pkg/config"
	"geminirouter/pkg/decision"

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

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "smartrouter-proxy-test-*")
	if err == nil {
		_ = os.Setenv("LOCAL_DATA_DIR", tmpDir)
		defer os.RemoveAll(tmpDir)
	}
	os.Exit(m.Run())
}

func TestRouterProxyCompatibility(t *testing.T) {
	// Setup temporary local dev database environment
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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
	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestRouterProxyCustomHeaders(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestRouterProxyHeaderRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestRouterProxyAppCentricFlows(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestRouterProxyGoogleIdentityAuth(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestRequestSchedulerOrdering(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	t.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	t.Setenv("ROUTER_MAX_QUEUE_SIZE", "5")

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestRequestSchedulerQueueFull(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	t.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	t.Setenv("ROUTER_MAX_QUEUE_SIZE", "1")

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

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
	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestRequestSchedulerClientDisconnect(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	t.Setenv("ROUTER_CONCURRENCY_LIMIT", "1")
	t.Setenv("ROUTER_MAX_QUEUE_SIZE", "5")

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

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
	item, err := rp.Scheduler.Enqueue(ctx, "app-1", "medium", "standard", "gemini-2.5-flash")
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
	rp.Scheduler.Release(item)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if executedCount != 0 {
		t.Errorf("expected 0 executed requests in backend (since client disconnected while enqueued), got %d", executedCount)
	}
	mu.Unlock()

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyUpstream429Retry(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyComplexityRoutingHeuristics(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyComplexityRoutingValidation(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyComplexityRoutingLLMClassifier(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyComplexityRoutingAdditionalInstructions(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

	// Seed Client, App with LLM Semantic Classification & Additional Instructions enabled
	app := config.App{
		ID:       "app-complexity-instructions",
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
			AdditionalInstructions: "TEST UNIQUE CRITERIA: always classify greetings as simple.",
		},
	}
	client1 := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{
		KeyHash:  config.HashKey("key-instructions"),
		AppID:    "app-complexity-instructions",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	var additionalInstructionsVerified bool
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(r.URL.Path, "/models/gemini-3.1-flash-lite:generateContent") {
			// Read and inspect body
			bodyBytes, _ := io.ReadAll(r.Body)
			var classifierReq struct {
				SystemInstruction struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"systemInstruction"`
			}
			_ = json.Unmarshal(bodyBytes, &classifierReq)
			if len(classifierReq.SystemInstruction.Parts) > 0 {
				sysInstrText := classifierReq.SystemInstruction.Parts[0].Text
				if strings.Contains(sysInstrText, "TEST UNIQUE CRITERIA: always classify greetings as simple.") {
					additionalInstructionsVerified = true
				}
			}

			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{
				"candidates": [
					{
						"content": {
							"parts": [
								{
									"text": "{\"complexity\": \"simple\"}"
								}
							]
						}
					}
				]
			}`))
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

	req := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-instructions", strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}]}`))
	req.Header.Set("x-goog-api-key", "key-instructions")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Response: %s", rr.Code, rr.Body.String())
	}

	mu.Lock()
	verified := additionalInstructionsVerified
	mu.Unlock()

	if !verified {
		t.Errorf("expected AdditionalInstructions to be passed to semantic classifier, but it was not found in system instructions")
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyAppOptOutDynamicRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

	// Seed Client, App with OptOutDynamicRouting enabled
	app := config.App{
		ID:                   "app-opt-out",
		ClientID:             "client-1",
		RPM:                  100,
		TPM:                  500000,
		Priority:             "high",
		OptOutDynamicRouting: true,
		Complexity: config.ComplexityRouting{
			Enabled: false,
		},
	}
	client1 := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{
		KeyHash:  config.HashKey("key-opt-out"),
		AppID:    "app-opt-out",
		Status:   "active",
	}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")

	// Seed a rule that would intercept gemini-2.5-flash-lite -> gemini-2.5-pro
	_ = store.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-opt-out-test",
		ModelPattern:   "gemini-2.5-flash-lite",
		ClientTier:     "all",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		PriorityWeight: 1,
	})

	var lastRoutedModel string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

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
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	req := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash-lite:generateContent?key=key-opt-out", strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}]}`))
	req.Header.Set("x-goog-api-key", "key-opt-out")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Response: %s", rr.Code, rr.Body.String())
	}

	mu.Lock()
	routed := lastRoutedModel
	mu.Unlock()

	// Should remain gemini-2.5-flash-lite because app is opted out of dynamic routing rules!
	if routed != "gemini-2.5-flash-lite" {
		t.Errorf("expected request to bypass dynamic routing and target gemini-2.5-flash-lite directly, but it got routed to %q", routed)
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}



func TestProxyDynamicRoutingSuite(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyRegionalModelRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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
	if locationHeader != "us" {
		t.Errorf("expected X-Routed-Model-Location to be 'us', got %q", locationHeader)
	}

	mu.Lock()
	path := receivedPath
	mu.Unlock()

	// The request host should be rewritten to "aiplatform.us.rep.googleapis.com"
	expectedHost := "aiplatform.us.rep.googleapis.com"
	if capturer.CapturedHost != expectedHost {
		t.Errorf("expected rewritten host to be %q, got %q", expectedHost, capturer.CapturedHost)
	}

	// The path location should be rewritten to "us"
	expectedPath := "/v1/projects/test-project/locations/us/publishers/google/models/gemini-custom-us:generateContent"
	if path != expectedPath {
		t.Errorf("expected rewritten path to be %q, got %q", expectedPath, path)
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
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
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	_, ok := store.LookupActiveModel(targetModelID, "us-central1")
	if ok {
		t.Fatalf("gemini-custom-us should not be active before discovery")
	}
	_, ok = store.LookupActiveModel(targetEndpointID, "us-central1")
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
	if !dash.WaitForRefresh(5 * time.Second) {
		t.Fatalf("timed out waiting for background RefreshModels to complete")
	}

	// 3. Verify models and endpoints were discovered and persisted
	models, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to fetch models: %v", err)
	}

	var foundModel, foundEndpoint *config.ModelConfig
	for _, m := range models {
		if m.ID == targetModelID+"@us-central1" {
			mCopy := m
			foundModel = &mCopy
		}
		if m.ID == targetEndpointID+"@us-central1" {
			mCopy := m
			foundEndpoint = &mCopy
		}
	}

	if foundModel == nil {
		t.Fatalf("expected to find discovered model %q", targetModelID+"@us-central1")
	}
	if foundEndpoint == nil {
		t.Fatalf("expected to find discovered endpoint %q", targetEndpointID+"@us-central1")
	}

	if foundModel.Active || foundEndpoint.Active {
		t.Fatalf("discovered items should not be active by default")
	}

	// 4. Toggle/Activate the custom model and endpoint
	wRecToggle1 := httptest.NewRecorder()
	form1 := url.Values{}
	form1.Add("model_id", targetModelID+"@us-central1")
	form1.Add("active", "true")
	reqToggle1 := httptest.NewRequest("POST", "/admin/models/toggle", strings.NewReader(form1.Encode()))
	reqToggle1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dash.ToggleModel(wRecToggle1, reqToggle1)

	wRecToggle2 := httptest.NewRecorder()
	form2 := url.Values{}
	form2.Add("model_id", targetEndpointID+"@us-central1")
	form2.Add("active", "true")
	reqToggle2 := httptest.NewRequest("POST", "/admin/models/toggle", strings.NewReader(form2.Encode()))
	reqToggle2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dash.ToggleModel(wRecToggle2, reqToggle2)

	// Verify active status
	activeModel, ok := store.LookupActiveModel(targetModelID, "us-central1")
	if !ok || !activeModel.Active {
		t.Fatalf("expected model %q to be active", targetModelID)
	}
	activeEndpoint, ok := store.LookupActiveModel(targetEndpointID, "us-central1")
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

	// 7. Test DeleteModel dynamic handler
	wRecDelete := httptest.NewRecorder()
	reqDelete := httptest.NewRequest("DELETE", "/admin/models/delete?id="+targetModelID+"@us-central1", nil)
	dash.DeleteModel(wRecDelete, reqDelete)

	if wRecDelete.Code != http.StatusOK {
		t.Fatalf("expected delete model status 200, got %d", wRecDelete.Code)
	}

	// Verify it was deleted from the store
	_, deletedOk := store.LookupActiveModel(targetModelID, "us-central1")
	if deletedOk {
		t.Errorf("expected model option %s@us-central1 to be deleted from registry", targetModelID)
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
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
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	// Location remains "us" because that is the smallest compatible location supported by the model (since downscaling is prevented)
	locationHeader1 := rr1.Header().Get("X-Routed-Model-Location")
	if locationHeader1 != "us" {
		t.Errorf("expected X-Routed-Model-Location to be 'us', got %q", locationHeader1)
	}

	expectedHost1 := "aiplatform.us.rep.googleapis.com"
	if capturer1.CapturedHost != expectedHost1 {
		t.Errorf("expected host to be %q, got %q", expectedHost1, capturer1.CapturedHost)
	}

	mu.Lock()
	path1 := receivedPath
	mu.Unlock()

	expectedPath1 := "/v1/projects/test-project/locations/us/publishers/google/models/gemini-3.5-pro:generateContent"
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

	expectedHost2 := "aiplatform.us.rep.googleapis.com"
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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyIncompatibleLocationRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
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

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestPublisherModelsDiscovery(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed initial App, Client, Key
	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 100, TPM: 10000}
	client := config.Client{ID: "client-1", Tier: "premium"}
	key := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}
	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Remove default models in local dev so we start clean
	models, _ := store.GetAllModels(ctx)
	for _, m := range models {
		_ = store.DeleteModel(ctx, m.ID)
	}

	// Instantiate dashboard controller
	dash := dashboard.NewDashboardController(store, "test-project", "us-central1")

	// Mock Vertex AI REST endpoints:
	// 1. return custom models (empty list)
	// 2. return endpoints (empty list)
	// 3. return publisher models containing gemini-3.5-flash and text-embedding-005
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/publishers/google/models") {
			w.Write([]byte(`{
				"publisherModels": [
					{
						"name": "publishers/google/models/gemini-3.5-flash",
						"displayName": "Gemini 3.5 Flash (Mocked)"
					},
					{
						"name": "publishers/google/models/text-embedding-005",
						"displayName": "Text Embedding 005 (Mocked)"
					}
				]
			}`))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer mockServer.Close()

	mockServerURL, _ := url.Parse(mockServer.URL)
	dash.HTTPClient = &http.Client{
		Transport: &mockGCPRoundTripper{Target: mockServerURL},
	}

	// Trigger refresh models
	wRec := httptest.NewRecorder()
	reqRefresh := httptest.NewRequest("POST", "/admin/models/refresh", nil)
	dash.RefreshModels(wRec, reqRefresh)

	if wRec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect from RefreshModels, got %d", wRec.Code)
	}
	if !dash.WaitForRefresh(5 * time.Second) {
		t.Fatalf("timed out waiting for background RefreshModels to complete")
	}

	// Fetch newly discovered/cached models in the store
	refreshedModels, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to load refreshed models: %v", err)
	}

	var found, foundEmbedding *config.ModelConfig
	for _, m := range refreshedModels {
		mCopy := m
		switch m.ID {
		case "gemini-3.5-flash@us-central1":
			found = &mCopy
		case "text-embedding-005@us-central1":
			foundEmbedding = &mCopy
		}
	}

	if found == nil {
		t.Fatalf("expected gemini-3.5-flash@us-central1 to be dynamically discovered and cached in store")
	}
	if found.DisplayName != "Gemini 3.5 Flash (Mocked)" {
		t.Errorf("expected display name 'Gemini 3.5 Flash (Mocked)', got %q", found.DisplayName)
	}
	if found.Location != "us-central1" {
		t.Errorf("expected model location 'us-central1', got %q", found.Location)
	}

	if foundEmbedding == nil {
		t.Fatalf("expected text-embedding-005@us-central1 to be dynamically discovered and cached in store")
	}
	if foundEmbedding.DisplayName != "Text Embedding 005 (Mocked)" {
		t.Errorf("expected display name 'Text Embedding 005 (Mocked)', got %q", foundEmbedding.DisplayName)
	}
	if foundEmbedding.Location != "us-central1" {
		t.Errorf("expected embedding model location 'us-central1', got %q", foundEmbedding.Location)
	}

	// The location should be resolved as compatible/coerced (in this case, matched)
	if found.Location != "us-central1" {
		t.Errorf("expected model location 'us-central1', got %q", found.Location)
	}

	if found.Type != "foundation" {
		t.Errorf("expected model type 'foundation', got %q", found.Type)
	}

	if found.Active {
		t.Errorf("newly discovered foundation model should be inactive by default")
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestFirstStartDiscoveryBootstrap(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Remove default models in local dev so we start clean
	models, _ := store.GetAllModels(ctx)
	for _, m := range models {
		_ = store.DeleteModel(ctx, m.ID)
	}

	// Verify that the pre-seeded models list starts as empty under our new logic
	models, err = store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to fetch models: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("expected models registry to start as empty, got %d models", len(models))
	}

	// Instantiate dashboard controller
	dash := dashboard.NewDashboardController(store, "test-project", "us-central1")

	// Mock Vertex AI REST endpoints
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/publishers/google/models") {
			w.Write([]byte(`{
				"publisherModels": [
					{
						"name": "publishers/google/models/gemini-2.5-pro",
						"displayName": "Gemini 2.5 Pro"
					}
				]
			}`))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer mockServer.Close()

	mockServerURL, _ := url.Parse(mockServer.URL)
	dash.HTTPClient = &http.Client{
		Transport: &mockGCPRoundTripper{Target: mockServerURL},
	}

	// Simulate main.go first start bootstrap logic
	if len(models) == 0 {
		if err := dash.DiscoverAndCacheModels(ctx); err != nil {
			t.Fatalf("failed to run DiscoverAndCacheModels bootstrap: %v", err)
		}
	}

	// Verify that models were dynamically discovered and cached
	bootstrappedModels, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to fetch bootstrapped models: %v", err)
	}

	if len(bootstrappedModels) == 0 {
		t.Fatalf("expected models registry to be bootstrapped, got 0 models")
	}

	var found *config.ModelConfig
	for _, m := range bootstrappedModels {
		if m.ID == "gemini-2.5-pro@us-central1" {
			mCopy := m
			found = &mCopy
			break
		}
	}

	if found == nil {
		t.Fatalf("expected gemini-2.5-pro@us-central1 to be bootstrapped in registry")
	}

	if found.Location != "us-central1" {
		t.Errorf("expected bootstrapped model location 'us-central1', got %q", found.Location)
	}

	if found.Type != "foundation" {
		t.Errorf("expected bootstrapped model type 'foundation', got %q", found.Type)
	}

	if found.Active {
		t.Errorf("bootstrapped foundation model should be inactive by default")
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestDiscoveryRegistryObsoleteDisabling(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Remove default models in local dev so we start clean
	models, _ := store.GetAllModels(ctx)
	for _, m := range models {
		_ = store.DeleteModel(ctx, m.ID)
	}

	// 1. Seed obsolete/placeholder foundation models to be soft-disabled
	fake1 := config.ModelConfig{ID: "gemini-3.0-flash@global", DisplayName: "Gemini 3.0 Flash", Location: "global", Type: "foundation", Active: true}
	fake2 := config.ModelConfig{ID: "gemini-3.1-pro@global", DisplayName: "Gemini 3.1 Pro", Location: "global", Type: "foundation", Active: true}
	
	// 2. Seed standard custom model that should NOT be altered
	custom := config.ModelConfig{ID: "my-custom-endpoint@us-central1", DisplayName: "My Custom Tuned Endpoint", Location: "us-central1", Type: "custom", Active: true}

	_ = store.SaveModel(ctx, fake1)
	_ = store.SaveModel(ctx, fake2)
	_ = store.SaveModel(ctx, custom)

	// Instantiate dashboard controller
	dash := dashboard.NewDashboardController(store, "test-project", "us-central1")

	// Mock Vertex AI REST publishers API to return ONLY gemini-2.5-flash as the active model
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/publishers/google/models") {
			w.Write([]byte(`{
				"publisherModels": [
					{
						"name": "publishers/google/models/gemini-2.5-flash",
						"displayName": "Gemini 2.5 Flash"
					}
				]
			}`))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer mockServer.Close()

	mockServerURL, _ := url.Parse(mockServer.URL)
	dash.HTTPClient = &http.Client{
		Transport: &mockGCPRoundTripper{Target: mockServerURL},
	}

	// Trigger dynamic discovery
	err = dash.DiscoverAndCacheModels(ctx)
	if err != nil {
		t.Fatalf("failed to run DiscoverAndCacheModels: %v", err)
	}

	// Load all models present in the store after discovery completes
	models, err = store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to fetch models: %v", err)
	}

	var m25Flash, m30Flash, m31Pro, mCustom *config.ModelConfig
	for _, m := range models {
		mCopy := m
		switch m.ID {
		case "gemini-2.5-flash@us-central1":
			m25Flash = &mCopy
		case "gemini-3.0-flash@global":
			m30Flash = &mCopy
		case "gemini-3.1-pro@global":
			m31Pro = &mCopy
		case "my-custom-endpoint@us-central1":
			mCustom = &mCopy
		}
	}

	// Assertions:
	if m25Flash == nil {
		t.Errorf("expected gemini-2.5-flash@us-central1 to be dynamically discovered and added")
	}
	if mCustom == nil {
		t.Errorf("expected custom model my-custom-endpoint@us-central1 to be preserved")
	}

	// Obsolete models must STILL EXIST in the database (soft-disabled, not deleted)
	if m30Flash == nil {
		t.Fatalf("expected obsolete model gemini-3.0-flash@global to remain in store, but it was deleted")
	}
	if m31Pro == nil {
		t.Fatalf("expected obsolete model gemini-3.1-pro@global to remain in store, but it was deleted")
	}

	// They must be soft-disabled (Active = false)
	if m30Flash.Active {
		t.Errorf("expected obsolete gemini-3.0-flash@global to be disabled (Active=false), but it was active")
	}
	if m31Pro.Active {
		t.Errorf("expected obsolete gemini-3.1-pro@global to be disabled (Active=false), but it was active")
	}

	// Their DisplayNames must be marked Obsolete
	if m30Flash.DisplayName != "Gemini 3.0 Flash (Obsolete)" {
		t.Errorf("expected display name 'Gemini 3.0 Flash (Obsolete)', got %q", m30Flash.DisplayName)
	}
	if m31Pro.DisplayName != "Gemini 3.1 Pro (Obsolete)" {
		t.Errorf("expected display name 'Gemini 3.1 Pro (Obsolete)', got %q", m31Pro.DisplayName)
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestStaticLocationsResolution(t *testing.T) {
	ctx := context.Background()

	// Scenario A: Deployed in specific region "europe-west9"
	dash1 := dashboard.NewDashboardController(nil, "test-project", "europe-west9")
	locs1, err := dash1.ServeLocationsTestHelper(ctx) // we will add a helper or invoke directly if exported
	if err != nil {
		t.Fatalf("failed to fetch locations: %v", err)
	}

	if len(locs1) != 3 {
		t.Fatalf("expected exactly 3 locations, got %d", len(locs1))
	}

	expected1 := map[string]bool{
		"europe-west9": true,  // Active
		"eu":           false, // Inactive
		"global":       false, // Inactive
	}

	for _, l := range locs1 {
		isActive, ok := expected1[l.ID]
		if !ok {
			t.Errorf("unexpected resolved location: %q", l.ID)
		}
		if l.Active != isActive {
			t.Errorf("expected location %s Active status to be %t, got %t", l.ID, isActive, l.Active)
		}
	}

	// Scenario B: Location is empty, should default to us-central1, us, global
	dash2 := dashboard.NewDashboardController(nil, "test-project", "")
	locs2, _ := dash2.ServeLocationsTestHelper(ctx)

	expected2 := map[string]bool{
		"us-central1": true,
		"us":          false,
		"global":      false,
	}

	for _, l := range locs2 {
		isActive, ok := expected2[l.ID]
		if !ok {
			t.Errorf("unexpected resolved location: %q", l.ID)
		}
		if l.Active != isActive {
			t.Errorf("expected default location %s Active status to be %t, got %t", l.ID, isActive, l.Active)
		}
	}
}

func TestModelRefreshLocationCorrectionAndDisabling(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// 1. Seed models initially in local region (us-central1)
	mFlash := config.ModelConfig{
		ID:          "gemini-3.5-flash",
		DisplayName: "Gemini 3.5 Flash",
		Location:    "us-central1",
		Type:        "foundation",
		Active:      true,
	}
	mPro := config.ModelConfig{
		ID:          "gemini-3.5-pro",
		DisplayName: "Gemini 3.5 Pro",
		Location:    "us-central1",
		Type:        "foundation",
		Active:      true,
	}
	_ = store.SaveModel(ctx, mFlash)
	_ = store.SaveModel(ctx, mPro)

	dash := dashboard.NewDashboardController(store, "test-project", "us-central1")

	// 2. Mock Vertex AI REST API
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/publishers/google/models") {
			if strings.Contains(r.URL.Path, "generateContent") {
				// Verification calls
				if strings.Contains(r.URL.Path, "/models/gemini-3.5-flash") {
					if strings.Contains(r.URL.Path, "/locations/us/") {
						w.WriteHeader(http.StatusOK)
						w.Write([]byte(`{"candidates": []}`))
					} else {
						w.WriteHeader(http.StatusNotFound)
						w.Write([]byte(`{"error": {"message": "Model not found"}}`))
					}
				} else if strings.Contains(r.URL.Path, "/models/gemini-3.5-pro") {
					// gemini-3.5-pro is completely unqueryable in all locations
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"error": {"message": "Model not found"}}`))
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			} else {
				// Fetch publisher models
				w.Write([]byte(`{
					"publisherModels": [
						{"name": "publishers/google/models/gemini-3.5-flash", "displayName": "Gemini 3.5 Flash"},
						{"name": "publishers/google/models/gemini-3.5-pro", "displayName": "Gemini 3.5 Pro"}
					]
				}`))
			}
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer mockServer.Close()

	mockServerURL, _ := url.Parse(mockServer.URL)
	dash.HTTPClient = &http.Client{
		Transport: &mockGCPRoundTripper{Target: mockServerURL},
	}

	// 3. Trigger refresh
	err = dash.DiscoverAndCacheModels(ctx)
	if err != nil {
		t.Fatalf("failed to run DiscoverAndCacheModels: %v", err)
	}

	// 4. Retrieve updated models
	updatedModels, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to load models: %v", err)
	}

	var resFlash, resPro *config.ModelConfig
	for _, m := range updatedModels {
		mCopy := m
		switch m.ID {
		case "gemini-3.5-flash@us":
			resFlash = &mCopy
		case "gemini-3.5-pro@us-central1":
			resPro = &mCopy
		}
	}

	if resFlash == nil {
		t.Fatalf("expected gemini-3.5-flash@us to remain in registry")
	}
	// Location should be corrected to "us"
	if resFlash.Location != "us" {
		t.Errorf("expected gemini-3.5-flash@us location to be 'us', got %q", resFlash.Location)
	}
	// It should still be active
	if !resFlash.Active {
		t.Errorf("expected gemini-3.5-flash@us to remain active")
	}

	if resPro != nil {
		t.Errorf("expected gemini-3.5-pro@us-central1 to be scrubbed/not registered since it failed all verifications")
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestSimplifiedModelDiscoveryAndRoutingCompoundKey(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Pre-seed Client, App, Key
	app := config.App{ID: "app-tdd", ClientID: "client-tdd", RPM: 100, TPM: 10000}
	client := config.Client{ID: "client-tdd", Tier: "premium"}
	key := config.APIKey{KeyHash: config.HashKey("key-tdd"), AppID: "app-tdd", Status: "active"}
	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Remove default models
	models, _ := store.GetAllModels(ctx)
	for _, m := range models {
		_ = store.DeleteModel(ctx, m.ID)
	}

	dash := dashboard.NewDashboardController(store, "test-project", "us-central1")

	// We mock Vertex AI responses
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/publishers/google/models") {
			w.Write([]byte(`{
				"publisherModels": [
					{
						"name": "publishers/google/models/gemini-2.5-flash",
						"displayName": "Gemini 2.5 Flash (TDD)"
					}
				]
			}`))
			return
		}

		// Verification query check
		if strings.Contains(r.URL.Path, "/locations/us/") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status": "verified"}`))
		} else if strings.Contains(r.URL.Path, "/locations/us-central1/") {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": {"message": "Model not found in us-central1"}}`))
		} else if strings.Contains(r.URL.Path, "/locations/global/") {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": {"message": "Model not found in global"}}`))
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	defer mockServer.Close()

	mockServerURL, _ := url.Parse(mockServer.URL)
	dash.HTTPClient = &http.Client{
		Transport: &mockGCPRoundTripper{Target: mockServerURL},
	}

	// Trigger discovery / refresh
	wRec := httptest.NewRecorder()
	reqRefresh := httptest.NewRequest("POST", "/admin/models/refresh", nil)
	dash.RefreshModels(wRec, reqRefresh)
	if !dash.WaitForRefresh(5 * time.Second) {
		t.Fatalf("timed out waiting for background RefreshModels to complete")
	}

	// Verify that us-central1, us, and global models are registered as separate options!
	refreshedModels, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to load refreshed models: %v", err)
	}

	var mCentral, mUs, mGlobal *config.ModelConfig
	for _, m := range refreshedModels {
		mCopy := m
		switch m.ID {
		case "gemini-2.5-flash@us-central1":
			mCentral = &mCopy
		case "gemini-2.5-flash@us":
			mUs = &mCopy
		case "gemini-2.5-flash@global":
			mGlobal = &mCopy
		}
	}

	if mCentral == nil || mUs == nil || mGlobal == nil {
		t.Fatalf("expected all 3 locations of gemini-2.5-flash to be registered in DB under simplified discovery")
	}

	// Central and global failed verification, so their Active status should be false
	if mCentral.Active {
		t.Errorf("expected gemini-2.5-flash@us-central1 to be inactive due to verification failure")
	}
	if mGlobal.Active {
		t.Errorf("expected gemini-2.5-flash@global to be inactive due to verification failure")
	}
	// "us" succeeded verification, but since it's newly discovered it's inactive by default
	if mUs.Active {
		t.Errorf("newly discovered gemini-2.5-flash@us should be inactive by default")
	}

	// Let's enable the "us" model option
	mUs.Active = true
	_ = store.SaveModel(ctx, *mUs)

	// Set up the reverse proxy pointing to mock backend to verify routing
	lastRoutedModel := ""
	lastRoutedLocation := ""
	proxyBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/locations/")
		if len(parts) >= 2 {
			locAndRest := parts[1]
			locParts := strings.Split(locAndRest, "/")
			lastRoutedLocation = locParts[0]
			modelParts := strings.Split(locAndRest, "/models/")
			if len(modelParts) >= 2 {
				lastRoutedModel = strings.Split(modelParts[1], ":")[0]
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
	}))
	defer proxyBackend.Close()

	proxyBackendURL, _ := url.Parse(proxyBackend.URL)
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}
	rp.TokenSource = &mockTokenSource{}
	rp.Target = proxyBackendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: proxyBackendURL}

	// Construct request targeting base model "gemini-2.5-flash"
	req := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-tdd", nil)
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Since "us" is the only active model option for gemini-2.5-flash, the proxy should route to "us"!
	if lastRoutedLocation != "us" {
		t.Errorf("expected routed location to be 'us', got %q", lastRoutedLocation)
	}
	if lastRoutedModel != "gemini-2.5-flash" {
		t.Errorf("expected routed model to be 'gemini-2.5-flash', got %q", lastRoutedModel)
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyGlobalLocationRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Seed App, Client, Key
	app := config.App{ID: "app-tdd", ClientID: "client-tdd", RPM: 100, TPM: 10000}
	client := config.Client{ID: "client-tdd", Tier: "premium"}
	key := config.APIKey{KeyHash: config.HashKey("key-tdd"), AppID: "app-tdd", Status: "active"}
	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveKey(ctx, key)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Seed global location model option
	model := config.ModelConfig{
		ID:          "gemini-3.1-pro-preview@global",
		DisplayName: "Gemini 3.1 Pro Preview",
		Location:    "global",
		Type:        "foundation",
		Active:      true,
	}
	_ = store.SaveModel(ctx, model)

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

	// Initialize RouterProxy in us-central1
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	capturer := &hostCapturingRoundTripper{Target: backendURL}
	rp.Proxy.Transport = capturer

	req := httptest.NewRequest("POST", "/v1/models/gemini-3.1-pro-preview:generateContent?key=key-tdd", strings.NewReader(`{}`))
	req.Header.Set("x-goog-api-key", "key-tdd")
	rr := httptest.NewRecorder()

	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	// Location must be global
	routedLocation := rr.Header().Get("X-Routed-Model-Location")
	if routedLocation != "global" {
		t.Errorf("expected X-Routed-Model-Location to be 'global', got %q", routedLocation)
	}

	// Target host must be rewritten to global non-regional host: aiplatform.googleapis.com!
	expectedHost := "aiplatform.googleapis.com"
	if capturer.CapturedHost != expectedHost {
		t.Errorf("expected host to be %q, got %q", expectedHost, capturer.CapturedHost)
	}

	mu.Lock()
	path := receivedPath
	mu.Unlock()

	expectedPath := "/v1/projects/test-project/locations/global/publishers/google/models/gemini-3.1-pro-preview:generateContent"
	if path != expectedPath {
		t.Errorf("expected rewritten path to be %q, got %q", expectedPath, path)
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyModelOverloadQueuingAndAPI(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	t.Setenv("ROUTER_CONCURRENCY_LIMIT", "10")
	t.Setenv("ROUTER_MAX_QUEUE_SIZE", "5")

	ctx := context.Background()
	store, _ := store.NewConfigStore(ctx, "test-project")

	app := config.App{ID: "app-1", ClientID: "client-1", RPM: 100, TPM: 500000, Priority: "high"}
	app2 := config.App{ID: "app-2", ClientID: "client-1", RPM: 100, TPM: 500000, Priority: "low"}
	client1 := config.Client{ID: "client-1", Tier: "premium"}
	key1 := config.APIKey{KeyHash: config.HashKey("key-1"), AppID: "app-1", Status: "active"}
	key2 := config.APIKey{KeyHash: config.HashKey("key-2"), AppID: "app-2", Status: "active"}

	_ = store.SaveClient(ctx, client1)
	_ = store.SaveApp(ctx, app)
	_ = store.SaveApp(ctx, app2)
	_ = store.SaveKey(ctx, key1)
	_ = store.SaveKey(ctx, key2)
	_ = store.DeleteHeader(ctx, "header-1")

	var mu sync.Mutex
	should429 := true
	backendCalls := 0

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backendCalls++
		is429 := should429
		mu.Unlock()

		if is429 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "too many requests"}`))
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status": "ok"}`))
		}
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// 1. Send first request which will return 429 (and retry, but fail)
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", nil)
	req1.Header.Set("x-goog-api-key", "key-1")
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rr1.Code)
	}

	// Verify that model is marked as overloaded
	if !rp.Scheduler.isModelOverloaded("gemini-2.5-flash") {
		t.Errorf("expected gemini-2.5-flash to be overloaded")
	}

	// 2. Hold one active request block to keep the overloaded slot occupied
	blockChan := make(chan struct{})
	defer func() {
		select {
		case <-blockChan:
		default:
			close(blockChan)
		}
	}()

	// Set backend to not 429 anymore so that we can block a successful request
	mu.Lock()
	should429 = false
	mu.Unlock()

	// Send a request that blocks in the backend, occupying the 1 overloaded slot
	backendBlocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockChan
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
	}))
	defer backendBlocked.Close()
	blockedURL, _ := url.Parse(backendBlocked.URL)

	rpBlocked, _ := NewRouterProxy(store, "test-project", "us-central1")
	rpBlocked.TokenSource = &mockTokenSource{}
	rpBlocked.Target = blockedURL
	rpBlocked.Proxy.Transport = &mockRoundTripper{Target: blockedURL}
	// Set it to also believe it's overloaded
	rpBlocked.Scheduler.overloadedModels["gemini-2.5-flash"] = time.Now().Add(15 * time.Second)

	var wg sync.WaitGroup
	// Launch the request to block
	reqBlock := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", nil)
	reqBlock.Header.Set("x-goog-api-key", "key-1")
	rrBlock := httptest.NewRecorder()
	wg.Add(1)
	go func() {
		defer wg.Done()
		rpBlocked.ServeHTTP(rrBlock, reqBlock)
	}()

	time.Sleep(100 * time.Millisecond)

	// Now, any subsequent requests should queue! Let's submit a low priority one first, then a high priority one.
	// Since they are queued, we submit them concurrently.
	reqLow := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-2", nil)
	reqLow.Header.Set("x-goog-api-key", "key-2")
	rrLow := httptest.NewRecorder()

	reqHigh := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-1", nil)
	reqHigh.Header.Set("x-goog-api-key", "key-1")
	rrHigh := httptest.NewRecorder()

	wg.Add(1)
	go func() {
		defer wg.Done()
		rpBlocked.ServeHTTP(rrLow, reqLow)
	}()
	time.Sleep(50 * time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		rpBlocked.ServeHTTP(rrHigh, reqHigh)
	}()
	time.Sleep(100 * time.Millisecond)

	// 3. Verify the queue snapshot API output
	snapshot := rpBlocked.Scheduler.GetQueueStatus()
	if len(snapshot) != 3 {
		t.Fatalf("expected 3 items in snapshot, got %d: %+v", len(snapshot), snapshot)
	}

	// The first one should be processing, and the next two should be queued, ordered by priority (high priority first)
	if snapshot[0].Status != "processing" {
		t.Errorf("expected first snapshot item to be processing, got %s", snapshot[0].Status)
	}
	if snapshot[1].Status != "queued" || snapshot[1].AppID != "app-1" {
		t.Errorf("expected second snapshot item to be queued for high priority app-1, got status %s, appID %s", snapshot[1].Status, snapshot[1].AppID)
	}
	if snapshot[2].Status != "queued" || snapshot[2].AppID != "app-2" {
		t.Errorf("expected third snapshot item to be queued for low priority app-2, got status %s, appID %s", snapshot[2].Status, snapshot[2].AppID)
	}

	// 4. Unblock! They should run and succeed.
	close(blockChan)
	wg.Wait()

	if rrLow.Code != http.StatusOK {
		t.Errorf("expected low priority request to succeed with 200, got %d", rrLow.Code)
	}
	if rrHigh.Code != http.StatusOK {
		t.Errorf("expected high priority request to succeed with 200, got %d", rrHigh.Code)
	}

	// Verify that overloaded status is now cleared because of successful requests!
	if rpBlocked.Scheduler.isModelOverloaded("gemini-2.5-flash") {
		t.Errorf("expected gemini-2.5-flash overloaded status to be cleared")
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestProxyTPMRateLimitingAndOptOut(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// 1. Seed Client, Apps, Keys
	client := config.Client{ID: "client-tpm", Tier: "standard"}
	
	appEnforced := config.App{
		ID:                   "app-tpm-enforced",
		ClientID:             "client-tpm",
		Name:                 "Enforced App",
		RPM:                  100,
		TPM:                  200, // very low TPM
		Priority:             "low", // fails instantly on breach
		OptOutTPM:            false,
	}
	
	appOptOut := config.App{
		ID:                   "app-tpm-optout",
		ClientID:             "client-tpm",
		Name:                 "Opt-out App",
		RPM:                  100,
		TPM:                  200, // same low TPM
		Priority:             "low",
		OptOutTPM:            true,
	}

	appRefund := config.App{
		ID:                   "app-tpm-refund",
		ClientID:             "client-tpm",
		Name:                 "Refund App",
		RPM:                  100,
		TPM:                  1000,
		Priority:             "low",
		OptOutTPM:            false,
	}

	keyEnforced := config.APIKey{KeyHash: config.HashKey("key-enforced"), AppID: "app-tpm-enforced", Status: "active"}
	keyOptOut := config.APIKey{KeyHash: config.HashKey("key-optout"), AppID: "app-tpm-optout", Status: "active"}
	keyRefund := config.APIKey{KeyHash: config.HashKey("key-refund"), AppID: "app-tpm-refund", Status: "active"}

	_ = store.SaveClient(ctx, client)
	_ = store.SaveApp(ctx, appEnforced)
	_ = store.SaveApp(ctx, appOptOut)
	_ = store.SaveApp(ctx, appRefund)
	_ = store.SaveKey(ctx, keyEnforced)
	_ = store.SaveKey(ctx, keyOptOut)
	_ = store.SaveKey(ctx, keyRefund)
	_ = store.DeleteHeader(ctx, "header-1")
	_ = store.DeleteRule(ctx, "rule-1")

	// Mock Vertex AI response with custom token counts
	var mockTokenCount int
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens := mockTokenCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fmt.Sprintf(`{
			"candidates": [],
			"usageMetadata": {
				"totalTokenCount": %d
			}
		}`, tokens)))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(store, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// --- SCENARIO A: Enforced App gets rate-limited ---
	// 1600 characters ≈ 400 estimated tokens, which exceeds the 200 TPM limit
	largeBody := strings.Repeat("a", 1600)
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-enforced", strings.NewReader(largeBody))
	req1.Header.Set("x-goog-api-key", "key-enforced")
	rr1 := httptest.NewRecorder()

	rp.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests for enforced app, got %d: %s", rr1.Code, rr1.Body.String())
	}

	// --- SCENARIO B: Opt-out App bypasses the limit ---
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-optout", strings.NewReader(largeBody))
	req2.Header.Set("x-goog-api-key", "key-optout")
	rr2 := httptest.NewRecorder()

	rp.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Errorf("expected 200 OK for opt-out app, got %d: %s", rr2.Code, rr2.Body.String())
	}

	// --- SCENARIO C: Estimate-and-Correct Refund logic ---
	mu.Lock()
	mockTokenCount = 50 // actual tokens is much lower than estimated 300
	mu.Unlock()

	// Request 1: 1200 chars ≈ 300 estimated tokens.
	body1 := strings.Repeat("a", 1200)
	req3 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-refund", strings.NewReader(body1))
	req3.Header.Set("x-goog-api-key", "key-refund")
	rr3 := httptest.NewRecorder()

	rp.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("expected first refund request to succeed, got %d: %s", rr3.Code, rr3.Body.String())
	}

	// Without refund, sending a 3200 character request (800 estimated tokens) immediately after
	// would make total tokens = 300 + 800 = 1100, exceeding the 1000 TPM limit.
	// But since we got refunded 250 tokens, the balance is 50 + 800 = 850 < 1000, so it should succeed!
	body2 := strings.Repeat("a", 3200)
	req4 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent?key=key-refund", strings.NewReader(body2))
	req4.Header.Set("x-goog-api-key", "key-refund")
	rr4 := httptest.NewRecorder()

	rp.ServeHTTP(rr4, req4)
	if rr4.Code != http.StatusOK {
		t.Errorf("expected second request to succeed due to refund adjustment, got %d: %s", rr4.Code, rr4.Body.String())
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestClusterQueuePriority(t *testing.T) {
	q := NewClusterQueue(10, 5*time.Second)

	jobLow := &config.QueueJob{ID: "job-low", Model: "gemma4:2b", Priority: "low", CreatedAt: time.Now()}
	jobHigh := &config.QueueJob{ID: "job-high", Model: "gemma4:2b", Priority: "high", CreatedAt: time.Now()}
	jobMed := &config.QueueJob{ID: "job-med", Model: "gemma4:2b", Priority: "medium", CreatedAt: time.Now()}

	_, _ = q.Enqueue(jobLow)
	_, _ = q.Enqueue(jobHigh)
	_, _ = q.Enqueue(jobMed)

	// High should be polled first
	polled1, ok := q.Poll([]string{"gemma4:2b"}, nil)
	if !ok || polled1.ID != "job-high" {
		t.Fatalf("expected high priority job first, got %v", polled1)
	}

	// Medium second
	polled2, ok := q.Poll([]string{"gemma4:2b"}, nil)
	if !ok || polled2.ID != "job-med" {
		t.Fatalf("expected medium priority job second, got %v", polled2)
	}

	// Low last
	polled3, ok := q.Poll([]string{"gemma4:2b"}, nil)
	if !ok || polled3.ID != "job-low" {
		t.Fatalf("expected low priority job last, got %v", polled3)
	}
}

func TestClusterQueueTTL(t *testing.T) {
	// Max queue age of 10ms for quick test
	q := NewClusterQueue(10, 10*time.Millisecond)

	job := &config.QueueJob{ID: "job-ttl", Model: "gemma4:2b", Priority: "high", CreatedAt: time.Now().Add(-1 * time.Second)}
	resChan, err := q.Enqueue(job)
	if err != nil {
		t.Fatalf("failed to enqueue job: %v", err)
	}

	select {
	case res := <-resChan:
		if res.StatusCode != http.StatusGatewayTimeout {
			t.Errorf("expected status 504 Gateway Timeout, got %d", res.StatusCode)
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("timed out waiting for reaper to expire job")
	}
}

func TestClusterProxyE2E(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to config store: %v", err)
	}
	
	q := NewClusterQueue(10, 5*time.Second)
	
	rp, err := NewRouterProxy(store, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to initialize proxy: %v", err)
	}
	rp.Queue = q // Set local cluster queue on proxy

	// Seed DB credentials for auth & priority
	_ = store.SaveClient(ctx, config.Client{ID: "client-1", Tier: "premium"})
	_ = store.SaveApp(ctx, config.App{ID: "app-1", ClientID: "client-1", Priority: "high", RPM: 100, TPM: 100000})
	_ = store.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("gr_test_key"), AppID: "app-1", ClientID: "client-1", Status: "active"})

	// Clear default seeded rules and headers
	_ = store.DeleteRule(ctx, "rule-1")
	_ = store.DeleteHeader(ctx, "header-1")
	
	// Seed cluster rule routing gemma4:2b to local cluster
	rule := config.RoutingRule{
		ID:             "rule-local-gemma4",
		ModelPattern:   "gemma4:2b",
		ClientTier:     "all",
		TargetModel:    "gemma4:2b",
		TargetLocation: "local-cluster",
		PriorityWeight: 1,
	}
	_ = store.SaveRule(ctx, rule)

	// 1. Call ServeHTTP -> blocks waiting on queue
	req := httptest.NewRequest("POST", "/v1/models/gemma4:2b:generateContent?key=gr_test_key", strings.NewReader(`{"prompt":"hello"}`))
	req.Header.Set("x-goog-api-key", "gr_test_key")
	rr := httptest.NewRecorder()

	clientDone := make(chan struct{})
	go func() {
		rp.ServeHTTP(rr, req)
		close(clientDone)
	}()

	time.Sleep(50 * time.Millisecond)

	// 2. Poll from queue
	job, ok := q.Poll([]string{"gemma4:2b"}, nil)
	if !ok || job == nil {
		t.Fatalf("failed to poll enqueued job. Response Code: %d, Body: %s", rr.Code, rr.Body.String())
	}

	// 3. Resolve job
	q.Resolve(job.ID, config.QueueResult{
		Payload:    []byte(`{"candidates": [{"content": {"parts": [{"text": "Hello from local Gemma 4 2B!"}]}}]}`),
		StatusCode: 200,
	})

	// 4. Wait and verify response
	<-clientDone
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Hello from local Gemma 4 2B!") {
		t.Errorf("expected response to contain candidate text, got: %s", rr.Body.String())
	}

	// local_db isolated via LOCAL_DATA_DIR in TestMain
}

func TestTDD_SmartRouting_PerPromptOverrides(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-smart", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-smart",
		ClientID: "client-smart",
		Complexity: config.ComplexityRouting{
			Enabled:      true,
			SimpleModel:  "gemini-3.5-flash-lite",
			MediumModel:  "gemini-3.8-flash",
			ComplexModel: "gemini-3.1-pro",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash: config.HashKey("key-smart-override"),
		AppID:   "app-smart",
		Status:  "active",
	})
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	var lastTargetModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastTargetModel = strings.Split(parts[1], ":")[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "OK"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// 1. Direct Per-Prompt Model Override via Header X-Router-Model
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-smart-override", strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}]}`))
	req1.Header.Set("X-Router-Model", "gemini-3.1-pro")
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr1.Code, rr1.Body.String())
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("expected routed model gemini-3.1-pro, got %s", lastTargetModel)
	}
	if rr1.Header().Get("X-Router-Engine-Used") != "prompt_override" {
		t.Errorf("expected engine prompt_override, got %s", rr1.Header().Get("X-Router-Engine-Used"))
	}

	// 2. Direct Per-Prompt Complexity Override via Header X-Router-Complexity: simple
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-smart-override", strings.NewReader(`{"contents":[{"parts":[{"text":"complex algorithmic proof with matrices"}]}]}`))
	req2.Header.Set("X-Router-Complexity", "simple")
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
	if lastTargetModel != "gemini-3.5-flash-lite" {
		t.Errorf("expected routed model gemini-3.5-flash-lite for simple override, got %s", lastTargetModel)
	}

	// 3. Direct Per-Prompt Engine Override via Header X-Router-Engine: clm
	req3 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-smart-override", strings.NewReader(`{"contents":[{"parts":[{"text":"What is 2+2?"}]}]}`))
	req3.Header.Set("X-Router-Engine", "clm")
	rr3 := httptest.NewRecorder()
	rp.ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr3.Code, rr3.Body.String())
	}
	if rr3.Header().Get("X-Router-Engine-Used") != "clm" {
		t.Errorf("expected engine clm, got %s", rr3.Header().Get("X-Router-Engine-Used"))
	}
}

func TestTDD_SmartRouting_SessionAffinity(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-sess", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-sess",
		ClientID: "client-sess",
		Complexity: config.ComplexityRouting{
			Enabled:      true,
			SimpleModel:  "gemini-3.5-flash-lite",
			MediumModel:  "gemini-3.8-flash",
			ComplexModel: "gemini-3.1-pro",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash: config.HashKey("key-session-smart"),
		AppID:   "app-sess",
		Status:  "active",
	})
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	var lastTargetModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastTargetModel = strings.Split(parts[1], ":")[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "OK"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	sessionID := "conv-user-12345"

	// Turn 1: Complex coding prompt -> dynamic Jev routes to gemini-3.1-pro
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-session-smart", strings.NewReader(`{"contents":[{"parts":[{"text":"func RunDistributedRaftElection() error {\n return nil \n}\nOptimize this Go consensus algorithm"}]}]}`))
	req1.Header.Set("X-Session-ID", sessionID)
	req1.Header.Set("X-Session-Policy", "lock")
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("Turn 1 failed: %d: %s", rr1.Code, rr1.Body.String())
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("Turn 1 expected gemini-3.1-pro, got %s", lastTargetModel)
	}
	if rr1.Header().Get("X-Session-Turn") != "1" {
		t.Errorf("expected session turn 1, got %s", rr1.Header().Get("X-Session-Turn"))
	}

	// Turn 2: Short prompt ("Thanks!") -> Sticky session lock should KEEP gemini-3.1-pro!
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-session-smart", strings.NewReader(`{"contents":[{"parts":[{"text":"Thanks! Looks great."}]}]}`))
	req2.Header.Set("X-Session-ID", sessionID)
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("Turn 2 failed: %d: %s", rr2.Code, rr2.Body.String())
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("Turn 2 expected sticky model gemini-3.1-pro, got %s", lastTargetModel)
	}
	if rr2.Header().Get("X-Router-Engine-Used") != "session_lock" {
		t.Errorf("Turn 2 expected engine session_lock, got %s", rr2.Header().Get("X-Router-Engine-Used"))
	}
	if rr2.Header().Get("X-Session-Turn") != "2" {
		t.Errorf("expected session turn 2, got %s", rr2.Header().Get("X-Session-Turn"))
	}

	// Turn 1 for new session with explicit X-Session-Model
	reqInit := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-session-smart", strings.NewReader(`{"contents":[{"parts":[{"text":"Start conversation"}]}]}`))
	reqInit.Header.Set("X-Session-ID", "conv-locked-flash")
	reqInit.Header.Set("X-Session-Model", "gemini-3.8-flash")
	rrInit := httptest.NewRecorder()
	rp.ServeHTTP(rrInit, reqInit)

	if rrInit.Code != http.StatusOK {
		t.Fatalf("reqInit failed: %d", rrInit.Code)
	}
	if lastTargetModel != "gemini-3.8-flash" {
		t.Errorf("expected explicit session model gemini-3.8-flash, got %s", lastTargetModel)
	}
}

func TestTDD_SmartRouting_AgentTaskSwitching(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-agent", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-agent",
		ClientID: "client-agent",
		Complexity: config.ComplexityRouting{
			Enabled:      true,
			SimpleModel:  "gemini-3.5-flash-lite",
			MediumModel:  "gemini-3.8-flash",
			ComplexModel: "gemini-3.1-pro",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash: config.HashKey("key-agent-smart"),
		AppID:   "app-agent",
		Status:  "active",
	})
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	var lastTargetModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastTargetModel = strings.Split(parts[1], ":")[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "OK"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// 1. Agent sets task: coding -> routes to gemini-3.1-pro
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-agent-smart", strings.NewReader(`{"contents":[{"parts":[{"text":"Implement the user profile view"}]}]}`))
	req1.Header.Set("X-Agent-Task", "code_generation")
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr1.Code, rr1.Body.String())
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("expected gemini-3.1-pro for code task, got %s", lastTargetModel)
	}
	if rr1.Header().Get("X-Router-Engine-Used") != "task_mapping" {
		t.Errorf("expected engine task_mapping, got %s", rr1.Header().Get("X-Router-Engine-Used"))
	}

	// 2. Agent switches task to: triage -> routes to gemini-3.5-flash-lite
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-agent-smart", strings.NewReader(`{"contents":[{"parts":[{"text":"Filter incoming error logs"}]}]}`))
	req2.Header.Set("X-Agent-Task", "triage")
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
	if lastTargetModel != "gemini-3.5-flash-lite" {
		t.Errorf("expected gemini-3.5-flash-lite for triage task, got %s", lastTargetModel)
	}

	// 3. Agent switches task to: summarization -> routes to gemini-3.8-flash
	req3 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-agent-smart", strings.NewReader(`{"contents":[{"parts":[{"text":"Summarize the results in 2 bullets"}]}]}`))
	req3.Header.Set("X-Agent-Task", "summarization")
	rr3 := httptest.NewRecorder()
	rp.ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr3.Code, rr3.Body.String())
	}
	if lastTargetModel != "gemini-3.8-flash" {
		t.Errorf("expected gemini-3.8-flash for summarization task, got %s", lastTargetModel)
	}
}

func TestTDD_SmartRouting_JevAndCLMEngines(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-engines", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-engines",
		ClientID: "client-engines",
		Complexity: config.ComplexityRouting{
			Enabled:      true,
			Engine:       "jev",
			SimpleModel:  "gemini-3.5-flash-lite",
			MediumModel:  "gemini-3.8-flash",
			ComplexModel: "gemini-3.1-pro",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash: config.HashKey("key-engine-test"),
		AppID:   "app-engines",
		Status:  "active",
	})
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	var lastTargetModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastTargetModel = strings.Split(parts[1], ":")[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "OK"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// 1. Jev Engine with Simple Prompt
	reqSimple := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-engine-test", strings.NewReader(`{"contents":[{"parts":[{"text":"Hello world!"}]}]}`))
	rrSimple := httptest.NewRecorder()
	rp.ServeHTTP(rrSimple, reqSimple)

	if rrSimple.Code != http.StatusOK {
		t.Fatalf("Jev simple failed: %d", rrSimple.Code)
	}
	if lastTargetModel != "gemini-3.5-flash-lite" {
		t.Errorf("Jev simple expected gemini-3.5-flash-lite, got %s", lastTargetModel)
	}
	if rrSimple.Header().Get("X-Router-Engine-Used") != "jev" {
		t.Errorf("expected engine jev, got %s", rrSimple.Header().Get("X-Router-Engine-Used"))
	}

	// 2. Jev Engine with Complex Code Prompt
	reqComplex := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-engine-test", strings.NewReader("{\"contents\":[{\"parts\":[{\"text\":\"```go\\nfunc ComputeEigenvalues(m [][]float64) []float64 {\\nreturn nil\\n}\\n```\\nSolve the mathematical algorithm with O(N^3) time complexity.\"}]}]}"))
	rrComplex := httptest.NewRecorder()
	rp.ServeHTTP(rrComplex, reqComplex)

	if rrComplex.Code != http.StatusOK {
		t.Fatalf("Jev complex failed: %d", rrComplex.Code)
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("Jev complex expected gemini-3.1-pro, got %s", lastTargetModel)
	}

	// 3. Switch to CLM Engine via app config
	app, _ := cfgStore.LookupApp("app-engines")
	app.Complexity.Engine = "clm"
	_ = cfgStore.SaveApp(ctx, app)

	reqCLM := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-engine-test", strings.NewReader("{\"contents\":[{\"parts\":[{\"text\":\"```python\\ndef solve(): pass\\n```\\nAnalyze the asymptotic upper bound and proofs.\"}]}]}"))
	rrCLM := httptest.NewRecorder()
	rp.ServeHTTP(rrCLM, reqCLM)

	if rrCLM.Code != http.StatusOK {
		t.Fatalf("CLM failed: %d", rrCLM.Code)
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("CLM expected gemini-3.1-pro, got %s", lastTargetModel)
	}
	if rrCLM.Header().Get("X-Router-Engine-Used") != "clm" {
		t.Errorf("expected engine clm, got %s", rrCLM.Header().Get("X-Router-Engine-Used"))
	}
}

func TestIndividualModelsByOfferedLocation(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "davenport-boutique")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-model-loc-test", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-model-loc-test",
		ClientID: "client-model-loc-test",
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash: config.HashKey("key-model-loc-test"),
		AppID:   "app-model-loc-test",
		Status:  "active",
	})

	testCases := []struct {
		model    string
		location string
	}{
		// Gemini 3.8
		{"gemini-3.8-flash", "global"},
		{"gemini-3.8-flash", "us"},
		{"gemini-3.8-flash", "us-central1"},
		{"gemini-3.8-flash", "europe-west1"},

		// Gemini 3.5
		{"gemini-3.5-flash", "global"},
		{"gemini-3.5-flash", "us"},
		{"gemini-3.5-flash", "us-central1"},
		{"gemini-3.5-flash", "europe-west9"},
		{"gemini-3.5-flash", "europe-west1"},

		{"gemini-3.5-flash-lite", "global"},
		{"gemini-3.5-flash-lite", "us"},
		{"gemini-3.5-flash-lite", "us-central1"},

		// Gemini 3.1
		{"gemini-3.1-pro", "global"},
		{"gemini-3.1-pro", "us"},
		{"gemini-3.1-pro", "us-central1"},

		{"gemini-3.1-flash-lite", "global"},
		{"gemini-3.1-flash-lite", "us"},
		{"gemini-3.1-flash-lite", "us-central1"},

		// Gemini 2.5
		{"gemini-2.5-flash", "us-central1"},
		{"gemini-2.5-flash", "us"},
		{"gemini-2.5-flash", "global"},

		{"gemini-2.5-pro", "us-central1"},
		{"gemini-2.5-pro", "us"},
		{"gemini-2.5-pro", "global"},

		{"gemini-2.5-flash-lite", "us-central1"},
		{"gemini-2.5-flash-lite", "us"},
		{"gemini-2.5-flash-lite", "global"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(fmt.Sprintf("%s_in_%s", tc.model, tc.location), func(t *testing.T) {
			var capturedUpstreamPath string

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedUpstreamPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "Hello from model test"}]}}]}`))
			}))
			defer backend.Close()

			backendURL, _ := url.Parse(backend.URL)
			rp, err := NewRouterProxy(cfgStore, "davenport-boutique", "us-central1")
			if err != nil {
				t.Fatalf("failed to create proxy: %v", err)
			}
			rp.TokenSource = &mockTokenSource{}
			rp.Target = backendURL

			// Capture the Director's target host before Transport rewrites to backend
			var directorHost string
			origDirector := rp.Proxy.Director
			rp.Proxy.Director = func(req *http.Request) {
				origDirector(req)
				directorHost = req.Host
			}

			rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

			// Configure explicit routing rule targeting model and location
			ruleID := fmt.Sprintf("rule-%s-%s", tc.model, tc.location)
			rule := config.RoutingRule{
				ID:             ruleID,
				AppID:          "app-model-loc-test",
				ModelPattern:   tc.model,
				ClientTier:     "all",
				TargetModel:    tc.model,
				TargetLocation: tc.location,
				PriorityWeight: 10,
			}
			_ = cfgStore.SaveRule(ctx, rule)
			defer cfgStore.DeleteRule(ctx, ruleID)

			req := httptest.NewRequest("POST", fmt.Sprintf("/v1/models/%s:generateContent?key=key-model-loc-test", tc.model), strings.NewReader(`{"contents":[{"parts":[{"text":"ping"}]}]}`))
			rr := httptest.NewRecorder()

			rp.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected HTTP 200, got %d: %s", rr.Code, rr.Body.String())
			}

			// 1. Verify response headers
			if routedModel := rr.Header().Get("X-Routed-Model"); routedModel != tc.model {
				t.Errorf("X-Routed-Model mismatch: expected %q, got %q", tc.model, routedModel)
			}
			if routedLoc := rr.Header().Get("X-Routed-Model-Location"); routedLoc != tc.location {
				t.Errorf("X-Routed-Model-Location mismatch: expected %q, got %q", tc.location, routedLoc)
			}

			// 2. Verify rewritten URL path
			expectedPath := fmt.Sprintf("/v1/projects/davenport-boutique/locations/%s/publishers/google/models/%s:generateContent", tc.location, tc.model)
			if capturedUpstreamPath != expectedPath {
				t.Errorf("Upstream URL path mismatch:\nexpected: %s\ngot:      %s", expectedPath, capturedUpstreamPath)
			}

			// 3. Verify director target host
			expectedHost := config.GetVertexEndpointHost(tc.location)
			if tc.location != "us-central1" || config.IsMultiRegionOrGlobal(tc.location) {
				if directorHost != expectedHost {
					t.Errorf("Director target host mismatch: expected %q, got %q", expectedHost, directorHost)
				}
			}
		})
	}
}

func TestClusterQueue_CloseAndCancelOnDisconnect(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "c-q", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-q", ClientID: "c-q", RPM: 100, TPM: 100000, Priority: "high"})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-q"), AppID: "app-q", Status: "active"})
	_ = cfgStore.SaveModel(ctx, config.ModelConfig{ID: "gemma-4-9b", Location: "local-cluster", Active: true})

	rp, err := NewRouterProxy(cfgStore, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to init proxy: %v", err)
	}
	defer rp.Queue.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/models/gemma-4-9b:generateContent?key=key-q", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`)).WithContext(reqCtx)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		rp.ServeHTTP(rr, req)
		close(done)
	}()

	// Wait until job is enqueued in rp.Queue
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rp.Queue.GetQueueSnapshot()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rp.Queue.GetQueueSnapshot()) != 1 {
		t.Fatalf("expected 1 job in cluster queue before cancel")
	}

	// Cancel client context and verify job is removed from queue
	cancel()
	<-done

	if len(rp.Queue.GetQueueSnapshot()) != 0 {
		t.Errorf("expected cluster queue to be empty after client disconnect, got %d items", len(rp.Queue.GetQueueSnapshot()))
	}
}

func TestProxyFallbackModelFailover(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "c-fb", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-fb", ClientID: "c-fb", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-fb"), AppID: "app-fb", Status: "active"})
	_ = cfgStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-fb",
		AppID:          "app-fb",
		ModelPattern:   "gemini-3.1-pro",
		ClientTier:     "all",
		TargetModel:    "gemini-3.1-pro",
		TargetLocation: "us-central1",
		FallbackModel:  "gemini-2.5-flash",
		PriorityWeight: 10,
	})

	var requestedPaths []string
	var mu sync.Mutex
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestedPaths = append(requestedPaths, r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/models/gemini-3.1-pro:") {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"primary model overloaded"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"fallback response"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	req := httptest.NewRequest("POST", "/v1/models/gemini-3.1-pro:generateContent?key=key-fb", strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}]}`))
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from fallback model, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Router-Fallback-Triggered") != "true" {
		t.Errorf("expected X-Router-Fallback-Triggered=true, got %q", rr.Header().Get("X-Router-Fallback-Triggered"))
	}
	if rr.Header().Get("X-Routed-Model") != "gemini-2.5-flash" {
		t.Errorf("expected X-Routed-Model=gemini-2.5-flash after failover, got %q", rr.Header().Get("X-Routed-Model"))
	}
}

func TestProxyUnregisteredModelLocationSuffixStripping(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	// Delete all seeded models so LookupActiveModel returns false
	models, _ := cfgStore.GetAllModels(ctx)
	for _, m := range models {
		_ = cfgStore.DeleteModel(ctx, m.ID)
	}

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "c-strip", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-strip", ClientID: "c-strip", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-strip"), AppID: "app-strip", Status: "active"})

	var upstreamPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates":[]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	req := httptest.NewRequest("POST", "/v1/models/gemini-3.8-flash@us:generateContent?key=key-strip", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Routed-Model") != "gemini-3.8-flash" {
		t.Errorf("expected stripped model 'gemini-3.8-flash', got %q", rr.Header().Get("X-Routed-Model"))
	}
	if rr.Header().Get("X-Routed-Model-Location") != "us" {
		t.Errorf("expected location 'us' from @us suffix, got %q", rr.Header().Get("X-Routed-Model-Location"))
	}
	if strings.Contains(upstreamPath, "@us") {
		t.Errorf("upstream path should not contain @us suffix, got %q", upstreamPath)
	}
}

func TestProxyLLMClassifierCacheAndLocationHost(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "c-llm", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-llm",
		ClientID: "c-llm",
		RPM:      100,
		TPM:      100000,
		Complexity: config.ComplexityRouting{
			Enabled:          true,
			Engine:           "llm",
			UseLLMClassifier: true,
			ClassifierModel:  "gemini-3.5-flash-lite@global",
			SimpleModel:      "gemini-3.5-flash-lite",
			MediumModel:      "gemini-3.8-flash",
			ComplexModel:     "gemini-3.1-pro",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-llm"), AppID: "app-llm", Status: "active"})

	var classifierCalls int
	var classifierPaths []string
	var mu sync.Mutex

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.Contains(r.URL.Path, "gemini-3.5-flash-lite") && classifierCalls == 0 {
			classifierCalls++
			classifierPaths = append(classifierPaths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"complexity\":\"medium\"}"}]}}]}`))
			return
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}
	rp.ClassifierClient = backend.Client()

	// Request 1: misses cache, invokes classifier
	body := `{"contents":[{"parts":[{"text":"Summarize this quarterly report."}]}]}`
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-llm", strings.NewReader(body))
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("req1 failed: %d", rr1.Code)
	}

	// Request 2: identical prompt -> should hit classifierCache without calling upstream classifier a second time
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-llm", strings.NewReader(body))
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("req2 failed: %d", rr2.Code)
	}

	mu.Lock()
	calls := classifierCalls
	paths := classifierPaths
	mu.Unlock()

	if calls != 1 {
		t.Errorf("expected classifier to be called once due to caching, got %d", calls)
	}
	if len(paths) > 0 && strings.Contains(paths[0], "@global") {
		t.Errorf("expected @global suffix to be stripped from classifier URL path, got %q", paths[0])
	}
}

func TestProxyOversizedBodyAndSafeJSONError(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "c-err", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-err", ClientID: "c-err", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-err"), AppID: "app-err", Status: "active"})
	_ = cfgStore.SaveHeader(ctx, config.CustomHeader{
		ID:          "h-quote",
		AppID:       "app-err",
		Name:        "X-Quote-Header",
		Description: `Must not break "JSON" encoding`,
		Required:    true,
	})

	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}

	// 1. Test missing header with quotes in description produces valid JSON
	req1 := httptest.NewRequest("POST", "/v1/models/gemini-3.8-flash:generateContent?key=key-err", strings.NewReader(`{}`))
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr1.Code)
	}
	var errPayload map[string]interface{}
	if err := json.Unmarshal(rr1.Body.Bytes(), &errPayload); err != nil {
		t.Errorf("expected valid JSON error response when description contains quotes, got error %v on body: %s", err, rr1.Body.String())
	}

	// 2. Test reader error on body returns 413 Request Entity Too Large
	errReader := &failingReader{err: fmt.Errorf("http: request body too large")}
	req2 := httptest.NewRequest("POST", "/v1/models/gemini-3.8-flash:generateContent?key=key-err", errReader)
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 Request Entity Too Large on unreadable/oversized body, got %d", rr2.Code)
	}
}

type failingReader struct {
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	return 0, f.err
}

func TestProxyStickyTurnSessionPolicyE2E(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "c-sticky", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-sticky",
		ClientID: "c-sticky",
		RPM:      100,
		TPM:      100000,
		Complexity: config.ComplexityRouting{
			Enabled:         true,
			Engine:          "heuristics",
			SimpleModel:     "gemini-3.5-flash-lite",
			MediumModel:     "gemini-3.8-flash",
			ComplexModel:    "gemini-3.1-pro",
			SimpleCharLimit: 20,
			MediumCharLimit: 80,
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-sticky"), AppID: "app-sticky", Status: "active"})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	sendTurn := func(prompt string) string {
		body := fmt.Sprintf(`{"contents":[{"parts":[{"text":%q}]}]}`, prompt)
		req := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-sticky", strings.NewReader(body))
		req.Header.Set("X-Session-ID", "sess-sticky-e2e")
		req.Header.Set("X-Session-Policy", "sticky-turn")
		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		return rr.Header().Get("X-Routed-Model")
	}

	// Turn 1: simple prompt -> gemini-3.5-flash-lite
	if m := sendTurn("Hi"); m != "gemini-3.5-flash-lite" {
		t.Fatalf("Turn 1 expected gemini-3.5-flash-lite, got %q", m)
	}
	// Turn 2: complex prompt (> 80 chars) -> upgrades immediately to gemini-3.1-pro
	if m := sendTurn(strings.Repeat("Complex architectural analysis prompt ", 4)); m != "gemini-3.1-pro" {
		t.Fatalf("Turn 2 expected upgrade to gemini-3.1-pro, got %q", m)
	}
	// Turn 3: simple prompt -> cooldown 1/3 keeps gemini-3.1-pro
	if m := sendTurn("Thanks"); m != "gemini-3.1-pro" {
		t.Fatalf("Turn 3 expected sticky lock to gemini-3.1-pro, got %q", m)
	}
	// Turn 4: simple prompt -> cooldown 2/3 keeps gemini-3.1-pro
	if m := sendTurn("Ok"); m != "gemini-3.1-pro" {
		t.Fatalf("Turn 4 expected sticky lock to gemini-3.1-pro, got %q", m)
	}
	// Turn 5: simple prompt -> cooldown 3/3 keeps gemini-3.1-pro
	if m := sendTurn("Hold"); m != "gemini-3.1-pro" {
		t.Fatalf("Turn 5 expected sticky lock to gemini-3.1-pro, got %q", m)
	}
	// Turn 6: simple prompt -> 3 turns elapsed since upgrade, controlled downgrade to gemini-3.5-flash-lite
	if m := sendTurn("Bye"); m != "gemini-3.5-flash-lite" {
		t.Fatalf("Turn 6 expected controlled downgrade to gemini-3.5-flash-lite, got %q", m)
	}
}

func TestTDD_SmartRouting_FastinoAndContrastiveLMAndConditionalRules(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-fc", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-fc",
		ClientID: "client-fc",
		Complexity: config.ComplexityRouting{
			Enabled:      true,
			Engine:       "fastino",
			SimpleModel:  "gemini-3.5-flash-lite",
			MediumModel:  "gemini-3.8-flash",
			ComplexModel: "gemini-3.1-pro",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash: config.HashKey("key-fc-test"),
		AppID:   "app-fc",
		Status:  "active",
	})

	var lastTargetModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastTargetModel = strings.Split(parts[1], ":")[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "OK"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	// 1. Fastino Engine with simple prompt
	reqFastinoSimple := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-fc-test", strings.NewReader(`{"contents":[{"parts":[{"text":"Hello!"}]}]}`))
	rrFastinoSimple := httptest.NewRecorder()
	rp.ServeHTTP(rrFastinoSimple, reqFastinoSimple)
	if rrFastinoSimple.Code != http.StatusOK {
		t.Fatalf("Fastino simple failed: %d: %s", rrFastinoSimple.Code, rrFastinoSimple.Body.String())
	}
	if lastTargetModel != "gemini-3.5-flash-lite" {
		t.Errorf("Fastino simple expected gemini-3.5-flash-lite, got %s", lastTargetModel)
	}
	if rrFastinoSimple.Header().Get("X-Router-Engine-Used") != "fastino" {
		t.Errorf("expected X-Router-Engine-Used=fastino, got %s", rrFastinoSimple.Header().Get("X-Router-Engine-Used"))
	}

	// 2. X-Router-Engine: contrastive-lm override with complex code prompt
	reqCLM := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-fc-test", strings.NewReader("{\"contents\":[{\"parts\":[{\"text\":\"```go\\nfunc DistributedPaxos() {}\\n```\\nProve safety and liveness in O(N^2) message complexity.\"}]}]}"))
	reqCLM.Header.Set("X-Router-Engine", "contrastive-lm")
	rrCLM := httptest.NewRecorder()
	rp.ServeHTTP(rrCLM, reqCLM)
	if rrCLM.Code != http.StatusOK {
		t.Fatalf("Contrastive-LM failed: %d", rrCLM.Code)
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("Contrastive-LM expected gemini-3.1-pro, got %s", lastTargetModel)
	}
	if rrCLM.Header().Get("X-Router-Engine-Used") != "contrastive-lm" {
		t.Errorf("expected X-Router-Engine-Used=contrastive-lm, got %s", rrCLM.Header().Get("X-Router-Engine-Used"))
	}

	// 3. Conditional RoutingRule using ComplexityTier, DecisionEngine, and MinConfidence
	_ = cfgStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-fastino-complex-override",
		AppID:          "app-fc",
		ModelPattern:   "*",
		ClientTier:     "all",
		ComplexityTier: "complex",
		DecisionEngine: "fastino",
		MinConfidence:  0.50,
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		PriorityWeight: 20,
	})

	reqRuleComplex := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-fc-test", strings.NewReader("{\"contents\":[{\"parts\":[{\"text\":\"```python\\ndef backprop(): pass\\n```\\nDerive the Hessian matrix and O(N^3) complexity step by step.\"}]}]}"))
	rrRuleComplex := httptest.NewRecorder()
	rp.ServeHTTP(rrRuleComplex, reqRuleComplex)
	if rrRuleComplex.Code != http.StatusOK {
		t.Fatalf("Rule complex failed: %d", rrRuleComplex.Code)
	}
	if lastTargetModel != "gemini-2.5-pro" {
		t.Errorf("expected conditional rule to override complex Fastino prompt to gemini-2.5-pro, got %s", lastTargetModel)
	}
}

func TestTDD_ContrastiveLMClusterDirectEndpointRouting(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	defer func() {
		decision.SetEngineClusterConfig("contrastive-lm", "", "local", "")
		decision.DeleteEngineSecret("contrastive-lm")
	}()

	var clusterHits int
	var clusterAuth string
	clmClusterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clusterHits++
		clusterAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		// Force even a 2-character greeting to "complex" so we prove the cluster endpoint was used instead of local anchor scoring
		_, _ = w.Write([]byte(`[
			{"rank": 1, "candidate": "complex: deep reasoning and code", "prob": 0.98, "score": 15.0},
			{"rank": 2, "candidate": "medium: standard summarization", "prob": 0.01, "score": 1.0},
			{"rank": 3, "candidate": "simple: short greeting", "prob": 0.01, "score": 0.1}
		]`))
	}))
	defer clmClusterSrv.Close()

	// Register Contrastive-LM cluster in Providers["local"] and secret vault
	decision.SetEngineSecret("contrastive-lm", "clm-cluster-bearer-token-9001")
	localProv := config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local / Open-Weights Clusters",
		Enabled:     true,
	}
	decision.ApplyDecisionProviderToLocalConfig(&localProv, "contrastive-lm", clmClusterSrv.URL, "cluster", "clm-cluster-default", "CLM_API_KEY")
	if err := cfgStore.SaveProvider(ctx, localProv); err != nil {
		t.Fatalf("failed to save local provider with CLM cluster: %v", err)
	}

	// Reset in-memory cluster config to verify RouterProxy syncs it dynamically from Store.GetAllProviders()["local"]
	decision.SetEngineClusterConfig("contrastive-lm", "", "local", "")

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-clm-cluster", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-clm-cluster",
		ClientID: "client-clm-cluster",
		Complexity: config.ComplexityRouting{
			Enabled:      true,
			Engine:       "contrastive-lm",
			CLMEndpoint:  "", // Empty per-app endpoint: must resolve from Providers["local"] cluster registration
			SimpleModel:  "gemini-3.5-flash-lite",
			MediumModel:  "gemini-3.8-flash",
			ComplexModel: "gemini-3.1-pro",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash: config.HashKey("key-clm-cluster-test"),
		AppID:   "app-clm-cluster",
		Status:  "active",
	})

	var lastTargetModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/models/")
		if len(parts) >= 2 {
			lastTargetModel = strings.Split(parts[1], ":")[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates": [{"content": {"parts": [{"text": "OK"}]}}]}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	req := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent?key=key-clm-cluster-test", strings.NewReader(`{"contents":[{"parts":[{"text":"Hi"}]}]}`))
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
	}
	if clusterHits != 1 {
		t.Fatalf("expected Contrastive-LM cluster server to be called once, got %d", clusterHits)
	}
	if clusterAuth != "Bearer clm-cluster-bearer-token-9001" {
		t.Errorf("expected Bearer clm-cluster-bearer-token-9001 sent to cluster runner, got %q", clusterAuth)
	}
	if lastTargetModel != "gemini-3.1-pro" {
		t.Errorf("expected cluster rank response to route 'Hi' to complex model gemini-3.1-pro, got %s", lastTargetModel)
	}
}

func TestTDD_OpenAIChatCompletionsAndModels(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()

	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-openai", Name: "OpenAI SDK Client", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-openai",
		ClientID: "client-openai",
		Name:     "OpenAI Compatible App",
		RPM:      500,
		TPM:      500000,
		Priority: "high",
		Complexity: config.ComplexityRouting{
			Enabled:                true,
			Engine:                 "jev",
			SimpleModel:            "gemini-3.5-flash-lite",
			MediumModel:            "gemini-3.8-flash",
			ComplexModel:           "gemini-3.1-pro",
			ForceComplexTools:      true,
			ForceComplexMultimodal: true,
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{
		KeyHash:  config.HashKey("key-openai-test"),
		AppID:    "app-openai",
		ClientID: "client-openai",
		Status:   "active",
	})

	var lastPath string
	var lastQuery string
	var lastReqBody []byte
	var simulateToolResponse bool
	var simulateStreamResponse bool
	var simulateGzipResponse bool

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastQuery = r.URL.RawQuery
		lastReqBody, _ = io.ReadAll(r.Body)

		if simulateGzipResponse {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			var gzBuf bytes.Buffer
			gz := gzip.NewWriter(&gzBuf)
			_, _ = gz.Write([]byte(`{
				"candidates": [{
					"content": {
						"role": "model",
						"parts": [{"text": "Hello from Gzipped Vertex AI!"}]
					},
					"finishReason": "STOP"
				}],
				"usageMetadata": {"promptTokenCount": 8, "candidatesTokenCount": 6, "totalTokenCount": 14}
			}`))
			_ = gz.Close()
			_, _ = w.Write(gzBuf.Bytes())
			return
		}

		if simulateStreamResponse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello \"}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"from OpenAI stream!\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":4,\"totalTokenCount\":9}}\n\n"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if simulateToolResponse {
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {
						"role": "model",
						"parts": [{
							"functionCall": {
								"name": "get_weather",
								"args": {"location": "San Francisco, CA"}
							}
						}]
					},
					"finishReason": "STOP"
				}],
				"usageMetadata": {"promptTokenCount": 14, "candidatesTokenCount": 8, "totalTokenCount": 22}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [{"text": "Hello from Gemini Smart Router via OpenAI Spec!"}]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 9, "totalTokenCount": 19}
		}`))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	rp, err := NewRouterProxy(cfgStore, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to create RouterProxy: %v", err)
	}
	rp.TokenSource = &mockTokenSource{}
	rp.Target = backendURL
	rp.Proxy.Transport = &mockRoundTripper{Target: backendURL}

	t.Run("OpenAI Non-Streaming Chat Completions with Dynamic Routing", func(t *testing.T) {
		simulateToolResponse = false
		simulateStreamResponse = false
		openAIPayload := `{
			"model": "gemini-dynamic",
			"messages": [
				{"role": "system", "content": "You are a concise assistant."},
				{"role": "user", "content": "Hi"}
			],
			"temperature": 0.3,
			"max_tokens": 128
		}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(openAIPayload))
		req.Header.Set("Authorization", "Bearer key-openai-test")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
		}
		if gotModel := rr.Header().Get("X-Routed-Model"); gotModel != "gemini-3.5-flash-lite" {
			t.Errorf("expected X-Routed-Model=gemini-3.5-flash-lite for simple greeting, got %q", gotModel)
		}
		if !strings.Contains(lastPath, "/models/gemini-3.5-flash-lite:generateContent") {
			t.Errorf("expected upstream path to target gemini-3.5-flash-lite:generateContent, got %q", lastPath)
		}
		if !strings.Contains(string(lastReqBody), "You are a concise assistant.") || !strings.Contains(string(lastReqBody), `"Hi"`) {
			t.Errorf("expected translated Gemini payload to include systemInstruction and user content, got: %s", string(lastReqBody))
		}

		var chatResp struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &chatResp); err != nil {
			t.Fatalf("failed to unmarshal OpenAI chat completion response: %v (body: %s)", err, rr.Body.String())
		}
		if chatResp.Object != "chat.completion" {
			t.Errorf("expected object='chat.completion', got %q", chatResp.Object)
		}
		if chatResp.Model != "gemini-3.5-flash-lite" {
			t.Errorf("expected model='gemini-3.5-flash-lite', got %q", chatResp.Model)
		}
		if len(chatResp.Choices) != 1 || chatResp.Choices[0].Message.Content != "Hello from Gemini Smart Router via OpenAI Spec!" {
			t.Errorf("unexpected choices in OpenAI response: %+v", chatResp.Choices)
		}
		if chatResp.Usage.TotalTokens != 19 {
			t.Errorf("expected total_tokens=19, got %d", chatResp.Usage.TotalTokens)
		}
	})

	t.Run("OpenAI v1beta Path and Function Tool Calling", func(t *testing.T) {
		simulateToolResponse = true
		simulateStreamResponse = false
		openAIToolPayload := `{
			"model": "gemini-dynamic",
			"messages": [
				{"role": "user", "content": "What is the weather in San Francisco?"}
			],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "get_weather",
						"description": "Get current weather for a location",
						"parameters": {
							"type": "object",
							"properties": {
								"location": {"type": "string"}
							},
							"required": ["location"]
						}
					}
				}
			]
		}`
		req := httptest.NewRequest("POST", "/v1beta/openai/chat/completions", strings.NewReader(openAIToolPayload))
		req.Header.Set("Authorization", "Bearer key-openai-test")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
		}
		// ForceComplexTools is true on app-openai, so presence of OpenAI tools must route to gemini-3.1-pro
		if gotModel := rr.Header().Get("X-Routed-Model"); gotModel != "gemini-3.1-pro" {
			t.Errorf("expected tool request to route to complex model gemini-3.1-pro, got %q", gotModel)
		}

		var toolResp struct {
			Choices []struct {
				Message struct {
					Role      string `json:"role"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &toolResp); err != nil {
			t.Fatalf("failed to unmarshal OpenAI tool call response: %v", err)
		}
		if len(toolResp.Choices) != 1 || len(toolResp.Choices[0].Message.ToolCalls) != 1 {
			t.Fatalf("expected 1 tool call in OpenAI response, got: %s", rr.Body.String())
		}
		if toolResp.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
			t.Errorf("expected tool call function name 'get_weather', got %q", toolResp.Choices[0].Message.ToolCalls[0].Function.Name)
		}
		if toolResp.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("expected finish_reason='tool_calls', got %q", toolResp.Choices[0].FinishReason)
		}
	})

	t.Run("OpenAI Streaming Chat Completions SSE", func(t *testing.T) {
		simulateToolResponse = false
		simulateStreamResponse = true
		openAIStreamPayload := `{
			"model": "gemini-3.8-flash",
			"messages": [{"role": "user", "content": "Stream hello"}],
			"stream": true
		}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(openAIStreamPayload))
		req.Header.Set("Authorization", "Bearer key-openai-test")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for stream, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(lastPath, ":streamGenerateContent") || !strings.Contains(lastQuery, "alt=sse") {
			t.Errorf("expected upstream streamGenerateContent?alt=sse, got path=%q query=%q", lastPath, lastQuery)
		}
		bodyStr := rr.Body.String()
		if !strings.Contains(bodyStr, `"object":"chat.completion.chunk"`) {
			t.Errorf("expected SSE stream to contain chat.completion.chunk, got: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, "from OpenAI stream!") {
			t.Errorf("expected SSE stream to contain streamed text, got: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, "data: [DONE]") {
			t.Errorf("expected SSE stream to terminate with data: [DONE], got: %s", bodyStr)
		}
	})

	t.Run("OpenAI Models List and Retrieve", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer key-openai-test")
		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on GET /v1/models, got %d: %s", rr.Code, rr.Body.String())
		}
		var listResp struct {
			Object string `json:"object"`
			Data   []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
			t.Fatalf("failed to unmarshal /v1/models response: %v", err)
		}
		if listResp.Object != "list" || len(listResp.Data) == 0 {
			t.Errorf("expected object='list' and non-empty data, got: %s", rr.Body.String())
		}
		foundDynamic := false
		for _, m := range listResp.Data {
			if m.ID == "gemini-dynamic" {
				foundDynamic = true
				break
			}
		}
		if !foundDynamic {
			t.Errorf("expected GET /v1/models to include virtual model 'gemini-dynamic'")
		}

		// Also test GET /v1/models/gemini-3.8-flash single model retrieve
		reqSingle := httptest.NewRequest("GET", "/v1/models/gemini-3.8-flash", nil)
		reqSingle.Header.Set("Authorization", "Bearer key-openai-test")
		rrSingle := httptest.NewRecorder()
		rp.ServeHTTP(rrSingle, reqSingle)
		if rrSingle.Code != http.StatusOK || !strings.Contains(rrSingle.Body.String(), `"id":"gemini-3.8-flash"`) {
			t.Errorf("expected GET /v1/models/gemini-3.8-flash to return model object, got %d: %s", rrSingle.Code, rrSingle.Body.String())
		}
	})

	t.Run("OpenAI Gzip Compressed Upstream Response Handling", func(t *testing.T) {
		simulateToolResponse = false
		simulateStreamResponse = false
		simulateGzipResponse = true
		defer func() { simulateGzipResponse = false }()

		openAIReq := `{
			"model": "gemini-dynamic",
			"messages": [{"role": "user", "content": "Hi"}]
		}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(openAIReq))
		req.Header.Set("Authorization", "Bearer key-openai-test")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Content-Encoding") != "" {
			t.Errorf("expected Content-Encoding header to be stripped after translating gzipped upstream response, got %q", rr.Header().Get("Content-Encoding"))
		}
		var oaiResp struct {
			Object  string `json:"object"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &oaiResp); err != nil {
			t.Fatalf("failed to unmarshal OpenAI response from gzipped upstream: %v (raw: %s)", err, rr.Body.String())
		}
		if oaiResp.Object != "chat.completion" || len(oaiResp.Choices) == 0 || oaiResp.Choices[0].Message.Content != "Hello from Gzipped Vertex AI!" {
			t.Errorf("expected translated OpenAI chat.completion from gzipped upstream, got: %s", rr.Body.String())
		}
	})
}

func TestNamedLocalClusterRoutingAndRunnerCapacityE2E(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// 1. Configure a named LocalCluster ("cluster-m4-pro") in Providers["local"]
	localProv := config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "On-Premises Local Cluster",
		Enabled:     true,
		Regions: map[string]config.RegionConfig{
			"local-cluster": {
				Code:   "local-cluster",
				Active: true,
				LocalConfig: &config.LocalConfig{
					Clusters: []config.LocalCluster{
						{
							ID:          "cluster-m4-pro",
							Name:        "M4 Pro Studio Cluster",
							QueueName:   "m4-pro-queue",
							Description: "Dedicated Apple Silicon cluster for Gemma 4 26B",
							MaxQueueAge: 90,
						},
					},
				},
			},
		},
	}
	if err := cfgStore.SaveProvider(ctx, localProv); err != nil {
		t.Fatalf("failed to save local provider: %v", err)
	}

	// 2. Register and update a runner assigned to "cluster-m4-pro" with custom capacity limits
	if err := cfgStore.SaveRegisteredRunner(ctx, config.RunnerResponse{
		Node: config.Node{
			ID:                "node-m4-studio-1",
			Name:              "M4 Max Studio Node",
			Status:            "online",
			MemoryAllocatedGB: 64,
			ComputeGPUCores:   40,
			SupportedModels:   []string{"gemma-4-26b"},
			MaxModelSizeGB:    32,
			MaxConcurrent:     4,
		},
		AssignedClusters: []string{"cluster-m4-pro"},
	}); err != nil {
		t.Fatalf("failed to save registered runner: %v", err)
	}

	// 3. Seed Client, App, API Key, and a Routing Rule targeting location "cluster-m4-pro"
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "client-m4", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-m4", ClientID: "client-m4", Priority: "high", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-m4-cluster"), AppID: "app-m4", ClientID: "client-m4", Status: "active"})
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	_ = cfgStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-m4-gemma4",
		ModelPattern:   "gemma-4-26b",
		ClientTier:     "all",
		TargetModel:    "gemma-4-26b",
		TargetLocation: "cluster-m4-pro",
		PriorityWeight: 10,
	})

	q := NewClusterQueue(10, 5*time.Second)
	rp, err := NewRouterProxy(cfgStore, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to initialize proxy: %v", err)
	}
	rp.Queue = q

	// 4. Send request through RouterProxy.ServeHTTP
	req := httptest.NewRequest("POST", "/v1/models/gemma-4-26b:generateContent?key=key-m4-cluster", strings.NewReader(`{"contents":[{"parts":[{"text":"Explain unified memory architecture"}]}]}`))
	req.Header.Set("x-goog-api-key", "key-m4-cluster")
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		rp.ServeHTTP(rr, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	// 5. Verify a runner bound to a DIFFERENT cluster ("cluster-other") does not pick up the job
	if jobOther, ok := q.Poll([]string{"gemma-4-26b"}, []string{"cluster-other"}); ok || jobOther != nil {
		t.Fatalf("expected runner bound to cluster-other NOT to receive job for cluster-m4-pro")
	}

	// 6. Verify runner bound to "cluster-m4-pro" polls and resolves the job
	job, ok := q.Poll([]string{"gemma-4-26b"}, []string{"cluster-m4-pro"})
	if !ok || job == nil {
		t.Fatalf("expected runner bound to cluster-m4-pro to poll job, but got none (rr.Code=%d, body=%s)", rr.Code, rr.Body.String())
	}
	if job.ClusterID != "cluster-m4-pro" {
		t.Errorf("expected job.ClusterID='cluster-m4-pro', got %q", job.ClusterID)
	}

	q.Resolve(job.ID, config.QueueResult{
		Payload:    []byte(`{"candidates":[{"content":{"parts":[{"text":"Unified memory shares high-bandwidth DRAM across CPU, GPU, and NPU."}]}}]}`),
		StatusCode: http.StatusOK,
	})

	<-done
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from proxy ServeHTTP, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Routed-Model-Location") != "cluster-m4-pro" {
		t.Errorf("expected X-Routed-Model-Location='cluster-m4-pro', got %q", rr.Header().Get("X-Routed-Model-Location"))
	}
	if !strings.Contains(rr.Body.String(), "Unified memory shares high-bandwidth DRAM") {
		t.Errorf("expected response body from cluster runner, got: %s", rr.Body.String())
	}
}

func TestClusterQueuePollForNodeConcurrencyEnforcement(t *testing.T) {
	q := NewClusterQueue(20, 10*time.Second)
	defer q.Close()

	for i := 1; i <= 3; i++ {
		_, err := q.Enqueue(&config.QueueJob{
			ID:        fmt.Sprintf("job-%d", i),
			ClusterID: "cluster-a",
			Model:     "gemma-4-4b",
			Priority:  "high",
			CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("failed to enqueue job-%d: %v", i, err)
		}
	}

	// Node 1 has MaxConcurrent=2 -> can pop 2 jobs, 3rd poll must return false
	j1, ok1 := q.PollForNode("node-1", []string{"gemma-4-4b"}, []string{"cluster-a"}, 2)
	if !ok1 || j1 == nil || j1.AssignedNodeID != "node-1" {
		t.Fatalf("expected node-1 to pop 1st job, got %v (ok=%v)", j1, ok1)
	}
	j2, ok2 := q.PollForNode("node-1", []string{"gemma-4-4b"}, []string{"cluster-a"}, 2)
	if !ok2 || j2 == nil || j2.AssignedNodeID != "node-1" {
		t.Fatalf("expected node-1 to pop 2nd job, got %v (ok=%v)", j2, ok2)
	}
	if j3, ok3 := q.PollForNode("node-1", []string{"gemma-4-4b"}, []string{"cluster-a"}, 2); ok3 || j3 != nil {
		t.Fatalf("expected node-1 3rd poll to be blocked by MaxConcurrent=2, got %v", j3)
	}

	// Node 2 (different node) can still pop the 3rd job!
	jNode2, okNode2 := q.PollForNode("node-2", []string{"gemma-4-4b"}, []string{"cluster-a"}, 1)
	if !okNode2 || jNode2 == nil || jNode2.ID != "job-3" {
		t.Fatalf("expected node-2 to pop job-3, got %v (ok=%v)", jNode2, okNode2)
	}

	// Resolving j1 frees a slot on node-1
	q.Resolve(j1.ID, config.QueueResult{StatusCode: http.StatusOK})
	_, _ = q.Enqueue(&config.QueueJob{
		ID:        "job-4",
		ClusterID: "cluster-a",
		Model:     "gemma-4-4b",
		Priority:  "high",
		CreatedAt: time.Now(),
	})
	j4, ok4 := q.PollForNode("node-1", []string{"gemma-4-4b"}, []string{"cluster-a"}, 2)
	if !ok4 || j4 == nil || j4.ID != "job-4" {
		t.Fatalf("expected node-1 to pop job-4 after resolving j1, got %v (ok=%v)", j4, ok4)
	}
}

func TestClusterQueueTierOrderingPerJobTTLAndSnapshot(t *testing.T) {
	q := NewClusterQueue(20, 10*time.Second)
	defer q.Close()

	now := time.Now()
	// Enqueue two high-priority jobs: free tier first, enterprise tier second
	_, _ = q.Enqueue(&config.QueueJob{
		ID:        "job-free",
		ClusterID: "cluster-a",
		AppID:     "app-free",
		Model:     "gemma-4-4b",
		Priority:  "high",
		Tier:      "free",
		CreatedAt: now,
	})
	_, _ = q.Enqueue(&config.QueueJob{
		ID:        "job-ent",
		ClusterID: "cluster-a",
		AppID:     "app-ent",
		Model:     "gemma-4-4b",
		Priority:  "high",
		Tier:      "enterprise",
		CreatedAt: now.Add(10 * time.Millisecond),
	})

	// Enterprise tier job within the same priority bucket must be popped before Free tier job
	first, ok := q.PollForNode("node-ent", []string{"gemma-4-4b"}, []string{"cluster-a"}, 2)
	if !ok || first == nil || first.ID != "job-ent" {
		t.Fatalf("expected enterprise tier job-ent to be popped before free tier job-free, got %v", first)
	}

	// Verify GetQueueSnapshot returns both in-flight ("processing") and queued ("queued") jobs
	snap := q.GetQueueSnapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 items in GetQueueSnapshot (1 processing, 1 queued), got %d", len(snap))
	}
	var sawProcessing, sawQueued bool
	for _, item := range snap {
		if item.QueueSource != "cluster" || item.ClusterID != "cluster-a" {
			t.Errorf("expected QueueSource='cluster' and ClusterID='cluster-a', got %+v", item)
		}
		if item.Status == "processing" && item.AssignedNodeID == "node-ent" && item.AppID == "app-ent" {
			sawProcessing = true
		}
		if item.Status == "queued" && item.AppID == "app-free" {
			sawQueued = true
		}
	}
	if !sawProcessing || !sawQueued {
		t.Fatalf("expected both processing and queued snapshot items, got %+v", snap)
	}

	// Test per-job MaxAge TTL expiration
	respCh, _ := q.Enqueue(&config.QueueJob{
		ID:        "job-short-ttl",
		ClusterID: "cluster-a",
		Model:     "gemma-4-4b",
		Priority:  "medium",
		MaxAge:    20 * time.Millisecond,
		CreatedAt: time.Now().Add(-50 * time.Millisecond),
	})
	q.cleanupExpiredJobs()
	select {
	case res := <-respCh:
		if res.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("expected 504 Gateway Timeout on per-job MaxAge expiry, got %d", res.StatusCode)
		}
	default:
		t.Fatalf("expected expired job-short-ttl to receive timeout result on ResponseChan")
	}
}

func TestRouterProxyClusterFallbackDirectEndpointAndOpenAIFormat(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	// Spin up a real local HTTP server acting as a DirectEndpoint (e.g., Ollama / vLLM / backup endpoint)
	directBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-direct","choices":[{"index":0,"message":{"role":"assistant","content":"Direct endpoint handled inference"}}],"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13}}`))
	}))
	defer directBackend.Close()

	// Configure 2 clusters:
	// 1) "cluster-strict" with NO FallbackModel (must fail with 504 when queue times out)
	// 2) "cluster-with-fallback" with FallbackModel="gemma-4-26b" and FallbackLocation="direct-vllm-ep"
	// Plus DirectEndpoint "direct-vllm-ep" pointing to directBackend.URL
	localProv := config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local Clusters",
		Enabled:     true,
		Regions: map[string]config.RegionConfig{
			"local-cluster": {
				Code:   "local-cluster",
				Active: true,
				LocalConfig: &config.LocalConfig{
					Clusters: []config.LocalCluster{
						{
							ID:          "cluster-strict",
							Name:        "Strict Local Cluster",
							QueueName:   "strict-q",
							MaxQueueAge: 50 * time.Millisecond,
						},
						{
							ID:               "cluster-with-fallback",
							Name:             "Cluster With User Fallback",
							QueueName:        "fallback-q",
							MaxQueueAge:      50 * time.Millisecond,
							FallbackModel:    "gemma-4-26b",
							FallbackLocation: "direct-vllm-ep",
						},
					},
					DirectEndpoints: []config.DirectEndpoint{
						{
							ID:        "direct-vllm-ep",
							Name:      "Direct vLLM Node",
							URL:       directBackend.URL,
							ModelName: "gemma-4-26b",
							Engine:    "vllm",
							Active:    true,
							ClusterID: "direct-vllm-ep",
						},
					},
				},
			},
		},
	}
	_ = cfgStore.SaveProvider(ctx, localProv)
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "cl-1", Tier: "enterprise"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-1", ClientID: "cl-1", Priority: "high", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-local-test"), AppID: "app-1", ClientID: "cl-1", Status: "active"})
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")

	// Rule 1: gemma-4-4b -> cluster-strict (no fallback)
	_ = cfgStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-strict",
		ModelPattern:   "gemma-4-4b",
		ClientTier:     "all",
		TargetModel:    "gemma-4-4b",
		TargetLocation: "cluster-strict",
		PriorityWeight: 10,
	})
	// Rule 2: gemma-4-31b -> cluster-with-fallback (falls back to gemma-4-26b @ direct-vllm-ep)
	_ = cfgStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-fallback",
		ModelPattern:   "gemma-4-31b",
		ClientTier:     "all",
		TargetModel:    "gemma-4-31b",
		TargetLocation: "cluster-with-fallback",
		PriorityWeight: 10,
	})

	q := NewClusterQueue(10, 50*time.Millisecond)
	defer q.Close()
	rp, err := NewRouterProxy(cfgStore, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to init proxy: %v", err)
	}
	rp.Queue = q

	// Case A: Strict cluster with no runners and no FallbackModel -> must return 504 Gateway Timeout
	reqStrict := httptest.NewRequest("POST", "/v1/models/gemma-4-4b:generateContent?key=key-local-test", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rrStrict := httptest.NewRecorder()
	rp.ServeHTTP(rrStrict, reqStrict)
	if rrStrict.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 Gateway Timeout for strict cluster with no fallback, got %d: %s", rrStrict.Code, rrStrict.Body.String())
	}

	// Case B: Cluster with user-specified FallbackModel="gemma-4-26b" and FallbackLocation="direct-vllm-ep" -> falls back and succeeds!
	reqFallback := httptest.NewRequest("POST", "/v1/models/gemma-4-31b:generateContent?key=key-local-test", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rrFallback := httptest.NewRecorder()
	rp.ServeHTTP(rrFallback, reqFallback)
	if rrFallback.Code != http.StatusOK {
		t.Fatalf("expected 200 OK via user-specified cluster fallback to direct endpoint, got %d: %s", rrFallback.Code, rrFallback.Body.String())
	}
	if rrFallback.Header().Get("X-Cluster-Fallback") != "true" {
		t.Errorf("expected X-Cluster-Fallback='true' header, got %q", rrFallback.Header().Get("X-Cluster-Fallback"))
	}
	if !strings.Contains(rrFallback.Body.String(), "Direct endpoint handled inference") {
		t.Errorf("expected fallback response from direct endpoint, got %s", rrFallback.Body.String())
	}

	// Case C: OpenAI /v1/chat/completions routed to local cluster queue translates Gemini runner payload back to OpenAI format
	qOpenAI := NewClusterQueue(10, 2*time.Second)
	defer qOpenAI.Close()
	rp.Queue = qOpenAI
	reqOAI := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemma-4-4b","messages":[{"role":"user","content":"Hello OpenAI format"}]}`))
	reqOAI.Header.Set("Authorization", "Bearer key-local-test")
	rrOAI := httptest.NewRecorder()
	doneOAI := make(chan struct{})
	go func() {
		rp.ServeHTTP(rrOAI, reqOAI)
		close(doneOAI)
	}()
	time.Sleep(40 * time.Millisecond)
	jobOAI, okOAI := qOpenAI.Poll([]string{"gemma-4-4b"}, []string{"cluster-strict"})
	if !okOAI || jobOAI == nil {
		t.Fatalf("expected OpenAI request to be enqueued on cluster-strict")
	}
	qOpenAI.Resolve(jobOAI.ID, config.QueueResult{
		StatusCode: http.StatusOK,
		Payload:    []byte(`{"candidates":[{"content":{"parts":[{"text":"Hello from Gemma 4 runner"}]}}]}`),
	})
	<-doneOAI
	if rrOAI.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for OpenAI chat completion via cluster queue, got %d: %s", rrOAI.Code, rrOAI.Body.String())
	}
	if !strings.Contains(rrOAI.Body.String(), `"object":"chat.completion"`) || !strings.Contains(rrOAI.Body.String(), "Hello from Gemma 4 runner") {
		t.Errorf("expected OpenAI chat.completion JSON response, got: %s", rrOAI.Body.String())
	}
}

func TestTDD_StructuredLogEmittedOnAllProxyPaths(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", tempDir)

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	directBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-direct","choices":[{"index":0,"message":{"role":"assistant","content":"Direct response"}}],"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13}}`))
	}))
	defer directBackend.Close()

	localProv := config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local Clusters",
		Enabled:     true,
		Regions: map[string]config.RegionConfig{
			"local-cluster": {
				Code:   "local-cluster",
				Active: true,
				LocalConfig: &config.LocalConfig{
					Clusters: []config.LocalCluster{
						{
							ID:          "cluster-strict-log",
							Name:        "Strict Cluster Log",
							QueueName:   "strict-log-q",
							MaxQueueAge: 50 * time.Millisecond,
						},
						{
							ID:               "cluster-fb-log",
							Name:             "Fallback Cluster Log",
							QueueName:        "fb-log-q",
							MaxQueueAge:      50 * time.Millisecond,
							FallbackModel:    "gemma-4-26b",
							FallbackLocation: "ep-direct-log",
						},
					},
					DirectEndpoints: []config.DirectEndpoint{
						{
							ID:        "ep-direct-log",
							Name:      "Direct Endpoint Log",
							URL:       directBackend.URL,
							ModelName: "gemma-4-26b",
							Engine:    "vllm",
							Active:    true,
							ClusterID: "ep-direct-log",
						},
					},
				},
			},
		},
	}
	_ = dbStore.SaveProvider(ctx, localProv)
	_ = dbStore.SaveClient(ctx, config.Client{ID: "client-log", Tier: "enterprise"})
	_ = dbStore.SaveApp(ctx, config.App{ID: "app-log", ClientID: "client-log", Priority: "high", RPM: 100, TPM: 100000})
	_ = dbStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey("key-log-test"), AppID: "app-log", ClientID: "client-log", Status: "active"})
	_ = dbStore.DeleteRule(ctx, "rule-1")
	_ = dbStore.DeleteHeader(ctx, "header-1")

	_ = dbStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-direct-log",
		ModelPattern:   "gemma-4-26b",
		ClientTier:     "all",
		TargetModel:    "gemma-4-26b",
		TargetLocation: "ep-direct-log",
		PriorityWeight: 10,
	})
	_ = dbStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-strict-log",
		ModelPattern:   "gemma-4-4b",
		ClientTier:     "all",
		TargetModel:    "gemma-4-4b",
		TargetLocation: "cluster-strict-log",
		PriorityWeight: 10,
	})
	_ = dbStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-fb-log",
		ModelPattern:   "gemma-4-31b",
		ClientTier:     "all",
		TargetModel:    "gemma-4-31b",
		TargetLocation: "cluster-fb-log",
		PriorityWeight: 10,
	})

	q := NewClusterQueue(10, 50*time.Millisecond)
	defer q.Close()
	rp, err := NewRouterProxy(dbStore, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to init proxy: %v", err)
	}
	rp.Queue = q

	readLogEntries := func(t *testing.T) []StructuredLog {
		t.Helper()
		logPath := tempDir + "/local_logs.jsonl"
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("expected %s to exist and be readable, got err: %v", logPath, err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var out []StructuredLog
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var sl StructuredLog
			if err := json.Unmarshal([]byte(line), &sl); err != nil {
				t.Fatalf("failed to unmarshal StructuredLog line %q: %v", line, err)
			}
			out = append(out, sl)
		}
		return out
	}

	// 1. Direct endpoint request -> must emit StructuredLog with status 200 and non-zero bytes
	req1 := httptest.NewRequest("POST", "/v1/models/gemma-4-26b:generateContent?key=key-log-test", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rr1 := httptest.NewRecorder()
	rp.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("step 1 expected 200, got %d", rr1.Code)
	}
	logs := readLogEntries(t)
	if len(logs) != 1 {
		t.Fatalf("expected 1 StructuredLog entry after direct endpoint call, got %d", len(logs))
	}
	if logs[0].Status != http.StatusOK || logs[0].ModelOut != "gemma-4-26b" || logs[0].Client != "client-log" || logs[0].App != "app-log" || logs[0].BytesSent <= 0 {
		t.Errorf("unexpected StructuredLog for direct endpoint: %+v", logs[0])
	}

	// 2. Cluster queue strict timeout (504) -> must emit StructuredLog with status 504
	req2 := httptest.NewRequest("POST", "/v1/models/gemma-4-4b:generateContent?key=key-log-test", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rr2 := httptest.NewRecorder()
	rp.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusGatewayTimeout {
		t.Fatalf("step 2 expected 504, got %d", rr2.Code)
	}
	logs = readLogEntries(t)
	if len(logs) != 2 {
		t.Fatalf("expected 2 StructuredLog entries after cluster timeout, got %d", len(logs))
	}
	if logs[1].Status != http.StatusGatewayTimeout || logs[1].ModelOut != "gemma-4-4b" {
		t.Errorf("unexpected StructuredLog for cluster timeout: %+v", logs[1])
	}

	// 3. Cluster queue fallback to direct endpoint -> must emit StructuredLog with status 200 and fallback model
	req3 := httptest.NewRequest("POST", "/v1/models/gemma-4-31b:generateContent?key=key-log-test", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rr3 := httptest.NewRecorder()
	rp.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("step 3 expected 200, got %d", rr3.Code)
	}
	logs = readLogEntries(t)
	if len(logs) != 3 {
		t.Fatalf("expected 3 StructuredLog entries after cluster fallback, got %d", len(logs))
	}
	if logs[2].Status != http.StatusOK || logs[2].ModelOut != "gemma-4-26b" || logs[2].BytesSent <= 0 {
		t.Errorf("unexpected StructuredLog for cluster fallback: %+v", logs[2])
	}

	// 4. Cluster queue success -> must emit StructuredLog with status 200 and bytesWritten > 0
	qSuccess := NewClusterQueue(10, 2*time.Second)
	defer qSuccess.Close()
	rp.Queue = qSuccess
	req4 := httptest.NewRequest("POST", "/v1/models/gemma-4-4b:generateContent?key=key-log-test", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rr4 := httptest.NewRecorder()
	done4 := make(chan struct{})
	go func() {
		rp.ServeHTTP(rr4, req4)
		close(done4)
	}()
	time.Sleep(30 * time.Millisecond)
	job4, ok4 := qSuccess.Poll([]string{"gemma-4-4b"}, []string{"cluster-strict-log"})
	if !ok4 || job4 == nil {
		t.Fatalf("expected cluster queue job to be polled")
	}
	qSuccess.Resolve(job4.ID, config.QueueResult{
		StatusCode: http.StatusOK,
		Payload:    []byte(`{"candidates":[{"content":{"parts":[{"text":"Cluster OK"}]}}]}`),
	})
	<-done4
	logs = readLogEntries(t)
	if len(logs) != 4 {
		t.Fatalf("expected 4 StructuredLog entries after cluster success, got %d", len(logs))
	}
	if logs[3].Status != http.StatusOK || logs[3].ModelOut != "gemma-4-4b" || logs[3].BytesSent <= 0 {
		t.Errorf("unexpected StructuredLog for cluster success: %+v", logs[3])
	}

	// 5. Early auth failure (401) -> must emit StructuredLog with status 401
	req5 := httptest.NewRequest("POST", "/v1/models/gemini-2.5-flash:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	rr5 := httptest.NewRecorder()
	rp.ServeHTTP(rr5, req5)
	if rr5.Code != http.StatusUnauthorized {
		t.Fatalf("step 5 expected 401, got %d", rr5.Code)
	}
	logs = readLogEntries(t)
	if len(logs) != 5 {
		t.Fatalf("expected 5 StructuredLog entries after 401 auth error, got %d", len(logs))
	}
	if logs[4].Status != http.StatusUnauthorized {
		t.Errorf("unexpected StructuredLog for 401 error: %+v", logs[4])
	}

	// 6. GET /v1/models metadata discovery -> must NOT emit StructuredLog
	req6 := httptest.NewRequest("GET", "/v1/models?key=key-log-test", nil)
	rr6 := httptest.NewRecorder()
	rp.ServeHTTP(rr6, req6)
	if rr6.Code != http.StatusOK {
		t.Fatalf("step 6 expected 200, got %d", rr6.Code)
	}
	logs = readLogEntries(t)
	if len(logs) != 5 {
		t.Fatalf("expected GET /v1/models not to emit StructuredLog, still 5 entries, got %d", len(logs))
	}
}




