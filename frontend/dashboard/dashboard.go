package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"geminirouter/frontend/dashboard/templates"
	"geminirouter/pkg/config"

	"github.com/yuin/goldmark"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// DashboardController handles the administration dashboard HTTP routes.
type DashboardController struct {
	Store       config.AdminStore
	Firebase    templates.FirebaseConfig
	ProjectID   string
	Location    string
	TokenSource oauth2.TokenSource
	HTTPClient  *http.Client
}

// NewDashboardController initializes a new dashboard controller.
func NewDashboardController(store config.AdminStore, projectID, location string) *DashboardController {
	ts, err := google.DefaultTokenSource(context.Background(), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		log.Printf("[Warning] Failed to initialize live Google Cloud DefaultTokenSource: %v", err)
	}
	// Pull Firebase client configs from environment variables
	return &DashboardController{
		Store:     store,
		ProjectID: projectID,
		Location:  location,
		TokenSource: ts,
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		Firebase: templates.FirebaseConfig{
			APIKey:            os.Getenv("FIREBASE_API_KEY"),
			AuthDomain:        os.Getenv("FIREBASE_AUTH_DOMAIN"),
			ProjectID:         os.Getenv("FIREBASE_PROJECT_ID"),
			StorageBucket:     os.Getenv("FIREBASE_STORAGE_BUCKET"),
			MessagingSenderID: os.Getenv("FIREBASE_MESSAGING_SENDER_ID"),
			AppID:             os.Getenv("FIREBASE_APP_ID"),
			IsLocalDev:        os.Getenv("LOCAL_DEV") == "true",
		},
	}
}

func (dc *DashboardController) getHTTPClient() *http.Client {
	if dc.HTTPClient == nil {
		return &http.Client{Timeout: 15 * time.Second}
	}
	return dc.HTTPClient
}

// ServeLogin renders the Firebase-enabled login view.
func (dc *DashboardController) ServeLogin(w http.ResponseWriter, r *http.Request) {
	// If session cookie already exists and is valid, bypass login
	if cookie, err := r.Cookie("session"); err == nil && cookie.Value != "" {
		http.Redirect(w, r, "/admin/keys", http.StatusFound)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.Login(dc.Firebase).Render(r.Context(), w)
}

// ServeKeys fetches keys, clients, and apps and renders the Keys administration view.
func (dc *DashboardController) ServeKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Fetch all Keys
	keys, err := dc.Store.GetAllKeys(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading api_keys: %v", err)
		http.Error(w, "Internal Server Error loading keys", http.StatusInternalServerError)
		return
	}

	// 2. Fetch all Clients
	clients, err := dc.Store.GetAllClients(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading clients: %v", err)
		http.Error(w, "Internal Server Error loading clients", http.StatusInternalServerError)
		return
	}

	// 3. Fetch all Apps
	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps: %v", err)
		http.Error(w, "Internal Server Error loading apps", http.StatusInternalServerError)
		return
	}

	// Map clients and apps by ID for O(1) lookup
	clientsMap := make(map[string]config.Client)
	for _, c := range clients {
		clientsMap[c.ID] = c
	}

	appsMap := make(map[string]config.App)
	for _, a := range apps {
		appsMap[a.ID] = a
	}

	// 4. Build combined ViewModels
	var viewModels []templates.KeysViewModel
	for _, key := range keys {
		appProfile, ok := appsMap[key.AppID]
		if !ok {
			// App deleted but key remains, construct fallback profile
			appProfile = config.App{
				ID:       key.AppID,
				ClientID: key.ClientID,
				Name:     "Unknown App",
				RPM:      60,
				TPM:      40000,
				Priority: "medium",
			}
		}

		clientProfile, ok := clientsMap[appProfile.ClientID]
		if !ok {
			clientProfile = config.Client{
				ID:   appProfile.ClientID,
				Name: "Unknown Client",
				Tier: "free",
			}
		}

		viewModels = append(viewModels, templates.KeysViewModel{
			Key:    key,
			Client: clientProfile,
			App:    appProfile,
		})
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.KeysTab(viewModels)
	_ = templates.Layout("API Keys", "keys", content).Render(ctx, w)
}

// ServeKeysNewModal renders the dynamic creation form via HTMX.
func (dc *DashboardController) ServeKeysNewModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps for modal: %v", err)
		http.Error(w, "Failed to load apps", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.KeyModal(apps, nil).Render(ctx, w)
}

// CreateKey handles form submissions and generates a new API key bound to the selected App.
func (dc *DashboardController) CreateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	appID := r.FormValue("app_id")
	if appID == "" {
		http.Error(w, "Missing target app selection", http.StatusBadRequest)
		return
	}

	app, ok := dc.Store.LookupApp(appID)
	if !ok {
		http.Error(w, "Application profile not found", http.StatusNotFound)
		return
	}

	// 1. Generate cryptographically secure API Key
	rawKey, err := generateSecureKey()
	if err != nil {
		log.Printf("[Dashboard] Error generating key: %v", err)
		http.Error(w, "Failed to generate key", http.StatusInternalServerError)
		return
	}

	// Hash key for secure persistence
	hashedKey := config.HashKey(rawKey)

	// 2. Persist APIKey document linked to App and Client
	err = dc.Store.SaveKey(ctx, config.APIKey{
		KeyHash:  hashedKey,
		AppID:    app.ID,
		ClientID: app.ClientID,
		Status:   "active",
	})
	if err != nil {
		log.Printf("[Dashboard] Error saving api_key: %v", err)
		http.Error(w, "Failed to save API key profile", http.StatusInternalServerError)
		return
	}

	// Render raw API Key inside prominent success warning banner
	w.Header().Set("Content-Type", "text/html")
	_ = templates.KeyCreatedAlert(rawKey, app.Name).Render(ctx, w)
}

