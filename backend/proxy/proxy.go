package proxy

import (
	"bufio"
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	store "geminirouter/backend/config"
	"geminirouter/pkg/config"
	"geminirouter/pkg/decision"
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
	Target           *url.URL
	Proxy            *httputil.ReverseProxy
	Store            *store.ConfigStore
	Limiter          *limiter.RateLimiterRegistry
	Scheduler        *RequestScheduler
	Queue            *ClusterQueue
	TokenSource      oauth2.TokenSource
	ProjectID        string
	Location         string
	transportMu      sync.Mutex
	ClassifierClient *http.Client
	classifierCache  sync.Map // cache for classifier prompt hashes
	Sessions             *config.SessionRegistry
	GeminiDecisionEngine *decision.GeminiDecisionEngine
	JevEngine            *decision.JevDecisionEngine
	FastinoEngine        *decision.FastinoDecisionEngine
	CLMEngine            *decision.CLMContrastiveEngine
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
	if fbModel := rw.ResponseWriter.Header().Get("X-Fallback-Routed-Model"); fbModel != "" {
		rw.ResponseWriter.Header().Set("X-Routed-Model", fbModel)
		rw.ResponseWriter.Header().Del("X-Fallback-Routed-Model")
	}
	if fbLoc := rw.ResponseWriter.Header().Get("X-Fallback-Routed-Location"); fbLoc != "" {
		rw.ResponseWriter.Header().Set("X-Routed-Model-Location", fbLoc)
		rw.ResponseWriter.Header().Del("X-Fallback-Routed-Location")
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

func writeJSONError(w http.ResponseWriter, statusCode int, statusText, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	type errBody struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status,omitempty"`
	}
	payload, _ := json.Marshal(struct {
		Error errBody `json:"error"`
	}{
		Error: errBody{
			Code:    statusCode,
			Message: message,
			Status:  statusText,
		},
	})
	_, _ = w.Write(payload)
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
	EngineUsed    string            `json:"engine_used,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	AgentTask     string            `json:"agent_task,omitempty"`
	Status        int               `json:"status"`
	LatencyMs     int64             `json:"latency_ms"`
	BytesSent     int64             `json:"bytes_sent"`
	ErrorMessage  string            `json:"error_message,omitempty"`
	CustomHeaders map[string]string `json:"custom_headers,omitempty"`
}

// lazyInit ensures all registries and engines are non-nil even in direct struct initialization in unit tests.
func (rp *RouterProxy) lazyInit() {
	if rp.Sessions == nil {
		rp.Sessions = config.NewSessionRegistry(30 * time.Minute)
	}
	if rp.GeminiDecisionEngine == nil {
		rp.GeminiDecisionEngine = decision.NewGeminiDecisionEngine(rp.TokenSource, rp.ProjectID)
	} else {
		if rp.TokenSource != nil {
			rp.GeminiDecisionEngine.TokenSource = rp.TokenSource
		}
		if rp.ProjectID != "" && rp.GeminiDecisionEngine.ProjectID == "" {
			rp.GeminiDecisionEngine.ProjectID = rp.ProjectID
		}
	}
	if rp.JevEngine == nil {
		rp.JevEngine = decision.NewJevDecisionEngine()
	}
	if rp.FastinoEngine == nil {
		rp.FastinoEngine = decision.NewFastinoDecisionEngine()
	}
	if rp.CLMEngine == nil {
		rp.CLMEngine = decision.NewCLMContrastiveEngine()
	}
}

// NewRouterProxy creates a new reverse proxy pointing to the upstream Gemini API.
func NewRouterProxy(store *store.ConfigStore, projectID, location string) (*RouterProxy, error) {
	targetHost := config.GetVertexEndpointHost(location)
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
		Target:        target,
		Store:         store,
		Limiter:       limiter.NewRateLimiterRegistry(),
		Scheduler:     NewRequestScheduler(maxQueueSize, activeLimit),
		Queue:         NewClusterQueue(maxQueueSize, 60*time.Second),
		TokenSource:   ts,
		ProjectID:            projectID,
		Location:             location,
		Sessions:             config.NewSessionRegistry(30 * time.Minute),
		GeminiDecisionEngine: decision.NewGeminiDecisionEngine(ts, projectID),
		JevEngine:            decision.NewJevDecisionEngine(),
		FastinoEngine:        decision.NewFastinoDecisionEngine(),
		CLMEngine:            decision.NewCLMContrastiveEngine(),
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
			var action string
			if lastIdx := strings.LastIndex(modelAndAction, ":"); lastIdx != -1 {
				action = modelAndAction[lastIdx:]
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
					_ = resp.Body.Close()
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
	rp.lazyInit()
	startTime := time.Now()
	wrapped := newResponseWriterWrapper(w)
	originalPath := r.URL.Path
	skipStructuredLog := false

	defer func() {
		if skipStructuredLog {
			return
		}
		latency := time.Since(startTime).Milliseconds()

		finalRoutedModel := wrapped.Header().Get("X-Fallback-Routed-Model")
		if finalRoutedModel == "" {
			finalRoutedModel = wrapped.Header().Get("X-Routed-Model")
		}
		if finalRoutedModel == "" {
			finalRoutedModel = r.Header.Get("X-Routed-Model")
		}
		if finalRoutedModel == "" {
			finalRoutedModel = r.Header.Get("X-Requested-Model")
		}

		appID := r.Header.Get("X-App-ID")
		logHeaders := make(map[string]string)
		if rp.Store != nil {
			for _, h := range rp.Store.GetHeaders() {
				if h.AppID != "" && h.AppID != "global" && h.AppID != appID {
					continue
				}
				if val := r.Header.Get(h.Name); val != "" {
					logHeaders[h.Name] = val
				}
			}
		}

		logEntry := StructuredLog{
			Severity:      "INFO",
			Time:          startTime.UTC().Format(time.RFC3339),
			Method:        r.Method,
			Path:          originalPath,
			Client:        r.Header.Get("X-Client-ID"),
			App:           appID,
			Tier:          r.Header.Get("X-Client-Tier"),
			ModelIn:       r.Header.Get("X-Requested-Model"),
			ModelOut:      finalRoutedModel,
			EngineUsed:    r.Header.Get("X-Router-Engine-Used"),
			SessionID:     r.Header.Get("X-Session-ID"),
			AgentTask:     r.Header.Get("X-Agent-Task"),
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
				logPath := "data/local_logs.jsonl"
				if dataDir := os.Getenv("LOCAL_DATA_DIR"); dataDir != "" {
					logPath = filepath.Join(dataDir, "local_logs.jsonl")
				}
				_ = os.MkdirAll(filepath.Dir(logPath), 0755)
				if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
					_, _ = f.WriteString(string(logJSON) + "\n")
					f.Close()
				}
			}
		} else {
			log.Printf("[Proxy] Error encoding log: %v", err)
		}
	}()

	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)
	}

	var bodyBytes []byte
	if r.Body != nil && r.Method == http.MethodPost {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			writeJSONError(wrapped, http.StatusRequestEntityTooLarge, "INVALID_ARGUMENT", "Request body too large or unreadable")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	// Extract user custom key first to check auth before proxy director executes
	clientKey := extractAPIKey(r)
	if clientKey == "" {
		writeJSONError(wrapped, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required. Use x-goog-api-key, Authorization Bearer token, or ?key query parameter.")
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
			writeJSONError(wrapped, http.StatusUnauthorized, "UNAUTHENTICATED", fmt.Sprintf("Invalid Google ID token: %v", err))
			return
		}

		// Lookup App directly by the Service Account email
		app, ok = rp.Store.LookupApp(email)
		if !ok {
			writeJSONError(wrapped, http.StatusUnauthorized, "UNAUTHENTICATED", fmt.Sprintf("No registered Application profile found for identity %s.", email))
			return
		}

		// Resolve parent Client details for billing / tier info
		client, ok = rp.Store.LookupClient(app.ClientID)
		if !ok {
			writeJSONError(wrapped, http.StatusInternalServerError, "INTERNAL", "Parent Client configuration missing.")
			return
		}
	} else {
		// Standard API Key flow
		keyData, ok := rp.Store.LookupKey(clientKey)
		if !ok {
			writeJSONError(wrapped, http.StatusUnauthorized, "UNAUTHENTICATED", "Invalid or inactive router API key.")
			return
		}

		// Resolve logical App configuration linked to the key
		app, ok = rp.Store.LookupApp(keyData.AppID)
		if !ok {
			writeJSONError(wrapped, http.StatusInternalServerError, "INTERNAL", "Application configuration missing.")
			return
		}

		// Resolve parent Client details for billing / tier info
		client, ok = rp.Store.LookupClient(app.ClientID)
		if !ok {
			writeJSONError(wrapped, http.StatusInternalServerError, "INTERNAL", "Client configuration missing.")
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

	if isList, modelID, matched := parseOpenAIModelsPath(r.Method, r.URL.Path); matched {
		skipStructuredLog = true
		rp.handleOpenAIModels(wrapped, r, isList, modelID)
		return
	}

	isOpenAIChat := isOpenAIChatCompletionsPath(r.URL.Path)
	var openAIStream bool
	if isOpenAIChat {
		if r.Method != http.MethodPost {
			writeJSONError(wrapped, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "Only POST is supported for chat completions.")
			return
		}
		reqModel, isStream, geminiBody, err := translateOpenAIToGeminiRequest(bodyBytes, app.Complexity.Enabled)
		if err != nil {
			writeJSONError(wrapped, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
		openAIStream = isStream
		bodyBytes = geminiBody
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		r.ContentLength = int64(len(bodyBytes))
		r.Header.Del("Accept-Encoding")
		if openAIStream {
			r.URL.Path = "/v1/models/" + reqModel + ":streamGenerateContent"
			q := r.URL.Query()
			q.Set("alt", "sse")
			r.URL.RawQuery = q.Encode()
		} else {
			r.URL.Path = "/v1/models/" + reqModel + ":generateContent"
		}
	}

	pathParts := strings.Split(r.URL.Path, "/")
	if len(pathParts) >= 3 {
		resource := pathParts[2]
		if resource == "models" {
			idx := strings.Index(r.URL.Path, "/models/")
			if idx != -1 {
				modelAndAction := r.URL.Path[idx+len("/models/"):]
				var requestedModel string
				if lastIdx := strings.LastIndex(modelAndAction, ":"); lastIdx != -1 {
					requestedModel = modelAndAction[:lastIdx]
				} else {
					requestedModel = modelAndAction
				}

				r.Header.Set("X-Requested-Model", requestedModel)

				// 1.2. Handle Dynamic Routing Opt-Out Check
				targetModel := requestedModel
				if app.OptOutDynamicRouting {
					modelLoc := rp.Location
					if config.ExtractLocationSuffix(targetModel) == "" {
						if localLoc, okLocal := rp.Store.ResolveLocalModelLocation(targetModel); okLocal {
							modelLoc = localLoc
							targetModel = config.StripLocationSuffix(targetModel)
						} else if activeModel, exists := rp.Store.LookupActiveModel(targetModel, rp.Location); exists {
							modelLoc = activeModel.Location
							targetModel = config.StripLocationSuffix(activeModel.ID)
						} else {
							modelLoc = config.GetDefaultLocationForModel(targetModel, rp.Location)
							targetModel = config.StripLocationSuffix(targetModel)
						}
					} else if activeModel, exists := rp.Store.LookupActiveModel(targetModel, rp.Location); exists {
						modelLoc = activeModel.Location
						targetModel = config.StripLocationSuffix(activeModel.ID)
					} else {
						modelLoc = config.GetDefaultLocationForModel(targetModel, rp.Location)
						targetModel = config.StripLocationSuffix(targetModel)
					}
					r.Header.Set("X-Routed-Model", targetModel)
					r.Header.Set("X-Routed-Model-Location", modelLoc)
				} else {
					// 1.2. Evaluate Validation for Virtual Model
					if requestedModel == "gemini-dynamic" && !app.Complexity.Enabled {
						writeJSONError(wrapped, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Virtual model 'gemini-dynamic' requested but dynamic complexity routing is not enabled for application '%s'. Please enable it in the smart router dashboard.", app.ID))
						return
					}

					// Stage 0: Parse Control Headers
					promptOverrideModel := r.Header.Get("X-Router-Model")
					promptOverrideComplexity := r.Header.Get("X-Router-Complexity")
					promptOverrideEngine := r.Header.Get("X-Router-Engine")
					sessionID := r.Header.Get("X-Session-ID")
					sessionPolicy := r.Header.Get("X-Session-Policy")
					sessionModel := r.Header.Get("X-Session-Model")
					agentTask := r.Header.Get("X-Agent-Task")
					if agentTask == "" {
						agentTask = r.Header.Get("X-Task-Kind")
					}

					var sess *config.SessionState
					if sessionID != "" {
						sess = rp.Sessions.GetOrCreateSession(sessionID, app.ID)
						if sessionPolicy != "" {
							sess.Policy = sessionPolicy
						}
						if sessionModel != "" {
							sess.SetModel(sessionModel, "session_init")
						}
						if agentTask != "" {
							sess.SetTask(agentTask)
						}
					}

					var engineUsed string
					var complexityTier string
					var evaluatedConfidence float64
					extractedTask := agentTask

					isDynamic := app.Complexity.Enabled && (app.Complexity.AlwaysOverride || requestedModel == "gemini-dynamic")

					// Stage 0: Per-Prompt Overrides
					if promptOverrideModel != "" {
						targetModel = promptOverrideModel
						engineUsed = "prompt_override"
						complexityTier = "override"
						evaluatedConfidence = 1.0
						if sess != nil {
							sess.SetModel(targetModel, engineUsed)
						}
					} else if promptOverrideComplexity != "" {
						complexityTier = strings.ToLower(promptOverrideComplexity)
						targetModel = decision.ResolveModelByTier(complexityTier, app.Complexity)
						engineUsed = "prompt_override"
						evaluatedConfidence = 1.0
						if sess != nil {
							sess.SetModel(targetModel, engineUsed)
						}
					} else if sess != nil && sessionModel != "" {
						// Explicit Session Model Initialization or Mid-Session Re-Pin
						targetModel = sessionModel
						engineUsed = "session_lock"
						complexityTier = "session_init"
						evaluatedConfidence = 1.0
						sess.SetModel(targetModel, engineUsed)
					} else if agentTask != "" {
						// Stage 2: Agent Task Switching (also upgrades sticky session lock when X-Agent-Task is provided)
						if tm, tier, ok := decision.ResolveTaskModel(agentTask, app.Complexity); ok {
							targetModel = tm
							complexityTier = tier
						} else {
							targetModel = decision.ResolveModelByTier("medium", app.Complexity)
							complexityTier = "task"
						}
						engineUsed = "task_mapping"
						evaluatedConfidence = 1.0
						if sess != nil {
							sess.SetModel(targetModel, engineUsed)
						}
					} else if sess != nil && sess.Turn > 0 && sess.Policy == "lock" && sess.LockedModel != "" {
						// Stage 1: Sticky Session Lock (Multi-Turn Continuation when no explicit task/model switch is requested)
						targetModel = sess.LockedModel
						engineUsed = "session_lock"
						complexityTier = "locked"
						evaluatedConfidence = 1.0
					} else if isDynamic {
						// Stage 3: Dynamic Decision Engines (Jev / Fastino / CLM / Contrastive-LM / LLM / Heuristics)
						engine := promptOverrideEngine
						if engine == "" {
							engine = app.Complexity.Engine
						}
						if engine == "" {
							if app.Complexity.UseLLMClassifier {
								engine = "llm"
							} else if app.Complexity.SimpleCharLimit > 0 || app.Complexity.MediumCharLimit > 0 {
								engine = "heuristics"
							} else {
								engine = "jev"
							}
						}
						engine = strings.ToLower(strings.TrimSpace(engine))

						snippet, charCount, hasTools, hasMultimodal := extractPromptAndMeta(bodyBytes)

						if hasTools && app.Complexity.ForceComplexTools {
							complexityTier = "complex"
							targetModel = decision.ResolveModelByTier(complexityTier, app.Complexity)
							engineUsed = engine
							evaluatedConfidence = 0.99
						} else if hasMultimodal && app.Complexity.ForceComplexMultimodal {
							complexityTier = "complex"
							targetModel = decision.ResolveModelByTier(complexityTier, app.Complexity)
							engineUsed = engine
							evaluatedConfidence = 0.99
						} else {
							effectiveComplexity := rp.resolveComplexityWithClusterProviders(app.Complexity)
							switch engine {
							case "gemini-decision", "gemini-decisions", "gemini-decision-1", "decisions":
								blocks, bTools, bMulti := decision.BuildDecisionInputBlocksFromGeminiBody(bodyBytes)
								if len(blocks) == 0 && snippet != "" {
									blocks = []decision.DecisionInputBlock{{Type: "text", Text: snippet}}
								}
								res, err := rp.GeminiDecisionEngine.ClassifyWithInput(r.Context(), blocks, effectiveComplexity, hasTools || bTools, hasMultimodal || bMulti)
								if err != nil {
									log.Printf("[Proxy Decision] Gemini Decisions API error: %v; falling back to heuristics", err)
									w.Header().Set("X-Router-Decision-Error", err.Error())
									complexityTier = rp.classifyHeuristics(charCount, hasTools, hasMultimodal, app.Complexity)
									targetModel = decision.ResolveModelByTier(complexityTier, app.Complexity)
									engineUsed = "heuristics"
									evaluatedConfidence = 0.80
								} else {
									complexityTier = res.Tier
									targetModel = res.TargetModel
									engineUsed = "gemini-decision"
									evaluatedConfidence = res.Confidence
									if extractedTask == "" {
										extractedTask = res.Features.ExtractedTask
									}
									if res.LocalEligibleProb > 0 {
										r.Header.Set("X-Router-Local-Eligible", fmt.Sprintf("%.2f", res.LocalEligibleProb))
									}
								}
							case "jev":
								res := rp.JevEngine.Classify(r.Context(), snippet, effectiveComplexity, hasTools, hasMultimodal)
								complexityTier = res.Tier
								targetModel = res.TargetModel
								engineUsed = "jev"
								evaluatedConfidence = res.Confidence
								if extractedTask == "" {
									extractedTask = res.Features.ExtractedTask
								}
							case "fastino":
								res := rp.FastinoEngine.Classify(r.Context(), snippet, effectiveComplexity, hasTools, hasMultimodal)
								complexityTier = res.Tier
								targetModel = res.TargetModel
								engineUsed = "fastino"
								evaluatedConfidence = res.Confidence
								if extractedTask == "" {
									extractedTask = res.Features.ExtractedTask
								}
							case "clm", "contrastive-lm":
								cCfg := effectiveComplexity
								cCfg.Engine = engine
								res := rp.CLMEngine.Classify(r.Context(), snippet, cCfg, hasTools, hasMultimodal)
								complexityTier = res.Tier
								targetModel = res.TargetModel
								engineUsed = engine
								evaluatedConfidence = res.Confidence
								if extractedTask == "" {
									extractedTask = res.Features.ExtractedTask
								}
							case "llm":
								classifierModel := app.Complexity.ClassifierModel
								if classifierModel == "" {
									classifierModel = "gemini-3.5-flash-lite"
								}
								tier, err := rp.callLLMClassifier(r.Context(), classifierModel, snippet, app.Complexity.AdditionalInstructions)
								if err == nil && (tier == "simple" || tier == "medium" || tier == "complex") {
									complexityTier = tier
									evaluatedConfidence = 0.90
								} else {
									complexityTier = rp.classifyHeuristics(charCount, hasTools, hasMultimodal, app.Complexity)
									evaluatedConfidence = 0.75
								}
								targetModel = decision.ResolveModelByTier(complexityTier, app.Complexity)
								engineUsed = "llm"
							default: // heuristics
								complexityTier = rp.classifyHeuristics(charCount, hasTools, hasMultimodal, app.Complexity)
								targetModel = decision.ResolveModelByTier(complexityTier, app.Complexity)
								engineUsed = "heuristics"
								evaluatedConfidence = 0.80
							}
						}

						// Lock session if policy is "lock" and not locked yet
						if sess != nil && sess.Policy == "lock" && sess.LockedModel == "" {
							sess.SetModel(targetModel, engineUsed)
						}
					}

					// Apply sticky-turn session policy if active
					if sess != nil && sess.Policy == "sticky-turn" {
						candTier := complexityTier
						if candTier == "" {
							candTier = decision.InferTierFromModel(targetModel, app.Complexity)
						}
						prevLocked := sess.LockedModel
						stickyModel, stickyTier, _ := sess.ApplyStickyTurn(targetModel, candTier)
						if stickyTier != candTier || stickyModel != targetModel {
							resolvedSticky := decision.ResolveModelByTier(stickyTier, app.Complexity)
							if resolvedSticky != "" {
								stickyModel = resolvedSticky
								sess.SetModelAndTier(stickyModel, stickyTier, sess.TurnsSinceUpgrade)
							} else if stickyModel == "" {
								stickyModel = prevLocked
							}
							engineUsed = "session_sticky_turn"
						}
						targetModel = stickyModel
						complexityTier = stickyTier
					}

					// Update session turn if session exists
					if sess != nil {
						turn := sess.IncrementTurn()
						r.Header.Set("X-Session-ID", sessionID)
						r.Header.Set("X-Session-Turn", strconv.Itoa(turn))
					}
					if agentTask != "" {
						r.Header.Set("X-Agent-Task", agentTask)
					}

					// Collect flat headers map for MatchRuleWithContext
					headersMap := make(map[string]string)
					for k, v := range r.Header {
						if len(v) > 0 {
							headersMap[k] = v[0]
						}
					}

					// 1.4. Apply standard dynamic rules-based routing matching on top of the complexity-classified model!
					modelLoc := rp.Location // default to router's location
					var ruleSetLocation bool
					if engineUsed != "prompt_override" || promptOverrideModel == "" {
						// If no classifier ran in Stage 3, check if any conditional rule requires classifier evaluation
						if complexityTier == "" {
							candEngines := rp.Store.ClassifierRuleCandidateEngines(targetModel, client.Tier, app.ID)
							if len(candEngines) > 0 {
								snippet, charCount, hasTools, hasMultimodal := extractPromptAndMeta(bodyBytes)
								candEng := strings.ToLower(strings.TrimSpace(candEngines[0]))
								effectiveComplexity := rp.resolveComplexityWithClusterProviders(app.Complexity)
								switch candEng {
								case "gemini-decision", "gemini-decisions", "gemini-decision-1", "decisions":
									blocks, bTools, bMulti := decision.BuildDecisionInputBlocksFromGeminiBody(bodyBytes)
									if len(blocks) == 0 && snippet != "" {
										blocks = []decision.DecisionInputBlock{{Type: "text", Text: snippet}}
									}
									res, err := rp.GeminiDecisionEngine.ClassifyWithInput(r.Context(), blocks, effectiveComplexity, hasTools || bTools, hasMultimodal || bMulti)
									if err == nil {
										complexityTier = res.Tier
										engineUsed = "gemini-decision"
										evaluatedConfidence = res.Confidence
										if extractedTask == "" {
											extractedTask = res.Features.ExtractedTask
										}
										if res.LocalEligibleProb > 0 {
											r.Header.Set("X-Router-Local-Eligible", fmt.Sprintf("%.2f", res.LocalEligibleProb))
										}
									} else {
										log.Printf("[Proxy Decision] Stage-4 Gemini Decisions API error: %v", err)
										w.Header().Set("X-Router-Decision-Error", err.Error())
									}
								case "llm":
									classifierModel := app.Complexity.ClassifierModel
									if classifierModel == "" {
										classifierModel = "gemini-3.5-flash-lite"
									}
									tier, err := rp.callLLMClassifier(r.Context(), classifierModel, snippet, app.Complexity.AdditionalInstructions)
									if err == nil && (tier == "simple" || tier == "medium" || tier == "complex") {
										complexityTier = tier
										engineUsed = "llm"
										evaluatedConfidence = 0.90
									}
								case "fastino":
									res := rp.FastinoEngine.Classify(r.Context(), snippet, effectiveComplexity, hasTools, hasMultimodal)
									complexityTier = res.Tier
									engineUsed = "fastino"
									evaluatedConfidence = res.Confidence
									if extractedTask == "" {
										extractedTask = res.Features.ExtractedTask
									}
								case "clm", "contrastive-lm":
									cCfg := effectiveComplexity
									cCfg.Engine = candEng
									res := rp.CLMEngine.Classify(r.Context(), snippet, cCfg, hasTools, hasMultimodal)
									complexityTier = res.Tier
									engineUsed = candEng
									evaluatedConfidence = res.Confidence
									if extractedTask == "" {
										extractedTask = res.Features.ExtractedTask
									}
								case "heuristics":
									complexityTier = rp.classifyHeuristics(charCount, hasTools, hasMultimodal, app.Complexity)
									engineUsed = "heuristics"
									evaluatedConfidence = 0.80
								default:
									res := rp.JevEngine.Classify(r.Context(), snippet, effectiveComplexity, hasTools, hasMultimodal)
									complexityTier = res.Tier
									engineUsed = "jev"
									evaluatedConfidence = res.Confidence
									if extractedTask == "" {
										extractedTask = res.Features.ExtractedTask
									}
								}
							}
						}

						routedRule, matched := rp.Store.MatchRuleWithContext(targetModel, client.Tier, app.ID, headersMap, complexityTier, engineUsed, evaluatedConfidence, extractedTask)
						if matched {
							hasRuleClassifierCondition := (routedRule.ComplexityTier != "" && routedRule.ComplexityTier != "all" && routedRule.ComplexityTier != "*") ||
								(routedRule.DecisionEngine != "" && routedRule.DecisionEngine != "all" && routedRule.DecisionEngine != "*") ||
								routedRule.MinConfidence > 0 ||
								(routedRule.TaskKind != "" && routedRule.TaskKind != "all" && routedRule.TaskKind != "*")
							// Unconditional wildcard rule without header or classifier conditions should not override dynamically selected model
							if !(engineUsed != "" && routedRule.ModelPattern == "*" && routedRule.HeaderName == "" && !hasRuleClassifierCondition) {
								log.Printf("[Proxy Routing] MatchRule matched rules-based routing on top of complexity routed model %s -> targeting: %s in location: %s", targetModel, routedRule.TargetModel, routedRule.TargetLocation)
								targetModel = routedRule.TargetModel
								if routedRule.TargetLocation != "" {
									modelLoc = routedRule.TargetLocation
									ruleSetLocation = true
								}
								if routedRule.FallbackModel != "" {
									fbModel := routedRule.FallbackModel
									fbLoc := rp.Location
									if activeFb, exists := rp.Store.LookupActiveModel(fbModel, rp.Location); exists {
										fbLoc = activeFb.Location
										fbModel = config.StripLocationSuffix(activeFb.ID)
									} else {
										fbLoc = config.GetDefaultLocationForModel(fbModel, rp.Location)
										fbModel = config.StripLocationSuffix(fbModel)
									}
									r.Header.Set("X-Fallback-Target-Model", fbModel)
									r.Header.Set("X-Fallback-Model-Location", fbLoc)
								}
							}
						}
					}

					if engineUsed != "" {
						r.Header.Set("X-Router-Engine-Used", engineUsed)
					}
					if complexityTier != "" {
						r.Header.Set("X-Router-Complexity-Tier", complexityTier)
					}
					if evaluatedConfidence > 0 && engineUsed != "" {
						r.Header.Set("X-Router-Confidence", fmt.Sprintf("%.2f", evaluatedConfidence))
					}

					// Resolve and set the routed model's location if not specifically overridden by the matched rule location
					if !ruleSetLocation && config.ExtractLocationSuffix(targetModel) == "" {
						if localLoc, okLocal := rp.Store.ResolveLocalModelLocation(targetModel); okLocal {
							modelLoc = localLoc
							targetModel = config.StripLocationSuffix(targetModel)
						} else if activeModel, exists := rp.Store.LookupActiveModel(targetModel, rp.Location); exists {
							modelLoc = activeModel.Location
							targetModel = config.StripLocationSuffix(activeModel.ID)
						} else {
							modelLoc = config.GetDefaultLocationForModel(targetModel, rp.Location)
							targetModel = config.StripLocationSuffix(targetModel)
						}
					} else if activeModel, exists := rp.Store.LookupActiveModel(targetModel, rp.Location); exists {
						if !ruleSetLocation || config.ExtractLocationSuffix(targetModel) != "" {
							modelLoc = activeModel.Location
						}
						targetModel = config.StripLocationSuffix(activeModel.ID)
					} else {
						if !ruleSetLocation || config.ExtractLocationSuffix(targetModel) != "" {
							if localLoc, okLocal := rp.Store.ResolveLocalModelLocation(targetModel); okLocal {
								modelLoc = localLoc
							} else {
								modelLoc = config.GetDefaultLocationForModel(targetModel, rp.Location)
							}
						}
						targetModel = config.StripLocationSuffix(targetModel)
					}
					r.Header.Set("X-Routed-Model", targetModel)
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
			writeJSONError(wrapped, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Missing required custom header: %s (%s)", h.Name, h.Description))
			return
		}
		if val != "" {
			switch h.Validation {
			case "non-empty":
				if strings.TrimSpace(val) == "" {
					writeJSONError(wrapped, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Custom header %s cannot be empty", h.Name))
					return
				}
			case "regex":
				if !rp.Store.MatchHeaderRegex(h.ID, val) {
					writeJSONError(wrapped, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Header %s does not match required pattern: %s", h.Name, h.ValuePattern))
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
					writeJSONError(wrapped, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("Header %s must be one of: %s", h.Name, h.ValuePattern))
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

	// Evaluate rate limits on the application boundary (EvaluateLimit performs any priority delay internally)
	allowed, delay := rp.Limiter.EvaluateLimit(r.Context(), app.ID, estimatedTokens)
	if !allowed {
		writeJSONError(wrapped, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "Resource has been exhausted (e.g. queries per minute) on the smart router.")
		return
	}
	if delay > 0 {
		log.Printf("[Proxy Queue] Delayed request for app %s by %v due to priority queueing", app.ID, delay)
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
		if isDecisionsPassthroughPath(r.URL.Path) {
			routedModel = decision.GeminiDecisionModel
		} else {
			routedModel = "gemini-3.8-flash"
		}
	}

	// Enqueue request in priority-based queue
	queueItem, err := rp.Scheduler.Enqueue(r.Context(), app.ID, app.Priority, client.Tier, routedModel)
	if err != nil {
		writeJSONError(wrapped, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "Router is under high load and request queue is full.")
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

	// Direct Gemini Decisions API pass-through (POST /v1beta/decisions or POST /v1/decisions)
	if isDecisionsPassthroughPath(r.URL.Path) {
		rp.handleDecisionsPassthrough(wrapped, r, bodyBytes, app, client, estimatedTokens)
		return
	}

	// Check if routed to a dynamic local cluster queue ("local-cluster" or a named LocalCluster ID)
	if clusterID, matchedCluster, isLocalCluster := rp.resolveLocalCluster(r.Header.Get("X-Routed-Model-Location")); isLocalCluster {
		// If no pull-queue cluster with this ID is configured (e.g., generic "local-cluster") and an active DirectEndpoint matches, skip queue
		if matchedCluster == nil {
			if ep, ok := rp.resolveDirectEndpoint(routedModel, r.Header.Get("X-Routed-Model-Location")); ok {
				if rp.forwardToDirectEndpoint(wrapped, r, ep, bodyBytes, routedModel, client.Tier, app.ID, isOpenAIChat || strings.EqualFold(r.Header.Get("X-OpenAI-Format"), "true"), openAIStream) {
					return
				}
				routedModel = r.Header.Get("X-Routed-Model")
			}
		}

		// Re-evaluate cluster match in case DirectEndpoint failover rewrote X-Routed-Model-Location to a cloud region
		if clusterID2, matchedCluster2, stillLocal := rp.resolveLocalCluster(r.Header.Get("X-Routed-Model-Location")); stillLocal {
			clusterID = clusterID2
			matchedCluster = matchedCluster2

			applyClusterFallback := func(reason string) bool {
				if matchedCluster == nil || strings.TrimSpace(matchedCluster.FallbackModel) == "" {
					return false
				}
				fbModel, fbLocSuffix := config.SplitModelAndLocation(strings.TrimSpace(matchedCluster.FallbackModel))
				fbLoc := strings.TrimSpace(matchedCluster.FallbackLocation)
				if fbLoc == "" {
					fbLoc = fbLocSuffix
				}
				if fbLoc == "" {
					fbLoc = config.GetDefaultLocationForModel(fbModel, rp.Location)
				}
				log.Printf("[Proxy Cluster Fallback] Cluster %q failed (%s); falling back to model %q in location %q", clusterID, reason, fbModel, fbLoc)
				routedModel = fbModel
				r.Header.Set("X-Routed-Model", routedModel)
				r.Header.Set("X-Routed-Model-Location", fbLoc)
				w.Header().Set("X-Cluster-Fallback", "true")
				return true
			}

			if rp.Queue == nil {
				if !applyClusterFallback("local cluster queue disabled") {
					writeJSONError(wrapped, http.StatusServiceUnavailable, "UNAVAILABLE", "Local cluster serving is not enabled on this router instance.")
					return
				}
			} else if matchedCluster != nil && strings.TrimSpace(matchedCluster.FallbackModel) != "" && !rp.hasOnlineCapableRunner(clusterID, matchedCluster, routedModel) {
				// Instant cloud spillover (<1ms) when FallbackModel is configured and 0 capable runners are online
				w.Header().Set("X-Cluster-Fallback-Reason", "no_online_runners")
				applyClusterFallback("no online capable runners")
			} else {
				var jobMaxAge time.Duration
				if matchedCluster != nil && matchedCluster.MaxQueueAge > 0 {
					if matchedCluster.MaxQueueAge < time.Millisecond {
						jobMaxAge = matchedCluster.MaxQueueAge * time.Second
					} else {
						jobMaxAge = matchedCluster.MaxQueueAge
					}
				}

				jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
				job := &config.QueueJob{
					ID:        jobID,
					ClusterID: clusterID,
					AppID:     app.ID,
					Model:     routedModel,
					Priority:  app.Priority,
					Tier:      client.Tier,
					MaxAge:    jobMaxAge,
					Payload:   bodyBytes,
					CreatedAt: time.Now(),
				}

				resChan, err := rp.Queue.Enqueue(job)
				if err != nil {
					if !applyClusterFallback(fmt.Sprintf("enqueue error: %v", err)) {
						writeJSONError(wrapped, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", fmt.Sprintf("Local cluster queue full: %v", err))
						return
					}
				} else {
					waitDuration := jobMaxAge
					if waitDuration <= 0 && rp.Queue.maxAge > 0 {
						waitDuration = rp.Queue.maxAge
					}
					if waitDuration <= 0 {
						waitDuration = 90 * time.Second
					}
					timer := time.NewTimer(waitDuration + 25*time.Millisecond)
					defer timer.Stop()

					select {
					case <-r.Context().Done():
						rp.Queue.CancelJob(job.ID)
						writeJSONError(wrapped, http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", "Client disconnected while waiting in cluster queue")
						return
					case <-timer.C:
						rp.Queue.CancelJob(job.ID)
						if !applyClusterFallback("queue wait timeout") {
							writeJSONError(wrapped, http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", "gateway timeout: request exceeded max queue age")
							return
						}
					case res := <-resChan:
						if res.Error != nil || res.StatusCode >= http.StatusInternalServerError {
							errMsg := "cluster runner error"
							if res.Error != nil {
								errMsg = res.Error.Error()
							}
							statusCode := res.StatusCode
							if statusCode == 0 {
								statusCode = http.StatusBadGateway
							}
							if !applyClusterFallback(errMsg) {
								writeJSONError(wrapped, statusCode, "INTERNAL", errMsg)
								return
							}
						} else {
							w.Header().Set("X-Routed-Model", routedModel)
							w.Header().Set("X-Requested-Model", r.Header.Get("X-Requested-Model"))
							w.Header().Set("X-Routed-Model-Location", clusterID)
							w.Header().Set("X-Client-Tier", client.Tier)
							w.Header().Set("X-App-ID", app.ID)
							if v := r.Header.Get("X-Router-Engine-Used"); v != "" {
								w.Header().Set("X-Router-Engine-Used", v)
							}
							if v := r.Header.Get("X-Router-Complexity-Tier"); v != "" {
								w.Header().Set("X-Router-Complexity-Tier", v)
							}
							if v := r.Header.Get("X-Router-Confidence"); v != "" {
								w.Header().Set("X-Router-Confidence", v)
							}
							if v := r.Header.Get("X-Router-Local-Eligible"); v != "" {
								w.Header().Set("X-Router-Local-Eligible", v)
							}
							if v := r.Header.Get("X-Session-ID"); v != "" {
								w.Header().Set("X-Session-ID", v)
							}
							if v := r.Header.Get("X-Session-Turn"); v != "" {
								w.Header().Set("X-Session-Turn", v)
							}
							if v := r.Header.Get("X-Agent-Task"); v != "" {
								w.Header().Set("X-Agent-Task", v)
							}

							wantOpenAI := isOpenAIChat || strings.EqualFold(r.Header.Get("X-OpenAI-Format"), "true")
							if wantOpenAI {
								if openAIStream {
									ssePayload := translateGeminiStreamToOpenAISSE(res.Payload, routedModel, fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), time.Now().Unix())
									wrapped.Header().Set("Content-Type", "text/event-stream")
									wrapped.WriteHeader(res.StatusCode)
									_, _ = wrapped.Write(ssePayload)
									return
								}
								oaiPayload, err := translateGeminiToOpenAIResponse(res.Payload, routedModel)
								if err == nil {
									wrapped.Header().Set("Content-Type", "application/json")
									wrapped.WriteHeader(res.StatusCode)
									_, _ = wrapped.Write(oaiPayload)
									return
								}
							}

							wrapped.Header().Set("Content-Type", "application/json")
							wrapped.WriteHeader(res.StatusCode)
							_, _ = wrapped.Write(res.Payload)
							return
						}
					}
				}
			}
		}
	}

	// Check if routed to (or fallen back to) a Direct HTTP Endpoint in Providers["local"]
	if ep, ok := rp.resolveDirectEndpoint(routedModel, r.Header.Get("X-Routed-Model-Location")); ok {
		if rp.forwardToDirectEndpoint(wrapped, r, ep, bodyBytes, routedModel, client.Tier, app.ID, isOpenAIChat || strings.EqualFold(r.Header.Get("X-OpenAI-Format"), "true"), openAIStream) {
			return
		}
		routedModel = r.Header.Get("X-Routed-Model")
	}

	// Fetch OAuth2 token for upstream Vertex AI API
	token, err := rp.TokenSource.Token()
	if err != nil {
		log.Printf("[Proxy] Error retrieving Google Cloud token: %v", err)
		writeJSONError(wrapped, http.StatusInternalServerError, "INTERNAL", "Failed to retrieve Google Cloud credentials.")
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
	if v := r.Header.Get("X-Router-Engine-Used"); v != "" {
		w.Header().Set("X-Router-Engine-Used", v)
	}
	if v := r.Header.Get("X-Router-Complexity-Tier"); v != "" {
		w.Header().Set("X-Router-Complexity-Tier", v)
	}
	if v := r.Header.Get("X-Router-Confidence"); v != "" {
		w.Header().Set("X-Router-Confidence", v)
	}
	if v := r.Header.Get("X-Router-Local-Eligible"); v != "" {
		w.Header().Set("X-Router-Local-Eligible", v)
	}
	if v := r.Header.Get("X-Session-ID"); v != "" {
		w.Header().Set("X-Session-ID", v)
	}
	if v := r.Header.Get("X-Session-Turn"); v != "" {
		w.Header().Set("X-Session-Turn", v)
	}
	if v := r.Header.Get("X-Agent-Task"); v != "" {
		w.Header().Set("X-Agent-Task", v)
	}

	// Execute standard reverse proxy (with OpenAI response translation if request was OpenAI spec)
	var finalRoutedModel string
	if isOpenAIChat {
		oaiWriter := newOpenAIInterceptorWriter(wrapped, openAIStream, r.Header.Get("X-Routed-Model"))
		rp.Proxy.ServeHTTP(oaiWriter, r)
		finalRoutedModel = oaiWriter.Header().Get("X-Routed-Model")
		if fb := oaiWriter.Header().Get("X-Fallback-Routed-Model"); fb != "" {
			finalRoutedModel = fb
		}
		if finalRoutedModel == "" {
			finalRoutedModel = routedModel
		}
		oaiWriter.Finalize(finalRoutedModel)
		w.Header().Set("X-Routed-Model", finalRoutedModel)
	} else {
		rp.Proxy.ServeHTTP(wrapped, r)
		finalRoutedModel = w.Header().Get("X-Routed-Model")
		if finalRoutedModel == "" {
			finalRoutedModel = routedModel
		}
	}

	// Report request status to scheduler for 429 overload detection
	if finalRoutedModel != routedModel && routedModel != "" {
		rp.Scheduler.ReportRequestStatus(routedModel, http.StatusTooManyRequests)
	}
	rp.Scheduler.ReportRequestStatus(finalRoutedModel, wrapped.statusCode)
}

// isJWT detects if a token is a JSON Web Token.
func isJWT(token string) bool {
	parts := strings.Split(token, ".")
	return len(parts) == 3 && strings.HasPrefix(parts[0], "eyJ")
}

// parseGoogleIdentity decodes and validates a Google-issued ID Token to extract the email.
func parseGoogleIdentity(ctx context.Context, tokenStr string, host string) (string, error) {
	// Unverified parsing is a local-dev convenience only; never on Cloud Run (K_SERVICE set).
	isLocalDev := os.Getenv("LOCAL_DEV") == "true" && os.Getenv("K_SERVICE") == ""
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

// retryTransport wraps a base http.RoundTripper to support dynamic retries, backoff for 429s, and FallbackModel failover.
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
	fbModel := req.Header.Get("X-Fallback-Target-Model")
	fbLoc := req.Header.Get("X-Fallback-Model-Location")

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
			break
		}

		// If status code is NOT 429, break out of 429 retry loop
		if resp.StatusCode != http.StatusTooManyRequests {
			break
		}

		// Upstream returned 429. If we have attempts left, close the body and loop to retry.
		if attempt < maxRetries {
			resp.Body.Close()
			continue
		}
	}

	// If primary upstream failed with an error, 429, or 5xx, check for FallbackModel
	if fbModel != "" && (err != nil || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		fbReq := req.Clone(req.Context())
		if bodyBytes != nil {
			fbReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		rewriteRequestForFallback(fbReq, fbModel, fbLoc)
		fbResp, fbErr := t.base.RoundTrip(fbReq)
		if fbErr != nil {
			return nil, fbErr
		}
		if fbResp.Header == nil {
			fbResp.Header = make(http.Header)
		}
		fbResp.Header.Set("X-Router-Fallback-Triggered", "true")
		fbResp.Header.Set("X-Fallback-Routed-Model", fbModel)
		if fbLoc != "" {
			fbResp.Header.Set("X-Fallback-Routed-Location", fbLoc)
		}
		return fbResp, nil
	}

	if err != nil {
		return nil, err
	}
	return resp, nil
}

func rewriteRequestForFallback(req *http.Request, fbModel, fbLoc string) {
	oldModel := req.Header.Get("X-Routed-Model")
	oldLoc := req.Header.Get("X-Routed-Model-Location")

	if oldModel != "" && strings.Contains(req.URL.Path, "/models/"+oldModel) {
		req.URL.Path = strings.Replace(req.URL.Path, "/models/"+oldModel, "/models/"+fbModel, 1)
	} else if idx := strings.Index(req.URL.Path, "/models/"); idx != -1 {
		modelAndAction := req.URL.Path[idx+len("/models/"):]
		var action string
		if lastIdx := strings.LastIndex(modelAndAction, ":"); lastIdx != -1 {
			action = modelAndAction[lastIdx:]
		}
		req.URL.Path = req.URL.Path[:idx+len("/models/")] + fbModel + action
	}

	if fbLoc != "" && oldLoc != "" && strings.Contains(req.URL.Path, "/locations/"+oldLoc+"/") {
		req.URL.Path = strings.Replace(req.URL.Path, "/locations/"+oldLoc+"/", "/locations/"+fbLoc+"/", 1)
	}

	req.Header.Set("X-Routed-Model", fbModel)
	if fbLoc != "" {
		req.Header.Set("X-Routed-Model-Location", fbLoc)
		if !strings.HasPrefix(req.URL.Host, "127.0.0.1") && !strings.HasPrefix(req.URL.Host, "localhost") {
			targetHost := config.GetVertexEndpointHost(fbLoc)
			req.URL.Host = targetHost
			req.Host = targetHost
		}
	}
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
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// extractPromptAndMeta parses user prompt text, char length, tools, and multimodal presence from request body.
func extractPromptAndMeta(body []byte) (snippet string, charCount int, hasTools bool, hasMultimodal bool) {
	if len(body) == 0 {
		return "", 0, false, false
	}
	var req geminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return string(body), len(body), false, false
	}
	hasTools = len(req.Tools) > 0
	var sb strings.Builder
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil || part.FileData != nil {
				hasMultimodal = true
			}
			charCount += len(part.Text)
			if part.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(part.Text)
			}
		}
	}
	snippet = sb.String()
	if len(snippet) > 4000 {
		snippet = snippet[:4000]
	}
	if snippet == "" && len(body) > 0 {
		snippet = string(body)
		charCount = len(body)
	}
	return snippet, charCount, hasTools, hasMultimodal
}

// classifyHeuristics evaluates static character limits and multimodal/tool bypasses.
func (rp *RouterProxy) classifyHeuristics(charCount int, hasTools, hasMultimodal bool, c config.ComplexityRouting) string {
	if hasTools && c.ForceComplexTools {
		return "complex"
	}
	if hasMultimodal && c.ForceComplexMultimodal {
		return "complex"
	}
	if charCount <= c.SimpleCharLimit {
		return "simple"
	}
	if charCount <= c.MediumCharLimit {
		return "medium"
	}
	return "complex"
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
			classifierModel = "gemini-3.5-flash-lite"
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

		complexityResult, err := rp.callLLMClassifier(ctx, classifierModel, snippetStr, c.AdditionalInstructions)
		if err == nil && (complexityResult == "simple" || complexityResult == "medium" || complexityResult == "complex") {
			log.Printf("[Complexity Classifier] LLM classified complexity: %s", complexityResult)
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
	tier, _, _, err := rp.callLLMClassifierWithUsage(ctx, modelName, prompt, additionalInstructions, true)
	return tier, err
}

func (rp *RouterProxy) callLLMClassifierWithUsage(ctx context.Context, modelName string, prompt string, additionalInstructions string, useCache bool) (string, int, int, error) {
	cleanModel := config.StripLocationSuffix(modelName)
	classifierLoc := config.GetDefaultLocationForModel(modelName, rp.Location)

	cacheKey := fmt.Sprintf("%s|%s|%s", cleanModel, prompt, additionalInstructions)
	if useCache {
		if cachedVal, ok := rp.classifierCache.Load(cacheKey); ok {
			if cachedStr, ok := cachedVal.(string); ok {
				log.Printf("[Complexity Classifier] Cache HIT: classified complexity as %s", cachedStr)
				return cachedStr, 0, 0, nil
			}
		}
	}

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
		Role  string         `json:"role,omitempty"`
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
				Role:  "user",
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
			MaxOutputTokens: 256,
		},
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, 0, err
	}

	// Resolve regional/global Vertex AI endpoint host for classifier model
	scheme := "https"
	host := config.GetVertexEndpointHost(classifierLoc)
	if rp.Target != nil && (strings.HasPrefix(rp.Target.Host, "127.0.0.1") || strings.HasPrefix(rp.Target.Host, "localhost")) {
		scheme = rp.Target.Scheme
		host = rp.Target.Host
	}

	targetURL := fmt.Sprintf("%s://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		scheme, host, rp.ProjectID, classifierLoc, cleanModel)

	// Tight timeout for classification overhead safety (5s)
	callCtx, cancel := context.WithTimeout(ctx, 5000*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, "POST", targetURL, bytes.NewReader(reqJSON))
	if err != nil {
		return "", 0, 0, err
	}

	token, err := rp.TokenSource.Token()
	if err != nil {
		return "", 0, 0, fmt.Errorf("failed to fetch credentials: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := rp.ClassifierClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyErr, _ := io.ReadAll(resp.Body)
		return "", 0, 0, fmt.Errorf("classifier API returned status %s: %s", resp.Status, string(bodyErr))
	}

	var res classifierResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", 0, 0, err
	}

	if len(res.Candidates) == 0 || len(res.Candidates[0].Content.Parts) == 0 {
		return "", 0, 0, fmt.Errorf("empty candidates in classifier response")
	}

	rawJSON := res.Candidates[0].Content.Parts[0].Text
	var out struct {
		Complexity string `json:"complexity"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &out); err != nil {
		return "", 0, 0, fmt.Errorf("failed to parse output JSON %q: %w", rawJSON, err)
	}

	if useCache && (out.Complexity == "simple" || out.Complexity == "medium" || out.Complexity == "complex") {
		rp.classifierCache.Store(cacheKey, out.Complexity)
	}

	outTokens := res.UsageMetadata.CandidatesTokenCount + res.UsageMetadata.ThoughtsTokenCount
	return out.Complexity, res.UsageMetadata.PromptTokenCount, outTokens, nil
}

