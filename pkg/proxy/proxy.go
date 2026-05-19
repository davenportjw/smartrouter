package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
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
	Severity      string            `json:"severity"`
	Time          string            `json:"time"`
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Client        string            `json:"client_id,omitempty"`
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

		// 1. Check for context errors passed from ServeHTTP
		if val := req.Header.Get("X-Router-Error"); val != "" {
			return
		}

		// 2. Remove client-side credentials
		query := req.URL.Query()
		query.Del("key")
		req.URL.RawQuery = query.Encode()
		req.Header.Del("x-goog-api-key")

		// 3. Fetch OAuth2 token for upstream Vertex AI API
		token, err := rp.TokenSource.Token()
		if err != nil {
			log.Printf("[Proxy] Error retrieving Google Cloud token: %v", err)
			req.Header.Set("X-Router-Error", "TokenError")
			return
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)

		// 4. Rewrite URL paths to project-level GCP endpoints
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

	// Populate client and model context headers on the original request
	r.Header.Set("X-Client-ID", client.ID)
	r.Header.Set("X-Client-Tier", client.Tier)
	r.Header.Set("X-Client-Priority", client.Priority)
	r.Header.Set("X-Client-RPM", fmt.Sprintf("%d", client.RPM))
	r.Header.Set("X-Client-TPM", fmt.Sprintf("%d", client.TPM))

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

				// Apply dynamic routing rules
				targetModel := requestedModel
				routedRule, matched := rp.Store.MatchRule(requestedModel, client.Tier)
				if matched {
					targetModel = routedRule.TargetModel
				}
				r.Header.Set("X-Routed-Model", targetModel)
			}
		}
	}

	// 1.5. Validate custom headers
	for _, h := range rp.Store.GetHeaders() {
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
				matched, err := regexp.MatchString(h.ValuePattern, val)
				if err != nil || !matched {
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

	logHeaders := make(map[string]string)
	for _, h := range rp.Store.GetHeaders() {
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