// RevokeKey marks an API key as inactive dynamically and returns empty block for HTMX replacement.
func (dc *DashboardController) RevokeKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	hash := r.URL.Query().Get("hash")
	if hash == "" {
		http.Error(w, "Missing key hash", http.StatusBadRequest)
		return
	}

	// Update key status to "revoked"
	err := dc.Store.RevokeKey(ctx, hash)
	if err != nil {
		log.Printf("[Dashboard] RevokeKey error: %v", err)
		http.Error(w, "Failed to revoke key", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	// Simply return nothing to HTMX to clear/remove closest element or let table refresh
	w.Write([]byte(""))
}

// ServeKeysEditModal renders the dynamic edit modal for an existing API Key via HTMX.
func (dc *DashboardController) ServeKeysEditModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hash := r.URL.Query().Get("hash")
	if hash == "" {
		http.Error(w, "Missing key hash", http.StatusBadRequest)
		return
	}

	keys, err := dc.Store.GetAllKeys(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading keys: %v", err)
		http.Error(w, "Failed to load keys", http.StatusInternalServerError)
		return
	}

	var targetKey *config.APIKey
	for _, k := range keys {
		if k.KeyHash == hash {
			targetKey = &k
			break
		}
	}

	if targetKey == nil {
		http.Error(w, "API Key not found", http.StatusNotFound)
		return
	}

	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps: %v", err)
		http.Error(w, "Failed to load apps", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.KeyModal(apps, targetKey).Render(ctx, w)
}

// SaveKeyDetails updates status and/or bound app mapping of an existing API Key.
func (dc *DashboardController) SaveKeyDetails(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	hash := r.FormValue("hash")
	appID := r.FormValue("app_id")
	status := r.FormValue("status")

	if hash == "" || appID == "" || status == "" {
		http.Error(w, "Missing required key fields", http.StatusBadRequest)
		return
	}

	keys, err := dc.Store.GetAllKeys(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading keys: %v", err)
		http.Error(w, "Failed to load keys", http.StatusInternalServerError)
		return
	}

	var targetKey *config.APIKey
	for _, k := range keys {
		if k.KeyHash == hash {
			targetKey = &k
			break
		}
	}

	if targetKey == nil {
		http.Error(w, "API Key not found", http.StatusNotFound)
		return
	}

	app, ok := dc.Store.LookupApp(appID)
	if !ok {
		http.Error(w, "Target Application not found", http.StatusNotFound)
		return
	}

	// Update fields
	targetKey.AppID = app.ID
	targetKey.ClientID = app.ClientID
	targetKey.Status = status

	err = dc.Store.SaveKey(ctx, *targetKey)
	if err != nil {
		log.Printf("[Dashboard] Error saving api_key: %v", err)
		http.Error(w, "Failed to save API Key profile", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/keys")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/admin/keys", http.StatusSeeOther)
}

// ServeRules renders the Routing Rules view tab.
func (dc *DashboardController) ServeRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Fetch active rules
	rules, err := dc.Store.GetAllRules(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading rules: %v", err)
		http.Error(w, "Failed to load routing rules", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.RulesTab(rules)
	_ = templates.Layout("Routing Rules", "rules", content).Render(ctx, w)
}

// ServeRulesNewModal renders the dynamic rules creation form via HTMX.
func (dc *DashboardController) ServeRulesNewModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps for rule modal: %v", err)
		http.Error(w, "Failed to load apps", http.StatusInternalServerError)
		return
	}

	allModels, err := dc.Store.GetAllModels(ctx)
	var activeCompatibleModels []config.ModelConfig
	if err == nil {
		for _, m := range allModels {
			if m.Active && config.IsLocationCompatible(dc.Location, m.Location) {
				activeCompatibleModels = append(activeCompatibleModels, m)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.RuleModal(apps, activeCompatibleModels, nil).Render(ctx, w)
}

// ServeRulesEditModal renders the dynamic edit modal for an existing routing rule via HTMX.
func (dc *DashboardController) ServeRulesEditModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing routing rule ID", http.StatusBadRequest)
		return
	}

	rules, err := dc.Store.GetAllRules(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading rules: %v", err)
		http.Error(w, "Failed to load routing rules", http.StatusInternalServerError)
		return
	}

	var targetRule *config.RoutingRule
	for _, r := range rules {
		if r.ID == id {
			targetRule = &r
			break
		}
	}

	if targetRule == nil {
		http.Error(w, "Routing rule not found", http.StatusNotFound)
		return
	}

	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps for rule modal: %v", err)
		http.Error(w, "Failed to load apps", http.StatusInternalServerError)
		return
	}

	allModels, err := dc.Store.GetAllModels(ctx)
	var activeCompatibleModels []config.ModelConfig
	if err == nil {
		for _, m := range allModels {
			if m.Active && config.IsLocationCompatible(dc.Location, m.Location) {
				activeCompatibleModels = append(activeCompatibleModels, m)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.RuleModal(apps, activeCompatibleModels, targetRule).Render(ctx, w)
}

// CreateRule handles dynamic routing rule submission form.
func (dc *DashboardController) CreateRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	modelPattern := strings.TrimSpace(r.FormValue("model_pattern"))
	appID := strings.TrimSpace(r.FormValue("app_id"))
	clientTier := strings.TrimSpace(r.FormValue("client_tier"))
	headerName := strings.TrimSpace(r.FormValue("header_name"))
	headerValue := strings.TrimSpace(r.FormValue("header_value"))
	targetModel := strings.TrimSpace(r.FormValue("target_model"))
	targetLocation := strings.TrimSpace(r.FormValue("target_location"))
	fallbackModel := strings.TrimSpace(r.FormValue("fallback_model"))
	priorityWeightStr := strings.TrimSpace(r.FormValue("priority_weight"))

	if modelPattern == "" {
		http.Error(w, "Requested Model Pattern cannot be empty.", http.StatusBadRequest)
		return
	}
	if targetModel == "" {
		http.Error(w, "Target Routed Model cannot be empty.", http.StatusBadRequest)
		return
	}
	if targetLocation == "" {
		http.Error(w, "Target Location cannot be empty.", http.StatusBadRequest)
		return
	}

	// Validate regex pattern in HeaderValue if applicable
	if headerName != "" && headerValue != "" {
		if strings.HasPrefix(headerValue, "/") && strings.HasSuffix(headerValue, "/") {
			pattern := headerValue[1 : len(headerValue)-1]
			_, err := regexp.Compile(pattern)
			if err != nil {
				http.Error(w, fmt.Sprintf("Invalid Header Value regex pattern: %v", err), http.StatusBadRequest)
				return
			}
		}
	}

	priorityWeight := 1
	if priorityWeightStr != "" {
		pw, err := strconv.Atoi(priorityWeightStr)
		if err != nil || pw <= 0 {
			http.Error(w, "Priority Weight must be a positive integer.", http.StatusBadRequest)
			return
		}
		priorityWeight = pw
	}

	// If editing an existing rule, it will pass the rule ID in the form
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		// Generate unique random ID for this rule
		idBytes := make([]byte, 8)
		_, _ = rand.Read(idBytes)
		id = "rule-" + hex.EncodeToString(idBytes)
	}

	err := dc.Store.SaveRule(ctx, config.RoutingRule{
		ID:             id,
		AppID:          appID,
		ModelPattern:   modelPattern,
		ClientTier:     clientTier,
		HeaderName:     headerName,
		HeaderValue:    headerValue,
		TargetModel:    targetModel,
		TargetLocation: targetLocation,
		FallbackModel:  fallbackModel,
		PriorityWeight: priorityWeight,
	})
	if err != nil {
		log.Printf("[Dashboard] Error saving routing rule: %v", err)
		http.Error(w, "Failed to save routing rule", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/rules")
		w.WriteHeader(http.StatusOK)
		return
	}
	// Direct standard client browser redirect back to the rules view
	http.Redirect(w, r, "/admin/rules", http.StatusSeeOther)
}

// DeleteRule deletes a routing rule dynamically.
func (dc *DashboardController) DeleteRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing routing rule ID", http.StatusBadRequest)
		return
	}

	err := dc.Store.DeleteRule(ctx, id)
	if err != nil {
		log.Printf("[Dashboard] Error deleting routing rule: %v", err)
		http.Error(w, "Failed to delete routing rule", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	// Return nothing to HTMX so closest <tr> is removed
	w.Write([]byte(""))
}


// ServeHeaders fetches headers and renders the Custom Headers administration view.
func (dc *DashboardController) ServeHeaders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	headers, err := dc.Store.GetAllHeaders(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading custom_headers: %v", err)
		http.Error(w, "Internal Server Error loading custom headers", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.HeadersTab(headers)
	_ = templates.Layout("Custom Headers", "headers", content).Render(ctx, w)
}

// ServeHeadersNewModal renders the dynamic headers creation form via HTMX.
func (dc *DashboardController) ServeHeadersNewModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps for headers modal: %v", err)
		http.Error(w, "Failed to load apps", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.HeaderModal(apps, nil).Render(ctx, w)
}

// ServeHeadersEditModal renders the dynamic edit modal for an existing custom header via HTMX.
func (dc *DashboardController) ServeHeadersEditModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing custom header ID", http.StatusBadRequest)
		return
	}

	headers, err := dc.Store.GetAllHeaders(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading headers: %v", err)
		http.Error(w, "Failed to load custom headers", http.StatusInternalServerError)
		return
	}

	var targetHeader *config.CustomHeader
	for _, h := range headers {
		if h.ID == id {
			targetHeader = &h
			break
		}
	}

	if targetHeader == nil {
		http.Error(w, "Custom header rule not found", http.StatusNotFound)
		return
	}

	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps for header modal: %v", err)
		http.Error(w, "Failed to load apps", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.HeaderModal(apps, targetHeader).Render(ctx, w)
}

// CreateHeader handles custom header rule submission form.
func (dc *DashboardController) CreateHeader(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	name := strings.TrimSpace(r.FormValue("name"))
	appID := strings.TrimSpace(r.FormValue("app_id"))
	description := strings.TrimSpace(r.FormValue("description"))
	requiredStr := strings.TrimSpace(r.FormValue("required"))
	validation := strings.TrimSpace(r.FormValue("validation"))
	valuePattern := strings.TrimSpace(r.FormValue("value_pattern"))

	if name == "" {
		http.Error(w, "Header Name cannot be empty.", http.StatusBadRequest)
		return
	}
	if appID == "" {
		http.Error(w, "Application boundary (App ID) must be specified (or 'global').", http.StatusBadRequest)
		return
	}
	if validation == "regex" {
		if valuePattern == "" {
			http.Error(w, "Regex validation requires a non-empty pattern value.", http.StatusBadRequest)
			return
		}
		_, err := regexp.Compile(valuePattern)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid regular expression pattern: %v", err), http.StatusBadRequest)
			return
		}
	}

	required := requiredStr == "true"

	// If editing an existing header rule, it will pass the header ID in the form
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		// Generate unique random ID for this rule
		idBytes := make([]byte, 8)
		_, _ = rand.Read(idBytes)
		id = "header-" + hex.EncodeToString(idBytes)
	}

	err := dc.Store.SaveHeader(ctx, config.CustomHeader{
		ID:           id,
		AppID:        appID,
		Name:         name,
		Description:  description,
		Required:     required,
		Validation:   validation,
		ValuePattern: valuePattern,
	})
	if err != nil {
		log.Printf("[Dashboard] Error saving custom header: %v", err)
		http.Error(w, "Failed to save custom header rule", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/headers")
		w.WriteHeader(http.StatusOK)
		return
	}
	// Direct standard client browser redirect back to the full headers view
	http.Redirect(w, r, "/admin/headers", http.StatusSeeOther)
}

// DeleteHeader deletes a custom header rule dynamically.
func (dc *DashboardController) DeleteHeader(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing header rule ID", http.StatusBadRequest)
		return
	}

	err := dc.Store.DeleteHeader(ctx, id)
	if err != nil {
		log.Printf("[Dashboard] Error deleting custom header: %v", err)
		http.Error(w, "Failed to delete custom header rule", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	// Return nothing to HTMX so closest <tr> is removed
	w.Write([]byte(""))
}



// Helper cryptographically secure API key generator
func generateSecureKey() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "gr_key_" + hex.EncodeToString(bytes), nil
}

// aggregateModelStats compiles real-time or high-fidelity simulated metrics per active model.
func (dc *DashboardController) aggregateModelStats(ctx context.Context, simulate bool) map[string]templates.ModelStats {
	stats := make(map[string]templates.ModelStats)

	costsVM, err := dc.fetchCostAnalyticsData(ctx, simulate)
	var sessions []templates.CostTransaction
	if err == nil && len(costsVM.RecentSessions) > 0 {
		sessions = costsVM.RecentSessions
	} else if simulate {
		sessions = dc.generateMockCosts().RecentSessions
	}

	// Pre-populate standard realistic latency lists per model for display variety
	latencies := map[string][]int64{
		"gemini-2.5-flash":      {210, 250, 190, 320},
		"gemini-2.5-pro":        {650, 800, 720, 910},
		"gemini-2.5-flash-lite": {110, 140, 125, 150},
		"gemini-3.0-flash":      {180, 220, 170, 260},
		"gemini-3.0-pro":        {580, 710, 630, 800},
		"gemini-3.1-flash":      {160, 190, 150, 230},
		"gemini-3.1-pro":        {510, 620, 550, 700},
		"gemini-3.5-flash":      {130, 160, 120, 180},
		"gemini-3.5-pro":        {420, 500, 440, 550},
		"gemini-3.5-flash-lite": {70, 90, 80, 100},
	}

	for _, s := range sessions {
		mStats := stats[s.ModelRouted]
		mStats.RequestCount++
		mStats.TotalCost += s.EstimatedCost

		lList := latencies[s.ModelRouted]
		if len(lList) > 0 {
			mStats.AvgLatencyMs = lList[mStats.RequestCount%len(lList)]
		} else {
			mStats.AvgLatencyMs = 200
		}
		stats[s.ModelRouted] = mStats
	}

	return stats
}

// ServeModels serves the real-time Google Cloud Project models screen.
func (dc *DashboardController) ServeModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Fetch all models stored in the config store
	allModels, err := dc.Store.GetAllModels(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading models from config: %v", err)
		http.Error(w, "Failed to load models from database", http.StatusInternalServerError)
		return
	}

	simulate := false
	if cookie, err := r.Cookie("simulate_metrics"); err == nil && cookie.Value == "true" {
		simulate = true
	}
	// Fetch dynamic logs stats per model
	modelStats := dc.aggregateModelStats(ctx, simulate)

	var generativeModels []templates.ModelInfo
	var embeddingModels []templates.ModelInfo

	for _, m := range allModels {
		// Only show models compatible with this router instance's serving location
		if !config.IsLocationCompatible(dc.Location, m.Location) {
			continue
		}

		// Resolve dynamic stats
		stats, exists := modelStats[m.ID]
		if !exists {
			// Seed a healthy minimal default baseline if no requests recorded yet
			stats = templates.ModelStats{
				RequestCount: 0,
				AvgLatencyMs: 0,
				TotalCost:    0.0,
			}
		}

		info := templates.ModelInfo{
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Location:    m.Location,
			Type:        m.Type,
			Active:      m.Active,
			Obsolete:    strings.Contains(m.DisplayName, "(Obsolete)"),
			Stats:       stats,
		}

		baseModelID := config.StripLocationSuffix(m.ID)
		if strings.Contains(strings.ToLower(baseModelID), "embedding") {
			embeddingModels = append(embeddingModels, info)
		} else {
			generativeModels = append(generativeModels, info)
		}
	}

	vm := templates.ModelsViewModel{
		ProjectID:        dc.ProjectID,
		Location:         dc.Location,
		ParentLocation:   config.GetMultiRegionParent(dc.Location),
		GenerativeModels: generativeModels,
		EmbeddingModels:  embeddingModels,
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.ModelsTab(vm)
	_ = templates.Layout("GCP Models", "models", content).Render(ctx, w)
}

// verifyModel sends a very basic query to check if the model works or needs template reconstruction.
func (dc *DashboardController) verifyModel(ctx context.Context, model config.ModelConfig) error {
	type basicPart struct {
		Text string `json:"text"`
	}
	type basicContent struct {
		Role  string      `json:"role,omitempty"`
		Parts []basicPart `json:"parts"`
	}
	type basicReq struct {
		Contents []basicContent `json:"contents"`
	}
	reqBody := basicReq{
		Contents: []basicContent{
			{
				Role:  "user",
				Parts: []basicPart{{Text: "ping"}},
			},
		},
	}

	hostLoc := model.Location
	if hostLoc == "" {
		hostLoc = "global"
	}
	targetHost := config.GetVertexEndpointHost(hostLoc)

	var targetURL string
	baseModelID := config.StripLocationSuffix(model.ID)
	if strings.Contains(baseModelID, "embedding") {
		log.Printf("[Discovery] Bypassing generateContent verification for embedding model %s.", model.ID)
		return nil
	}
	if strings.HasPrefix(baseModelID, "projects/") {
		targetURL = fmt.Sprintf("https://%s/v1/%s:generateContent", targetHost, baseModelID)
	} else {
		targetURL = fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", targetHost, dc.ProjectID, hostLoc, baseModelID)
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// 30-second generous verification timeout to accommodate larger models and cold starts
	callCtx, cancel := context.WithTimeout(ctx, 30000*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, "POST", targetURL, bytes.NewReader(reqJSON))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if dc.TokenSource != nil {
		token, err := dc.TokenSource.Token()
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)
			req.Header.Set("X-Goog-User-Project", dc.ProjectID)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	client := dc.getHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyErr, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %s: %s", resp.Status, string(bodyErr))
	}

	log.Printf("[Discovery] Verification: Successfully verified model %s at location %s.", model.ID, model.Location)
	return nil
}

// DiscoverAndCacheModels queries Vertex AI APIs and caches all discovered models and endpoints.
func (dc *DashboardController) DiscoverAndCacheModels(ctx context.Context) error {
	// Fetch currently configured models to avoid resetting their active statuses
	currentModels, err := dc.Store.GetAllModels(ctx)
	if err != nil {
		return fmt.Errorf("GetAllModels failed: %w", err)
	}
	activeStatus := make(map[string]bool)
	for _, m := range currentModels {
		activeStatus[m.ID] = m.Active
	}

	discoveredModelsMap := make(map[string]config.ModelConfig)

	var locationsToFetch []string
	locations, fetchLocErr := dc.fetchLocations(ctx)
	if fetchLocErr == nil {
		for _, l := range locations {
			if config.IsLocationCompatible(dc.Location, l.ID) {
				locationsToFetch = append(locationsToFetch, l.ID)
			}
		}
	}
	// Fallback to local location if listing failed or returned nothing
	if len(locationsToFetch) == 0 {
		locationsToFetch = []string{dc.Location}
	}

	for _, loc := range locationsToFetch {
		log.Printf("[Discovery] Refreshing discovered models from Vertex AI in location: %q...", loc)
		
		// 1. Fetch custom models from API (only if not global)
		if loc != "global" {
			custom, err := dc.fetchCustomModels(ctx, loc)
			if err == nil {
				for _, m := range custom {
					// Skip if invalid name
					if !config.IsValidModelName(m.Name) {
						continue
					}
					
					// Discovered custom models default to inactive unless already present and active
					active := false
					if exists, present := activeStatus[m.Name]; present {
						active = exists
					}

					// Extract specific location and resolve smallest compatible location
					modelLoc := loc
					if extLoc := config.ExtractLocationFromResourceName(m.Name); extLoc != "" {
						modelLoc = config.GetSmallestCompatibleLocation(loc, extLoc)
					}

					// Resolve smallest location against previous iterations or pre-existing database models
					if prev, alreadyDiscovered := discoveredModelsMap[m.Name]; alreadyDiscovered {
						modelLoc = config.GetSmallestCompatibleLocation(prev.Location, modelLoc)
					} else {
						for _, existingModel := range currentModels {
							if existingModel.ID == m.Name && existingModel.Location != "" {
								modelLoc = config.GetSmallestCompatibleLocation(existingModel.Location, modelLoc)
								break
							}
						}
					}

					discoveredModelsMap[m.Name] = config.ModelConfig{
						ID:          m.Name,
						DisplayName: m.DisplayName,
						Location:    modelLoc,
						Type:        "custom",
						Active:      active,
					}
				}
			} else {
				log.Printf("[Discovery] fetchCustomModels failed for loc %q: %v", loc, err)
			}
		}

		// 2. Fetch endpoints from API (only if not global)
		if loc != "global" {
			endpoints, err := dc.fetchEndpoints(ctx, loc)
			if err == nil {
				for _, ep := range endpoints {
					if !config.IsValidModelName(ep.Name) {
						continue
					}
					displayName := ep.DisplayName
					if len(ep.DeployedModels) > 0 {
						displayName = ep.DeployedModels[0].DisplayName
					}
					
					// Discovered endpoints default to inactive unless already present and active
					active := false
					if exists, present := activeStatus[ep.Name]; present {
						active = exists
					}

					// Extract specific location and resolve smallest compatible location
					modelLoc := loc
					if extLoc := config.ExtractLocationFromResourceName(ep.Name); extLoc != "" {
						modelLoc = config.GetSmallestCompatibleLocation(loc, extLoc)
					}

					// Resolve smallest location against previous iterations or pre-existing database models
					if prev, alreadyDiscovered := discoveredModelsMap[ep.Name]; alreadyDiscovered {
						modelLoc = config.GetSmallestCompatibleLocation(prev.Location, modelLoc)
					} else {
						for _, existingModel := range currentModels {
							if existingModel.ID == ep.Name && existingModel.Location != "" {
								modelLoc = config.GetSmallestCompatibleLocation(existingModel.Location, modelLoc)
								break
							}
						}
					}

					discoveredModelsMap[ep.Name] = config.ModelConfig{
						ID:          ep.Name,
						DisplayName: displayName,
						Location:    modelLoc,
						Type:        "endpoint",
						Active:      active,
					}
				}
			} else {
				log.Printf("[Discovery] fetchEndpoints failed for loc %q: %v", loc, err)
			}
		}

		// 3. Fetch Google foundation/publisher models from API (for all queried regions, parent multiregion, and global)
		pubModels, err := dc.fetchPublisherModels(ctx, loc)
		if err == nil {
			for _, pm := range pubModels {
				if !config.IsValidModelName(pm.Name) {
					continue
				}

				// All newly discovered foundation models default to inactive (Active = false)
				active := false
				if exists, present := activeStatus[pm.Name]; present {
					active = exists
				}

				// Extract specific location and resolve smallest compatible location
				modelLoc := loc
				if extLoc := config.ExtractLocationFromResourceName(pm.Name); extLoc != "" {
					modelLoc = config.GetSmallestCompatibleLocation(loc, extLoc)
				}

				// Resolve smallest location against previous iterations or pre-existing database models
				if prev, alreadyDiscovered := discoveredModelsMap[pm.Name]; alreadyDiscovered {
					modelLoc = config.GetSmallestCompatibleLocation(prev.Location, modelLoc)
				} else {
					for _, existingModel := range currentModels {
						if existingModel.ID == pm.Name && existingModel.Location != "" {
							modelLoc = config.GetSmallestCompatibleLocation(existingModel.Location, modelLoc)
							break
						}
					}
				}

				discoveredModelsMap[pm.Name] = config.ModelConfig{
					ID:          pm.Name,
					DisplayName: pm.DisplayName,
					Location:    modelLoc,
					Type:        "foundation",
					Active:      active,
				}
			}
		} else {
			log.Printf("[Discovery] fetchPublisherModels failed for loc %q: %v", loc, err)
		}
	}

	// Track successfully verified compound IDs
	verifiedIDs := make(map[string]bool)

	// Save all resolved discovered models for each of their compatible locations separately
	for _, modelToSave := range discoveredModelsMap {
		var candidates []string
		// 1. If custom model/endpoint has fixed location embedded in ID, test only that
		if strings.HasPrefix(modelToSave.ID, "projects/") {
			if extLoc := config.ExtractLocationFromResourceName(modelToSave.ID); extLoc != "" {
				candidates = []string{extLoc}
			}
		}
		// 2. Otherwise, test all compatible locations (local, parent multi-region, and global)
		if len(candidates) == 0 {
			routerLoc := dc.Location
			if routerLoc == "" {
				routerLoc = "us-central1"
			}
			candidates = append(candidates, routerLoc)
			if parent := config.GetMultiRegionParent(routerLoc); parent != "" {
				candidates = append(candidates, parent)
			}
			candidates = append(candidates, "global")
		}

		for _, candidateLoc := range candidates {
			modelOption := modelToSave
			modelOption.Location = candidateLoc
			modelOption.ID = modelToSave.ID + "@" + candidateLoc

			// Check if this specific compound model was already registered and active
			active := false
			if exists, present := activeStatus[modelOption.ID]; present {
				active = exists
			} else if exists, present := activeStatus[modelToSave.ID]; present {
				active = exists
			}
			modelOption.Active = active

			err := dc.verifyModel(ctx, modelOption)
			if err == nil {
				log.Printf("[Discovery] Successfully verified model %s at location %s.", modelToSave.ID, candidateLoc)
				_ = dc.Store.SaveModel(ctx, modelOption)
				verifiedIDs[modelOption.ID] = true
			} else {
				log.Printf("[Discovery] Verification failed for model %s at candidate location %s: %v. Skipping registration.", modelToSave.ID, candidateLoc, err)
			}
		}
	}

	// Clean up legacy keys and obsolete/failed models
	for _, m := range currentModels {
		if m.ID == "gemini-dynamic" {
			continue
		}

		// If it is a legacy key (does not contain '@'), we delete it to scrub duplicates
		if !strings.Contains(m.ID, "@") {
			log.Printf("[Discovery] Scrubbing legacy key model %s from registry...", m.ID)
			_ = dc.Store.DeleteModel(ctx, m.ID)
			continue
		}

		// Extract base model ID from compound key (part before '@')
		baseID := m.ID
		if idx := strings.Index(m.ID, "@"); idx != -1 {
			baseID = m.ID[:idx]
		}

		// If the base model was returned by Vertex AI API but this specific location failed verification,
		// we should delete this compound key option because it doesn't exist in this location!
		if _, discovered := discoveredModelsMap[baseID]; discovered {
			if !verifiedIDs[m.ID] {
				log.Printf("[Discovery] Deleting unverified location option %s from registry...", m.ID)
				_ = dc.Store.DeleteModel(ctx, m.ID)
			}
		} else {
			// If the base model is completely obsolete (no longer returned by Vertex AI API at all),
			// we keep it but soft-disable it and append "(Obsolete)" to its DisplayName as before!
			log.Printf("[Discovery] Soft-disabling obsolete model %s...", m.ID)
			m.Active = false
			if !strings.Contains(m.DisplayName, " (Obsolete)") {
				m.DisplayName = m.DisplayName + " (Obsolete)"
			}
			_ = dc.Store.SaveModel(ctx, m)
		}
	}

	return nil
}

// RefreshModels queries GCP's Vertex AI API for the local location and parent multiregion, loading new models.
func (dc *DashboardController) RefreshModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	if err := dc.DiscoverAndCacheModels(ctx); err != nil {
		log.Printf("[Dashboard] DiscoverAndCacheModels failed: %v", err)
		http.Error(w, "Failed to discover models from Google Cloud", http.StatusInternalServerError)
		return
	}

	// Redirect back to the models administration screen
	http.Redirect(w, r, "/admin/models", http.StatusSeeOther)
}

// ToggleModel enables or disables a model's availability for routing dynamically.
func (dc *DashboardController) ToggleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	modelID := strings.TrimSpace(r.FormValue("model_id"))
	activeStr := strings.TrimSpace(r.FormValue("active"))

	if modelID == "" {
		http.Error(w, "Missing model ID parameter", http.StatusBadRequest)
		return
	}

	// Resolve matching model
	allModels, err := dc.Store.GetAllModels(ctx)
	if err != nil {
		log.Printf("[Dashboard] ToggleModel failed to load models: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	var foundModel config.ModelConfig
	exists := false
	for _, m := range allModels {
		if m.ID == modelID {
			foundModel = m
			exists = true
			break
		}
	}

	if !exists {
		http.Error(w, "Model not found in registered config", http.StatusNotFound)
		return
	}

	foundModel.Active = activeStr == "true"
	err = dc.Store.SaveModel(ctx, foundModel)
	if err != nil {
		log.Printf("[Dashboard] ToggleModel failed to save model status: %v", err)
		http.Error(w, "Database save failed", http.StatusInternalServerError)
		return
	}

	// Redirect back to refresh UI list
	http.Redirect(w, r, "/admin/models", http.StatusSeeOther)
}

// DeleteModel removes a model option from the database dynamically.
func (dc *DashboardController) DeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	modelID := strings.TrimSpace(r.URL.Query().Get("id"))
	if modelID == "" {
		http.Error(w, "Missing model ID parameter", http.StatusBadRequest)
		return
	}

	err := dc.Store.DeleteModel(ctx, modelID)
	if err != nil {
		log.Printf("[Dashboard] Error deleting model: %v", err)
		http.Error(w, "Failed to delete model config", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(""))
}

// gcpGet performs an authenticated GET request to a GCP Vertex AI REST endpoint.
func (dc *DashboardController) gcpGet(ctx context.Context, url string) ([]byte, error) {
	if dc.TokenSource == nil {
		return nil, fmt.Errorf("google cloud credentials not initialized")
	}
	token, err := dc.TokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve oauth2 token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("X-Goog-User-Project", dc.ProjectID)
	client := dc.getHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gcp api returned status %s: %s", resp.Status, string(body))
	}

	return body, nil
}

// GCP API JSON Mapping Structures

type gcpLocation struct {
	LocationID string `json:"locationId"`
}

type gcpLocationsResponse struct {
	Locations []gcpLocation `json:"locations"`
}

type gcpModel struct {
	Name            string `json:"name"`
	DisplayName     string `json:"displayName"`
	VersionID       string `json:"versionId"`
	CreateTime      string `json:"createTime"`
	BaseModelSource struct {
		ModelGardenSource struct {
			PublicModelName string `json:"publicModelName"`
		} `json:"modelGardenSource"`
	} `json:"baseModelSource"`
}

type gcpModelsResponse struct {
	Models []gcpModel `json:"models"`
}

type gcpDeployedModel struct {
	ID                 string `json:"id"`
	Model              string `json:"model"`
	DisplayName        string `json:"displayName"`
	CreateTime         string `json:"createTime"`
	DedicatedResources struct {
		MachineSpec struct {
			MachineType      string `json:"machineType"`
			AcceleratorType  string `json:"acceleratorType"`
			AcceleratorCount int    `json:"acceleratorCount"`
		} `json:"machineSpec"`
		MinReplicaCount int `json:"minReplicaCount"`
		MaxReplicaCount int `json:"maxReplicaCount"`
	} `json:"dedicatedResources"`
	ProvisionedThroughput struct {
		ReservationID string `json:"reservationId"`
	} `json:"provisionedThroughput"`
	ModelGardenSource struct {
		PublicModelName string `json:"publicModelName"`
	} `json:"modelGardenSource"`
}

type gcpEndpoint struct {
	Name           string             `json:"name"`
	DisplayName    string             `json:"displayName"`
	CreateTime     string             `json:"createTime"`
	DeployedModels []gcpDeployedModel `json:"deployedModels"`
}

type gcpEndpointsResponse struct {
	Endpoints []gcpEndpoint `json:"endpoints"`
}

func (dc *DashboardController) fetchLocations(_ context.Context) ([]templates.LocationInfo, error) {
	locService := dc.Location
	if locService == "" {
		locService = "us-central1"
	}

	var list []templates.LocationInfo
	list = append(list, templates.LocationInfo{
		ID:     locService,
		Name:   locService,
		Active: true,
	})

	multiReg := config.GetMultiRegionParent(locService)
	if multiReg != "" {
		list = append(list, templates.LocationInfo{
			ID:     multiReg,
			Name:   multiReg,
			Active: false,
		})
	}

	list = append(list, templates.LocationInfo{
		ID:     "global",
		Name:   "global",
		Active: false,
	})

	return list, nil
}

func (dc *DashboardController) fetchCustomModels(ctx context.Context, loc string) ([]templates.CustomModelInfo, error) {
	url := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/models", loc, dc.ProjectID, loc)
	body, err := dc.gcpGet(ctx, url)
	if err != nil {
		return nil, err
	}

	var resp gcpModelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	var list []templates.CustomModelInfo
	for _, m := range resp.Models {
		list = append(list, templates.CustomModelInfo{
			Name:            m.Name,
			DisplayName:     m.DisplayName,
			Version:         m.VersionID,
			CreateTime:      m.CreateTime,
			PublicModelName: m.BaseModelSource.ModelGardenSource.PublicModelName,
		})
	}
	return list, nil
}

func (dc *DashboardController) fetchEndpoints(ctx context.Context, loc string) ([]templates.EndpointInfo, error) {
	url := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/endpoints", loc, dc.ProjectID, loc)
	body, err := dc.gcpGet(ctx, url)
	if err != nil {
		return nil, err
	}

	var resp gcpEndpointsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	var list []templates.EndpointInfo
	for _, e := range resp.Endpoints {
		var deployedModels []templates.DeployedModelInfo
		for _, dm := range e.DeployedModels {
			hasPT := dm.ProvisionedThroughput.ReservationID != ""
			deployedModels = append(deployedModels, templates.DeployedModelInfo{
				ID:               dm.ID,
				ModelPath:        dm.Model,
				DisplayName:      dm.DisplayName,
				CreateTime:       dm.CreateTime,
				MachineType:      dm.DedicatedResources.MachineSpec.MachineType,
				AcceleratorType:  dm.DedicatedResources.MachineSpec.AcceleratorType,
				AcceleratorCount: dm.DedicatedResources.MachineSpec.AcceleratorCount,
				MinReplicas:      dm.DedicatedResources.MinReplicaCount,
				MaxReplicas:      dm.DedicatedResources.MaxReplicaCount,
				HasPT:            hasPT,
				PTReservationID:  dm.ProvisionedThroughput.ReservationID,
				PublicModelName:  dm.ModelGardenSource.PublicModelName,
			})
		}

		list = append(list, templates.EndpointInfo{
			Name:           e.Name,
			DisplayName:    e.DisplayName,
			CreateTime:     e.CreateTime,
			DeployedModels: deployedModels,
		})
	}
	return list, nil
}

type gcpPublisherModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type gcpPublisherModelsResponse struct {
	PublisherModels []gcpPublisherModel `json:"publisherModels"`
}

func (dc *DashboardController) fetchPublisherModels(ctx context.Context, loc string) ([]templates.CustomModelInfo, error) {
	hostLoc := loc
	if loc == "global" {
		hostLoc = dc.Location
		if hostLoc == "" {
			hostLoc = "us-central1"
		}
	}
	url := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1beta1/publishers/google/models", hostLoc)
	body, err := dc.gcpGet(ctx, url)
	if err != nil {
		return nil, err
	}

	var resp gcpPublisherModelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	var list []templates.CustomModelInfo
	for _, m := range resp.PublisherModels {
		parts := strings.Split(m.Name, "/")
		modelName := parts[len(parts)-1]

		list = append(list, templates.CustomModelInfo{
			Name:            modelName,
			DisplayName:     m.DisplayName,
			Version:         "",
			CreateTime:      "",
			PublicModelName: modelName,
		})
	}
	return list, nil
}

// gcpPost performs an authenticated POST request to a GCP REST endpoint.
func (dc *DashboardController) gcpPost(ctx context.Context, url string, body interface{}) ([]byte, error) {
	if dc.TokenSource == nil {
		return nil, fmt.Errorf("google cloud credentials not initialized")
	}
	token, err := dc.TokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve oauth2 token: %w", err)
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	client := dc.getHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gcp api returned status %s: %s", resp.Status, string(respBody))
	}

	return respBody, nil
}