// InspectLLMClassifier runs an uncached live generative JSON classification (e.g. gemini-3.5-flash-lite)
// and returns full token telemetry and execution mode for administrative inspection and evaluation.
func (rp *RouterProxy) InspectLLMClassifier(ctx context.Context, prompt string, c config.ComplexityRouting, hasTools, hasMultimodal bool) (decision.DecisionResult, error) {
	features := decision.ExtractPromptFeatures(prompt, hasTools, hasMultimodal)
	if hasTools && c.ForceComplexTools {
		return decision.DecisionResult{
			Tier:          "complex",
			TargetModel:   decision.ResolveModelByTier("complex", c),
			Engine:        "llm",
			ExecutionMode: "policy_bypass",
			Confidence:    0.99,
			Rationale:     "Forced complex tier due to active tools.",
			Features:      features,
		}, nil
	}
	if hasMultimodal && c.ForceComplexMultimodal {
		return decision.DecisionResult{
			Tier:          "complex",
			TargetModel:   decision.ResolveModelByTier("complex", c),
			Engine:        "llm",
			ExecutionMode: "policy_bypass",
			Confidence:    0.99,
			Rationale:     "Forced complex tier due to multimodal payload.",
			Features:      features,
		}, nil
	}

	classifierModel := strings.TrimSpace(c.ClassifierModel)
	if classifierModel == "" {
		classifierModel = "gemini-3.5-flash-lite"
	}
	snippet := prompt
	if len(snippet) > 4000 {
		snippet = snippet[:4000]
	}

	tier, inTok, outTok, err := rp.callLLMClassifierWithUsage(ctx, classifierModel, snippet, c.AdditionalInstructions, false)
	if err != nil {
		return decision.DecisionResult{}, err
	}
	return decision.DecisionResult{
		Tier:          tier,
		TargetModel:   decision.ResolveModelByTier(tier, c),
		Engine:        "llm",
		ExecutionMode: "vertex_generative_json",
		InputTokens:   inTok,
		OutputTokens:  outTok,
		Rationale:     fmt.Sprintf("%s generative JSON: tier=%s in_tokens=%d out_tokens=%d (uncalibrated)", classifierModel, tier, inTok, outTok),
		Features:      features,
	}, nil
}

