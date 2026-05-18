package proxy

import (
	"context"
	"net/http/httptest"
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