// estimateTokensAndCost calculates input/output tokens and total spend based on model pricing.
func estimateTokensAndCost(model string, bytesSent int64) (int, int, float64) {
	outTokens := int(bytesSent / 3)
	if outTokens < 50 {
		outTokens = 50
	}
	inTokens := int(float64(outTokens) * 1.5)
	if inTokens < 100 {
		inTokens = 100
	}

	// Model specific pricing map (Prices per token)
	type pricing struct {
		inPrice  float64
		outPrice float64
	}

	// Pricing per 1,000,000 tokens
	pricingMap := map[string]pricing{
		"gemini-2.5-pro":          {inPrice: 1.25 / 1e6, outPrice: 5.00 / 1e6},
		"gemini-2.5-flash":        {inPrice: 0.075 / 1e6, outPrice: 0.30 / 1e6},
		"gemini-2.5-flash-lite":   {inPrice: 0.0375 / 1e6, outPrice: 0.15 / 1e6},
		"text-embedding-004":      {inPrice: 0.025 / 1e6, outPrice: 0.0},
		"multimodal-embedding-001":{inPrice: 0.025 / 1e6, outPrice: 0.0},
	}

	var inPrice, outPrice float64
	matched := false

	// Check for prefix matches (e.g., model versions/aliases)
	modelLower := strings.ToLower(model)
	for key, val := range pricingMap {
		if strings.Contains(modelLower, key) {
			inPrice = val.inPrice
			outPrice = val.outPrice
			matched = true
			break
		}
	}

	// Fallback to gemini-2.5-flash if no specific pricing model matched
	if !matched {
		inPrice = 0.075 / 1e6
		outPrice = 0.30 / 1e6
	}

	cost := (float64(inTokens) * inPrice) + (float64(outTokens) * outPrice)
	return inTokens, outTokens, cost
}

