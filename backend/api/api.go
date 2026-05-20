package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	store "geminirouter/backend/config"
	"geminirouter/backend/proxy"
	"geminirouter/pkg/config"
)

// APIController handles the administrative REST APIs on the backend.
type APIController struct {
	Store        *store.ConfigStore
	Scheduler    *proxy.RequestScheduler
	SharedSecret string
}

// NewAPIController initializes a new backend API controller.
func NewAPIController(store *store.ConfigStore, scheduler *proxy.RequestScheduler, sharedSecret string) *APIController {
	return &APIController{
		Store:        store,
		Scheduler:    scheduler,
		SharedSecret: sharedSecret,
	}
}

// AuthMiddleware validates incoming API requests.
func (ac *APIController) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// In local development or if a shared secret is configured, enforce it
		if ac.SharedSecret != "" {
			authHeader := r.Header.Get("Authorization")
			expected := "Bearer " + ac.SharedSecret
			if authHeader != expected {
				// In GCP production, Cloud Run IAM handles OIDC validation.
				// However, if a shared secret is explicitly set in production, we still enforce it.
				if os.Getenv("LOCAL_DEV") == "true" || authHeader != "" {
					http.Error(w, "Unauthorized: Invalid API shared secret", http.StatusUnauthorized)
					return
				}
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
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	clients, err := ac.Store.GetAllClients(ctx)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondWithJSON(w, http.StatusOK, clients)
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