// resolveComplexityWithClusterProviders enriches a ComplexityRouting configuration with any
// active decision engine endpoints configured in Providers["local"] (DirectEndpoints or DecisionProviders).
func (rp *RouterProxy) resolveComplexityWithClusterProviders(c config.ComplexityRouting) config.ComplexityRouting {
	if rp.Store != nil {
		if provs, err := rp.Store.GetAllProviders(context.Background()); err == nil {
			for _, p := range provs {
				if p.ID == config.ProviderLocal && p.Enabled {
					decision.SyncDecisionProvidersFromLocalConfig(p)
				}
			}
		}
	}
	if strings.TrimSpace(c.GeminiDecisionEndpoint) == "" {
		if ep := decision.GetEngineEndpoint("gemini-decision"); ep != "" {
			c.GeminiDecisionEndpoint = ep
		}
	}
	if strings.TrimSpace(c.CLMEndpoint) == "" {
		if ep := decision.GetEngineEndpoint("contrastive-lm"); ep != "" {
			c.CLMEndpoint = ep
		}
	}
	if strings.TrimSpace(c.JevEndpoint) == "" {
		if ep := decision.GetEngineEndpoint("jev"); ep != "" {
			c.JevEndpoint = ep
		}
	}
	if strings.TrimSpace(c.FastinoEndpoint) == "" {
		if ep := decision.GetEngineEndpoint("fastino"); ep != "" {
			c.FastinoEndpoint = ep
		}
	}
	return c
}