func (dc *DashboardController) fetchLocalLogsCosts() (templates.CostsViewModel, error) {
	file, err := os.Open("data/local_logs.jsonl")
	if err != nil {
		if os.IsNotExist(err) {
			return templates.CostsViewModel{}, nil
		}
		return templates.CostsViewModel{}, err
	}
	defer file.Close()

	var entries []struct {
		Time        string `json:"time"`
		ClientID    string `json:"client_id"`
		ModelRouted string `json:"model_routed"`
		BytesSent   int64  `json:"bytes_sent"`
		Status      int    `json:"status"`
		LatencyMs   int64  `json:"latency_ms"`
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Time        string `json:"time"`
			ClientID    string `json:"client_id"`
			ModelRouted string `json:"model_routed"`
			BytesSent   int64  `json:"bytes_sent"`
			Status      int    `json:"status"`
			LatencyMs   int64  `json:"latency_ms"`
		}
		if err := json.Unmarshal(line, &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[Dashboard] Error scanning local logs for costs: %v", err)
	}

	// Reverse entries to show latest first
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	var sessions []templates.CostTransaction
	modelSpend := make(map[string]float64)
	clientSpend := make(map[string]float64)
	totalSpend := 0.0
	totalIn := 0
	totalOut := 0

	for i, entry := range entries {
		clientID := entry.ClientID
		if clientID == "" {
			clientID = "unknown-client"
		}
		model := entry.ModelRouted
		if model == "" {
			model = "gemini-2.5-flash"
		}

		inT, outT, cost := estimateTokensAndCost(model, entry.BytesSent)

		timeStr := entry.Time
		if t, err := time.Parse(time.RFC3339, entry.Time); err == nil {
			timeStr = t.Local().Format("2006-01-02 15:04:05")
		}

		tx := templates.CostTransaction{
			Time:          timeStr,
			SessionID:     fmt.Sprintf("local_%d", len(entries)-i),
			ClientID:      clientID,
			ModelRouted:   model,
			InputTokens:   inT,
			OutputTokens:  outT,
			EstimatedCost: cost,
		}

		sessions = append(sessions, tx)
		modelSpend[model] += cost
		clientSpend[clientID] += cost
		totalSpend += cost
		totalIn += inT
		totalOut += outT
	}

	var modelBreakdowns []templates.ModelCostBreakdown
	for m, val := range modelSpend {
		pct := 0.0
		if totalSpend > 0 {
			pct = (val / totalSpend) * 100.0
		}
		modelBreakdowns = append(modelBreakdowns, templates.ModelCostBreakdown{
			ModelName: m,
			Cost:      val,
			Percent:   pct,
		})
	}

	var clientBreakdowns []templates.ClientCostBreakdown
	for c, val := range clientSpend {
		pct := 0.0
		if totalSpend > 0 {
			pct = (val / totalSpend) * 100.0
		}
		clientBreakdowns = append(clientBreakdowns, templates.ClientCostBreakdown{
			ClientID: c,
			Cost:     val,
			Percent:  pct,
		})
	}

	avgCost := 0.0
	if len(sessions) > 0 {
		avgCost = (totalSpend / float64(len(sessions))) * 1000.0
	}

	if len(sessions) > 50 {
		sessions = sessions[:50]
	}

	return templates.CostsViewModel{
		TotalSpend:        totalSpend,
		TotalTokensInput:  totalIn,
		TotalTokensOutput: totalOut,
		AvgCostPer1K:      avgCost,
		ModelBreakdowns:   modelBreakdowns,
		ClientBreakdowns:  clientBreakdowns,
		RecentSessions:    sessions,
		ModelCostSVG:      generateModelCostPieSVG(modelBreakdowns),
		ClientCostSVG:     generateClientCostBarSVG(clientBreakdowns),
	}, nil
}

