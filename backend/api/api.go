package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	store "geminirouter/backend/config"
	"geminirouter/backend/proxy"
	"geminirouter/pkg/config"
	"geminirouter/pkg/decision"
)

// APIController handles the administrative REST APIs on the backend.
type APIController struct {
	Store         *store.ConfigStore
	Scheduler     *proxy.RequestScheduler
	Queue         *proxy.ClusterQueue
	Sessions      *config.SessionRegistry
	JevEngine     *decision.JevDecisionEngine
	FastinoEngine *decision.FastinoDecisionEngine
	CLMEngine     *decision.CLMContrastiveEngine
	SharedSecret  string

	// Thread-safe dynamic registry of active nodes and their cluster assignments
	runnersMu         sync.RWMutex
	registeredRunners map[string]config.Node
	nodeAttachments   map[string][]string
}

// NewAPIController initializes a new backend API controller.
func NewAPIController(store *store.ConfigStore, scheduler *proxy.RequestScheduler, queue *proxy.ClusterQueue, sharedSecret string) *APIController {
	return &APIController{
		Store:             store,
		Scheduler:         scheduler,
		Queue:             queue,
		Sessions:          config.NewSessionRegistry(30 * time.Minute),
		JevEngine:         decision.NewJevDecisionEngine(),
		FastinoEngine:     decision.NewFastinoDecisionEngine(),
		CLMEngine:         decision.NewCLMContrastiveEngine(),
		SharedSecret:      sharedSecret,
		registeredRunners: make(map[string]config.Node),
		nodeAttachments:   make(map[string][]string),
	}
}

// AuthMiddleware validates incoming API requests using X-Shared-Secret, Bearer shared secret, or Google OIDC ID token.
//
// OIDC tokens are accepted only when they were minted for one of API_OIDC_AUDIENCES and the
// verified email claim belongs to API_ALLOWED_CALLERS. If either variable is empty, OIDC is
// rejected (fail closed) and only the shared-secret path can authenticate.
func (ac *APIController) AuthMiddleware(next http.Handler) http.Handler {
	oidc := loadOIDCPolicy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrap state-changing requests in MaxBytesReader (1MB limit) to prevent memory exhaustion attacks
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
		}

		// Local-dev relaxations never apply on Cloud Run (K_SERVICE is always set there).
		isLocalDev := os.Getenv("LOCAL_DEV") == "true" && os.Getenv("K_SERVICE") == ""

		secret := r.Header.Get("X-Shared-Secret")
		bearerToken := ""
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			bearerToken = strings.TrimPrefix(authHeader, "Bearer ")
		}

		authenticated := false
		if ac.SharedSecret != "" && (secretEquals(secret, ac.SharedSecret) || secretEquals(bearerToken, ac.SharedSecret)) {
			authenticated = true
		}

		if !authenticated && !isLocalDev && bearerToken != "" {
			if err := oidc.authorize(r.Context(), bearerToken); err == nil {
				authenticated = true
			}
		}

		// In production (LOCAL_DEV != true), we MUST enforce API authentication
		if !isLocalDev && !authenticated {
			http.Error(w, "Unauthorized: Valid shared secret or OIDC token required", http.StatusUnauthorized)
			return
		}

		if ac.SharedSecret != "" && !authenticated {
			http.Error(w, "Unauthorized: Invalid API shared secret", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secretEquals compares secrets in constant time.
func secretEquals(got, want string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// RegisterRoutes registers all administrative REST API routes under /api/*.
func (ac *APIController) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("/api/apps", ac.AuthMiddleware(http.HandlerFunc(ac.HandleApps)))
	mux.Handle("/api/apps/lookup", ac.AuthMiddleware(http.HandlerFunc(ac.HandleAppsLookup)))
	mux.Handle("/api/clients", ac.AuthMiddleware(http.HandlerFunc(ac.HandleClients)))
	mux.Handle("/api/keys", ac.AuthMiddleware(http.HandlerFunc(ac.HandleKeys)))
	mux.Handle("/api/keys/revoke", ac.AuthMiddleware(http.HandlerFunc(ac.HandleKeysRevoke)))
	mux.Handle("/api/rules", ac.AuthMiddleware(http.HandlerFunc(ac.HandleRules)))
	mux.Handle("/api/headers", ac.AuthMiddleware(http.HandlerFunc(ac.HandleHeaders)))
	mux.Handle("/api/complexity", ac.AuthMiddleware(http.HandlerFunc(ac.HandleComplexity)))
	mux.Handle("/api/models", ac.AuthMiddleware(http.HandlerFunc(ac.HandleModels)))
	mux.Handle("/api/models/refresh", ac.AuthMiddleware(http.HandlerFunc(ac.HandleModelsRefresh)))
	mux.Handle("/api/models/toggle", ac.AuthMiddleware(http.HandlerFunc(ac.HandleModelsToggle)))
	mux.Handle("/api/queue", ac.AuthMiddleware(http.HandlerFunc(ac.HandleQueue)))
	mux.Handle("/api/v1/queue", ac.AuthMiddleware(http.HandlerFunc(ac.HandleQueue)))
	mux.Handle("/api/providers", ac.AuthMiddleware(http.HandlerFunc(ac.HandleProviders)))

	// Dynamic Local Cluster REST API routes (protected by AuthMiddleware)
	mux.Handle("/api/v1/cluster/runners/register", ac.AuthMiddleware(http.HandlerFunc(ac.HandleClusterRegisterNode)))
	mux.Handle("/api/v1/cluster/register", ac.AuthMiddleware(http.HandlerFunc(ac.HandleClusterRegisterNode)))
	mux.Handle("/v1/clusters/register", ac.AuthMiddleware(http.HandlerFunc(ac.HandleClusterRegisterNode)))
	mux.Handle("/api/v1/cluster/runners/heartbeat", ac.AuthMiddleware(http.HandlerFunc(ac.HandleClusterHeartbeat)))
	mux.Handle("/api/v1/cluster/queue/poll", ac.AuthMiddleware(http.HandlerFunc(ac.HandleClusterPollQueue)))
	mux.Handle("/api/v1/cluster/queue/resolve", ac.AuthMiddleware(http.HandlerFunc(ac.HandleClusterResolveJob)))

	// Registered Runners Admin REST API routes
	mux.Handle("/api/v1/admin/runners", ac.AuthMiddleware(http.HandlerFunc(ac.HandleAdminRunnersList)))
	mux.Handle("/api/v1/admin/runners/attach", ac.AuthMiddleware(http.HandlerFunc(ac.HandleAdminRunnersAttach)))
	mux.Handle("/api/v1/admin/runners/deregister", ac.AuthMiddleware(http.HandlerFunc(ac.HandleAdminRunnersDeregister)))

	// Multi-Turn Session Affinity & Agent Task Switching routes
	mux.Handle("/api/sessions", ac.AuthMiddleware(http.HandlerFunc(ac.HandleSessions)))
	mux.Handle("/api/v1/sessions", ac.AuthMiddleware(http.HandlerFunc(ac.HandleSessions)))
	mux.Handle("/api/v1/sessions/task", ac.AuthMiddleware(http.HandlerFunc(ac.HandleSessionTask)))
	mux.Handle("/api/v1/sessions/reset", ac.AuthMiddleware(http.HandlerFunc(ac.HandleSessionReset)))

	// Smart Routing Explainability & Inspector routes
	mux.Handle("/api/complexity/inspect", ac.AuthMiddleware(http.HandlerFunc(ac.HandleComplexityInspect)))
	mux.Handle("/api/v1/complexity/inspect", ac.AuthMiddleware(http.HandlerFunc(ac.HandleComplexityInspect)))
	mux.Handle("/api/complexity/keys", ac.AuthMiddleware(http.HandlerFunc(ac.HandleComplexityKeys)))
	mux.Handle("/api/v1/complexity/keys", ac.AuthMiddleware(http.HandlerFunc(ac.HandleComplexityKeys)))
	mux.Handle("/api/decision/providers", ac.AuthMiddleware(http.HandlerFunc(ac.HandleComplexityKeys)))
	mux.Handle("/api/v1/decision/providers", ac.AuthMiddleware(http.HandlerFunc(ac.HandleComplexityKeys)))
	mux.Handle("/api/decision/providers/probe", ac.AuthMiddleware(http.HandlerFunc(ac.HandleDecisionProviderProbe)))
	mux.Handle("/api/v1/decision/providers/probe", ac.AuthMiddleware(http.HandlerFunc(ac.HandleDecisionProviderProbe)))
}

// Response helpers

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Internal Server Error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}

// APIS handlers

func (ac *APIController) HandleApps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		apps, err := ac.Store.GetAllApps(ctx)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, apps)

	case http.MethodPost:
		var app config.App
		if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if app.ID == "" {
			respondWithError(w, http.StatusBadRequest, "Missing app ID")
			return
		}
		if err := ac.Store.SaveApp(ctx, app); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, app)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			respondWithError(w, http.StatusBadRequest, "Missing app id query parameter")
			return
		}
		if err := ac.Store.DeleteApp(ctx, id); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (ac *APIController) HandleAppsLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		respondWithError(w, http.StatusBadRequest, "Missing app ID parameter")
		return
	}
	app, ok := ac.Store.LookupApp(id)
	if !ok {
		respondWithError(w, http.StatusNotFound, "App not found")
		return
	}
	respondWithJSON(w, http.StatusOK, app)
}