// resolveLocalClusterID checks whether a target location refers to the default "local-cluster"
// or a named LocalCluster configured in Providers["local"].
func (rp *RouterProxy) resolveLocalClusterID(loc string) (string, bool) {
	id, _, ok := rp.resolveLocalCluster(loc)
	return id, ok
}

// resolveLocalCluster returns the resolved cluster ID, optional matched LocalCluster definition, and whether it is a local cluster target.
func (rp *RouterProxy) resolveLocalCluster(loc string) (string, *config.LocalCluster, bool) {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return "", nil, false
	}
	if rp.Store != nil {
		if provs, err := rp.Store.GetAllProviders(context.Background()); err == nil {
			for _, p := range provs {
				if p.ID != config.ProviderLocal {
					continue
				}
				for _, reg := range p.Regions {
					if reg.LocalConfig == nil {
						continue
					}
					for _, cl := range reg.LocalConfig.Clusters {
						if cl.ID == loc || (cl.QueueName != "" && cl.QueueName == loc) {
							clCopy := cl
							return cl.ID, &clCopy, true
						}
					}
				}
			}
		}
	}
	if loc == "local-cluster" {
		return "local-cluster", nil, true
	}
	return "", nil, false
}

// resolveDirectEndpoint checks whether a target location or model maps to an active DirectEndpoint in Providers["local"].
func (rp *RouterProxy) resolveDirectEndpoint(routedModel, loc string) (config.DirectEndpoint, bool) {
	if rp.Store == nil {
		return config.DirectEndpoint{}, false
	}
	loc = strings.TrimSpace(loc)
	strippedModel := config.StripLocationSuffix(strings.TrimSpace(routedModel))
	provs, err := rp.Store.GetAllProviders(context.Background())
	if err != nil {
		return config.DirectEndpoint{}, false
	}
	for _, p := range provs {
		if p.ID != config.ProviderLocal || !p.Enabled {
			continue
		}
		for _, reg := range p.Regions {
			if reg.LocalConfig == nil {
				continue
			}
			for _, ep := range reg.LocalConfig.DirectEndpoints {
				if !ep.Active || strings.TrimSpace(ep.URL) == "" {
					continue
				}
				if ep.Role == "decision_engine" && !strings.EqualFold(ep.ID, loc) && !strings.EqualFold(ep.ClusterID, loc) {
					continue
				}
				if loc != "" && (strings.EqualFold(ep.ID, loc) || (ep.ClusterID != "" && strings.EqualFold(ep.ClusterID, loc))) {
					return ep, true
				}
				if (loc == "" || loc == "local-cluster") && strippedModel != "" && strings.EqualFold(config.StripLocationSuffix(ep.ModelName), strippedModel) {
					return ep, true
				}
			}
		}
	}
	return config.DirectEndpoint{}, false
}