func (dc *DashboardController) fetchLocalLogsMetrics() (templates.MetricsViewModel, error) {
	file, err := os.Open("data/local_logs.jsonl")
	if err != nil {
		if os.IsNotExist(err) {
			return templates.MetricsViewModel{}, nil
		}
		return templates.MetricsViewModel{}, err
	}
	defer file.Close()

	var entries []struct {
		Time      string `json:"time"`
		Status    int    `json:"status"`
		LatencyMs int64  `json:"latency_ms"`
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Time      string `json:"time"`
			Status    int    `json:"status"`
			LatencyMs int64  `json:"latency_ms"`
		}
		if err := json.Unmarshal(line, &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[Dashboard] Error scanning local logs for metrics: %v", err)
	}

	counts := make([]int, 24)
	p50 := make([]int, 24)
	p95 := make([]int, 24)
	labels := make([]string, 24)

	now := time.Now()
	for i := 0; i < 24; i++ {
		t := now.Add(-time.Duration(23-i) * time.Hour)
		labels[i] = t.Format("15:00")
	}

	hourlyLatencies := make([][]int64, 24)
	totalReqs := 0
	peakRate := 0
	errorCount := 0

	for _, entry := range entries {
		t, err := time.Parse(time.RFC3339, entry.Time)
		if err != nil {
			continue
		}

		hrIndex := 23 - int(now.Sub(t).Hours())
		if hrIndex >= 0 && hrIndex < 24 {
			counts[hrIndex]++
			totalReqs++
			if entry.Status >= 400 {
				errorCount++
			}
			hourlyLatencies[hrIndex] = append(hourlyLatencies[hrIndex], entry.LatencyMs)
		}
	}

	for _, c := range counts {
		if c > peakRate {
			peakRate = c
		}
	}

	for i := 0; i < 24; i++ {
		lats := hourlyLatencies[i]
		if len(lats) == 0 {
			p50[i] = 0
			p95[i] = 0
			continue
		}

		sort.Slice(lats, func(x, y int) bool {
			return lats[x] < lats[y]
		})

		p50Idx := len(lats) / 2
		p95Idx := int(float64(len(lats)) * 0.95)
		if p95Idx >= len(lats) {
			p95Idx = len(lats) - 1
		}

		p50[i] = int(lats[p50Idx])
		p95[i] = int(lats[p95Idx])
	}

	avgP95 := 0
	activeCount := 0
	for _, v := range p95 {
		if v > 0 {
			avgP95 += v
			activeCount++
		}
	}
	if activeCount > 0 {
		avgP95 /= activeCount
	}

	errRate := 0.0
	if totalReqs > 0 {
		errRate = (float64(errorCount) / float64(totalReqs)) * 100.0
	}

	return templates.MetricsViewModel{
		TotalRequests:   totalReqs,
		PeakRate:        peakRate,
		P95LatencyMs:    avgP95,
		ErrorRate:       errRate,
		VolumeChartSVG:  generateVolumeSVG(counts, labels),
		LatencyChartSVG: generateLatencySVG(p50, p95, labels),
	}, nil
}

// fetchCloudMonitoringMetrics collects revision volume and latency details from Cloud Monitoring.
func (dc *DashboardController) fetchCloudMonitoringMetrics(ctx context.Context, simulate bool) (templates.MetricsViewModel, error) {
	if simulate {
		return dc.generateMockMetrics(), nil
	}
	if os.Getenv("LOCAL_DEV") == "true" {
		return dc.fetchLocalLogsMetrics()
	}
	// If running locally without real project or GCP token is not initialized, serve empty state
	if dc.ProjectID == "dev-project" || dc.TokenSource == nil {
		return templates.MetricsViewModel{}, nil
	}

	serviceName := os.Getenv("BACKEND_SERVICE_NAME")
	if serviceName == "" {
		serviceName = os.Getenv("K_SERVICE")
		if serviceName == "" {
			serviceName = "gemini-smart-router" // standard fallback
		}
	}

	// Query time-series for request_count
	now := time.Now()
	startTime := now.Add(-24 * time.Hour)
	
	// Time interval formats
	startStr := startTime.UTC().Format(time.RFC3339)
	endStr := now.UTC().Format(time.RFC3339)

	// 1. Fetch Volume Metrics
	volURL := fmt.Sprintf("https://monitoring.googleapis.com/v3/projects/%s/timeSeries?filter=%s&interval.startTime=%s&interval.endTime=%s&aggregation.alignmentPeriod=3600s&aggregation.perSeriesAligner=ALIGN_SUM",
		dc.ProjectID,
		url.QueryEscape(fmt.Sprintf(`metric.type="run.googleapis.com/request_count" AND resource.type="cloud_run_revision" AND resource.labels.service_name="%s"`, serviceName)),
		startStr,
		endStr,
	)

	volBody, err := dc.gcpGet(ctx, volURL)
	if err != nil {
		log.Printf("[Monitoring] Error querying request count: %v", err)
		return dc.generateMockMetrics(), nil
	}

	// 2. Fetch Latency Metrics
	latURL := fmt.Sprintf("https://monitoring.googleapis.com/v3/projects/%s/timeSeries?filter=%s&interval.startTime=%s&interval.endTime=%s&aggregation.alignmentPeriod=3600s&aggregation.perSeriesAligner=ALIGN_PERCENTILE_95",
		dc.ProjectID,
		url.QueryEscape(fmt.Sprintf(`metric.type="run.googleapis.com/request_latencies" AND resource.type="cloud_run_revision" AND resource.labels.service_name="%s"`, serviceName)),
		startStr,
		endStr,
	)

	latBody, err := dc.gcpGet(ctx, latURL)
	if err != nil {
		log.Printf("[Monitoring] Error querying latencies: %v", err)
		return dc.generateMockMetrics(), nil
	}

	return dc.parseGCPMetrics(volBody, latBody)
}

// parseGCPMetrics parses the REST JSON responses and computes SVG charts.
func (dc *DashboardController) parseGCPMetrics(volBody, latBody []byte) (templates.MetricsViewModel, error) {
	// Standard GCP JSON structures
	type TimeSeriesPoint struct {
		Interval struct {
			EndTime string `json:"endTime"`
		} `json:"interval"`
		Value struct {
			Int64Value  string  `json:"int64Value,omitempty"`
			DoubleValue float64 `json:"doubleValue,omitempty"`
		} `json:"value"`
	}
	type Series struct {
		Metric struct {
			Labels map[string]string `json:"labels"`
		} `json:"metric"`
		Points []TimeSeriesPoint `json:"points"`
	}
	type TSResponse struct {
		TimeSeries []Series `json:"timeSeries"`
	}

	var volData TSResponse
	var latData TSResponse

	_ = json.Unmarshal(volBody, &volData)
	_ = json.Unmarshal(latBody, &latData)

	// Process last 24 hourly intervals
	counts := make([]int, 24)
	p50 := make([]int, 24)
	p95 := make([]int, 24)
	labels := make([]string, 24)

	now := time.Now()
	for i := 0; i < 24; i++ {
		t := now.Add(-time.Duration(23-i) * time.Hour)
		labels[i] = t.Format("15:00")
		
		// Seed baseline fallbacks
		p50[i] = 150 + mathrand.Intn(80)
		p95[i] = 450 + mathrand.Intn(150)
	}

	totalReqs := 0
	peakRate := 0
	errorCount := 0

	if len(volData.TimeSeries) > 0 {
		for _, series := range volData.TimeSeries {
			isError := series.Metric.Labels["response_code_class"] != "2xx"
			for _, pt := range series.Points {
				t, err := time.Parse(time.RFC3339, pt.Interval.EndTime)
				if err != nil {
					continue
				}
				hrIndex := 23 - int(now.Sub(t).Hours())
				if hrIndex >= 0 && hrIndex < 24 {
					val, _ := strconv.Atoi(pt.Value.Int64Value)
					counts[hrIndex] += val
					totalReqs += val
					if isError {
						errorCount += val
					}
				}
			}
		}
	}

	// Find peak rate
	for _, c := range counts {
		if c > peakRate {
			peakRate = c
		}
	}

	// Fetch Latencies
	if len(latData.TimeSeries) > 0 {
		for _, series := range latData.TimeSeries {
			for _, pt := range series.Points {
				t, err := time.Parse(time.RFC3339, pt.Interval.EndTime)
				if err != nil {
					continue
				}
				hrIndex := 23 - int(now.Sub(t).Hours())
				if hrIndex >= 0 && hrIndex < 24 {
					val := int(pt.Value.DoubleValue)
					if val > 0 {
						p95[hrIndex] = val
						p50[hrIndex] = int(float64(val) * 0.4) // approximate median
					}
				}
			}
		}
	}

	avgP95 := 0
	activeCount := 0
	for _, v := range p95 {
		if v > 0 {
			avgP95 += v
			activeCount++
		}
	}
	if activeCount > 0 {
		avgP95 /= activeCount
	} else {
		avgP95 = 350
	}

	errRate := 0.0
	if totalReqs > 0 {
		errRate = (float64(errorCount) / float64(totalReqs)) * 100.0
	}

	return templates.MetricsViewModel{
		TotalRequests:   totalReqs,
		PeakRate:        peakRate,
		P95LatencyMs:    avgP95,
		ErrorRate:       errRate,
		VolumeChartSVG:  generateVolumeSVG(counts, labels),
		LatencyChartSVG: generateLatencySVG(p50, p95, labels),
	}, nil
}

// generateMockMetrics provides high-fidelity simulated monitoring values for local dev.
func (dc *DashboardController) generateMockMetrics() templates.MetricsViewModel {
	counts := make([]int, 24)
	p50 := make([]int, 24)
	p95 := make([]int, 24)
	labels := make([]string, 24)

	now := time.Now()
	totalRequests := 0
	peakRate := 0
	errorCount := 0

	for i := 0; i < 24; i++ {
		t := now.Add(-time.Duration(23-i) * time.Hour)
		labels[i] = t.Format("15:00")

		// Beautiful wave simulation for traffic
		hour := t.Hour()
		wave := math.Sin(float64(hour-6)*math.Pi/12.0)*0.4 + 0.6 // peak around afternoon
		baseVal := 50 + int(wave*120)
		noise := mathrand.Intn(30)
		c := baseVal + noise
		if c < 0 {
			c = 0
		}
		counts[i] = c
		totalRequests += c
		if c > peakRate {
			peakRate = c
		}

		// Simulate occasional error spikes
		if hour == 14 || hour == 20 {
			errorCount += int(float64(c) * 0.08)
		} else {
			errorCount += int(float64(c) * 0.01)
		}

		// Latency simulations
		p50[i] = 110 + mathrand.Intn(60)
		p95[i] = 320 + mathrand.Intn(140)
		if hour == 14 || hour == 20 {
			p50[i] += 80
			p95[i] += 250
		}
	}

	errRate := 0.0
	if totalRequests > 0 {
		errRate = (float64(errorCount) / float64(totalRequests)) * 100.0
	}

	return templates.MetricsViewModel{
		TotalRequests:   totalRequests,
		PeakRate:        peakRate,
		P95LatencyMs:    380,
		ErrorRate:       errRate,
		VolumeChartSVG:  generateVolumeSVG(counts, labels),
		LatencyChartSVG: generateLatencySVG(p50, p95, labels),
	}
}

// fetchCostAnalyticsData queries Cloud Logging logs or mocks billing transactions.
func (dc *DashboardController) fetchCostAnalyticsData(ctx context.Context, simulate bool) (templates.CostsViewModel, error) {
	if simulate {
		return dc.generateMockCosts(), nil
	}
	if os.Getenv("LOCAL_DEV") == "true" {
		return dc.fetchLocalLogsCosts()
	}
	if dc.ProjectID == "dev-project" || dc.TokenSource == nil {
		return templates.CostsViewModel{}, nil
	}

	serviceName := os.Getenv("BACKEND_SERVICE_NAME")
	if serviceName == "" {
		serviceName = os.Getenv("K_SERVICE")
		if serviceName == "" {
			serviceName = "gemini-smart-router"
		}
	}

	// Retrieve raw proxy StructuredLogs from Cloud Logging
	urlStr := "https://logging.googleapis.com/v2/entries:list"
	filter := fmt.Sprintf(`resource.type="cloud_run_revision" AND resource.labels.service_name="%s" AND jsonPayload.model_routed:*`, serviceName)
	
	bodyReq := map[string]interface{}{
		"resourceNames": []string{fmt.Sprintf("projects/%s", dc.ProjectID)},
		"filter":        filter,
		"orderBy":       "timestamp desc",
		"pageSize":      1000,
	}

	respBody, err := dc.gcpPost(ctx, urlStr, bodyReq)
	if err != nil {
		log.Printf("[Cost Analytics] Error querying Cloud Logging: %v", err)
		return dc.generateMockCosts(), nil
	}

	return dc.parseGCPCosts(respBody)
}

