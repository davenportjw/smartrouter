package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	store "geminirouter/backend/config"
	"geminirouter/backend/proxy"
	"geminirouter/pkg/config"
)

// APIController handles the administrative REST APIs on the backend.
type APIController struct {
	Store        *store.ConfigStore
	Scheduler    *proxy.RequestScheduler
	Queue        *proxy.ClusterQueue
	SharedSecret string

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
		SharedSecret:      sharedSecret,
		registeredRunners: make(map[string]config.Node),
		nodeAttachments:   make(map[string][]string),
	}
}

// AuthMiddleware validates incoming API requests.
func (ac *APIController) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrap state-changing requests in MaxBytesReader (1MB limit) to prevent memory exhaustion attacks
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
		}

		isLocalDev := os.Getenv("LOCAL_DEV") == "true"

		// In production (LOCAL_DEV != true), we MUST enforce API authentication
		if !isLocalDev && ac.SharedSecret == "" {
			http.Error(w, "Unauthorized: Administrative API is not configured securely", http.StatusUnauthorized)
			return
		}

		if ac.SharedSecret != "" {
			secret := r.Header.Get("X-Shared-Secret")
			if secret == "" {
				// Fallback to Authorization header for compatibility
				authHeader := r.Header.Get("Authorization")
				if strings.HasPrefix(authHeader, "Bearer ") {
					secret = strings.TrimPrefix(authHeader, "Bearer ")
				}
			}
			if secret != ac.SharedSecret {
				http.Error(w, "Unauthorized: Invalid API shared secret", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
	mux.Handle("/api/providers", ac.AuthMiddleware(http.HandlerFunc(ac.HandleProviders)))

	// Dynamic Local Cluster REST API routes
	mux.Handle("/api/v1/cluster/runners/register", http.HandlerFunc(ac.HandleClusterRegisterNode))
	mux.Handle("/api/v1/cluster/runners/heartbeat", http.HandlerFunc(ac.HandleClusterHeartbeat))
	mux.Handle("/api/v1/cluster/queue/poll", http.HandlerFunc(ac.HandleClusterPollQueue))
	mux.Handle("/api/v1/cluster/queue/resolve", http.HandlerFunc(ac.HandleClusterResolveJob))

	// Registered Runners Admin REST API routes
	mux.Handle("/api/v1/admin/runners", ac.AuthMiddleware(http.HandlerFunc(ac.HandleAdminRunnersList)))
	mux.Handle("/api/v1/admin/runners/attach", ac.AuthMiddleware(http.HandlerFunc(ac.HandleAdminRunnersAttach)))
	mux.Handle("/api/v1/admin/runners/deregister", ac.AuthMiddleware(http.HandlerFunc(ac.HandleAdminRunnersDeregister)))
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
	// Dynamic models refresh triggers GCP models discovery.
	// Since discovery is hosted on the backend, this endpoint simply triggers discovery.
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	// Logically we return a redirect request or successfully triggered block
	respondWithJSON(w, http.StatusOK, map[string]string{"status": "triggered"})
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
	if ac.Scheduler == nil {
		respondWithError(w, http.StatusInternalServerError, "Scheduler not initialized")
		return
	}
	status := ac.Scheduler.GetQueueStatus()
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

func (ac *APIController) HandleClusterRegisterNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var node config.Node
	if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if node.ID == "" {
		respondWithError(w, http.StatusBadRequest, "Missing node ID")
		return
	}

	// Dynamic capacity limit checks
	node.MaxModelSizeGB = calculateMaxModelSize(node.MemoryAllocatedGB)
	node.MaxConcurrent = calculateMaxConcurrency(node.MemoryAllocatedGB)
	node.Status = "online"
	node.LastHeartbeat = time.Now()

	ac.runnersMu.Lock()
	ac.registeredRunners[node.ID] = node
	ac.runnersMu.Unlock()

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

	ac.runnersMu.Lock()
	if node, ok := ac.registeredRunners[hb.NodeID]; ok {
		node.LastHeartbeat = time.Now()
		ac.registeredRunners[hb.NodeID] = node
	} else {
		// Auto-register dynamic node on heartbeat
		newNode := config.Node{
			ID:                hb.NodeID,
			Name:              "Dynamic Connected Node",
			Status:            "online",
			LastHeartbeat:     time.Now(),
			MemoryAllocatedGB: 16,
			ComputeGPUCores:   8,
			SupportedModels:   []string{"llama3:8b"},
			MaxModelSizeGB:    8,
			MaxConcurrent:     2,
		}
		ac.registeredRunners[hb.NodeID] = newNode
	}

	// Auto-associate the runner to ClusterID on heartbeat if provided and not already mapped
	if hb.ClusterID != "" {
		alreadyAssigned := false
		for _, assigned := range ac.nodeAttachments[hb.NodeID] {
			if assigned == hb.ClusterID {
				alreadyAssigned = true
				break
			}
		}
		if !alreadyAssigned {
			ac.nodeAttachments[hb.NodeID] = append(ac.nodeAttachments[hb.NodeID], hb.ClusterID)
		}

		// Also auto-associate to "local-cluster" so it matches standard queued jobs
		hasLocalCluster := false
		for _, assigned := range ac.nodeAttachments[hb.NodeID] {
			if assigned == "local-cluster" {
				hasLocalCluster = true
				break
			}
		}
		if !hasLocalCluster {
			ac.nodeAttachments[hb.NodeID] = append(ac.nodeAttachments[hb.NodeID], "local-cluster")
		}
	}
	ac.runnersMu.Unlock()

	maxSizeLimit := 32
	ac.runnersMu.RLock()
	if n, ok := ac.registeredRunners[hb.NodeID]; ok {
		maxSizeLimit = n.MaxModelSizeGB
	}
	ac.runnersMu.RUnlock()

	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"status":            "alive",
		"max_model_size_gb": maxSizeLimit,
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
		SupportedModels []string `json:"supported_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	// Check if runner has administrative cluster mappings
	var allowedClusters []string
	if req.NodeID != "" {
		ac.runnersMu.RLock()
		allowedClusters = ac.nodeAttachments[req.NodeID]
		ac.runnersMu.RUnlock()
	}

	job, found := ac.Queue.Poll(req.SupportedModels, allowedClusters)
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
	if r.Method != http.MethodGet {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	ac.runnersMu.RLock()
	defer ac.runnersMu.RUnlock()

	type RunnerResponse struct {
		Node             config.Node `json:"node"`
		AssignedClusters []string    `json:"assigned_clusters"`
	}

	// Return sorted slice for deterministic presentation
	var list []RunnerResponse
	var keys []string
	for id := range ac.registeredRunners {
		keys = append(keys, id)
	}
	sort.Strings(keys) // sort keys

	for _, id := range keys {
		node := ac.registeredRunners[id]
		clusters := ac.nodeAttachments[id]
		if clusters == nil {
			clusters = []string{}
		}
		list = append(list, RunnerResponse{
			Node:             node,
			AssignedClusters: clusters,
		})
	}

	// Fallback to empty slice instead of nil in JSON
	if len(list) == 0 {
		respondWithJSON(w, http.StatusOK, []RunnerResponse{})
		return
	}

	respondWithJSON(w, http.StatusOK, list)
}

func (ac *APIController) HandleAdminRunnersAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		NodeID     string   `json:"node_id"`
		ClusterIDs []string `json:"cluster_ids"`
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
	ac.runnersMu.Unlock()

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

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "deregistered"})
}

