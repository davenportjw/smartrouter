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
	"sync"
	"time"

	store "geminirouter/backend/config"
	"geminirouter/pkg/config"
	"geminirouter/backend/limiter"

	"github.com/golang-jwt/jwt/v4"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"
)

type contextKey string
const (
	appIDKey           contextKey = "app_id"
	estimatedTokensKey contextKey = "estimated_tokens"
)

// RouterProxy wraps the httputil.ReverseProxy to add custom routing and logging.
type RouterProxy struct {
	Target      *url.URL
	Proxy       *httputil.ReverseProxy
	Store       *store.ConfigStore
	Limiter     *limiter.RateLimiterRegistry
	Scheduler   *RequestScheduler
	TokenSource oauth2.TokenSource
	ProjectID   string
	Location    string
	transportMu sync.Mutex
	ClassifierClient *http.Client
	classifierCache  sync.Map // cache for classifier prompt hashes
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
func NewRouterProxy(store *store.ConfigStore, projectID, location string) (*RouterProxy, error) {
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
		ClassifierClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)

		// Resolve dynamic model location for rewriting host and path
		modelLoc := req.Header.Get("X-Routed-Model-Location")
		if modelLoc == "" {
			modelLoc = rp.Location
		}

		// Resolve target host using official GetVertexEndpointHost helper
		var targetHost string
		if modelLoc != rp.Location || config.IsMultiRegionOrGlobal(modelLoc) {
			targetHost = config.GetVertexEndpointHost(modelLoc)
		} else {
			targetHost = target.Host
		}
		req.URL.Scheme = "https"
		req.URL.Host = targetHost
		req.Host = targetHost

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

		switch resource {
		case "models":
			targetModel := req.Header.Get("X-Routed-Model")
			if targetModel == "" {
				targetModel = req.Header.Get("X-Requested-Model")
			}

			idx := strings.Index(req.URL.Path, "/models/")
			if idx == -1 {
				return
			}
			modelAndAction := req.URL.Path[idx+len("/models/"):]
			actionParts := strings.Split(modelAndAction, ":")

			var action string
			if len(actionParts) > 1 {
				action = ":" + actionParts[1]
			}

			if strings.HasPrefix(targetModel, "projects/") {
				req.URL.Path = "/v1/" + targetModel + action
			} else {
				newPath := fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models/%s%s",
					rp.ProjectID, modelLoc, targetModel, action)
				req.URL.Path = newPath
			}
		case "reasoningEngines", "ragCorpora":
			remainingPath := strings.Join(pathParts[2:], "/")
			newPath := fmt.Sprintf("/%s/projects/%s/locations/%s/%s",
				version, rp.ProjectID, modelLoc, remainingPath)
			req.URL.Path = newPath
		}
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusOK && resp.Request != nil {
			appID, _ := resp.Request.Context().Value(appIDKey).(string)
			estimatedTokens, _ := resp.Request.Context().Value(estimatedTokensKey).(int)

			if appID != "" && estimatedTokens > 0 {
				if !strings.Contains(resp.Request.URL.Path, ":streamGenerateContent") && resp.Body != nil {
					bodyBytes, err := io.ReadAll(resp.Body)
					if err == nil {
						resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

						var res struct {
							UsageMetadata struct {
								TotalTokenCount int `json:"totalTokenCount"`
							} `json:"usageMetadata"`
						}
						if err := json.Unmarshal(bodyBytes, &res); err == nil {
							actualTokens := res.UsageMetadata.TotalTokenCount
							if actualTokens > 0 {
								correction := actualTokens - estimatedTokens
								rp.Limiter.AdjustLimiter(appID, correction)
							}
						}
					}
				}
			}
		}
		return nil
	}

	proxy.Transport = &retryTransport{base: http.DefaultTransport}
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

	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)
	}

	var bodyBytes []byte
	if r.Body != nil && r.Method == http.MethodPost {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
	}

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
			idx := strings.Index(r.URL.Path, "/models/")
			if idx != -1 {
				modelAndAction := r.URL.Path[idx+len("/models/"):]
				actionParts := strings.Split(modelAndAction, ":")
				requestedModel := actionParts[0]

				r.Header.Set("X-Requested-Model", requestedModel)

				// 1.2. Handle Dynamic Routing Opt-Out Check
				targetModel := requestedModel
				if app.OptOutDynamicRouting {
					r.Header.Set("X-Routed-Model", targetModel)
					modelLoc := rp.Location
					if activeModel, exists := rp.Store.LookupActiveModel(targetModel, rp.Location); exists {
						modelLoc = activeModel.Location
						targetModel = config.StripLocationSuffix(activeModel.ID)
						r.Header.Set("X-Routed-Model", targetModel)
					}
					r.Header.Set("X-Routed-Model-Location", modelLoc)
				} else {
					// 1.2. Evaluate Validation for Virtual Model
					if requestedModel == "gemini-dynamic" && !app.Complexity.Enabled {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						w.Write([]byte(fmt.Sprintf(`{
							"error": {
								"code": 400,
								"message": "Virtual model 'gemini-dynamic' requested but dynamic complexity routing is not enabled for application '%s'. Please enable it in the smart router dashboard.",
								"status": "INVALID_ARGUMENT"
							}
						}`, app.ID)))
						return
					}

					// 1.3. Execute Dynamic Complexity Routing
					if app.Complexity.Enabled && (app.Complexity.AlwaysOverride || requestedModel == "gemini-dynamic") {
						if len(bodyBytes) > 0 {
							// Run dynamic classifier
							complexityTier, classifyErr := rp.classifyComplexity(r.Context(), bodyBytes, app.Complexity)
							if classifyErr == nil {
								switch complexityTier {
								case "simple":
									targetModel = app.Complexity.SimpleModel
									if targetModel == "" {
										targetModel = "gemini-2.5-flash-lite"
									}
								case "medium":
									targetModel = app.Complexity.MediumModel
									if targetModel == "" {
										targetModel = "gemini-2.5-flash"
									}
								case "complex":
									targetModel = app.Complexity.ComplexModel
									if targetModel == "" {
										targetModel = "gemini-2.5-pro"
									}
								}
								log.Printf("[Proxy Routing] Dynamic complexity router classified prompt as '%s' -> routed target model: %s", complexityTier, targetModel)
							} else {
								log.Printf("[Proxy Routing] Complexity classification failed: %v. Falling back to requested model %s", classifyErr, requestedModel)
							}
						}
					}

					// Collect flat headers map for MatchRule
					headersMap := make(map[string]string)
					for k, v := range r.Header {
						if len(v) > 0 {
							headersMap[k] = v[0]
						}
					}

					// 1.4. Apply standard dynamic rules-based routing matching on top of the complexity-classified model!
					routedRule, matched := rp.Store.MatchRule(targetModel, client.Tier, app.ID, headersMap)
					if matched {
						log.Printf("[Proxy Routing] MatchRule matched rules-based routing on top of complexity routed model %s -> targeting: %s", targetModel, routedRule.TargetModel)
						targetModel = routedRule.TargetModel
					}
					
					r.Header.Set("X-Routed-Model", targetModel)

					// Resolve and set the routed model's location
					modelLoc := rp.Location // default to router's location
					if activeModel, exists := rp.Store.LookupActiveModel(targetModel, rp.Location); exists {
						modelLoc = activeModel.Location
						targetModel = config.StripLocationSuffix(activeModel.ID)
						r.Header.Set("X-Routed-Model", targetModel)
					}
					r.Header.Set("X-Routed-Model-Location", modelLoc)
				}
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
	rp.Limiter.UpdateLimiter(app.ID, app.RPM, app.TPM, app.Priority, app.OptOutTPM)

	// Estimate tokens for TPM rate limiting if not opted out
	estimatedTokens := 1
	if !app.OptOutTPM && len(bodyBytes) > 0 {
		estimatedTokens = len(bodyBytes) / 4
		if estimatedTokens < 1 {
			estimatedTokens = 1
		}
	}

	// Evaluate rate limits on the application boundary
	allowed, delay := rp.Limiter.EvaluateLimit(r.Context(), app.ID, estimatedTokens)
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

	// Store app ID and estimated tokens in request context for post-request correction
	ctx := context.WithValue(r.Context(), appIDKey, app.ID)
	ctx = context.WithValue(ctx, estimatedTokensKey, estimatedTokens)
	r = r.WithContext(ctx)

	routedModel := r.Header.Get("X-Routed-Model")
	if routedModel == "" {
		routedModel = r.Header.Get("X-Requested-Model")
	}
	if routedModel == "" {
		routedModel = "gemini-2.5-flash"
	}

	// Enqueue request in priority-based queue
	queueItem, err := rp.Scheduler.Enqueue(r.Context(), app.ID, app.Priority, client.Tier, routedModel)
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
	defer rp.Scheduler.Release(queueItem)

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

	// Thread-safe dynamic transport wrapping to support unit tests overriding rp.Proxy.Transport
	rp.transportMu.Lock()
	if rp.Proxy.Transport != nil {
		if _, ok := rp.Proxy.Transport.(*retryTransport); !ok {
			rp.Proxy.Transport = &retryTransport{base: rp.Proxy.Transport}
		}
	} else {
		rp.Proxy.Transport = &retryTransport{base: http.DefaultTransport}
	}
	rp.transportMu.Unlock()

	// Set custom audit headers in ResponseWriter for client programmatic visibility
	w.Header().Set("X-Routed-Model", r.Header.Get("X-Routed-Model"))
	w.Header().Set("X-Requested-Model", r.Header.Get("X-Requested-Model"))
	w.Header().Set("X-Routed-Model-Location", r.Header.Get("X-Routed-Model-Location"))
	w.Header().Set("X-Client-Tier", client.Tier)
	w.Header().Set("X-App-ID", app.ID)

	// Execute standard reverse proxy
	rp.Proxy.ServeHTTP(wrapped, r)

	// Report request status to scheduler for 429 overload detection
	rp.Scheduler.ReportRequestStatus(routedModel, wrapped.statusCode)

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
		if os.Getenv("LOCAL_DEV") == "true" {
			if f, err := os.OpenFile("data/local_logs.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
				_, _ = f.WriteString(string(logJSON) + "\n")
				f.Close()
			}
		}
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

type geminiPart struct {
	Text       string                 `json:"text,omitempty"`
	InlineData map[string]interface{} `json:"inlineData,omitempty"`
	FileData   map[string]interface{} `json:"fileData,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts,omitempty"`
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents,omitempty"`
	Tools    []interface{}   `json:"tools,omitempty"`
}

type classifierResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// classifyComplexity evaluates the incoming body payload to determine query complexity.
func (rp *RouterProxy) classifyComplexity(ctx context.Context, body []byte, c config.ComplexityRouting) (string, error) {
	var req geminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Graceful fallback if unmarshalling fails
		return "simple", nil
	}

	// 1. Evaluate Fast Heuristics Bypasses
	hasMultimodal := false
	hasTools := len(req.Tools) > 0

	charCount := 0
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil || part.FileData != nil {
				hasMultimodal = true
			}
			charCount += len(part.Text)
		}
	}

	if hasTools && c.ForceComplexTools {
		log.Printf("[Complexity Classifier] Request has active tools. Bypassing LLM classifier, routing as complex.")
		return "complex", nil
	}
	if hasMultimodal && c.ForceComplexMultimodal {
		log.Printf("[Complexity Classifier] Request has multimodal inline/file payload. Bypassing LLM classifier, routing as complex.")
		return "complex", nil
	}

	// 2. Run LLM-Based Semantic Classification if configured
	if c.UseLLMClassifier {
		classifierModel := c.ClassifierModel
		if classifierModel == "" {
			classifierModel = "gemini-3.1-flash-lite"
		}

		log.Printf("[Complexity Classifier] Invoking dynamic classifier (%s) for prompt (length: %d characters)...", classifierModel, charCount)
		
		// Concat user text prompts to feed the semantic classifier
		var promptSnippet strings.Builder
		for _, content := range req.Contents {
			for _, part := range content.Parts {
				if part.Text != "" {
					promptSnippet.WriteString(part.Text)
					promptSnippet.WriteByte('\n')
				}
			}
		}
		snippetStr := promptSnippet.String()
		if len(snippetStr) > 4000 {
			snippetStr = snippetStr[:4000]
		}

		// Check classifier cache using a simple hash or direct key
		cacheKey := fmt.Sprintf("%s|%s|%s", classifierModel, snippetStr, c.AdditionalInstructions)
		if cachedVal, ok := rp.classifierCache.Load(cacheKey); ok {
			if cachedStr, ok := cachedVal.(string); ok {
				log.Printf("[Complexity Classifier] Cache HIT: classified complexity as %s", cachedStr)
				return cachedStr, nil
			}
		}

		complexityResult, err := rp.callLLMClassifier(ctx, classifierModel, snippetStr, c.AdditionalInstructions)
		if err == nil && (complexityResult == "simple" || complexityResult == "medium" || complexityResult == "complex") {
			log.Printf("[Complexity Classifier] LLM classified complexity: %s", complexityResult)
			rp.classifierCache.Store(cacheKey, complexityResult)
			return complexityResult, nil
		}
		log.Printf("[Complexity Classifier] LLM classification call failed: %v. Falling back to static thresholds.", err)
	}

	// 3. Heuristic Rules: Classify strictly by character thresholds
	if charCount <= c.SimpleCharLimit {
		return "simple", nil
	}
	if charCount <= c.MediumCharLimit {
		return "medium", nil
	}
	return "complex", nil
}