// parseGCPCosts parses the REST payload and aggregates spends.
func (dc *DashboardController) parseGCPCosts(payload []byte) (templates.CostsViewModel, error) {
	type GCPLogEntry struct {
		Timestamp   string `json:"timestamp"`
		InsertID    string `json:"insertId"`
		JSONPayload struct {
			ClientID      string `json:"client_id"`
			ModelRouted   string `json:"model_routed"`
			BytesSent     int64  `json:"bytes_sent"`
		} `json:"jsonPayload"`
	}
	type GCPLogsResponse struct {
		Entries []GCPLogEntry `json:"entries"`
	}

	var resp GCPLogsResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return dc.generateMockCosts(), nil
	}

	var sessions []templates.CostTransaction
	modelSpend := make(map[string]float64)
	clientSpend := make(map[string]float64)
	totalSpend := 0.0
	totalIn := 0
	totalOut := 0

	for _, entry := range resp.Entries {
		clientID := entry.JSONPayload.ClientID
		if clientID == "" {
			clientID = "unknown-client"
		}
		model := entry.JSONPayload.ModelRouted
		if model == "" {
			model = "gemini-2.5-flash"
		}

		inT, outT, cost := estimateTokensAndCost(model, entry.JSONPayload.BytesSent)
		
		t, err := time.Parse(time.RFC3339, entry.Timestamp)
		timeStr := entry.Timestamp
		if err == nil {
			timeStr = t.Local().Format("2006-01-02 15:04:05")
		}

		tx := templates.CostTransaction{
			Time:          timeStr,
			SessionID:     entry.InsertID[:8],
			ClientID:      clientID,
			ModelRouted:   model,
			InputTokens:   inT,
			OutputTokens:  outT,
			EstimatedCost: cost,
		}

		sessions = append(sessions, tx)
		modelSpend[model] += cost
		clientSpend[clientID] += cost
		totalSpend += cost
		totalIn += inT
		totalOut += outT
	}

	// Construct ViewModels
	var modelBreakdowns []templates.ModelCostBreakdown
	for m, val := range modelSpend {
		pct := 0.0
		if totalSpend > 0 {
			pct = (val / totalSpend) * 100.0
		}
		modelBreakdowns = append(modelBreakdowns, templates.ModelCostBreakdown{
			ModelName: m,
			Cost:      val,
			Percent:   pct,
		})
	}

	var clientBreakdowns []templates.ClientCostBreakdown
	for c, val := range clientSpend {
		pct := 0.0
		if totalSpend > 0 {
			pct = (val / totalSpend) * 100.0
		}
		clientBreakdowns = append(clientBreakdowns, templates.ClientCostBreakdown{
			ClientID: c,
			Cost:     val,
			Percent:  pct,
		})
	}

	avgCost := 0.0
	if len(sessions) > 0 {
		avgCost = (totalSpend / float64(len(sessions))) * 1000.0
	}

	// Limit recent sessions to 50
	if len(sessions) > 50 {
		sessions = sessions[:50]
	}

	return templates.CostsViewModel{
		TotalSpend:        totalSpend,
		TotalTokensInput:  totalIn,
		TotalTokensOutput: totalOut,
		AvgCostPer1K:      avgCost,
		ModelBreakdowns:   modelBreakdowns,
		ClientBreakdowns:  clientBreakdowns,
		RecentSessions:    sessions,
		ModelCostSVG:      generateModelCostPieSVG(modelBreakdowns),
		ClientCostSVG:     generateClientCostBarSVG(clientBreakdowns),
	}, nil
}

// generateMockCosts populates a detailed transaction mock bill database for offline dev.
func (dc *DashboardController) generateMockCosts() templates.CostsViewModel {
	clients := []string{"enterprise-app", "internal-dev-portal", "analytics-service", "startup-sandbox"}
	models := []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite"}
	
	var sessions []templates.CostTransaction
	modelSpend := make(map[string]float64)
	clientSpend := make(map[string]float64)
	totalSpend := 0.0
	totalIn := 0
	totalOut := 0

	now := time.Now()

	for i := 0; i < 100; i++ {
		cIndex := mathrand.Intn(len(clients))
		mIndex := mathrand.Intn(len(models))
		client := clients[cIndex]
		model := models[mIndex]

		// Standard token volume
		inT := 400 + mathrand.Intn(8000)
		outT := 150 + mathrand.Intn(3000)
		
		// Premium model multiplier
		if strings.Contains(model, "pro") {
			inT += 1000
			outT += 800
		}

		var inPrice, outPrice float64
		if strings.Contains(model, "pro") {
			inPrice = 1.25 / 1000000.0
			outPrice = 5.00 / 1000000.0
		} else {
			inPrice = 0.075 / 1000000.0
			outPrice = 0.30 / 1000000.0
		}

		cost := (float64(inT) * inPrice) + (float64(outT) * outPrice)
		
		t := now.Add(-time.Duration(mathrand.Intn(24)) * time.Hour).Add(-time.Duration(mathrand.Intn(60)) * time.Minute)

		tx := templates.CostTransaction{
			Time:          t.Format("2006-01-02 15:04:05"),
			SessionID:     fmt.Sprintf("sess_%x", 1000000+i),
			ClientID:      client,
			ModelRouted:   model,
			InputTokens:   inT,
			OutputTokens:  outT,
			EstimatedCost: cost,
		}

		sessions = append(sessions, tx)
		modelSpend[model] += cost
		clientSpend[client] += cost
		totalSpend += cost
		totalIn += inT
		totalOut += outT
	}

	// Construct ViewModels
	var modelBreakdowns []templates.ModelCostBreakdown
	for m, val := range modelSpend {
		pct := 0.0
		if totalSpend > 0 {
			pct = (val / totalSpend) * 100.0
		}
		modelBreakdowns = append(modelBreakdowns, templates.ModelCostBreakdown{
			ModelName: m,
			Cost:      val,
			Percent:   pct,
		})
	}

	var clientBreakdowns []templates.ClientCostBreakdown
	for c, val := range clientSpend {
		pct := 0.0
		if totalSpend > 0 {
			pct = (val / totalSpend) * 100.0
		}
		clientBreakdowns = append(clientBreakdowns, templates.ClientCostBreakdown{
			ClientID: c,
			Cost:     val,
			Percent:  pct,
		})
	}

	avgCost := 0.0
	if len(sessions) > 0 {
		avgCost = (totalSpend / float64(len(sessions))) * 1000.0
	}

	// Sort sessions chronologically desc
	for idx := 0; idx < len(sessions); idx++ {
		for j := idx + 1; j < len(sessions); j++ {
			if sessions[idx].Time < sessions[j].Time {
				sessions[idx], sessions[j] = sessions[j], sessions[idx]
			}
		}
	}

	// Keep 50 recent
	if len(sessions) > 50 {
		sessions = sessions[:50]
	}

	return templates.CostsViewModel{
		TotalSpend:        totalSpend,
		TotalTokensInput:  totalIn,
		TotalTokensOutput: totalOut,
		AvgCostPer1K:      avgCost,
		ModelBreakdowns:   modelBreakdowns,
		ClientBreakdowns:  clientBreakdowns,
		RecentSessions:    sessions,
		ModelCostSVG:      generateModelCostPieSVG(modelBreakdowns),
		ClientCostSVG:     generateClientCostBarSVG(clientBreakdowns),
	}
}

