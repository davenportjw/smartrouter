package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"geminirouter/pkg/config"
	"geminirouter/pkg/limiter"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// RouterProxy wraps the httputil.ReverseProxy to add custom routing and logging.
type RouterProxy struct {
	Target      *url.URL
	Proxy       *httputil.ReverseProxy
	Store       *config.ConfigStore
	Limiter     *limiter.RateLimiterRegistry
	TokenSource oauth2.TokenSource
	ProjectID   string
	Location    string
}

// responseWriterWrapper intercepts writes to capture status code and response body size.
type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode    int
	bytesWritten  int64
	headerWritten bool
}

func newResponseWriterWrapper(w http.ResponseWriter) *responseWriterWrapper {
	return &responseWriterWrapper{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}
}

func (rw *responseWriterWrapper) WriteHeader(code int) {
	if rw.headerWritten {
		return
	}
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
	rw.headerWritten = true
}

func (rw *responseWriterWrapper) Write(b []byte) (int, error) {
	if !rw.headerWritten {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += int64(n)
	return n, err
}

// Flush supports standard streaming flush operations if the underlying ResponseWriter supports it.
func (rw *responseWriterWrapper) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// StructuredLog defines the JSON format for our server metrics.
type StructuredLog struct {
	Severity     string `json:"severity"`
	Time         string `json:"time"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Client       string `json:"client_id,omitempty"`
	Tier         string `json:"tier,omitempty"`
	ModelIn      string `json:"model_requested,omitempty"`
	ModelOut     string `json:"model_routed,omitempty"`
	Status       int    `json:"status"`
	LatencyMs    int64  `json:"latency_ms"`
	BytesSent    int64  `json:"bytes_sent"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// NewRouterProxy creates a new reverse proxy pointing to the upstream Gemini API.
func NewRouterProxy(store *config.ConfigStore, projectID, location string) (*RouterProxy, error) {
	targetHost := location + "-aiplatform.googleapis.com"
	target, err := url.Parse("https://" + targetHost)
	if err != nil {
		return nil, err
	}

	ts, err := google.DefaultTokenSource(context.Background(), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("failed to get default token source: %w", err)
	}

	rp := &RouterProxy{
		Target:      target,
		Store:       store,
		Limiter:     limiter.NewRateLimiterRegistry(),
		TokenSource: ts,
		ProjectID:   projectID,
		Location:    location,
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host

		// 1. Extract and Validate API Key
		clientKey := extractAPIKey(req)
		keyData, ok := rp.Store.LookupKey(clientKey)
		if !ok {
			// In director, we can't easily abort request. But we can flag it
			req.Header.Set("X-Router-Error", "Unauthorized")
			return
		}

		client, ok := rp.Store.LookupClient(keyData.ClientID)
		if !ok {
			req.Header.Set("X-Router-Error", "ClientNotFound")
			return
		}

		// Save context variables inside request headers so we can access them in logs or downstream
		req.Header.Set("X-Client-ID", client.ID)
		req.Header.Set("X-Client-Tier", client.Tier)
		req.Header.Set("X-Client-Priority", client.Priority)
		req.Header.Set("X-Client-RPM", string(rune(client.RPM)))
		req.Header.Set("X-Client-TPM", string(rune(client.TPM)))

		// 2. Parse path parts to identify API version and resource type
		pathParts := strings.Split(req.URL.Path, "/")
		if len(pathParts) < 3 {
			return
		}
		version := pathParts[1]  // e.g. "v1", "v1beta", "v1beta1"
		resource := pathParts[2] // e.g. "models", "reasoningEngines", "ragCorpora"

		if resource == "models" {
			// Format: /v1beta/models/gemini-1.5-pro:generateContent
			parts := strings.Split(req.URL.Path, "/models/")
			if len(parts) < 2 {
				return
			}
			modelAndAction := parts[1]
			actionParts := strings.Split(modelAndAction, ":")
			requestedModel := actionParts[0]

			req.Header.Set("X-Requested-Model", requestedModel)

			// Apply dynamic routing rules
			targetModel := requestedModel
			routedRule, matched := rp.Store.MatchRule(requestedModel, client.Tier)
			if matched {
				targetModel = routedRule.TargetModel
			}

			req.Header.Set("X-Routed-Model", targetModel)

			// Remove client-side credentials
			query := req.URL.Query()
			query.Del("key")
			req.URL.RawQuery = query.Encode()
			req.Header.Del("x-goog-api-key")

			// Fetch OAuth2 token for upstream
			token, err := rp.TokenSource.Token()
			if err != nil {
				log.Printf("[Proxy] Error retrieving Google Cloud token: %v", err)
				req.Header.Set("X-Router-Error", "TokenError")
				return
			}
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)

			var action string
			if len(actionParts) > 1 {
				action = ":" + actionParts[1]
			}

			newPath := fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models/%s%s",
				rp.ProjectID, rp.Location, targetModel, action)
			req.URL.Path = newPath

		} else if resource == "reasoningEngines" || resource == "ragCorpora" {
			// Remove client-side credentials
			query := req.URL.Query()
			query.Del("key")
			req.URL.RawQuery = query.Encode()
			req.Header.Del("x-goog-api-key")

			// Fetch OAuth2 token for upstream
			token, err := rp.TokenSource.Token()
			if err != nil {
				log.Printf("[Proxy] Error retrieving Google Cloud token: %v", err)
				req.Header.Set("X-Router-Error", "TokenError")
				return
			}
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)

			// Reconstruct path with GCP project and location
			remainingPath := strings.Join(pathParts[2:], "/")
			newPath := fmt.Sprintf("/%s/projects/%s/locations/%s/%s",
				version, rp.ProjectID, rp.Location, remainingPath)
			req.URL.Path = newPath
		}
	}

	rp.Proxy = proxy
	return rp, nil
}