// callLLMClassifier sends a rapid structured OIDC JSON query to the Vertex AI Gemini API.
func (rp *RouterProxy) callLLMClassifier(ctx context.Context, modelName string, prompt string, additionalInstructions string) (string, error) {
	systemInstr := "You are a low-overhead API routing complexity classifier. Classify the user prompt into one of three tiers:\n" +
		"- simple: Greetings, chit-chat, simple factual lookups, single basic questions.\n" +
		"- medium: Summarization, standard instructions, translations, content generation.\n" +
		"- complex: Complex coding, math/logic puzzles, multi-step debugging, deep reasoning.\n" +
		"Return JSON format: {\"complexity\": \"simple\" | \"medium\" | \"complex\"}"

	if additionalInstructions != "" {
		systemInstr += "\n\nAdditional classification criteria and application-specific context to respect:\n" + additionalInstructions
	}

	type schemaProp struct {
		Type string   `json:"type"`
		Enum []string `json:"enum,omitempty"`
	}
	type schemaObj struct {
		Type       string                `json:"type"`
		Properties map[string]schemaProp `json:"properties"`
		Required   []string              `json:"required"`
	}
	type genConfig struct {
		ResponseMimeType string    `json:"responseMimeType"`
		ResponseSchema   schemaObj `json:"responseSchema"`
		Temperature      float64   `json:"temperature"`
		MaxOutputTokens  int       `json:"maxOutputTokens"`
	}

	type internalPart struct {
		Text string `json:"text"`
	}
	type internalContent struct {
		Parts []internalPart `json:"parts"`
	}
	type internalReq struct {
		Contents          []internalContent `json:"contents"`
		SystemInstruction internalContent   `json:"systemInstruction"`
		GenerationConfig  genConfig         `json:"generationConfig"`
	}

	reqBody := internalReq{
		Contents: []internalContent{
			{
				Parts: []internalPart{{Text: "Prompt to classify:\n" + prompt}},
			},
		},
		SystemInstruction: internalContent{
			Parts: []internalPart{{Text: systemInstr}},
		},
		GenerationConfig: genConfig{
			ResponseMimeType: "application/json",
			ResponseSchema: schemaObj{
				Type: "OBJECT",
				Properties: map[string]schemaProp{
					"complexity": {
						Type: "STRING",
						Enum: []string{"simple", "medium", "complex"},
					},
				},
				Required: []string{"complexity"},
			},
			Temperature:     0.0,
			MaxOutputTokens: 20,
		},
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	// Target the smart-router's regional host configuration dynamically
	targetURL := fmt.Sprintf("%s://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		rp.Target.Scheme, rp.Target.Host, rp.ProjectID, rp.Location, modelName)

	// Tight timeout for classification overhead safety (3s)
	callCtx, cancel := context.WithTimeout(ctx, 3000*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, "POST", targetURL, bytes.NewReader(reqJSON))
	if err != nil {
		return "", err
	}

	token, err := rp.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("failed to fetch credentials: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := rp.ClassifierClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyErr, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("classifier API returned status %s: %s", resp.Status, string(bodyErr))
	}

	var res classifierResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	if len(res.Candidates) == 0 || len(res.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty candidates in classifier response")
	}

	rawJSON := res.Candidates[0].Content.Parts[0].Text
	var out struct {
		Complexity string `json:"complexity"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &out); err != nil {
		return "", fmt.Errorf("failed to parse output JSON %q: %w", rawJSON, err)
	}

	return out.Complexity, nil
}