func (ac *APIController) HandleClients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		clients, err := ac.Store.GetAllClients(ctx)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, clients)

	case http.MethodPost:
		var client config.Client
		if err := json.NewDecoder(r.Body).Decode(&client); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if client.ID == "" {
			respondWithError(w, http.StatusBadRequest, "Missing client ID")
			return
		}
		if err := ac.Store.SaveClient(ctx, client); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, client)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			respondWithError(w, http.StatusBadRequest, "Missing client id query parameter")
			return
		}
		if err := ac.Store.DeleteClient(ctx, id); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (ac *APIController) HandleKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		keys, err := ac.Store.GetAllKeys(ctx)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, keys)

	case http.MethodPost:
		var key config.APIKey
		if err := json.NewDecoder(r.Body).Decode(&key); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if err := ac.Store.SaveKey(ctx, key); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, key)

	default:
		w.Header().Set("Allow", "GET, POST")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (ac *APIController) HandleKeysRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	hash := r.URL.Query().Get("hash")
	if hash == "" {
		respondWithError(w, http.StatusBadRequest, "Missing key hash")
		return
	}
	if err := ac.Store.RevokeKey(ctx, hash); err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (ac *APIController) HandleRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		rules, err := ac.Store.GetAllRules(ctx)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, rules)

	case http.MethodPost:
		var rule config.RoutingRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if err := ac.Store.SaveRule(ctx, rule); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, rule)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			respondWithError(w, http.StatusBadRequest, "Missing rule id")
			return
		}
		if err := ac.Store.DeleteRule(ctx, id); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (ac *APIController) HandleHeaders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		headers, err := ac.Store.GetAllHeaders(ctx)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, headers)

	case http.MethodPost:
		var header config.CustomHeader
		if err := json.NewDecoder(r.Body).Decode(&header); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if err := ac.Store.SaveHeader(ctx, header); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, header)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			respondWithError(w, http.StatusBadRequest, "Missing header id")
			return
		}
		if err := ac.Store.DeleteHeader(ctx, id); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (ac *APIController) HandleComplexity(w http.ResponseWriter, r *http.Request) {
	// Complexity routing settings are linked directly to App boundary models now.
	// Hence we return the complexity settings for an App ID parameter.
	if r.Method != http.MethodGet {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	id := r.URL.Query().Get("app_id")
	if id == "" {
		respondWithError(w, http.StatusBadRequest, "Missing app_id")
		return
	}
	app, ok := ac.Store.LookupApp(id)
	if !ok {
		respondWithError(w, http.StatusNotFound, "App not found")
		return
	}
	respondWithJSON(w, http.StatusOK, app.Complexity)
}

func (ac *APIController) HandleModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		models, err := ac.Store.GetAllModels(ctx)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, models)

	case http.MethodPost:
		var m config.ModelConfig
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if err := ac.Store.SaveModel(ctx, m); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, m)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			respondWithError(w, http.StatusBadRequest, "Missing model id query parameter")
			return
		}
		if err := ac.Store.DeleteModel(ctx, id); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (ac *APIController) HandleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	if ac.Store == nil {
		respondWithError(w, http.StatusInternalServerError, "Store not initialized")
		return
	}

	existingModels, err := ac.Store.GetAllModels(ctx)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	known := make(map[string]config.ModelConfig, len(existingModels))
	for _, m := range existingModels {
		known[m.ID] = m
	}

	baselineModels := []config.ModelConfig{
		{ID: "gemini-4-argon", DisplayName: "gemini-4-argon", Location: "global", Type: "foundation", Active: true},
		{ID: "gemini-3.8-flash", DisplayName: "gemini-3.8-flash", Location: "global", Type: "foundation", Active: true},
		{ID: "gemini-3.5-flash", DisplayName: "gemini-3.5-flash", Location: "global", Type: "foundation", Active: true},
		{ID: "gemini-3.5-flash-lite", DisplayName: "gemini-3.5-flash-lite", Location: "global", Type: "foundation", Active: true},
		{ID: "gemini-3.1-pro", DisplayName: "gemini-3.1-pro", Location: "global", Type: "foundation", Active: true},
		{ID: "gemini-2.5-pro", DisplayName: "gemini-2.5-pro", Location: "us-central1", Type: "foundation", Active: true},
		{ID: "gemini-2.5-flash", DisplayName: "gemini-2.5-flash", Location: "us-central1", Type: "foundation", Active: true},
		{ID: "gemini-2.5-flash-lite", DisplayName: "gemini-2.5-flash-lite", Location: "us-central1", Type: "foundation", Active: true},
	}

	for _, bm := range baselineModels {
		_, hasBare := known[bm.ID]
		_, hasSuffixed := known[fmt.Sprintf("%s@%s", bm.ID, bm.Location)]
		if !hasBare && !hasSuffixed {
			_ = ac.Store.SaveModel(ctx, bm)
			known[bm.ID] = bm
		}
	}

	if providers, pErr := ac.Store.GetAllProviders(ctx); pErr == nil {
		for _, prov := range providers {
			if !prov.Enabled {
				continue
			}
			for _, reg := range prov.Regions {
				if !reg.Active || reg.LocalConfig == nil {
					continue
				}
				for _, ep := range reg.LocalConfig.DirectEndpoints {
					if ep.ModelName != "" {
						if _, ok := known[ep.ModelName]; !ok {
							m := config.ModelConfig{
								ID:          ep.ModelName,
								DisplayName: ep.ModelName,
								Location:    reg.Code,
								Type:        "local",
								Active:      ep.Active,
							}
							_ = ac.Store.SaveModel(ctx, m)
							known[m.ID] = m
						}
					}
				}
				for _, cl := range reg.LocalConfig.Clusters {
					for _, n := range cl.Nodes {
						for _, sm := range n.SupportedModels {
							if sm != "" {
								if _, ok := known[sm]; !ok {
									m := config.ModelConfig{
										ID:          sm,
										DisplayName: sm,
										Location:    cl.ID,
										Type:        "local",
										Active:      true,
									}
									_ = ac.Store.SaveModel(ctx, m)
									known[m.ID] = m
								}
							}
						}
					}
				}
			}
		}
	}

	updatedModels, _ := ac.Store.GetAllModels(ctx)
	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"status": "refreshed",
		"count":  len(updatedModels),
	})
}