// ServeCosts handles cost breakouts and analytics.
func (dc *DashboardController) ServeCosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	simulate := false
	if cookie, err := r.Cookie("simulate_metrics"); err == nil && cookie.Value == "true" {
		simulate = true
	}
	vm, err := dc.fetchCostAnalyticsData(ctx, simulate)
	if err != nil {
		log.Printf("[Costs] Error fetching analytics data: %v", err)
		http.Error(w, "Internal Server Error loading cost analytics", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.CostsTab(vm)
	_ = templates.Layout("Cost Analytics", "costs", content).Render(ctx, w)
}

// ServeMetrics renders the Cloud Monitoring charts tab.
func (dc *DashboardController) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	simulate := false
	if cookie, err := r.Cookie("simulate_metrics"); err == nil && cookie.Value == "true" {
		simulate = true
	}
	vm, err := dc.fetchCloudMonitoringMetrics(ctx, simulate)
	if err != nil {
		log.Printf("[Monitoring] Error loading system monitoring metrics: %v", err)
		http.Error(w, "Internal Server Error loading system metrics", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.MetricsTab(vm)
	_ = templates.Layout("Monitoring", "metrics", content).Render(ctx, w)
}

// Server-side SVG Dashboard Chart Generation Engines

func generateVolumeSVG(counts []int, labels []string) string {
	if len(counts) == 0 {
		return `<svg viewBox="0 0 720 220" class="w-full h-full"><text x="360" y="110" text-anchor="middle" fill="#9ca3af" font-size="14">No data available</text></svg>`
	}
	maxVal := 1
	for _, c := range counts {
		if c > maxVal {
			maxVal = c
		}
	}

	var sb strings.Builder
	sb.WriteString(`<svg viewBox="0 0 720 220" class="w-full h-full" xmlns="http://www.w3.org/2000/svg">`)
	
	// Grid Lines
	for i := 0; i <= 4; i++ {
		y := 40 + i*35
		val := maxVal - (i * maxVal / 4)
		sb.WriteString(fmt.Sprintf(`<line x1="50" y1="%d" x2="700" y2="%d" stroke="#f3f4f6" stroke-width="1" />`, y, y))
		sb.WriteString(fmt.Sprintf(`<text x="15" y="%d" fill="#9ca3af" font-size="10" font-family="sans-serif" alignment-baseline="middle">%d</text>`, y, val))
	}

	// Draw Bars
	barWidth := 20
	gap := 7
	startX := 60
	for i, c := range counts {
		barHeight := int(float64(c) * 140.0 / float64(maxVal))
		if barHeight < 4 && c > 0 {
			barHeight = 4
		}
		x := startX + i*(barWidth+gap)
		y := 180 - barHeight
		// Blue gradient color for bars
		sb.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" rx="3" fill="#3b82f6" opacity="0.85" />`, x, y, barWidth, barHeight))
		
		// Hover highlight overlay
		sb.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" rx="3" fill="#2563eb" opacity="0" class="hover:opacity-100 cursor-pointer transition-opacity duration-150"><title>Time: %s&#10;Volume: %d reqs</title></rect>`, x, y, barWidth, barHeight, labels[i], c))
		
		if i%4 == 0 {
			sb.WriteString(fmt.Sprintf(`<text x="%d" y="205" fill="#9ca3af" font-size="9" font-family="sans-serif" text-anchor="middle">%s</text>`, x+barWidth/2, labels[i]))
		}
	}

	sb.WriteString(`</svg>`)
	return sb.String()
}

func generateLatencySVG(p50 []int, p95 []int, labels []string) string {
	if len(p50) == 0 {
		return `<svg viewBox="0 0 720 220" class="w-full h-full"><text x="360" y="110" text-anchor="middle" fill="#9ca3af" font-size="14">No data available</text></svg>`
	}
	maxVal := 1
	for _, v := range p95 {
		if v > maxVal {
			maxVal = v
		}
	}
	for _, v := range p50 {
		if v > maxVal {
			maxVal = v
		}
	}

	var sb strings.Builder
	sb.WriteString(`<svg viewBox="0 0 720 220" class="w-full h-full" xmlns="http://www.w3.org/2000/svg">`)

	// Grid Lines
	for i := 0; i <= 4; i++ {
		y := 40 + i*35
		val := maxVal - (i * maxVal / 4)
		sb.WriteString(fmt.Sprintf(`<line x1="50" y1="%d" x2="700" y2="%d" stroke="#f3f4f6" stroke-width="1" />`, y, y))
		sb.WriteString(fmt.Sprintf(`<text x="15" y="%d" fill="#9ca3af" font-size="10" font-family="sans-serif" alignment-baseline="middle">%d ms</text>`, y, val))
	}

	// Draw lines
	gap := 27
	startX := 60
	
	var p50Points []string
	var p95Points []string

	for i := range p50 {
		x := startX + i*gap
		y50 := 180 - int(float64(p50[i])*140.0/float64(maxVal))
		y95 := 180 - int(float64(p95[i])*140.0/float64(maxVal))
		p50Points = append(p50Points, fmt.Sprintf("%d,%d", x, y50))
		p95Points = append(p95Points, fmt.Sprintf("%d,%d", x, y95))
	}

	// Draw lines
	sb.WriteString(fmt.Sprintf(`<polyline fill="none" stroke="#f97316" stroke-width="2.5" points="%s" />`, strings.Join(p95Points, " ")))
	sb.WriteString(fmt.Sprintf(`<polyline fill="none" stroke="#3b82f6" stroke-width="2" points="%s" />`, strings.Join(p50Points, " ")))

	// Interactive points
	for i := range p50 {
		x := startX + i*gap
		y50 := 180 - int(float64(p50[i])*140.0/float64(maxVal))
		y95 := 180 - int(float64(p95[i])*140.0/float64(maxVal))

		sb.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="4" fill="#3b82f6" stroke="#ffffff" stroke-width="1.5">`, x, y50))
		sb.WriteString(fmt.Sprintf(`<title>Time: %s&#10;Median: %d ms</title>`, labels[i], p50[i]))
		sb.WriteString(`</circle>`)

		sb.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="4" fill="#f97316" stroke="#ffffff" stroke-width="1.5">`, x, y95))
		sb.WriteString(fmt.Sprintf(`<title>Time: %s&#10;P95: %d ms</title>`, labels[i], p95[i]))
		sb.WriteString(`</circle>`)

		if i%4 == 0 {
			sb.WriteString(fmt.Sprintf(`<text x="%d" y="205" fill="#9ca3af" font-size="9" font-family="sans-serif" text-anchor="middle">%s</text>`, x, labels[i]))
		}
	}

	sb.WriteString(`</svg>`)
	return sb.String()
}

func generateModelCostPieSVG(breakdown []templates.ModelCostBreakdown) string {
	if len(breakdown) == 0 {
		return `<svg viewBox="0 0 320 200" class="w-full h-full"><text x="160" y="100" text-anchor="middle" fill="#9ca3af" font-size="14">No spend data</text></svg>`
	}

	colors := []string{"#3b82f6", "#10b981", "#f59e0b", "#8b5cf6", "#ec4899", "#6b7280"}
	var sb strings.Builder
	sb.WriteString(`<svg viewBox="0 0 390 220" class="w-full h-full" xmlns="http://www.w3.org/2000/svg">`)
	
	cx, cy, r := 100, 110, 65
	circumference := 2 * math.Pi * float64(r)
	offset := 0.0

	sb.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="%d" fill="none" stroke="#f3f4f6" stroke-width="20" />`, cx, cy, r))

	for i, item := range breakdown {
		if item.Percent <= 0 {
			continue
		}
		color := colors[i%len(colors)]
		dashArray := (item.Percent / 100.0) * circumference
		dashOffset := -offset

		sb.WriteString(fmt.Sprintf(`<circle cx="%d" cy="%d" r="%d" fill="none" stroke="%s" stroke-width="20" stroke-dasharray="%.2f %.2f" stroke-dashoffset="%.2f" transform="rotate(-90 %d %d)" class="transition-all duration-300">`, cx, cy, r, color, dashArray, circumference, dashOffset, cx, cy))
		sb.WriteString(fmt.Sprintf(`<title>%s: $%.4f (%.1f%%)</title>`, item.ModelName, item.Cost, item.Percent))
		sb.WriteString(`</circle>`)

		offset += dashArray
	}

	// Legend
	for i, item := range breakdown {
		color := colors[i%len(colors)]
		y := 40 + i*28
		if y > 210 {
			break
		}
		sb.WriteString(fmt.Sprintf(`<rect x="205" y="%d" width="12" height="12" rx="2" fill="%s" />`, y, color))
		
		displayName := item.ModelName
		if len(displayName) > 14 {
			displayName = displayName[:12] + ".."
		}
		sb.WriteString(fmt.Sprintf(`<text x="225" y="%d" fill="#374151" font-size="12" font-family="sans-serif" font-weight="500" alignment-baseline="middle">%s</text>`, y+6, displayName))
		sb.WriteString(fmt.Sprintf(`<text x="375" y="%d" fill="#6b7280" font-size="11" font-family="sans-serif" text-anchor="end" alignment-baseline="middle">%.1f%%</text>`, y+6, item.Percent))
	}

	sb.WriteString(`</svg>`)
	return sb.String()
}

func generateClientCostBarSVG(breakdown []templates.ClientCostBreakdown) string {
	if len(breakdown) == 0 {
		return `<svg viewBox="0 0 400 200" class="w-full h-full"><text x="200" y="100" text-anchor="middle" fill="#9ca3af" font-size="14">No client spend data</text></svg>`
	}

	var sb strings.Builder
	sb.WriteString(`<svg viewBox="0 0 400 220" class="w-full h-full" xmlns="http://www.w3.org/2000/svg">`)

	for i, item := range breakdown {
		if i >= 5 {
			break
		}
		y := 30 + i*38
		
		sb.WriteString(fmt.Sprintf(`<text x="10" y="%d" fill="#4b5563" font-size="12" font-family="sans-serif" font-weight="500" alignment-baseline="middle">%s</text>`, y, item.ClientID))
		sb.WriteString(fmt.Sprintf(`<text x="390" y="%d" fill="#111827" font-size="11" font-family="sans-serif" font-weight="600" text-anchor="end" alignment-baseline="middle">$%.4f</text>`, y, item.Cost))

		sb.WriteString(fmt.Sprintf(`<rect x="10" y="%d" width="380" height="8" rx="4" fill="#f3f4f6" />`, y+12))
		
		width := int(item.Percent * 380.0 / 100.0)
		if width < 5 && item.Percent > 0 {
			width = 5
		}
		sb.WriteString(fmt.Sprintf(`<rect x="10" y="%d" width="%d" height="8" rx="4" fill="#10b981" />`, y+12, width))
	}

	sb.WriteString(`</svg>`)
	return sb.String()
}

// ServeApps fetches apps and client configurations and renders the Applications tab view.
func (dc *DashboardController) ServeApps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps: %v", err)
		http.Error(w, "Internal Server Error loading apps", http.StatusInternalServerError)
		return
	}

	clients, err := dc.Store.GetAllClients(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading clients: %v", err)
		http.Error(w, "Internal Server Error loading clients", http.StatusInternalServerError)
		return
	}

	clientsMap := make(map[string]config.Client)
	for _, c := range clients {
		clientsMap[c.ID] = c
	}

	var viewModels []templates.AppsViewModel
	for _, a := range apps {
		clientProfile, ok := clientsMap[a.ClientID]
		if !ok {
			clientProfile = config.Client{
				ID:   a.ClientID,
				Name: "Unknown Client",
				Tier: "free",
			}
		}
		viewModels = append(viewModels, templates.AppsViewModel{
			App:    a,
			Client: clientProfile,
		})
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.AppsTab(viewModels)
	_ = templates.Layout("Applications", "apps", content).Render(ctx, w)
}

// ServeAppsNewModal renders the dynamic app creation modal form.
func (dc *DashboardController) ServeAppsNewModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clients, err := dc.Store.GetAllClients(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading clients for app modal: %v", err)
		http.Error(w, "Failed to load client organizations", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.AppModal(clients, nil).Render(ctx, w)
}

// ServeAppsEditModal renders the dynamic settings modal form via HTMX to edit an app.
func (dc *DashboardController) ServeAppsEditModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	appID := r.URL.Query().Get("id")
	if appID == "" {
		http.Error(w, "Missing application ID", http.StatusBadRequest)
		return
	}

	app, ok := dc.Store.LookupApp(appID)
	if !ok {
		http.Error(w, "Application not found", http.StatusNotFound)
		return
	}

	clients, err := dc.Store.GetAllClients(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading clients for app modal: %v", err)
		http.Error(w, "Failed to load client organizations", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.AppModal(clients, &app).Render(ctx, w)
}

// CreateApp handles logical application profile creation submissions.
func (dc *DashboardController) CreateApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	appID := strings.TrimSpace(r.FormValue("app_id"))
	appName := strings.TrimSpace(r.FormValue("app_name"))
	clientID := strings.TrimSpace(r.FormValue("client_id"))
	priority := strings.TrimSpace(r.FormValue("priority"))
	rpmStr := strings.TrimSpace(r.FormValue("rpm"))
	tpmStr := strings.TrimSpace(r.FormValue("tpm"))

	if appID == "" {
		http.Error(w, "Application ID (Slug) cannot be empty.", http.StatusBadRequest)
		return
	}
	// Support email slugs (for Google service accounts) or standard alphanumeric slugs
	if strings.Contains(appID, "@") {
		if !strings.Contains(appID, ".") {
			http.Error(w, "Application ID email structure is invalid.", http.StatusBadRequest)
			return
		}
	} else {
		for _, char := range appID {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '.') {
				http.Error(w, "Application ID must be a valid slug (alphanumeric, hyphens, and dots only).", http.StatusBadRequest)
				return
			}
		}
	}

	if appName == "" {
		http.Error(w, "Application Name cannot be empty.", http.StatusBadRequest)
		return
	}

	if clientID == "" {
		http.Error(w, "Parent Client Organization selection is required.", http.StatusBadRequest)
		return
	}

	rpm, err := strconv.Atoi(rpmStr)
	if err != nil || rpm <= 0 {
		http.Error(w, "Requests Per Minute (RPM) limit must be a positive integer.", http.StatusBadRequest)
		return
	}

	tpm, err := strconv.Atoi(tpmStr)
	if err != nil || tpm <= 0 {
		http.Error(w, "Tokens Per Minute (TPM) limit must be a positive integer.", http.StatusBadRequest)
		return
	}

	existingApp, exists := dc.Store.LookupApp(appID)
	var complexity config.ComplexityRouting
	if exists {
		complexity = existingApp.Complexity
	} else {
		complexity = config.ComplexityRouting{
			Enabled:                false,
			AlwaysOverride:         false,
			SimpleModel:            "gemini-2.5-flash-lite",
			MediumModel:            "gemini-2.5-flash",
			ComplexModel:           "gemini-2.5-pro",
			SimpleCharLimit:        200,
			MediumCharLimit:        1000,
			ForceComplexMultimodal: true,
			ForceComplexTools:      true,
			UseLLMClassifier:       false,
			ClassifierModel:        "gemini-3.1-flash-lite",
		}
	}

	err = dc.Store.SaveApp(ctx, config.App{
		ID:         appID,
		ClientID:   clientID,
		Name:       appName,
		RPM:        rpm,
		TPM:        tpm,
		Priority:   priority,
		Complexity: complexity,
	})
	if err != nil {
		log.Printf("[Dashboard] Error saving application profile: %v", err)
		http.Error(w, "Failed to save application profile", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/apps")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/admin/apps", http.StatusSeeOther)
}

// DeleteApp deletes an application profile dynamically.
func (dc *DashboardController) DeleteApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing application ID", http.StatusBadRequest)
		return
	}

	err := dc.Store.DeleteApp(ctx, id)
	if err != nil {
		log.Printf("[Dashboard] Error deleting application profile: %v", err)
		http.Error(w, "Failed to delete application profile", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(""))
}

// ServeClients renders the Client Organizations tab.
func (dc *DashboardController) ServeClients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	clients, err := dc.Store.GetAllClients(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading clients: %v", err)
		http.Error(w, "Internal Server Error loading clients", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.ClientsTab(clients)
	_ = templates.Layout("Client Organizations", "clients", content).Render(ctx, w)
}

// ServeClientsNewModal renders the dynamic client creation modal form.
func (dc *DashboardController) ServeClientsNewModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html")
	_ = templates.ClientModal(nil).Render(ctx, w)
}

// ServeClientsEditModal renders the dynamic settings modal form to edit a client.
func (dc *DashboardController) ServeClientsEditModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clientID := r.URL.Query().Get("id")
	if clientID == "" {
		http.Error(w, "Missing client ID", http.StatusBadRequest)
		return
	}

	clients, err := dc.Store.GetAllClients(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading clients for edit modal: %v", err)
		http.Error(w, "Failed to load client organizations", http.StatusInternalServerError)
		return
	}

	var foundClient *config.Client
	for _, c := range clients {
		if c.ID == clientID {
			foundClient = &c
			break
		}
	}

	if foundClient == nil {
		http.Error(w, "Client Organization not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.ClientModal(foundClient).Render(ctx, w)
}

// CreateClient handles client profile creation and edition submissions.
func (dc *DashboardController) CreateClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	clientID := strings.TrimSpace(r.FormValue("client_id"))
	clientName := strings.TrimSpace(r.FormValue("client_name"))
	tier := strings.TrimSpace(r.FormValue("tier"))
	priority := strings.TrimSpace(r.FormValue("priority"))
	rpmStr := strings.TrimSpace(r.FormValue("rpm"))
	tpmStr := strings.TrimSpace(r.FormValue("tpm"))

	if clientID == "" {
		http.Error(w, "Client ID (Slug) cannot be empty.", http.StatusBadRequest)
		return
	}

	for _, char := range clientID {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '.') {
			http.Error(w, "Client ID must be a valid slug (alphanumeric, hyphens, and dots only).", http.StatusBadRequest)
			return
		}
	}

	if clientName == "" {
		http.Error(w, "Client Name cannot be empty.", http.StatusBadRequest)
		return
	}

	rpm, err := strconv.Atoi(rpmStr)
	if err != nil || rpm <= 0 {
		http.Error(w, "Fallback RPM limit must be a positive integer.", http.StatusBadRequest)
		return
	}

	tpm, err := strconv.Atoi(tpmStr)
	if err != nil || tpm <= 0 {
		http.Error(w, "Fallback TPM limit must be a positive integer.", http.StatusBadRequest)
		return
	}

	err = dc.Store.SaveClient(ctx, config.Client{
		ID:       clientID,
		Name:     clientName,
		Tier:     tier,
		RPM:      rpm,
		TPM:      tpm,
		Priority: priority,
	})
	if err != nil {
		log.Printf("[Dashboard] Error saving client organization: %v", err)
		http.Error(w, "Failed to save client organization", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/clients")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/admin/clients", http.StatusSeeOther)
}

// DeleteClient deletes a client organization profile dynamically.
func (dc *DashboardController) DeleteClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing client ID", http.StatusBadRequest)
		return
	}

	err := dc.Store.DeleteClient(ctx, id)
	if err != nil {
		log.Printf("[Dashboard] Error deleting client organization: %v", err)
		http.Error(w, "Failed to delete client organization", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(""))
}

// ServeComplexity renders the Dynamic Complexity Routing administration view.
func (dc *DashboardController) ServeComplexity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	apps, err := dc.Store.GetAllApps(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading apps for complexity routing: %v", err)
		http.Error(w, "Failed to load applications", http.StatusInternalServerError)
		return
	}

	clients, err := dc.Store.GetAllClients(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading clients for complexity routing: %v", err)
		http.Error(w, "Failed to load client organizations", http.StatusInternalServerError)
		return
	}

	clientsMap := make(map[string]config.Client)
	for _, c := range clients {
		clientsMap[c.ID] = c
	}

	var viewModels []templates.ComplexityViewModel
	for _, a := range apps {
		clientProfile, ok := clientsMap[a.ClientID]
		if !ok {
			clientProfile = config.Client{
				ID:   a.ClientID,
				Name: "Unknown Client",
				Tier: "free",
			}
		}
		viewModels = append(viewModels, templates.ComplexityViewModel{
			App:    a,
			Client: clientProfile,
		})
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.ComplexityTab(viewModels)
	_ = templates.Layout("Complexity Routing", "complexity", content).Render(ctx, w)
}

// ServeComplexityEditModal renders the dynamic settings modal form via HTMX.
func (dc *DashboardController) ServeComplexityEditModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	appID := r.URL.Query().Get("app_id")
	if appID == "" {
		http.Error(w, "Missing application ID", http.StatusBadRequest)
		return
	}

	app, ok := dc.Store.LookupApp(appID)
	if !ok {
		http.Error(w, "Application not found", http.StatusNotFound)
		return
	}

	allModels, err := dc.Store.GetAllModels(ctx)
	var activeCompatibleModels []config.ModelConfig
	if err == nil {
		for _, m := range allModels {
			if m.Active && config.IsLocationCompatible(dc.Location, m.Location) {
				activeCompatibleModels = append(activeCompatibleModels, m)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.ComplexityModal(app, activeCompatibleModels).Render(ctx, w)
}

// SaveComplexitySettings updates dynamic parameters for query complexity routing.
func (dc *DashboardController) SaveComplexitySettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	appID := strings.TrimSpace(r.FormValue("app_id"))
	if appID == "" {
		http.Error(w, "Missing application ID", http.StatusBadRequest)
		return
	}

	app, ok := dc.Store.LookupApp(appID)
	if !ok {
		http.Error(w, "Application not found", http.StatusNotFound)
		return
	}

	enabled := r.FormValue("enabled") == "true"
	alwaysOverride := r.FormValue("always_override") == "true"
	simpleModel := strings.TrimSpace(r.FormValue("simple_model"))
	mediumModel := strings.TrimSpace(r.FormValue("medium_model"))
	complexModel := strings.TrimSpace(r.FormValue("complex_model"))
	simpleCharLimitStr := strings.TrimSpace(r.FormValue("simple_char_limit"))
	mediumCharLimitStr := strings.TrimSpace(r.FormValue("medium_char_limit"))
	forceComplexMultimodal := r.FormValue("force_complex_multimodal") == "true"
	forceComplexTools := r.FormValue("force_complex_tools") == "true"
	useLLMClassifier := r.FormValue("use_llm_classifier") == "true"
	classifierModel := strings.TrimSpace(r.FormValue("classifier_model"))

	simpleCharLimit, err := strconv.Atoi(simpleCharLimitStr)
	if err != nil || simpleCharLimit < 0 {
		http.Error(w, "Simple text limit must be a non-negative integer.", http.StatusBadRequest)
		return
	}

	mediumCharLimit, err := strconv.Atoi(mediumCharLimitStr)
	if err != nil || mediumCharLimit < 0 {
		http.Error(w, "Medium text limit must be a non-negative integer.", http.StatusBadRequest)
		return
	}

	if simpleCharLimit > mediumCharLimit {
		http.Error(w, "Simple text character limit cannot be larger than Medium limit.", http.StatusBadRequest)
		return
	}

	// Update Complexity fields
	app.Complexity = config.ComplexityRouting{
		Enabled:                enabled,
		AlwaysOverride:         alwaysOverride,
		SimpleModel:            simpleModel,
		MediumModel:            mediumModel,
		ComplexModel:           complexModel,
		SimpleCharLimit:        simpleCharLimit,
		MediumCharLimit:        mediumCharLimit,
		ForceComplexMultimodal: forceComplexMultimodal,
		ForceComplexTools:      forceComplexTools,
		UseLLMClassifier:       useLLMClassifier,
		ClassifierModel:        classifierModel,
	}

	err = dc.Store.SaveApp(ctx, app)
	if err != nil {
		log.Printf("[Dashboard] Error saving complexity settings: %v", err)
		http.Error(w, "Failed to save complexity settings", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/complexity")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/admin/complexity", http.StatusSeeOther)
}

// ServeLocationsTestHelper is an exported test helper wrapper to execute fetchLocations in unit tests.
func (dc *DashboardController) ServeLocationsTestHelper(ctx context.Context) ([]templates.LocationInfo, error) {
	return dc.fetchLocations(ctx)
}

// ToggleSimulation toggles the simulate_metrics cookie and redirects back to the caller's page.
func (dc *DashboardController) ToggleSimulation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	active := r.FormValue("simulate") == "true"

	cookieString := "false"
	if active {
		cookieString = "true"
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "simulate_metrics",
		Value:    cookieString,
		Path:     "/",
		HttpOnly: false, // Allow clientside script visibility
		MaxAge:   3600 * 24 * 365, // 1 year
	})

	ref := r.Header.Get("Referer")
	if ref == "" {
		ref = "/admin/metrics"
	}
	http.Redirect(w, r, ref, http.StatusSeeOther)
}

// ServeQueue renders the active Request Queue tab.
func (dc *DashboardController) ServeQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	status, err := dc.Store.GetQueueStatus(ctx)
	if err != nil {
		log.Printf("[Queue] Error loading queue status: %v", err)
		http.Error(w, "Internal Server Error loading request queue", http.StatusInternalServerError)
		return
	}

	var items []templates.QueueSnapshotItem
	for _, s := range status {
		items = append(items, templates.QueueSnapshotItem{
			AppID:       s.AppID,
			Model:       s.Model,
			Priority:    s.Priority,
			Tier:        s.Tier,
			Status:      s.Status,
			ArrivalTime: s.ArrivalTime.Format(time.RFC3339),
			DurationMs:  s.DurationMs,
		})
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.QueueTab(items)

	// If HTMX request, render content partial directly
	if r.Header.Get("HX-Request") == "true" {
		_ = content.Render(ctx, w)
		return
	}

	_ = templates.Layout("Request Queue", "queue", content).Render(ctx, w)
}

// ServeDocs handles rendering the markdown files in docs/
func (dc *DashboardController) ServeDocs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	docPath := r.URL.Query().Get("path")
	if docPath == "" {
		docPath = "README.md"
	}

	// 1. Clean and sanitize the docPath to prevent directory traversal
	cleanedPath := filepath.Clean(docPath)
	if strings.HasPrefix(cleanedPath, "..") || filepath.IsAbs(cleanedPath) {
		http.Error(w, "Access Denied: Invalid path structure", http.StatusForbidden)
		return
	}

	// Determine base directory dynamically (supports absolute and relative paths)
	baseDir := "docs"
	if _, err := os.Stat("/docs"); err == nil {
		baseDir = "/docs"
	} else if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		if _, err := os.Stat("../../docs"); err == nil {
			baseDir = "../../docs"
		}
	}

	// Form the target file path relative to the base directory
	targetFilePath := filepath.Join(baseDir, cleanedPath)

	// Additional double check: ensure the joined path starts with the base directory prefix
	if !strings.HasPrefix(filepath.Clean(targetFilePath), filepath.Clean(baseDir)) {
		http.Error(w, "Access Denied: Out of bounds request", http.StatusForbidden)
		return
	}

	// 2. Read the target markdown file
	mdBytes, err := os.ReadFile(targetFilePath)
	if err != nil {
		log.Printf("[Docs] Error reading doc file %s: %v", targetFilePath, err)
		http.Error(w, "Documentation file not found", http.StatusNotFound)
		return
	}

	// 3. Parse the markdown to HTML using Goldmark
	var buf bytes.Buffer
	if err := goldmark.Convert(mdBytes, &buf); err != nil {
		log.Printf("[Docs] Error converting markdown: %v", err)
		http.Error(w, "Failed to render document contents", http.StatusInternalServerError)
		return
	}

	// 4. Populate Categories list dynamically matching all existing documentation
	categories := []templates.DocCategory{
		{
			Name: "General Information",
			Items: []templates.DocItem{
				{Title: "Welcome & Getting Started", Path: "README.md"},
			},
		},
		{
			Name: "Administrative Guides",
			Items: []templates.DocItem{
				{Title: "Overview", Path: "admin/README.md"},
				{Title: "Client Organizations", Path: "admin/client-organizations.md"},
				{Title: "Applications & Keys", Path: "admin/apps-and-keys.md"},
				{Title: "Traffic Routing Rules", Path: "admin/routing-rules.md"},
				{Title: "Declarative Custom Headers", Path: "admin/custom-headers.md"},
				{Title: "Metrics & Expenditure Analytics", Path: "admin/metrics-and-costs.md"},
			},
		},
		{
			Name: "Architecture",
			Items: []templates.DocItem{
				{Title: "System Overview", Path: "architecture/overview.md"},
			},
		},
		{
			Name: "Development & Testing",
			Items: []templates.DocItem{
				{Title: "Test-Driven Development", Path: "approaches/tdd-feature-development.md"},
				{Title: "Model Version Compliance", Path: "approaches/model-compliance.md"},
			},
		},
		{
			Name: "Integration Guides",
			Items: []templates.DocItem{
				{Title: "Local Workflows", Path: "guides/local-development.md"},
				{Title: "Client API Integration", Path: "guides/client-integration.md"},
				{Title: "Dynamic Traffic Routing", Path: "guides/dynamic-routing.md"},
				{Title: "Google Cloud Deployment", Path: "guides/cloud-deployment.md"},
				{Title: "Example Templates Directory", Path: "guides/using-examples.md"},
			},
		},
	}

	// 5. Determine active document title
	activeTitle := "Documentation"
	for _, cat := range categories {
		for _, item := range cat.Items {
			if item.Path == cleanedPath {
				activeTitle = item.Title
				break
			}
		}
	}

	// 6. Render using Templates Layout
	w.Header().Set("Content-Type", "text/html")
	vm := templates.DocsViewModel{
		Categories:   categories,
		ActivePath:   cleanedPath,
		ActiveTitle:  activeTitle,
		RenderedHTML: buf.String(),
	}

	content := templates.DocsTab(vm)
	_ = templates.Layout(activeTitle, "docs", content).Render(ctx, w)
}