// isDecisionsPassthroughPath checks if a request targets the Gemini Decisions API endpoint directly.
func isDecisionsPassthroughPath(path string) bool {
	cleaned := strings.TrimRight(strings.TrimSpace(path), "/")
	return cleaned == "/v1beta/decisions" || cleaned == "/v1/decisions"
}

// handleDecisionsPassthrough proxies native Gemini Decisions API calls (POST /v1beta/decisions)
// authenticated with Smart Router App API keys/OIDC, scrubbing client credentials and attaching keyless upstream ADC.
func (rp *RouterProxy) handleDecisionsPassthrough(w *responseWriterWrapper, r *http.Request, bodyBytes []byte, app config.App, client config.Client, estimatedTokens int) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "Only POST is supported for /v1beta/decisions.")
		return
	}

	reqModel := decision.GeminiDecisionModel
	var bodyMeta struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(bodyBytes, &bodyMeta); err == nil && strings.TrimSpace(bodyMeta.Model) != "" {
		reqModel = strings.TrimSpace(bodyMeta.Model)
	}
	r.Header.Set("X-Requested-Model", reqModel)
	r.Header.Set("X-Routed-Model", reqModel)
	r.Header.Set("X-Routed-Model-Location", "global")

	effectiveCfg := rp.resolveComplexityWithClusterProviders(app.Complexity)
	endpoint := rp.GeminiDecisionEngine.ResolveEndpoint(effectiveCfg)
	if err := rp.GeminiDecisionEngine.ValidatePassthroughEndpoint(endpoint); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}

	outReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "INTERNAL", fmt.Sprintf("Failed constructing upstream decisions request: %v", err))
		return
	}
	outReq.Header.Set("Content-Type", "application/json")
	if err := rp.GeminiDecisionEngine.AttachUpstreamAuth(r.Context(), outReq, effectiveCfg.SecretRef); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "INTERNAL", fmt.Sprintf("Failed attaching upstream credentials: %v", err))
		return
	}

	httpClient := rp.ClassifierClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := httpClient.Do(outReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "BAD_GATEWAY", fmt.Sprintf("Gemini Decisions API request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "BAD_GATEWAY", fmt.Sprintf("Failed reading Gemini Decisions API response: %v", err))
		return
	}

	if resp.StatusCode == http.StatusOK && !app.OptOutTPM && estimatedTokens > 0 {
		var usageEnvelope struct {
			Usage struct {
				TotalTokens      int `json:"total_tokens"`
				TotalInputTokens int `json:"total_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &usageEnvelope) == nil {
			actual := usageEnvelope.Usage.TotalTokens
			if actual == 0 {
				actual = usageEnvelope.Usage.TotalInputTokens
			}
			if actual > 0 {
				rp.Limiter.AdjustLimiter(app.ID, actual-estimatedTokens)
			}
		}
	}

	w.Header().Set("X-Routed-Model", reqModel)
	w.Header().Set("X-Requested-Model", reqModel)
	w.Header().Set("X-Routed-Model-Location", "global")
	w.Header().Set("X-Client-Tier", client.Tier)
	w.Header().Set("X-App-ID", app.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBytes)
	rp.Scheduler.ReportRequestStatus(reqModel, resp.StatusCode)
}

// hasOnlineCapableRunner checks whether at least one online runner (heartbeat <= 30s) assigned to clusterID supports routedModel.
func (rp *RouterProxy) hasOnlineCapableRunner(clusterID string, matchedCluster *config.LocalCluster, routedModel string) bool {
	if rp.Store == nil {
		return false
	}
	baseModel := config.StripLocationSuffix(strings.TrimSpace(routedModel))
	supports := func(models []string) bool {
		if len(models) == 0 || baseModel == "" {
			return true
		}
		for _, sm := range models {
			t := strings.TrimSpace(sm)
			if t == "*" || strings.EqualFold(config.StripLocationSuffix(t), baseModel) {
				return true
			}
		}
		return false
	}

	for _, r := range rp.Store.GetCachedRunners() {
		if !store.IsRunnerNodeOnline(r.Node) || !supports(r.Node.SupportedModels) {
			continue
		}
		if clusterID == "" || clusterID == "local-cluster" {
			if len(r.AssignedClusters) == 0 {
				return true
			}
		}
		for _, cid := range r.AssignedClusters {
			if cid == clusterID || (matchedCluster != nil && matchedCluster.QueueName != "" && cid == matchedCluster.QueueName) {
				return true
			}
		}
	}

	if matchedCluster != nil {
		for _, n := range matchedCluster.Nodes {
			if store.IsRunnerNodeOnline(n) && supports(n.SupportedModels) {
				return true
			}
		}
	}
	return false
}

// forwardToDirectEndpoint forwards inference requests directly to an unmanaged HTTP server (Ollama, vLLM, SGLang, MLX, llama.cpp).
// Returns true if the request was fully handled (response written to w), or false if a configured cloud FallbackModel was activated so ServeHTTP should continue to Vertex AI.
func (rp *RouterProxy) forwardToDirectEndpoint(w http.ResponseWriter, r *http.Request, ep config.DirectEndpoint, bodyBytes []byte, routedModel, clientTier, appID string, wantOpenAI, openAIStream bool) bool {
	baseURL := strings.TrimRight(strings.TrimSpace(ep.URL), "/")
	targetURL := baseURL
	if !strings.HasSuffix(baseURL, "/chat/completions") && !strings.HasSuffix(baseURL, "/api/chat") && !strings.HasSuffix(baseURL, "/generate") {
		targetURL = baseURL + "/v1/chat/completions"
	}

	targetModel := strings.TrimSpace(ep.ModelName)
	if targetModel == "" {
		targetModel = config.StripLocationSuffix(routedModel)
	}

	isStream := openAIStream || strings.Contains(r.URL.Path, ":streamGenerateContent")

	applyDirectFailover := func(reason string) bool {
		fbRaw := strings.TrimSpace(ep.FallbackModel)
		fbLoc := strings.TrimSpace(ep.FallbackLocation)
		if fbRaw == "" {
			fbRaw = strings.TrimSpace(r.Header.Get("X-Fallback-Target-Model"))
			if fbLoc == "" {
				fbLoc = strings.TrimSpace(r.Header.Get("X-Fallback-Model-Location"))
			}
		}
		if fbRaw == "" {
			return false
		}
		fbModel, fbLocSuffix := config.SplitModelAndLocation(fbRaw)
		if fbLoc == "" {
			fbLoc = fbLocSuffix
		}
		if fbLoc == "" {
			fbLoc = config.GetDefaultLocationForModel(fbModel, rp.Location)
		}
		log.Printf("[Proxy DirectEndpoint Failover] Endpoint %q failed (%s); failing over to model %q in location %q", ep.ID, reason, fbModel, fbLoc)
		r.Header.Set("X-Routed-Model", fbModel)
		r.Header.Set("X-Routed-Model-Location", fbLoc)
		w.Header().Set("X-Cluster-Fallback", "true")
		w.Header().Set("X-Cluster-Fallback-Reason", "direct_endpoint_error")
		return true
	}

	// Build OpenAI-compatible chat request from Gemini bodyBytes (or forward if already OpenAI format)
	var reqPayload []byte
	var rawMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawMap); err == nil {
		if _, hasMessages := rawMap["messages"]; hasMessages {
			rawMap["model"] = targetModel
			rawMap["stream"] = isStream
			reqPayload, _ = json.Marshal(rawMap)
		} else {
			var gReq struct {
				Contents []struct {
					Role  string `json:"role"`
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"contents"`
				SystemInstruction *struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"systemInstruction"`
			}
			_ = json.Unmarshal(bodyBytes, &gReq)
			var msgs []map[string]string
			if gReq.SystemInstruction != nil {
				var sysParts []string
				for _, p := range gReq.SystemInstruction.Parts {
					if p.Text != "" {
						sysParts = append(sysParts, p.Text)
					}
				}
				if len(sysParts) > 0 {
					msgs = append(msgs, map[string]string{"role": "system", "content": strings.Join(sysParts, "\n")})
				}
			}
			for _, c := range gReq.Contents {
				role := c.Role
				if role == "model" {
					role = "assistant"
				} else if role == "" {
					role = "user"
				}
				var parts []string
				for _, p := range c.Parts {
					if p.Text != "" {
						parts = append(parts, p.Text)
					}
				}
				msgs = append(msgs, map[string]string{"role": role, "content": strings.Join(parts, "\n")})
			}
			reqPayload, _ = json.Marshal(map[string]interface{}{
				"model":    targetModel,
				"messages": msgs,
				"stream":   isStream,
			})
		}
	}
	if len(reqPayload) == 0 {
		reqPayload = bodyBytes
	}

	outReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(reqPayload))
	if err != nil {
		if applyDirectFailover(err.Error()) {
			return false
		}
		writeJSONError(w, http.StatusBadGateway, "BAD_GATEWAY", fmt.Sprintf("invalid direct endpoint request: %v", err))
		return true
	}
	outReq.Header.Set("Content-Type", "application/json")
	if isStream {
		outReq.Header.Set("Accept", "text/event-stream")
	}
	if ep.SecretRef != "" {
		if secret := decision.ResolveEngineSecret(ep.Engine, ep.SecretRef); secret != "" {
			outReq.Header.Set("Authorization", "Bearer "+secret)
		}
	}

	client := rp.ClassifierClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(outReq)
	if err != nil {
		if applyDirectFailover(err.Error()) {
			return false
		}
		writeJSONError(w, http.StatusBadGateway, "BAD_GATEWAY", fmt.Sprintf("direct endpoint %q unreachable: %v", ep.ID, err))
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusInternalServerError {
		errBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if applyDirectFailover(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBodyBytes)))) {
			return false
		}
		writeJSONError(w, http.StatusBadGateway, "BAD_GATEWAY", fmt.Sprintf("direct endpoint %q returned %d: %s", ep.ID, resp.StatusCode, strings.TrimSpace(string(errBodyBytes))))
		return true
	}

	locHeader := ep.ClusterID
	if locHeader == "" {
		locHeader = ep.ID
	}
	setDirectAuditHeaders := func() {
		w.Header().Set("X-Routed-Model", routedModel)
		w.Header().Set("X-Requested-Model", r.Header.Get("X-Requested-Model"))
		w.Header().Set("X-Routed-Model-Location", locHeader)
		w.Header().Set("X-Client-Tier", clientTier)
		w.Header().Set("X-App-ID", appID)
		if v := r.Header.Get("X-Router-Engine-Used"); v != "" {
			w.Header().Set("X-Router-Engine-Used", v)
		}
		if v := r.Header.Get("X-Router-Complexity-Tier"); v != "" {
			w.Header().Set("X-Router-Complexity-Tier", v)
		}
		if v := r.Header.Get("X-Router-Confidence"); v != "" {
			w.Header().Set("X-Router-Confidence", v)
		}
		if v := r.Header.Get("X-Router-Local-Eligible"); v != "" {
			w.Header().Set("X-Router-Local-Eligible", v)
		}
	}

	// Real-time SSE streaming path (OpenAI stream:true or Gemini :streamGenerateContent?alt=sse)
	if isStream && resp.StatusCode == http.StatusOK && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		setDirectAuditHeaders()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if wantOpenAI {
				_, _ = w.Write([]byte(line + "\n"))
				if line == "" && flusher != nil {
					flusher.Flush()
				}
				continue
			}
			// Translate OpenAI SSE chunks into Gemini SSE candidate events
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "data:") {
				continue
			}
			dataStr := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if dataStr == "" || dataStr == "[DONE]" {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(dataStr), &chunk) == nil && len(chunk.Choices) > 0 {
				cand := map[string]interface{}{
					"content": map[string]interface{}{
						"role":  "model",
						"parts": []map[string]string{{"text": chunk.Choices[0].Delta.Content}},
					},
				}
				if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
					cand["finishReason"] = "STOP"
				}
				gemEvent, _ := json.Marshal(map[string]interface{}{
					"candidates": []interface{}{cand},
				})
				_, _ = w.Write([]byte("data: " + string(gemEvent) + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[Proxy DirectEndpoint Stream] Scanner error on %q: %v", ep.ID, err)
		}
		return true
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		if applyDirectFailover(err.Error()) {
			return false
		}
		writeJSONError(w, http.StatusBadGateway, "BAD_GATEWAY", fmt.Sprintf("failed reading direct endpoint %q response: %v", ep.ID, err))
		return true
	}
	if resp.StatusCode >= http.StatusBadRequest {
		writeJSONError(w, http.StatusBadGateway, "BAD_GATEWAY", fmt.Sprintf("direct endpoint %q returned %d: %s", ep.ID, resp.StatusCode, strings.TrimSpace(string(respBytes))))
		return true
	}

	setDirectAuditHeaders()

	// Convert between OpenAI choices and Gemini candidates based on client expectation
	var oaiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	isUpstreamOpenAI := json.Unmarshal(respBytes, &oaiResp) == nil && len(oaiResp.Choices) > 0

	if wantOpenAI {
		if isUpstreamOpenAI {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBytes)
			return true
		}
		if oaiPayload, err := translateGeminiToOpenAIResponse(respBytes, routedModel); err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(oaiPayload)
			return true
		}
	} else if isUpstreamOpenAI {
		geminiPayload, _ := json.Marshal(map[string]interface{}{
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"role":  "model",
						"parts": []map[string]string{{"text": oaiResp.Choices[0].Message.Content}},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]int{
				"promptTokenCount":     oaiResp.Usage.PromptTokens,
				"candidatesTokenCount": oaiResp.Usage.CompletionTokens,
				"totalTokenCount":      oaiResp.Usage.TotalTokens,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(geminiPayload)
		return true
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
	return true
}