func (ac *APIController) HandleModelsToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		respondWithError(w, http.StatusBadRequest, "Missing model ID")
		return
	}

	allModels, err := ac.Store.GetAllModels(ctx)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var foundModel config.ModelConfig
	found := false
	for _, m := range allModels {
		if m.ID == id {
			foundModel = m
			found = true
			break
		}
	}

	if !found {
		respondWithError(w, http.StatusNotFound, "Model not found")
		return
	}

	foundModel.Active = !foundModel.Active
	if err := ac.Store.SaveModel(ctx, foundModel); err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "toggled", "active": fmt.Sprintf("%t", foundModel.Active)})
}

func (ac *APIController) HandleQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if ac.Scheduler == nil && ac.Queue == nil {
		respondWithError(w, http.StatusInternalServerError, "Scheduler not initialized")
		return
	}
	var status []config.QueueSnapshotItem
	if ac.Scheduler != nil {
		status = ac.Scheduler.GetQueueStatus()
		for i := range status {
			if status[i].QueueSource == "" {
				status[i].QueueSource = "scheduler"
			}
		}
	}
	if ac.Queue != nil {
		clusterSnapshot := ac.Queue.GetQueueSnapshot()
		status = append(status, clusterSnapshot...)
	}
	if status == nil {
		status = []config.QueueSnapshotItem{}
	}
	respondWithJSON(w, http.StatusOK, status)
}

func (ac *APIController) HandleProviders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		providers, err := ac.Store.GetAllProviders(ctx)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, providers)

	case http.MethodPost:
		var provider config.ProviderConfig
		if err := json.NewDecoder(r.Body).Decode(&provider); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if provider.ID == "" {
			respondWithError(w, http.StatusBadRequest, "Missing provider ID")
			return
		}
		if err := ac.Store.SaveProvider(ctx, provider); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, provider)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			respondWithError(w, http.StatusBadRequest, "Missing provider id query parameter")
			return
		}
		if err := ac.Store.DeleteProvider(ctx, id); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func calculateMaxModelSize(mem int) int {
	if mem < 8 {
		return 2
	}
	if mem <= 16 {
		return 8
	}
	if mem <= 32 {
		return 16
	}
	if mem <= 64 {
		return 32
	}
	if mem <= 128 {
		return 64
	}
	return 128
}

func calculateMaxConcurrency(mem int) int {
	if mem < 8 {
		return 1
	}
	if mem <= 16 {
		return 2
	}
	if mem <= 32 {
		return 4
	}
	if mem <= 64 {
		return 6
	}
	if mem <= 128 {
		return 8
	}
	return 16
}

var modelParamRegex = regexp.MustCompile(`(?i)(?:^|[-_:.])(\d+)b(?:$|[-_:.])`)