// extractAPIKey checks header, query param, and bearer formats for the router API key.
func extractAPIKey(req *http.Request) string {
	// 1. Query Param ?key=
	if k := req.URL.Query().Get("key"); k != "" {
		return k
	}
	// 2. Header x-goog-api-key:
	if k := req.Header.Get("x-goog-api-key"); k != "" {
		return k
	}
	// 3. Header Authorization: Bearer
	if auth := req.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// ServeHTTP routes the request and captures structured logs.
func (rp *RouterProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	wrapped := newResponseWriterWrapper(w)

	// Extract user custom key first to check auth before proxy director executes
	clientKey := extractAPIKey(r)
	if clientKey == "" {
		http.Error(w, `{"error": {"message": "API key required. Use x-goog-api-key header or ?key query parameter."}}`, http.StatusUnauthorized)
		return
	}

	keyData, ok := rp.Store.LookupKey(clientKey)
	if !ok {
		http.Error(w, `{"error": {"message": "Invalid or inactive router API key."}}`, http.StatusUnauthorized)
		return
	}

	// Resolve the client to verify active rate limits
	client, ok := rp.Store.LookupClient(keyData.ClientID)
	if !ok {
		http.Error(w, `{"error": {"message": "Client configuration missing."}}`, http.StatusInternalServerError)
		return
	}

	// Sync local rate limiter with current client configurations from cache
	// (Runs in-memory O(1) update inside registry)
	rp.Limiter.UpdateLimiter(client.ID, client.RPM, client.TPM, client.Priority)

	// Evaluate rate limits (checking request count, estimating 0 tokens requested pre-request)
	allowed, delay := rp.Limiter.EvaluateLimit(r.Context(), client.ID, 1)
	if !allowed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{
			"error": {
				"code": 429,
				"message": "Resource has been exhausted (e.g. queries per minute) on the smart router.",
				"status": "RESOURCE_EXHAUSTED"
			}
		}`))
		return
	}

	// Apply queueing delay if a wait-time was assigned for priority
	if delay > 0 {
		log.Printf("[Proxy Queue] Delaying request for client %s by %v due to priority queueing", client.ID, delay)
		time.Sleep(delay)
	}

	// Execute standard reverse proxy
	rp.Proxy.ServeHTTP(wrapped, r)

	// Access internal headers we populated during Director
	routerError := r.Header.Get("X-Router-Error")
	if routerError == "ClientNotFound" {
		http.Error(w, `{"error": {"message": "Client configuration missing."}}`, http.StatusInternalServerError)
		return
	}
	if routerError == "TokenError" {
		http.Error(w, `{"error": {"message": "Failed to retrieve Google Cloud credentials."}}`, http.StatusInternalServerError)
		return
	}

	latency := time.Since(startTime).Milliseconds()

	logEntry := StructuredLog{
		Severity:  "INFO",
		Time:      startTime.UTC().Format(time.RFC3339),
		Method:    r.Method,
		Path:      r.URL.Path,
		Client:    r.Header.Get("X-Client-ID"),
		Tier:      r.Header.Get("X-Client-Tier"),
		ModelIn:   r.Header.Get("X-Requested-Model"),
		ModelOut:  r.Header.Get("X-Routed-Model"),
		Status:    wrapped.statusCode,
		LatencyMs: latency,
		BytesSent: wrapped.bytesWritten,
	}

	if wrapped.statusCode >= 400 {
		logEntry.Severity = "WARNING"
		logEntry.ErrorMessage = "Proxy request failed"
	}
	if wrapped.statusCode >= 500 {
		logEntry.Severity = "ERROR"
	}

	logJSON, err := json.Marshal(logEntry)
	if err == nil {
		log.Println(string(logJSON))
	} else {
		log.Printf("[Proxy] Error encoding log: %v", err)
	}
}
