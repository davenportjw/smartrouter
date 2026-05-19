package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"geminirouter/pkg/config"
	"geminirouter/pkg/limiter"

	"github.com/golang-jwt/jwt/v4"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"
)

// RouterProxy wraps the httputil.ReverseProxy to add custom routing and logging.
type RouterProxy struct {
	Target      *url.URL
	Proxy       *httputil.ReverseProxy
	Store       *config.ConfigStore
	Limiter     *limiter.RateLimiterRegistry
	Scheduler   *RequestScheduler
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
	Severity      string            `json:"severity"`
	Time          string            `json:"time"`
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Client        string            `json:"client_id,omitempty"`
	App           string            `json:"app_id,omitempty"`
	Tier          string            `json:"tier,omitempty"`
	ModelIn       string            `json:"model_requested,omitempty"`
	ModelOut      string            `json:"model_routed,omitempty"`
	Status        int               `json:"status"`
	LatencyMs     int64             `json:"latency_ms"`
	BytesSent     int64             `json:"bytes_sent"`
	ErrorMessage  string            `json:"error_message,omitempty"`
	CustomHeaders map[string]string `json:"custom_headers,omitempty"`
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

	maxQueueSize := 1000
	if val := os.Getenv("ROUTER_MAX_QUEUE_SIZE"); val != "" {
		if limit, err := strconv.Atoi(val); err == nil && limit > 0 {
			maxQueueSize = limit
		}
	}

	activeLimit := 100
	if val := os.Getenv("ROUTER_CONCURRENCY_LIMIT"); val != "" {
		if limit, err := strconv.Atoi(val); err == nil && limit > 0 {
			activeLimit = limit
		}
	}

	rp := &RouterProxy{
		Target:      target,
		Store:       store,
		Limiter:     limiter.NewRateLimiterRegistry(),
		Scheduler:   NewRequestScheduler(maxQueueSize, activeLimit),
		TokenSource: ts,
		ProjectID:   projectID,
		Location:    location,
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host

		// 1. Remove client-side credentials
		query := req.URL.Query()
		query.Del("key")
		req.URL.RawQuery = query.Encode()
		req.Header.Del("x-goog-api-key")

		// 2. Rewrite URL paths to project-level GCP endpoints
		pathParts := strings.Split(req.URL.Path, "/")
		if len(pathParts) < 3 {
			return
		}
		version := pathParts[1]
		resource := pathParts[2]

		if resource == "models" {
			targetModel := req.Header.Get("X-Routed-Model")
			if targetModel == "" {
				targetModel = req.Header.Get("X-Requested-Model")
			}

			parts := strings.Split(req.URL.Path, "/models/")
			if len(parts) < 2 {
				return
			}
			modelAndAction := parts[1]
			actionParts := strings.Split(modelAndAction, ":")

			var action string
			if len(actionParts) > 1 {
				action = ":" + actionParts[1]
			}

			newPath := fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models/%s%s",
				rp.ProjectID, rp.Location, targetModel, action)
			req.URL.Path = newPath
		} else if resource == "reasoningEngines" || resource == "ragCorpora" {
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
		http.Error(w, `{"error": {"message": "Authentication required. Use x-goog-api-key, Authorization Bearer token, or ?key query parameter."}}`, http.StatusUnauthorized)
		return
	}

	var app config.App
	var client config.Client
	var ok bool

	if isJWT(clientKey) {
		// Parse and validate Google ID Token
		email, err := parseGoogleIdentity(r.Context(), clientKey, r.Host)
		if err != nil {
			log.Printf("[Auth] Google ID token validation failed: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(fmt.Sprintf(`{"error": {"message": "Invalid Google ID token: %v"}}`, err)))
			return
		}

		// Lookup App directly by the Service Account email
		app, ok = rp.Store.LookupApp(email)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(fmt.Sprintf(`{"error": {"message": "No registered Application profile found for identity %s."}}`, email)))
			return
		}

		// Resolve parent Client details for billing / tier info
		client, ok = rp.Store.LookupClient(app.ClientID)
		if !ok {
			http.Error(w, `{"error": {"message": "Parent Client configuration missing."}}`, http.StatusInternalServerError)
			return
		}
	} else {
		// Standard API Key flow
		keyData, ok := rp.Store.LookupKey(clientKey)
		if !ok {
			http.Error(w, `{"error": {"message": "Invalid or inactive router API key."}}`, http.StatusUnauthorized)
			return
		}

		// Resolve logical App configuration linked to the key
		app, ok = rp.Store.LookupApp(keyData.AppID)
		if !ok {
			http.Error(w, `{"error": {"message": "Application configuration missing."}}`, http.StatusInternalServerError)
			return
		}

		// Resolve parent Client details for billing / tier info
		client, ok = rp.Store.LookupClient(app.ClientID)
		if !ok {
			http.Error(w, `{"error": {"message": "Client configuration missing."}}`, http.StatusInternalServerError)
			return
		}
	}

	// Populate context headers on the original request
	r.Header.Set("X-Client-ID", client.ID)
	r.Header.Set("X-App-ID", app.ID)
	r.Header.Set("X-Client-Tier", client.Tier)
	r.Header.Set("X-App-Priority", app.Priority)
	r.Header.Set("X-App-RPM", fmt.Sprintf("%d", app.RPM))
	r.Header.Set("X-App-TPM", fmt.Sprintf("%d", app.TPM))

	pathParts := strings.Split(r.URL.Path, "/")
	if len(pathParts) >= 3 {
		resource := pathParts[2]
		if resource == "models" {
			parts := strings.Split(r.URL.Path, "/models/")
			if len(parts) >= 2 {
				modelAndAction := parts[1]
				actionParts := strings.Split(modelAndAction, ":")
				requestedModel := actionParts[0]

				r.Header.Set("X-Requested-Model", requestedModel)

				// Collect flat headers map for MatchRule
				headersMap := make(map[string]string)
				for k, v := range r.Header {
					if len(v) > 0 {
						headersMap[k] = v[0]
					}
				}

				// Apply dynamic routing rules (bound to App or Global)
				targetModel := requestedModel
				routedRule, matched := rp.Store.MatchRule(requestedModel, client.Tier, app.ID, headersMap)
				if matched {
					targetModel = routedRule.TargetModel
				}
				r.Header.Set("X-Routed-Model", targetModel)
			}
		}
	}

	// 1.5. Validate custom headers (Global and App-specific)
	for _, h := range rp.Store.GetHeaders() {
		if h.AppID != "" && h.AppID != "global" && h.AppID != app.ID {
			continue
		}
		val := r.Header.Get(h.Name)
		if h.Required && val == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(fmt.Sprintf(`{"error": {"code": 400, "message": "Missing required custom header: %s (%s)"}}`, h.Name, h.Description)))
			return
		}
		if val != "" {
			switch h.Validation {
			case "non-empty":
				if strings.TrimSpace(val) == "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(fmt.Sprintf(`{"error": {"code": 400, "message": "Custom header %s cannot be empty"}}`, h.Name)))
					return
				}
			case "regex":
				if !rp.Store.MatchHeaderRegex(h.ID, val) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(fmt.Sprintf(`{"error": {"code": 400, "message": "Header %s does not match required pattern: %s"}}`, h.Name, h.ValuePattern)))
					return
				}
			case "enum":
				options := strings.Split(h.ValuePattern, ",")
				valid := false
				for _, opt := range options {
					if strings.TrimSpace(opt) == val {
						valid = true
						break
					}
				}
				if !valid {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(fmt.Sprintf(`{"error": {"code": 400, "message": "Header %s must be one of: %s"}}`, h.Name, h.ValuePattern)))
					return
				}
			}
		}
	}

	// Sync local rate limiter with current application capacity
	rp.Limiter.UpdateLimiter(app.ID, app.RPM, app.TPM, app.Priority)

	// Evaluate rate limits on the application boundary
	allowed, delay := rp.Limiter.EvaluateLimit(r.Context(), app.ID, 1)
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

	// Enqueue request in priority-based queue
	queueItem, err := rp.Scheduler.Enqueue(r.Context(), app.Priority, client.Tier)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{
			"error": {
				"code": 429,
				"message": "Router is under high load and request queue is full.",
				"status": "RESOURCE_EXHAUSTED"
			}
		}`))
		return
	}

	// Wait for scheduling slot or context cancellation
	select {
	case <-r.Context().Done():
		return
	case <-queueItem.Done:
		// Slot acquired!
	}
	defer rp.Scheduler.Release()

	// Apply queueing delay using cancelable select if a wait-time was assigned for priority
	if delay > 0 {
		log.Printf("[Proxy Queue] Delaying request for app %s by %v due to priority queueing", app.ID, delay)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
	}

	// Fetch OAuth2 token for upstream Vertex AI API
	token, err := rp.TokenSource.Token()
	if err != nil {
		log.Printf("[Proxy] Error retrieving Google Cloud token: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": {"message": "Failed to retrieve Google Cloud credentials."}}`))
		return
	}
	r.Header.Set("Authorization", "Bearer "+token.AccessToken)

	// Wrap the transport dynamically with the retry transport wrapper
	if rp.Proxy.Transport != nil {
		if _, ok := rp.Proxy.Transport.(*retryTransport); !ok {
			rp.Proxy.Transport = &retryTransport{base: rp.Proxy.Transport}
		}
	} else {
		rp.Proxy.Transport = &retryTransport{base: http.DefaultTransport}
	}

	// Execute standard reverse proxy
	rp.Proxy.ServeHTTP(wrapped, r)

	latency := time.Since(startTime).Milliseconds()

	logHeaders := make(map[string]string)
	for _, h := range rp.Store.GetHeaders() {
		if h.AppID != "" && h.AppID != "global" && h.AppID != app.ID {
			continue
		}
		if val := r.Header.Get(h.Name); val != "" {
			logHeaders[h.Name] = val
		}
	}

	logEntry := StructuredLog{
		Severity:      "INFO",
		Time:          startTime.UTC().Format(time.RFC3339),
		Method:        r.Method,
		Path:          r.URL.Path,
		Client:        r.Header.Get("X-Client-ID"),
		App:           r.Header.Get("X-App-ID"),
		Tier:          r.Header.Get("X-Client-Tier"),
		ModelIn:       r.Header.Get("X-Requested-Model"),
		ModelOut:      r.Header.Get("X-Routed-Model"),
		Status:        wrapped.statusCode,
		LatencyMs:     latency,
		BytesSent:     wrapped.bytesWritten,
		CustomHeaders: logHeaders,
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
		_, _ = os.Stdout.WriteString(string(logJSON) + "\n")
	} else {
		log.Printf("[Proxy] Error encoding log: %v", err)
	}
}