func estimateModelSizeGB(modelID string) int {
	matches := modelParamRegex.FindStringSubmatch(strings.TrimSpace(modelID))
	if len(matches) >= 2 {
		if val, err := strconv.Atoi(matches[1]); err == nil && val > 0 {
			return val
		}
	}
	return 1
}

func (ac *APIController) HandleClusterRegisterNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var regReq struct {
		config.Node
		ClusterID string `json:"cluster_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&regReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	node := regReq.Node
	if node.ID == "" {
		respondWithError(w, http.StatusBadRequest, "Missing node ID")
		return
	}

	incomingMaxModelSize := node.MaxModelSizeGB
	incomingMaxConcurrent := node.MaxConcurrent

	var persistedRunner *config.RunnerResponse
	if ac.Store != nil {
		if persisted, err := ac.Store.GetRegisteredRunners(r.Context()); err == nil {
			for i := range persisted {
				if persisted[i].Node.ID == node.ID {
					pr := persisted[i]
					persistedRunner = &pr
					break
				}
			}
		}
	}

	// Dynamic capacity limit checks (only calculate defaults when not explicitly provided)
	if node.MaxModelSizeGB <= 0 {
		if persistedRunner != nil && persistedRunner.Node.MaxModelSizeGB > 0 {
			node.MaxModelSizeGB = persistedRunner.Node.MaxModelSizeGB
		} else {
			node.MaxModelSizeGB = calculateMaxModelSize(node.MemoryAllocatedGB)
		}
	}
	if node.MaxConcurrent <= 0 {
		if persistedRunner != nil && persistedRunner.Node.MaxConcurrent > 0 {
			node.MaxConcurrent = persistedRunner.Node.MaxConcurrent
		} else {
			node.MaxConcurrent = calculateMaxConcurrency(node.MemoryAllocatedGB)
		}
	}
	if persistedRunner != nil {
		if persistedRunner.Node.Name != "" && (node.Name == "" || node.Name == "Dynamic Connected Runner" || node.Name == "Dynamic Connected Node") {
			node.Name = persistedRunner.Node.Name
		}
		if node.MemoryAllocatedGB <= 0 && persistedRunner.Node.MemoryAllocatedGB > 0 {
			node.MemoryAllocatedGB = persistedRunner.Node.MemoryAllocatedGB
		}
		if node.ComputeGPUCores <= 0 && persistedRunner.Node.ComputeGPUCores > 0 {
			node.ComputeGPUCores = persistedRunner.Node.ComputeGPUCores
		}
		if len(node.SupportedModels) == 0 && len(persistedRunner.Node.SupportedModels) > 0 {
			node.SupportedModels = persistedRunner.Node.SupportedModels
		}
		if incomingMaxModelSize <= 0 && persistedRunner.Node.MaxModelSizeGB > 0 {
			node.MaxModelSizeGB = persistedRunner.Node.MaxModelSizeGB
		}
		if incomingMaxConcurrent <= 0 && persistedRunner.Node.MaxConcurrent > 0 {
			node.MaxConcurrent = persistedRunner.Node.MaxConcurrent
		}
	}

	node.Status = "online"
	node.LastHeartbeat = time.Now()

	qClusterID := strings.TrimSpace(r.URL.Query().Get("cluster_id"))
	if qClusterID == "" {
		qClusterID = strings.TrimSpace(regReq.ClusterID)
	}

	ac.runnersMu.Lock()
	_, hadPriorInMemory := ac.registeredRunners[node.ID]
	ac.registeredRunners[node.ID] = node
	clusters := ac.nodeAttachments[node.ID]
	if persistedRunner != nil && persistedRunner.AssignedClusters != nil {
		clusters = persistedRunner.AssignedClusters
		ac.nodeAttachments[node.ID] = clusters
	} else if !hadPriorInMemory && persistedRunner == nil && len(clusters) == 0 {
		if qClusterID != "" {
			clusters = []string{qClusterID}
		} else {
			clusters = []string{"local-cluster"}
		}
		ac.nodeAttachments[node.ID] = clusters
	}
	if clusters == nil {
		clusters = []string{}
	}
	ac.runnersMu.Unlock()

	if ac.Store != nil {
		_ = ac.Store.SaveRegisteredRunner(r.Context(), config.RunnerResponse{
			Node:             node,
			AssignedClusters: clusters,
		})
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

func (ac *APIController) HandleClusterHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var hb struct {
		NodeID    string `json:"node_id"`
		ClusterID string `json:"cluster_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	var persistedRunner *config.RunnerResponse
	if ac.Store != nil && hb.NodeID != "" {
		if persisted, err := ac.Store.GetRegisteredRunners(r.Context()); err == nil {
			for i := range persisted {
				if persisted[i].Node.ID == hb.NodeID {
					pr := persisted[i]
					persistedRunner = &pr
					break
				}
			}
		}
	}

	ac.runnersMu.Lock()
	var updatedNode config.Node
	node, ok := ac.registeredRunners[hb.NodeID]
	hadPriorRunner := ok || persistedRunner != nil
	if ok {
		if persistedRunner != nil {
			if persistedRunner.Node.Name != "" && persistedRunner.Node.Name != "Dynamic Connected Node" && persistedRunner.Node.Name != "Dynamic Connected Runner" {
				node.Name = persistedRunner.Node.Name
			}
			if persistedRunner.Node.MemoryAllocatedGB > 0 {
				node.MemoryAllocatedGB = persistedRunner.Node.MemoryAllocatedGB
			}
			if persistedRunner.Node.ComputeGPUCores > 0 {
				node.ComputeGPUCores = persistedRunner.Node.ComputeGPUCores
			}
			if persistedRunner.Node.MaxModelSizeGB > 0 {
				node.MaxModelSizeGB = persistedRunner.Node.MaxModelSizeGB
			}
			if persistedRunner.Node.MaxConcurrent > 0 {
				node.MaxConcurrent = persistedRunner.Node.MaxConcurrent
			}
			if len(persistedRunner.Node.SupportedModels) > 0 {
				node.SupportedModels = persistedRunner.Node.SupportedModels
			}
			if persistedRunner.AssignedClusters != nil {
				ac.nodeAttachments[hb.NodeID] = persistedRunner.AssignedClusters
			}
		}
		node.Status = "online"
		node.LastHeartbeat = time.Now()
		ac.registeredRunners[hb.NodeID] = node
		updatedNode = node
	} else if persistedRunner != nil {
		updatedNode = persistedRunner.Node
		updatedNode.Status = "online"
		updatedNode.LastHeartbeat = time.Now()
		ac.registeredRunners[hb.NodeID] = updatedNode
		if persistedRunner.AssignedClusters != nil {
			ac.nodeAttachments[hb.NodeID] = persistedRunner.AssignedClusters
		}
	} else {
		// Auto-register dynamic node on heartbeat
		newNode := config.Node{
			ID:                hb.NodeID,
			Name:              "Dynamic Connected Node",
			Status:            "online",
			LastHeartbeat:     time.Now(),
			MemoryAllocatedGB: 16,
			ComputeGPUCores:   8,
			SupportedModels:   []string{"gemma-4-4b", "gemma-4-26b"},
			MaxModelSizeGB:    8,
			MaxConcurrent:     2,
		}
		ac.registeredRunners[hb.NodeID] = newNode
		updatedNode = newNode
	}

	// Only auto-associate on heartbeat when the node is newly discovered (!hadPriorRunner)
	// so administrative cluster edits (including detaching local-cluster) are never overwritten.
	if !hadPriorRunner {
		targetCluster := strings.TrimSpace(hb.ClusterID)
		if targetCluster == "" {
			targetCluster = "local-cluster"
		}
		alreadyAssigned := false
		for _, assigned := range ac.nodeAttachments[hb.NodeID] {
			if assigned == targetCluster {
				alreadyAssigned = true
				break
			}
		}
		if !alreadyAssigned {
			ac.nodeAttachments[hb.NodeID] = append(ac.nodeAttachments[hb.NodeID], targetCluster)
		}
	}
	clusters := ac.nodeAttachments[hb.NodeID]
	if clusters == nil {
		clusters = []string{}
	}
	maxSizeLimit := updatedNode.MaxModelSizeGB
	if maxSizeLimit == 0 {
		maxSizeLimit = 32
	}
	maxConcurrentLimit := updatedNode.MaxConcurrent
	if maxConcurrentLimit == 0 {
		maxConcurrentLimit = 2
	}
	ac.runnersMu.Unlock()

	if ac.Store != nil && updatedNode.ID != "" {
		_ = ac.Store.SaveRegisteredRunner(r.Context(), config.RunnerResponse{
			Node:             updatedNode,
			AssignedClusters: clusters,
		})
	}

	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"status":            "alive",
		"max_model_size_gb": maxSizeLimit,
		"max_concurrent":    maxConcurrentLimit,
		"supported_models":  updatedNode.SupportedModels,
		"assigned_clusters": clusters,
	})
}