// isJWT detects if a token is a JSON Web Token.
func isJWT(token string) bool {
	parts := strings.Split(token, ".")
	return len(parts) == 3 && strings.HasPrefix(parts[0], "eyJ")
}

// parseGoogleIdentity decodes and validates a Google-issued ID Token to extract the email.
func parseGoogleIdentity(ctx context.Context, tokenStr string, host string) (string, error) {
	isLocalDev := os.Getenv("LOCAL_DEV") == "true"
	if isLocalDev {
		// In local dev, parse unverified payload directly to extract email claim
		parser := &jwt.Parser{}
		token, _, err := parser.ParseUnverified(tokenStr, jwt.MapClaims{})
		if err != nil {
			return "", fmt.Errorf("failed to parse unverified local JWT: %w", err)
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			return "", fmt.Errorf("invalid local JWT claims")
		}
		email, ok := claims["email"].(string)
		if !ok || email == "" {
			return "", fmt.Errorf("missing email claim in local JWT")
		}
		return email, nil
	}

	// In production, validate the token using Google's verification library
	audience := os.Getenv("ROUTER_URL")
	if audience == "" {
		audience = "https://" + host
	}

	payload, err := idtoken.Validate(ctx, tokenStr, audience)
	if err != nil {
		return "", fmt.Errorf("google id token validation failed: %w", err)
	}

	email, ok := payload.Claims["email"].(string)
	if !ok || email == "" {
		return "", fmt.Errorf("missing email claim in validated token")
	}
	return email, nil
}

// retryTransport wraps a base http.RoundTripper to support dynamic retries and backoff for 429s.
type retryTransport struct {
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer request body if present
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
	}

	maxRetries := 3
	baseBackoff := 500 * time.Millisecond

	var resp *http.Response
	var err error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Backoff before retry: exponential backoff
			backoff := baseBackoff * time.Duration(1<<(attempt-1))
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(backoff):
			}
			log.Printf("[Retry Transport] Retrying request upstream (attempt %d/%d) after %v due to 429", attempt, maxRetries, backoff)
		}

		// Re-populate request body from buffer
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		resp, err = t.base.RoundTrip(req)
		if err != nil {
			if attempt < maxRetries {
				log.Printf("[Retry Transport] Upstream roundtrip error on attempt %d: %v, retrying...", attempt, err)
				continue
			}
			return nil, err
		}

		// If status code is NOT 429, return response immediately
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// Upstream returned 429. If we have attempts left, close the body and loop to retry.
		if attempt < maxRetries {
			resp.Body.Close()
			continue
		}

		// No attempts left, return the final 429 response to the client
		return resp, nil
	}

	if err != nil {
		return nil, err
	}
	return resp, nil
}