func (ac *APIController) HandleClusterPollQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if ac.Queue == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Local cluster queue not enabled")
		return
	}
	var req struct {
		NodeID          string   `json:"node_id"`
		ClusterID       string   `json:"cluster_id"`
		SupportedModels []string `json:"supported_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	// Check if runner has administrative cluster mappings, model lists, and capacity limits
	var allowedClusters []string
	var runnerNode *config.Node
	isRegisteredRunner := false
	if req.NodeID != "" {
		ac.runnersMu.RLock()
		if n, ok := ac.registeredRunners[req.NodeID]; ok {
			nCopy := n
			runnerNode = &nCopy
			isRegisteredRunner = true
		}
		if att, hasAtt := ac.nodeAttachments[req.NodeID]; hasAtt {
			allowedClusters = att
			isRegisteredRunner = true
		}
		ac.runnersMu.RUnlock()

		if ac.Store != nil {
			if persisted, pErr := ac.Store.GetRegisteredRunners(r.Context()); pErr == nil {
				for _, pr := range persisted {
					if pr.Node.ID == req.NodeID {
						isRegisteredRunner = true
						pCopy := pr.Node
						if runnerNode != nil {
							if pCopy.MaxModelSizeGB <= 0 {
								pCopy.MaxModelSizeGB = runnerNode.MaxModelSizeGB
							}
							if pCopy.MaxConcurrent <= 0 {
								pCopy.MaxConcurrent = runnerNode.MaxConcurrent
							}
							if len(pCopy.SupportedModels) == 0 {
								pCopy.SupportedModels = runnerNode.SupportedModels
							}
						}
						runnerNode = &pCopy
						if pr.AssignedClusters != nil {
							allowedClusters = pr.AssignedClusters
						}
						break
					}
				}
			}
		}
	}

	if isRegisteredRunner && len(allowedClusters) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !isRegisteredRunner && strings.TrimSpace(req.ClusterID) != "" {
		allowedClusters = []string{strings.TrimSpace(req.ClusterID)}
	}

	effectiveModels := req.SupportedModels
	if len(effectiveModels) == 0 && runnerNode != nil && len(runnerNode.SupportedModels) > 0 {
		effectiveModels = runnerNode.SupportedModels
	} else if runnerNode != nil && len(runnerNode.SupportedModels) > 0 && len(req.SupportedModels) > 0 {
		allowedSet := make(map[string]bool, len(runnerNode.SupportedModels))
		for _, m := range runnerNode.SupportedModels {
			allowedSet[strings.TrimSpace(m)] = true
		}
		filtered := make([]string, 0, len(effectiveModels))
		for _, m := range effectiveModels {
			if allowedSet[strings.TrimSpace(m)] {
				filtered = append(filtered, m)
			}
		}
		effectiveModels = filtered
	}

	if runnerNode != nil && runnerNode.MaxModelSizeGB > 0 {
		filtered := make([]string, 0, len(effectiveModels))
		for _, m := range effectiveModels {
			if estimateModelSizeGB(m) <= runnerNode.MaxModelSizeGB {
				filtered = append(filtered, m)
			}
		}
		effectiveModels = filtered
	}

	hadRequestedModels := len(req.SupportedModels) > 0 || (runnerNode != nil && len(runnerNode.SupportedModels) > 0)
	if hadRequestedModels && len(effectiveModels) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	maxConcurrent := 0
	if runnerNode != nil {
		maxConcurrent = runnerNode.MaxConcurrent
	}

	job, found := ac.Queue.PollForNode(req.NodeID, effectiveModels, allowedClusters, maxConcurrent)
	if !found {
		w.WriteHeader(http.StatusNoContent) // 204 No Content
		return
	}

	respondWithJSON(w, http.StatusOK, job)
}

func (ac *APIController) HandleClusterResolveJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if ac.Queue == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Local cluster queue not enabled")
		return
	}
	var res struct {
		JobID      string `json:"job_id"`
		Payload    []byte `json:"payload"`
		StatusCode int    `json:"status_code"`
		Error      string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	var err error
	if res.Error != "" {
		err = fmt.Errorf("%s", res.Error)
	}

	result := config.QueueResult{
		Payload:    res.Payload,
		StatusCode: res.StatusCode,
		Error:      err,
	}

	if resolved := ac.Queue.Resolve(res.JobID, result); !resolved {
		respondWithError(w, http.StatusNotFound, "Job not found or already expired")
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (ac *APIController) HandleAdminRunnersList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var runner config.RunnerResponse
		if err := json.NewDecoder(r.Body).Decode(&runner); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
		if runner.Node.ID == "" {
			respondWithError(w, http.StatusBadRequest, "Missing node ID")
			return
		}
		if runner.AssignedClusters == nil {
			runner.AssignedClusters = []string{}
		}
		ac.runnersMu.Lock()
		ac.registeredRunners[runner.Node.ID] = runner.Node
		ac.nodeAttachments[runner.Node.ID] = runner.AssignedClusters
		ac.runnersMu.Unlock()

		if ac.Store != nil {
			if err := ac.Store.SaveRegisteredRunner(r.Context(), runner); err != nil {
				respondWithError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}

	if r.Method != http.MethodGet {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	merged := make(map[string]config.RunnerResponse)
	if ac.Store != nil {
		if persisted, err := ac.Store.GetRegisteredRunners(r.Context()); err == nil {
			for _, pr := range persisted {
				if pr.Node.ID != "" {
					if pr.AssignedClusters == nil {
						pr.AssignedClusters = []string{}
					}
					if !pr.Node.LastHeartbeat.IsZero() && time.Since(pr.Node.LastHeartbeat) > 30*time.Second && pr.Node.Status == "online" {
						pr.Node.Status = "offline"
					}
					merged[pr.Node.ID] = pr
				}
			}
		}
	}

	ac.runnersMu.RLock()
	for id, node := range ac.registeredRunners {
		clusters := ac.nodeAttachments[id]
		if existing, ok := merged[id]; ok {
			if existing.Node.Name != "" && existing.Node.Name != "Dynamic Connected Node" && existing.Node.Name != "Dynamic Connected Runner" {
				node.Name = existing.Node.Name
			}
			if existing.Node.MemoryAllocatedGB > 0 {
				node.MemoryAllocatedGB = existing.Node.MemoryAllocatedGB
			}
			if existing.Node.ComputeGPUCores > 0 {
				node.ComputeGPUCores = existing.Node.ComputeGPUCores
			}
			if existing.Node.MaxModelSizeGB > 0 {
				node.MaxModelSizeGB = existing.Node.MaxModelSizeGB
			}
			if existing.Node.MaxConcurrent > 0 {
				node.MaxConcurrent = existing.Node.MaxConcurrent
			}
			if len(existing.Node.SupportedModels) > 0 {
				node.SupportedModels = existing.Node.SupportedModels
			}
			if existing.AssignedClusters != nil {
				clusters = existing.AssignedClusters
			}
		}
		if clusters == nil {
			clusters = []string{}
		}
		if !node.LastHeartbeat.IsZero() && time.Since(node.LastHeartbeat) > 30*time.Second && node.Status == "online" {
			node.Status = "offline"
		}
		merged[id] = config.RunnerResponse{
			Node:             node,
			AssignedClusters: clusters,
		}
	}
	ac.runnersMu.RUnlock()

	keys := make([]string, 0, len(merged))
	for id := range merged {
		keys = append(keys, id)
	}
	sort.Strings(keys)

	list := make([]config.RunnerResponse, 0, len(keys))
	for _, id := range keys {
		list = append(list, merged[id])
	}

	respondWithJSON(w, http.StatusOK, list)
}

func (ac *APIController) HandleAdminRunnersAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		NodeID         string   `json:"node_id"`
		ClusterIDs     []string `json:"cluster_ids"`
		MaxModelSizeGB int      `json:"max_model_size_gb,omitempty"`
		MaxConcurrent  int      `json:"max_concurrent,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.NodeID == "" {
		respondWithError(w, http.StatusBadRequest, "Missing node ID")
		return
	}

	ac.runnersMu.Lock()
	ac.nodeAttachments[req.NodeID] = req.ClusterIDs
	node, exists := ac.registeredRunners[req.NodeID]
	if exists {
		if req.MaxModelSizeGB > 0 {
			node.MaxModelSizeGB = req.MaxModelSizeGB
		}
		if req.MaxConcurrent > 0 {
			node.MaxConcurrent = req.MaxConcurrent
		}
		ac.registeredRunners[req.NodeID] = node
	}
	ac.runnersMu.Unlock()

	if ac.Store != nil {
		_ = ac.Store.AttachRunnerToClusters(r.Context(), req.NodeID, req.ClusterIDs)
		if req.MaxModelSizeGB > 0 || req.MaxConcurrent > 0 {
			if !exists {
				node = config.Node{ID: req.NodeID}
				if persisted, err := ac.Store.GetRegisteredRunners(r.Context()); err == nil {
					for _, pr := range persisted {
						if pr.Node.ID == req.NodeID {
							node = pr.Node
							break
						}
					}
				}
				if req.MaxModelSizeGB > 0 {
					node.MaxModelSizeGB = req.MaxModelSizeGB
				}
				if req.MaxConcurrent > 0 {
					node.MaxConcurrent = req.MaxConcurrent
				}
			}
			_ = ac.Store.SaveRegisteredRunner(r.Context(), config.RunnerResponse{
				Node:             node,
				AssignedClusters: req.ClusterIDs,
			})
		}
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "attached"})
}

func (ac *APIController) HandleAdminRunnersDeregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	nodeID := r.URL.Query().Get("id")
	if nodeID == "" {
		respondWithError(w, http.StatusBadRequest, "Missing runner id")
		return
	}

	ac.runnersMu.Lock()
	delete(ac.registeredRunners, nodeID)
	delete(ac.nodeAttachments, nodeID)
	ac.runnersMu.Unlock()

	if ac.Store != nil {
		_ = ac.Store.DeregisterRunner(r.Context(), nodeID)
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "deregistered"})
}

// Session Management Handlers

func (ac *APIController) HandleSessions(w http.ResponseWriter, r *http.Request) {
	if ac.Sessions == nil {
		ac.Sessions = config.NewSessionRegistry(30 * time.Minute)
	}

	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		if id != "" {
			sess, exists := ac.Sessions.GetSession(id)
			if !exists {
				respondWithError(w, http.StatusNotFound, "Session not found or expired")
				return
			}
			respondWithJSON(w, http.StatusOK, sess)
			return
		}
		sessions := ac.Sessions.ListSessions()
		respondWithJSON(w, http.StatusOK, map[string]interface{}{
			"sessions": sessions,
			"count":    len(sessions),
		})

	case http.MethodDelete:
		if r.URL.Query().Get("clear") == "true" {
			ac.Sessions.Clear()
			respondWithJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			respondWithError(w, http.StatusBadRequest, "Missing session id")
			return
		}
		if ok := ac.Sessions.DeleteSession(id); !ok {
			respondWithError(w, http.StatusNotFound, "Session not found")
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// HandleSessionTask enables agents and caller services to switch active tasks or lock models.
func (ac *APIController) HandleSessionTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if ac.Sessions == nil {
		ac.Sessions = config.NewSessionRegistry(30 * time.Minute)
	}

	var req struct {
		SessionID string `json:"session_id"`
		AppID     string `json:"app_id"`
		Task      string `json:"task"`
		Model     string `json:"model"`
		Policy    string `json:"policy"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.SessionID == "" {
		respondWithError(w, http.StatusBadRequest, "Missing required session_id")
		return
	}

	sess := ac.Sessions.GetOrCreateSession(req.SessionID, req.AppID, req.Policy)
	if req.Policy != "" {
		sess.Policy = req.Policy
	}

	// Resolve app complexity configuration if available
	var compConfig config.ComplexityRouting
	if req.AppID != "" {
		if app, ok := ac.Store.LookupApp(req.AppID); ok {
			compConfig = app.Complexity
		}
	}

	if req.Task != "" {
		sess.SetTask(req.Task)
		if req.Model == "" {
			if targetModel, _, ok := decision.ResolveTaskModel(req.Task, compConfig); ok {
				sess.SetModel(targetModel, "agent_task_api")
			}
		}
	}

	if req.Model != "" {
		sess.SetModel(req.Model, "agent_model_api")
	}

	respondWithJSON(w, http.StatusOK, sess)
}

// HandleSessionReset clears or expires a specific session state.
func (ac *APIController) HandleSessionReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if ac.Sessions == nil {
		ac.Sessions = config.NewSessionRegistry(30 * time.Minute)
	}

	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.SessionID == "" {
		respondWithError(w, http.StatusBadRequest, "Missing required session_id")
		return
	}

	ac.Sessions.DeleteSession(req.SessionID)
	respondWithJSON(w, http.StatusOK, map[string]string{"status": "reset", "session_id": req.SessionID})
}

// HandleComplexityInspect evaluates a prompt and provides transparent explainability metrics.
func (ac *APIController) HandleComplexityInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		AppID         string `json:"app_id"`
		Prompt        string `json:"prompt"`
		Engine        string `json:"engine"`
		HasTools      bool   `json:"has_tools"`
		HasMultimodal bool   `json:"has_multimodal"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	compConfig := config.ComplexityRouting{
		Enabled:      true,
		SimpleModel:  "gemini-3.5-flash-lite",
		MediumModel:  "gemini-3.8-flash",
		ComplexModel: "gemini-3.1-pro",
		Engine:       "jev",
	}

	if req.AppID != "" {
		if app, ok := ac.Store.LookupApp(req.AppID); ok {
			compConfig = app.Complexity
		}
	}

	selectedEngine := strings.ToLower(req.Engine)
	if selectedEngine == "" {
		if compConfig.Engine != "" {
			selectedEngine = strings.ToLower(compConfig.Engine)
		} else {
			selectedEngine = "jev"
		}
	}

	if ac.JevEngine == nil {
		ac.JevEngine = decision.NewJevDecisionEngine()
	}
	if ac.FastinoEngine == nil {
		ac.FastinoEngine = decision.NewFastinoDecisionEngine()
	}
	if ac.CLMEngine == nil {
		ac.CLMEngine = decision.NewCLMContrastiveEngine()
	}

	if ac.Store != nil {
		if provs, err := ac.Store.GetAllProviders(r.Context()); err == nil {
			for _, p := range provs {
				if p.ID == config.ProviderLocal && p.Enabled {
					decision.SyncDecisionProvidersFromLocalConfig(p)
				}
			}
		}
	}

	var result decision.DecisionResult
	switch selectedEngine {
	case "fastino":
		result = ac.FastinoEngine.Classify(r.Context(), req.Prompt, compConfig, req.HasTools, req.HasMultimodal)
	case "clm", "contrastive-lm":
		result = ac.CLMEngine.Classify(r.Context(), req.Prompt, compConfig, req.HasTools, req.HasMultimodal)
	case "heuristics":
		charCount := len([]rune(req.Prompt))
		tier := "simple"
		if req.HasTools && compConfig.ForceComplexTools {
			tier = "complex"
		} else if req.HasMultimodal && compConfig.ForceComplexMultimodal {
			tier = "complex"
		} else if compConfig.MediumCharLimit > 0 && charCount > compConfig.MediumCharLimit {
			tier = "complex"
		} else if compConfig.SimpleCharLimit > 0 && charCount > compConfig.SimpleCharLimit {
			tier = "medium"
		}
		result = decision.DecisionResult{
			Tier:        tier,
			TargetModel: decision.ResolveModelByTier(tier, compConfig),
			Engine:      "heuristics",
			Features:    decision.ExtractPromptFeatures(req.Prompt, req.HasTools, req.HasMultimodal),
			Rationale:   fmt.Sprintf("Character limit evaluation (count: %d, tier: %s)", charCount, tier),
		}
	default: // jev
		result = ac.JevEngine.Classify(r.Context(), req.Prompt, compConfig, req.HasTools, req.HasMultimodal)
	}

	respondWithJSON(w, http.StatusOK, result)
}

// HandleComplexityKeys manages the in-memory secret vault and provider/cluster configuration
// for decision engines (jev, fastino, contrastive-lm). Raw keys are never returned in responses or persisted to disk.
func (ac *APIController) HandleComplexityKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if ac.Store != nil {
			if provs, err := ac.Store.GetAllProviders(r.Context()); err == nil {
				for _, p := range provs {
					if p.ID == config.ProviderLocal && p.Enabled {
						decision.SyncDecisionProvidersFromLocalConfig(p)
					}
				}
			}
		}
		secretRef := r.URL.Query().Get("secret_ref")
		statuses := decision.GetAllEngineSecretStatuses(secretRef)
		respondWithJSON(w, http.StatusOK, map[string]interface{}{
			"keys":      statuses,
			"providers": statuses,
		})

	case http.MethodPost:
		var req struct {
			Engine         string `json:"engine"`
			APIKey         string `json:"api_key"`
			SecretRef      string `json:"secret_ref"`
			Endpoint       string `json:"endpoint"`
			ConnectionMode string `json:"connection_mode"`
			ClusterID      string `json:"cluster_id"`
		}
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				respondWithError(w, http.StatusBadRequest, "Invalid request payload")
				return
			}
		} else {
			if err := r.ParseForm(); err != nil {
				respondWithError(w, http.StatusBadRequest, "Invalid form payload")
				return
			}
			req.Engine = r.FormValue("engine")
			req.APIKey = r.FormValue("api_key")
			req.SecretRef = r.FormValue("secret_ref")
			req.Endpoint = r.FormValue("endpoint")
			req.ConnectionMode = r.FormValue("connection_mode")
			req.ClusterID = r.FormValue("cluster_id")
		}
		if req.Engine == "" && req.SecretRef == "" {
			respondWithError(w, http.StatusBadRequest, "Missing engine or secret_ref")
			return
		}
		hasClusterOrEndpointUpdate := strings.TrimSpace(req.Endpoint) != "" ||
			strings.TrimSpace(req.ConnectionMode) != "" ||
			strings.TrimSpace(req.ClusterID) != ""
		if strings.TrimSpace(req.APIKey) == "" && !hasClusterOrEndpointUpdate {
			respondWithError(w, http.StatusBadRequest, "Missing api_key or provider endpoint/cluster configuration")
			return
		}
		target := req.Engine
		if target == "" {
			target = req.SecretRef
		}
		if strings.TrimSpace(req.APIKey) != "" {
			decision.SetEngineSecret(target, req.APIKey)
			if req.SecretRef != "" && req.SecretRef != target {
				decision.SetEngineSecret(req.SecretRef, req.APIKey)
			}
		}
		if hasClusterOrEndpointUpdate {
			if ac.Store != nil {
				localProv := config.ProviderConfig{
					ID:          config.ProviderLocal,
					DisplayName: "Local / Open-Weights Clusters",
					Enabled:     true,
				}
				if provs, err := ac.Store.GetAllProviders(r.Context()); err == nil {
					for _, p := range provs {
						if p.ID == config.ProviderLocal {
							localProv = p
							break
						}
					}
				}
				decision.ApplyDecisionProviderToLocalConfig(&localProv, target, req.Endpoint, req.ConnectionMode, req.ClusterID, req.SecretRef)
				_ = ac.Store.SaveProvider(r.Context(), localProv)
			} else {
				decision.SetEngineClusterConfig(target, req.Endpoint, req.ConnectionMode, req.ClusterID)
			}
		}
		status := decision.GetEngineSecretStatus(target, req.SecretRef)
		respondWithJSON(w, http.StatusOK, status)

	case http.MethodDelete:
		engine := r.URL.Query().Get("engine")
		secretRef := r.URL.Query().Get("secret_ref")
		if engine == "" && secretRef == "" {
			respondWithError(w, http.StatusBadRequest, "Missing engine or secret_ref query parameter")
			return
		}
		if engine != "" {
			decision.DeleteEngineSecret(engine)
		}
		if secretRef != "" {
			decision.DeleteEngineSecret(secretRef)
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// HandleDecisionProviderProbe performs a live connectivity & classification probe against a decision engine provider.
func (ac *APIController) HandleDecisionProviderProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req struct {
		Engine   string `json:"engine"`
		Endpoint string `json:"endpoint"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid request payload")
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid form payload")
			return
		}
		req.Engine = r.FormValue("engine")
		req.Endpoint = r.FormValue("endpoint")
	}
	if strings.TrimSpace(req.Engine) == "" {
		respondWithError(w, http.StatusBadRequest, "Missing engine")
		return
	}
	if ac.Store != nil {
		if provs, err := ac.Store.GetAllProviders(r.Context()); err == nil {
			for _, p := range provs {
				if p.ID == config.ProviderLocal && p.Enabled {
					decision.SyncDecisionProvidersFromLocalConfig(p)
				}
			}
		}
	}
	res, elapsed, target, err := decision.ProbeDecisionProvider(r.Context(), req.Engine, req.Endpoint)
	if err != nil {
		respondWithJSON(w, http.StatusBadGateway, map[string]interface{}{
			"ok":         false,
			"engine":     decision.NormalizeEngineName(req.Engine),
			"endpoint":   target,
			"latency_ms": elapsed.Milliseconds(),
			"error":      err.Error(),
		})
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"engine":     res.Engine,
		"endpoint":   target,
		"tier":       res.Tier,
		"confidence": res.Confidence,
		"rationale":  res.Rationale,
		"latency_ms": elapsed.Milliseconds(),
	})
}
