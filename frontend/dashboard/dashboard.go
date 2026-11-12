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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"geminirouter/frontend/dashboard/templates"
	"geminirouter/pkg/config"
	"geminirouter/pkg/decision"

	"github.com/yuin/goldmark"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ModelRefreshStatus aliases templates.ModelRefreshStatus for dashboard callers and tests.
type ModelRefreshStatus = templates.ModelRefreshStatus

// DashboardController handles the administration dashboard HTTP routes.
type DashboardController struct {
	Store           config.AdminStore
	Firebase        templates.FirebaseConfig
	ProjectID       string
	Location        string
	TokenSource     oauth2.TokenSource
	HTTPClient      *http.Client
	SessionVerifier func(r *http.Request) bool

	refreshMu     sync.RWMutex
	refreshStatus ModelRefreshStatus
}

// NewDashboardController initializes a new dashboard controller.
func NewDashboardController(store config.AdminStore, projectID, location string) *DashboardController {
	ts, err := google.DefaultTokenSource(context.Background(), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		log.Printf("[Warning] Failed to initialize live Google Cloud DefaultTokenSource: %v", err)
	}
	// Pull Firebase client configs from environment variables with fallback and project-derived defaults
	apiKey := strings.TrimSpace(os.Getenv("FIREBASE_API_KEY"))
	if apiKey == "" || apiKey == "AIzaSyYourFirebaseWebApiKey" {
		apiKey = strings.TrimSpace(os.Getenv("FIREBASE_WEB_API_KEY"))
	}
	fbProjectID := strings.TrimSpace(os.Getenv("FIREBASE_PROJECT_ID"))
	if fbProjectID == "" || fbProjectID == "your-gcp-project-id" {
		fbProjectID = strings.TrimSpace(projectID)
	}
	authDomain := strings.TrimSpace(os.Getenv("FIREBASE_AUTH_DOMAIN"))
	if (authDomain == "" || authDomain == "your-project-id.firebaseapp.com") && fbProjectID != "" {
		authDomain = fbProjectID + ".firebaseapp.com"
	}
	storageBucket := strings.TrimSpace(os.Getenv("FIREBASE_STORAGE_BUCKET"))
	if (storageBucket == "" || storageBucket == "your-project-id.appspot.com") && fbProjectID != "" {
		storageBucket = fbProjectID + ".firebasestorage.app"
	}

	return &DashboardController{
		Store:       store,
		ProjectID:   projectID,
		Location:    location,
		TokenSource: ts,
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		Firebase: templates.FirebaseConfig{
			APIKey:            apiKey,
			AuthDomain:        authDomain,
			ProjectID:         fbProjectID,
			StorageBucket:     storageBucket,
			MessagingSenderID: strings.TrimSpace(os.Getenv("FIREBASE_MESSAGING_SENDER_ID")),
			AppID:             strings.TrimSpace(os.Getenv("FIREBASE_APP_ID")),
		},
	}
}

func (dc *DashboardController) getHTTPClient() *http.Client {
	if dc.HTTPClient == nil {
		return &http.Client{Timeout: 15 * time.Second}
	}
	return dc.HTTPClient
}

func (dc *DashboardController) renderFormError(w http.ResponseWriter, r *http.Request, msg string, code int) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Retarget", "#error-alert-container")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_ = templates.FormErrorAlert(msg).Render(r.Context(), w)
		return
	}
	http.Error(w, msg, code)
}

// ServeLogin renders the Firebase-enabled login view.
func (dc *DashboardController) ServeLogin(w http.ResponseWriter, r *http.Request) {
	// If session cookie already exists and is valid, bypass login
	if cookie, err := r.Cookie("session"); err == nil && cookie.Value != "" {
		if dc.SessionVerifier != nil && dc.SessionVerifier(r) {
			http.Redirect(w, r, "/admin/apps", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   os.Getenv("K_SERVICE") != "" || os.Getenv("LOCAL_DEV") != "true",
			SameSite: http.SameSiteLaxMode,
		})
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

	appID := strings.TrimSpace(r.FormValue("app_id"))
	if appID == "" {
		dc.renderFormError(w, r, "Missing target app selection", http.StatusBadRequest)
		return
	}

	app, ok := dc.Store.LookupApp(appID)
	if !ok {
		dc.renderFormError(w, r, "Application profile not found", http.StatusNotFound)
		return
	}

	// 1. Generate cryptographically secure API Key
	rawKey, err := generateSecureKey()
	if err != nil {
		log.Printf("[Dashboard] Error generating key: %v", err)
		dc.renderFormError(w, r, "Failed to generate key", http.StatusInternalServerError)
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
		dc.renderFormError(w, r, "Failed to save API key profile", http.StatusInternalServerError)
		return
	}

	// Render raw API Key inside prominent success warning banner
	w.Header().Set("Content-Type", "text/html")
	_ = templates.KeyCreatedAlert(rawKey, app.Name).Render(ctx, w)
}

// RevokeKey marks an API key as inactive dynamically and triggers an HTMX refresh.
func (dc *DashboardController) RevokeKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
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

	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
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
	hash := strings.TrimSpace(r.FormValue("hash"))
	appID := strings.TrimSpace(r.FormValue("app_id"))
	status := strings.TrimSpace(r.FormValue("status"))

	if hash == "" || appID == "" || status == "" {
		dc.renderFormError(w, r, "Missing required key fields", http.StatusBadRequest)
		return
	}

	keys, err := dc.Store.GetAllKeys(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading keys: %v", err)
		dc.renderFormError(w, r, "Failed to load keys", http.StatusInternalServerError)
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
		dc.renderFormError(w, r, "API Key not found", http.StatusNotFound)
		return
	}

	app, ok := dc.Store.LookupApp(appID)
	if !ok {
		dc.renderFormError(w, r, "Target Application not found", http.StatusNotFound)
		return
	}

	// Update fields
	targetKey.AppID = app.ID
	targetKey.ClientID = app.ClientID
	targetKey.Status = status

	err = dc.Store.SaveKey(ctx, *targetKey)
	if err != nil {
		log.Printf("[Dashboard] Error saving api_key: %v", err)
		dc.renderFormError(w, r, "Failed to save API Key profile", http.StatusInternalServerError)
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

// SaveRule is an alias for CreateRule to support /admin/rules/save form submissions.
func (dc *DashboardController) SaveRule(w http.ResponseWriter, r *http.Request) {
	dc.CreateRule(w, r)
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
	complexityTier := strings.TrimSpace(r.FormValue("complexity_tier"))
	decisionEngine := strings.TrimSpace(r.FormValue("decision_engine"))
	minConfidenceStr := strings.TrimSpace(r.FormValue("min_confidence"))
	taskKind := strings.TrimSpace(r.FormValue("task_kind"))
	targetModel := strings.TrimSpace(r.FormValue("target_model"))
	targetLocation := strings.TrimSpace(r.FormValue("target_location"))
	fallbackModel := strings.TrimSpace(r.FormValue("fallback_model"))
	priorityWeightStr := strings.TrimSpace(r.FormValue("priority_weight"))

	showError := func(msg string, code int) {
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Retarget", "#error-alert-container")
			w.Header().Set("HX-Reswap", "innerHTML")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_ = templates.FormErrorAlert(msg).Render(ctx, w)
			return
		}
		http.Error(w, msg, code)
	}

	if modelPattern == "" {
		showError("Requested Model Pattern cannot be empty.", http.StatusBadRequest)
		return
	}
	if targetModel == "" {
		showError("Target Routed Model cannot be empty.", http.StatusBadRequest)
		return
	}
	if targetLocation == "" {
		showError("Target Location cannot be empty.", http.StatusBadRequest)
		return
	}

	// Validate regex pattern in HeaderValue if applicable
	if headerName != "" && headerValue != "" {
		if strings.HasPrefix(headerValue, "/") && strings.HasSuffix(headerValue, "/") {
			pattern := headerValue[1 : len(headerValue)-1]
			_, err := regexp.Compile(pattern)
			if err != nil {
				showError(fmt.Sprintf("Invalid Header Value regex pattern: %v", err), http.StatusBadRequest)
				return
			}
		}
	}

	priorityWeight := 1
	if priorityWeightStr != "" {
		pw, err := strconv.Atoi(priorityWeightStr)
		if err != nil || pw <= 0 {
			showError("Priority Weight must be a positive integer.", http.StatusBadRequest)
			return
		}
		priorityWeight = pw
	}

	var minConfidence float64
	if minConfidenceStr != "" {
		mc, err := strconv.ParseFloat(minConfidenceStr, 64)
		if err != nil || mc < 0 || mc > 1 {
			showError("Minimum Confidence must be between 0.0 and 1.0.", http.StatusBadRequest)
			return
		}
		minConfidence = mc
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
		ComplexityTier: complexityTier,
		DecisionEngine: decisionEngine,
		MinConfidence:  minConfidence,
		TaskKind:       taskKind,
		TargetModel:    targetModel,
		TargetLocation: targetLocation,
		FallbackModel:  fallbackModel,
		PriorityWeight: priorityWeight,
	})
	if err != nil {
		log.Printf("[Dashboard] Error saving routing rule: %v", err)
		showError("Failed to save routing rule config to database", http.StatusInternalServerError)
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
		dc.renderFormError(w, r, "Header Name cannot be empty.", http.StatusBadRequest)
		return
	}
	if appID == "" {
		dc.renderFormError(w, r, "Application boundary (App ID) must be specified (or 'global').", http.StatusBadRequest)
		return
	}
	if validation == "regex" {
		if valuePattern == "" {
			dc.renderFormError(w, r, "Regex validation requires a non-empty pattern value.", http.StatusBadRequest)
			return
		}
		_, err := regexp.Compile(valuePattern)
		if err != nil {
			dc.renderFormError(w, r, fmt.Sprintf("Invalid regular expression pattern: %v", err), http.StatusBadRequest)
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
		dc.renderFormError(w, r, "Failed to save custom header rule", http.StatusInternalServerError)
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

// aggregateModelStats compiles real-time telemetry metrics per active model from logs.
func (dc *DashboardController) aggregateModelStats(ctx context.Context) map[string]templates.ModelStats {
	stats := make(map[string]templates.ModelStats)

	costsVM, err := dc.fetchCostAnalyticsData(ctx)
	var sessions []templates.CostTransaction
	if err == nil && len(costsVM.RecentSessions) > 0 {
		sessions = costsVM.RecentSessions
	}

	latencySums := make(map[string]int64)
	latencyCounts := make(map[string]int64)
	loadedFromLocalLogs := false
	if os.Getenv("LOCAL_DEV") == "true" {
		if file, err := os.Open(localLogsPath()); err == nil {
			loadedFromLocalLogs = true
			defer file.Close()
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := scanner.Bytes()
				if len(line) == 0 {
					continue
				}
				var entry struct {
					ModelRouted string `json:"model_routed"`
					LatencyMs   int64  `json:"latency_ms"`
				}
				if err := json.Unmarshal(line, &entry); err == nil && entry.ModelRouted != "" {
					latencySums[entry.ModelRouted] += entry.LatencyMs
					latencyCounts[entry.ModelRouted]++
				}
			}
		}
	}

	if !loadedFromLocalLogs {
		for _, s := range sessions {
			if s.ModelRouted != "" && s.LatencyMs > 0 {
				latencySums[s.ModelRouted] += s.LatencyMs
				latencyCounts[s.ModelRouted]++
			}
		}
	}

	for _, s := range sessions {
		mStats := stats[s.ModelRouted]
		mStats.RequestCount++
		mStats.TotalCost += s.EstimatedCost
		if cnt := latencyCounts[s.ModelRouted]; cnt > 0 {
			mStats.AvgLatencyMs = latencySums[s.ModelRouted] / cnt
		}
		stats[s.ModelRouted] = mStats
	}

	return stats
}

// GetRefreshStatus returns a thread-safe snapshot of the background model discovery state.
func (dc *DashboardController) GetRefreshStatus() ModelRefreshStatus {
	dc.refreshMu.RLock()
	defer dc.refreshMu.RUnlock()
	return dc.refreshStatus
}

func (dc *DashboardController) updateRefreshStatus(fn func(*ModelRefreshStatus)) {
	dc.refreshMu.Lock()
	defer dc.refreshMu.Unlock()
	fn(&dc.refreshStatus)
	dc.refreshStatus.UpdatedAt = time.Now().UTC()
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

	// Fetch dynamic logs stats per model
	modelStats := dc.aggregateModelStats(ctx)

	var generativeModels []templates.ModelInfo
	var embeddingModels []templates.ModelInfo

	for _, m := range allModels {
		// Only show models compatible with this router instance's serving location
		if !config.IsLocationCompatible(dc.Location, m.Location) {
			continue
		}

		baseModelID := config.StripLocationSuffix(m.ID)

		// Resolve dynamic stats (try exact compound ID first, then stripped base model ID)
		stats, exists := modelStats[m.ID]
		if !exists {
			stats, exists = modelStats[baseModelID]
		}
		if !exists {
			// Seed a healthy minimal default baseline if no requests recorded yet
			stats = templates.ModelStats{
				RequestCount: 0,
				AvgLatencyMs: 0,
				TotalCost:    0.0,
			}
		}

		displayName := strings.TrimSpace(m.DisplayName)
		if displayName == "" {
			displayName = baseModelID
		}

		pricingID := m.ID
		if (m.Location == "local" || m.Type == "local") && !strings.Contains(pricingID, "@") {
			pricingID = pricingID + "@local"
		}
		inCostPer1M, outCostPer1M := GetModelPricing(pricingID)

		info := templates.ModelInfo{
			ID:              m.ID,
			DisplayName:     displayName,
			Location:        m.Location,
			Type:            m.Type,
			Active:          m.Active,
			Obsolete:        strings.Contains(displayName, "(Obsolete)"),
			InputCostPer1M:  inCostPer1M,
			OutputCostPer1M: outCostPer1M,
			Stats:           stats,
		}

		if strings.Contains(strings.ToLower(baseModelID), "embedding") {
			embeddingModels = append(embeddingModels, info)
		} else {
			generativeModels = append(generativeModels, info)
		}
	}

	refreshStatus := dc.GetRefreshStatus()
	if r.URL.Query().Get("refresh") == "started" && refreshStatus.Phase == "" {
		refreshStatus.Running = true
		refreshStatus.Phase = "discovering"
		refreshStatus.Percent = 5
		refreshStatus.Message = "Starting background model discovery across local clusters and Vertex AI regions..."
	}

	vm := templates.ModelsViewModel{
		ProjectID:        dc.ProjectID,
		Location:         dc.Location,
		ParentLocation:   config.GetMultiRegionParent(dc.Location),
		GenerativeModels: generativeModels,
		EmbeddingModels:  embeddingModels,
		RefreshStatus:    refreshStatus,
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

// DiscoverAndCacheModels queries local clusters and Vertex AI APIs and caches all discovered models and endpoints.
func (dc *DashboardController) DiscoverAndCacheModels(ctx context.Context) error {
	now := time.Now().UTC()
	dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
		st.Running = true
		st.Phase = "discovering"
		st.Percent = 5
		st.CompletedSteps = 0
		st.TotalSteps = 4
		st.VerifiedCount = 0
		st.Error = ""
		if st.StartedAt.IsZero() {
			st.StartedAt = now
		}
		st.Message = "Stage 1/4: Discovering compatible Vertex AI regions and local clusters..."
	})

	// Fetch currently configured models to avoid resetting their active statuses
	currentModels, err := dc.Store.GetAllModels(ctx)
	if err != nil {
		dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
			st.Running = false
			st.Phase = "failed"
			st.Error = err.Error()
			st.Message = fmt.Sprintf("Failed to load existing models: %v", err)
		})
		return fmt.Errorf("GetAllModels failed: %w", err)
	}
	activeStatus := make(map[string]bool)
	for _, m := range currentModels {
		activeStatus[m.ID] = m.Active
		activeStatus[config.StripLocationSuffix(m.ID)] = m.Active
	}

	discoveredModelsMap := make(map[string]config.ModelConfig)

	// Discover local cluster & direct endpoint models
	if providers, provErr := dc.Store.GetAllProviders(ctx); provErr == nil {
		for _, p := range providers {
			if p.ID != config.ProviderLocal {
				continue
			}
			for _, reg := range p.Regions {
				if reg.LocalConfig == nil {
					continue
				}
				for _, ep := range reg.LocalConfig.DirectEndpoints {
					mName := strings.TrimSpace(ep.ModelName)
					if mName == "" || ep.Role == "decision_engine" || !config.IsValidModelName(mName) {
						continue
					}
					active := true
					if exists, present := activeStatus[mName+"@local"]; present {
						active = exists
					}
					discoveredModelsMap[mName+"@local"] = config.ModelConfig{
						ID:          mName,
						DisplayName: mName,
						Location:    "local",
						Type:        "local",
						Active:      active,
					}
				}
			}
		}
	}
	if runners, runErr := dc.Store.GetRegisteredRunners(ctx); runErr == nil {
		for _, r := range runners {
			for _, mName := range r.Node.SupportedModels {
				mName = strings.TrimSpace(mName)
				if mName == "" || !config.IsValidModelName(mName) {
					continue
				}
				active := true
				if exists, present := activeStatus[mName+"@local"]; present {
					active = exists
				}
				discoveredModelsMap[mName+"@local"] = config.ModelConfig{
					ID:          mName,
					DisplayName: mName,
					Location:    "local",
					Type:        "local",
					Active:      active,
				}
			}
		}
	}

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

	dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
		st.Phase = "discovering"
		st.Percent = 15
		st.CompletedSteps = 1
		st.TotalSteps = 1 + len(locationsToFetch)
		st.Message = fmt.Sprintf("Stage 2/4: Querying Vertex AI model catalogs across %d compatible location(s)...", len(locationsToFetch))
	})

	for idx, loc := range locationsToFetch {
		log.Printf("[Discovery] Refreshing discovered models from Vertex AI in location: %q...", loc)
		dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
			st.Message = fmt.Sprintf("Stage 2/4: Fetching publisher & custom models in %s (%d/%d)...", loc, idx+1, len(locationsToFetch))
		})

		// 1. Fetch custom models from API (only if not global)
		if loc != "global" {
			custom, err := dc.fetchCustomModels(ctx, loc)
			if err == nil {
				for _, m := range custom {
					if !config.IsValidModelName(m.Name) {
						continue
					}
					active := false
					if exists, present := activeStatus[m.Name]; present {
						active = exists
					}

					modelLoc := loc
					if extLoc := config.ExtractLocationFromResourceName(m.Name); extLoc != "" {
						modelLoc = config.GetSmallestCompatibleLocation(loc, extLoc)
					}

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

					disp := strings.TrimSpace(m.DisplayName)
					if disp == "" {
						disp = config.StripLocationSuffix(m.Name)
					}
					discoveredModelsMap[m.Name] = config.ModelConfig{
						ID:          m.Name,
						DisplayName: disp,
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
					if len(ep.DeployedModels) > 0 && ep.DeployedModels[0].DisplayName != "" {
						displayName = ep.DeployedModels[0].DisplayName
					}
					if strings.TrimSpace(displayName) == "" {
						displayName = config.StripLocationSuffix(ep.Name)
					}

					active := false
					if exists, present := activeStatus[ep.Name]; present {
						active = exists
					}

					modelLoc := loc
					if extLoc := config.ExtractLocationFromResourceName(ep.Name); extLoc != "" {
						modelLoc = config.GetSmallestCompatibleLocation(loc, extLoc)
					}

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

		// 3. Fetch Google foundation/publisher models from API
		pubModels, err := dc.fetchPublisherModels(ctx, loc)
		if err == nil {
			for _, pm := range pubModels {
				if !config.IsValidModelName(pm.Name) {
					continue
				}

				active := false
				if exists, present := activeStatus[pm.Name]; present {
					active = exists
				}

				modelLoc := loc
				if extLoc := config.ExtractLocationFromResourceName(pm.Name); extLoc != "" {
					modelLoc = config.GetSmallestCompatibleLocation(loc, extLoc)
				}

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

				disp := strings.TrimSpace(pm.DisplayName)
				if disp == "" {
					disp = config.StripLocationSuffix(pm.Name)
				}

				discoveredModelsMap[pm.Name] = config.ModelConfig{
					ID:          pm.Name,
					DisplayName: disp,
					Location:    modelLoc,
					Type:        "foundation",
					Active:      active,
				}
			}
		} else {
			log.Printf("[Discovery] fetchPublisherModels failed for loc %q: %v", loc, err)
		}
	}

	// Build candidate verification list
	var candidateOptions []config.ModelConfig
	for key, modelToSave := range discoveredModelsMap {
		if strings.HasSuffix(key, "@local") || modelToSave.Location == "local" || modelToSave.Type == "local" {
			baseID := config.StripLocationSuffix(modelToSave.ID)
			modelOption := modelToSave
			modelOption.Location = "local"
			modelOption.Type = "local"
			modelOption.ID = baseID + "@local"
			if modelOption.DisplayName == "" {
				modelOption.DisplayName = baseID
			}
			candidateOptions = append(candidateOptions, modelOption)
			continue
		}

		var candidates []string
		if strings.HasPrefix(modelToSave.ID, "projects/") {
			if extLoc := config.ExtractLocationFromResourceName(modelToSave.ID); extLoc != "" {
				candidates = []string{extLoc}
			}
		}
		if len(candidates) == 0 {
			routerLoc := dc.Location
			if routerLoc == "" {
				routerLoc = "us-central1"
			}
			candidates = append(candidates, routerLoc)
			if parent := config.GetMultiRegionParent(routerLoc); parent != "" && parent != routerLoc {
				candidates = append(candidates, parent)
			}
			if routerLoc != "global" {
				candidates = append(candidates, "global")
			}
		}

		for _, candidateLoc := range candidates {
			modelOption := modelToSave
			modelOption.Location = candidateLoc
			modelOption.ID = modelToSave.ID + "@" + candidateLoc
			if strings.TrimSpace(modelOption.DisplayName) == "" {
				modelOption.DisplayName = config.StripLocationSuffix(modelToSave.ID)
			}

			active := false
			if exists, present := activeStatus[modelOption.ID]; present {
				active = exists
			} else if exists, present := activeStatus[modelToSave.ID]; present {
				active = exists
			}
			modelOption.Active = active
			candidateOptions = append(candidateOptions, modelOption)
		}
	}

	totalSteps := len(candidateOptions)
	if totalSteps == 0 {
		totalSteps = 1
	}
	dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
		st.Phase = "verifying"
		st.CompletedSteps = 0
		st.TotalSteps = totalSteps
		st.Percent = 20
		st.Message = fmt.Sprintf("Stage 3/4: Probing %d candidate model location endpoint(s)...", len(candidateOptions))
	})

	verifiedIDs := make(map[string]bool)
	var verifyMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // 4-worker bounded concurrency pool
	completedCount := 0
	verifiedCount := 0

	for _, opt := range candidateOptions {
		wg.Add(1)
		go func(modelOption config.ModelConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var err error
			if modelOption.Location != "local" && modelOption.Type != "local" {
				err = dc.verifyModel(ctx, modelOption)
			}

			verifyMu.Lock()
			completedCount++
			if err == nil {
				verifiedCount++
				verifiedIDs[modelOption.ID] = true
				_ = dc.Store.SaveModel(ctx, modelOption)
				log.Printf("[Discovery] Successfully verified model %s at location %s.", config.StripLocationSuffix(modelOption.ID), modelOption.Location)
			} else {
				log.Printf("[Discovery] Verification failed for model %s at candidate location %s: %v. Skipping registration.", config.StripLocationSuffix(modelOption.ID), modelOption.Location, err)
			}
			curCompleted := completedCount
			curVerified := verifiedCount
			verifyMu.Unlock()

			pct := 20 + int((float64(curCompleted)/float64(totalSteps))*70.0)
			if pct > 90 {
				pct = 90
			}
			dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
				st.Phase = "verifying"
				st.CompletedSteps = curCompleted
				st.TotalSteps = totalSteps
				st.VerifiedCount = curVerified
				st.Percent = pct
				st.Message = fmt.Sprintf("Stage 3/4: Verified %s (%d/%d endpoints checked, %d active)...", modelOption.ID, curCompleted, totalSteps, curVerified)
			})
		}(opt)
	}
	wg.Wait()

	dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
		st.Phase = "cleanup"
		st.Percent = 95
		st.Message = "Stage 4/4: Scrubbing unverified regional duplicates and obsolete keys..."
	})

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

		baseID := config.StripLocationSuffix(m.ID)

		if _, discovered := discoveredModelsMap[baseID]; discovered {
			if !verifiedIDs[m.ID] {
				log.Printf("[Discovery] Deleting unverified location option %s from registry...", m.ID)
				_ = dc.Store.DeleteModel(ctx, m.ID)
			}
		} else if _, discoveredLocal := discoveredModelsMap[m.ID]; discoveredLocal {
			if !verifiedIDs[m.ID] {
				_ = dc.Store.DeleteModel(ctx, m.ID)
			}
		} else {
			log.Printf("[Discovery] Soft-disabling obsolete model %s...", m.ID)
			m.Active = false
			if strings.TrimSpace(m.DisplayName) == "" {
				m.DisplayName = baseID
			}
			if !strings.Contains(m.DisplayName, " (Obsolete)") {
				m.DisplayName = m.DisplayName + " (Obsolete)"
			}
			_ = dc.Store.SaveModel(ctx, m)
		}
	}

	dc.updateRefreshStatus(func(st *ModelRefreshStatus) {
		st.Running = false
		st.Phase = "completed"
		st.CompletedSteps = totalSteps
		st.TotalSteps = totalSteps
		st.VerifiedCount = verifiedCount
		st.Percent = 100
		st.Message = fmt.Sprintf("Model discovery complete: %d compatible model endpoint(s) verified across %d probed options.", verifiedCount, len(candidateOptions))
	})

	return nil
}

// RefreshModels starts an asynchronous background job to query Vertex AI and local clusters for models.
func (dc *DashboardController) RefreshModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dc.refreshMu.Lock()
	alreadyRunning := dc.refreshStatus.Running
	if !alreadyRunning {
		now := time.Now().UTC()
		dc.refreshStatus = ModelRefreshStatus{
			Running:        true,
			Phase:          "discovering",
			Message:        "Starting background model discovery across local clusters and Vertex AI regions...",
			CompletedSteps: 0,
			TotalSteps:     4,
			Percent:        5,
			VerifiedCount:  0,
			StartedAt:      now,
			UpdatedAt:      now,
		}
	}
	statusSnapshot := dc.refreshStatus
	dc.refreshMu.Unlock()

	if !alreadyRunning {
		go func() {
			if err := dc.DiscoverAndCacheModels(context.Background()); err != nil {
				log.Printf("[Dashboard] Background DiscoverAndCacheModels failed: %v", err)
			}
		}()
	}

	if strings.Contains(r.Header.Get("Accept"), "application/json") || r.Header.Get("X-Requested-With") == "fetch" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(statusSnapshot)
		return
	}

	// Redirect back to the models administration screen with status banner active
	http.Redirect(w, r, "/admin/models?refresh=started", http.StatusSeeOther)
}

// RefreshModelsStatus returns the current JSON progress status for background model discovery.
func (dc *DashboardController) RefreshModelsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(dc.GetRefreshStatus())
}

// WaitForRefresh waits until an active background model refresh completes or the timeout expires.
func (dc *DashboardController) WaitForRefresh(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !dc.GetRefreshStatus().Running {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return !dc.GetRefreshStatus().Running
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
		displayName := strings.TrimSpace(m.DisplayName)
		if displayName == "" {
			displayName = modelName
		}

		list = append(list, templates.CustomModelInfo{
			Name:            modelName,
			DisplayName:     displayName,
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

// GetModelPricing returns the input and output cost per 1,000,000 tokens for a given model ID.
// It uses a deterministic ordered tier table so more specific substrings (e.g. flash-lite) match first.
func GetModelPricing(modelID string) (inPer1M float64, outPer1M float64) {
	lower := strings.ToLower(strings.TrimSpace(modelID))

	// Local models or @local suffixes incur $0.00 cloud API spend
	if strings.HasSuffix(lower, "@local") {
		return 0.00, 0.00
	}

	base := config.StripLocationSuffix(lower)

	if strings.Contains(base, "embedding") {
		return 0.025, 0.00
	}

	if strings.HasPrefix(base, "gemma-4") || strings.Contains(base, "gemma-4") {
		return 0.15, 0.60
	}

	type pricingRule struct {
		match  string
		in1M   float64
		out1M  float64
	}

	orderedRules := []pricingRule{
		{match: "gemini-decision-1", in1M: 0.10, out1M: 0.00},
		{match: "gemini-decision", in1M: 0.10, out1M: 0.00},
		{match: "gemini-4-argon", in1M: 2.00, out1M: 10.00},
		{match: "gemini-4-pro", in1M: 2.00, out1M: 10.00},
		{match: "gemini-4", in1M: 2.00, out1M: 10.00},
		{match: "gemini-3.8-flash", in1M: 0.15, out1M: 0.60},
		{match: "gemini-3.5-flash-lite", in1M: 0.05, out1M: 0.20},
		{match: "gemini-3.1-flash-lite", in1M: 0.05, out1M: 0.20},
		{match: "gemini-3.7-flash", in1M: 0.10, out1M: 0.40},
		{match: "gemini-3.6-flash", in1M: 0.10, out1M: 0.40},
		{match: "gemini-3.5-flash", in1M: 0.10, out1M: 0.40},
		{match: "gemini-3-flash", in1M: 0.10, out1M: 0.40},
		{match: "gemini-3.1-pro", in1M: 1.50, out1M: 6.00},
		{match: "gemini-3-pro", in1M: 1.50, out1M: 6.00},
		{match: "gemini-2.5-flash-lite", in1M: 0.0375, out1M: 0.15},
		{match: "gemini-2.5-pro", in1M: 1.25, out1M: 5.00},
		{match: "gemini-2.5-flash", in1M: 0.075, out1M: 0.30},
	}

	for _, rule := range orderedRules {
		if strings.Contains(base, rule.match) {
			return rule.in1M, rule.out1M
		}
	}

	// Default fallback to gemini-2.5-flash pricing
	return 0.075, 0.30
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
	if strings.Contains(strings.ToLower(model), "gemini-decision") {
		outTokens = 0
	}

	inPer1M, outPer1M := GetModelPricing(model)
	inPrice := inPer1M / 1e6
	outPrice := outPer1M / 1e6

	cost := (float64(inTokens) * inPrice) + (float64(outTokens) * outPrice)
	return inTokens, outTokens, cost
}

func localLogsPath() string {
	if dataDir := os.Getenv("LOCAL_DATA_DIR"); dataDir != "" {
		p := filepath.Join(dataDir, "local_logs.jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "data/local_logs.jsonl"
}

func (dc *DashboardController) resolveBackendServiceName() string {
	if svc := strings.TrimSpace(os.Getenv("BACKEND_SERVICE_NAME")); svc != "" {
		return svc
	}
	if svc := strings.TrimSpace(os.Getenv("K_SERVICE")); svc != "" {
		svc = strings.TrimSuffix(svc, "-ui")
		if svc != "" {
			return svc
		}
	}
	return "gemini-smart-router"
}

type logMetricEntry struct {
	Time      string `json:"time"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
}

func computeMetricsFromEntries(entries []logMetricEntry, now time.Time) templates.MetricsViewModel {
	counts := make([]int, 24)
	p50 := make([]int, 24)
	p95 := make([]int, 24)
	labels := make([]string, 24)

	for i := 0; i < 24; i++ {
		t := now.Add(-time.Duration(23-i) * time.Hour)
		labels[i] = t.Format("15:00")
	}

	hourlyLatencies := make([][]int64, 24)
	var allLatencies []int64
	totalReqs := 0
	peakRate := 0
	errorCount := 0

	for _, entry := range entries {
		t, err := time.Parse(time.RFC3339, entry.Time)
		if err != nil {
			t, err = time.Parse(time.RFC3339Nano, entry.Time)
			if err != nil {
				continue
			}
		}

		diffHours := now.Sub(t).Hours()
		if diffHours < 0 || diffHours >= 24 {
			continue
		}
		hrIndex := 23 - int(diffHours)
		if hrIndex >= 0 && hrIndex < 24 {
			counts[hrIndex]++
			totalReqs++
			if entry.Status >= 400 {
				errorCount++
			}
			hourlyLatencies[hrIndex] = append(hourlyLatencies[hrIndex], entry.LatencyMs)
			allLatencies = append(allLatencies, entry.LatencyMs)
		}
	}

	for _, c := range counts {
		if c > peakRate {
			peakRate = c
		}
	}

	for i := 0; i < 24; i++ {
		lats := hourlyLatencies[i]
		n := len(lats)
		if n == 0 {
			p50[i] = 0
			p95[i] = 0
			continue
		}

		sort.Slice(lats, func(x, y int) bool {
			return lats[x] < lats[y]
		})

		p50Idx := (n - 1) / 2
		p95Idx := int(math.Ceil(float64(n)*0.95)) - 1
		if p95Idx < 0 {
			p95Idx = 0
		}
		if p95Idx >= n {
			p95Idx = n - 1
		}

		p50[i] = int(lats[p50Idx])
		p95[i] = int(lats[p95Idx])
	}

	overallP95 := 0
	if n := len(allLatencies); n > 0 {
		sort.Slice(allLatencies, func(x, y int) bool {
			return allLatencies[x] < allLatencies[y]
		})
		p95Idx := int(math.Ceil(float64(n)*0.95)) - 1
		if p95Idx < 0 {
			p95Idx = 0
		}
		if p95Idx >= n {
			p95Idx = n - 1
		}
		overallP95 = int(allLatencies[p95Idx])
	}

	errRate := 0.0
	if totalReqs > 0 {
		errRate = (float64(errorCount) / float64(totalReqs)) * 100.0
	}

	return templates.MetricsViewModel{
		TotalRequests:   totalReqs,
		PeakRate:        peakRate,
		P95LatencyMs:    overallP95,
		ErrorRate:       errRate,
		VolumeChartSVG:  generateVolumeSVG(counts, labels),
		LatencyChartSVG: generateLatencySVG(p50, p95, labels),
	}
}

func (dc *DashboardController) fetchLocalLogsCosts() (templates.CostsViewModel, error) {
	file, err := os.Open(localLogsPath())
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
			LatencyMs:     entry.LatencyMs,
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
	file, err := os.Open(localLogsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return templates.MetricsViewModel{}, nil
		}
		return templates.MetricsViewModel{}, err
	}
	defer file.Close()

	var entries []logMetricEntry

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry logMetricEntry
		if err := json.Unmarshal(line, &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[Dashboard] Error scanning local logs for metrics: %v", err)
	}

	return computeMetricsFromEntries(entries, time.Now()), nil
}

// fetchCloudMonitoringMetrics collects proxy request volume, error rate, and P50/P95 latency from Cloud Logging StructuredLogs.
func (dc *DashboardController) fetchCloudMonitoringMetrics(ctx context.Context) (templates.MetricsViewModel, error) {
	if os.Getenv("LOCAL_DEV") == "true" {
		return dc.fetchLocalLogsMetrics()
	}
	if dc.ProjectID == "" || dc.ProjectID == "dev-project" || dc.TokenSource == nil {
		return templates.MetricsViewModel{}, fmt.Errorf("google cloud monitoring credentials not initialized for project %q", dc.ProjectID)
	}

	serviceName := dc.resolveBackendServiceName()
	now := time.Now()
	startTime := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	urlStr := "https://logging.googleapis.com/v2/entries:list"
	filter := fmt.Sprintf(`resource.type="cloud_run_revision" AND resource.labels.service_name="%s" AND jsonPayload.path:* AND jsonPayload.latency_ms:* AND timestamp>="%s"`, serviceName, startTime)

	type gcpMetricLogEntry struct {
		Timestamp   string `json:"timestamp"`
		JSONPayload struct {
			Time      string `json:"time"`
			Status    int    `json:"status"`
			LatencyMs int64  `json:"latency_ms"`
		} `json:"jsonPayload"`
	}
	type gcpMetricLogsResponse struct {
		Entries       []gcpMetricLogEntry `json:"entries"`
		NextPageToken string              `json:"nextPageToken"`
	}

	var entries []logMetricEntry
	pageToken := ""
	for page := 0; page < 10; page++ {
		bodyReq := map[string]interface{}{
			"resourceNames": []string{fmt.Sprintf("projects/%s", dc.ProjectID)},
			"filter":        filter,
			"orderBy":       "timestamp desc",
			"pageSize":      1000,
		}
		if pageToken != "" {
			bodyReq["pageToken"] = pageToken
		}

		respBody, err := dc.gcpPost(ctx, urlStr, bodyReq)
		if err != nil {
			log.Printf("[Monitoring] Error querying Cloud Logging metrics: %v", err)
			return templates.MetricsViewModel{}, err
		}

		var parsed gcpMetricLogsResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return templates.MetricsViewModel{}, fmt.Errorf("failed to unmarshal cloud logging metrics: %w", err)
		}

		for _, e := range parsed.Entries {
			ts := e.JSONPayload.Time
			if ts == "" {
				ts = e.Timestamp
			}
			entries = append(entries, logMetricEntry{
				Time:      ts,
				Status:    e.JSONPayload.Status,
				LatencyMs: e.JSONPayload.LatencyMs,
			})
		}

		if parsed.NextPageToken == "" {
			break
		}
		pageToken = parsed.NextPageToken
	}

	return computeMetricsFromEntries(entries, now), nil
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

	if err := json.Unmarshal(volBody, &volData); err != nil {
		return templates.MetricsViewModel{}, fmt.Errorf("failed to unmarshal volume metrics: %w", err)
	}
	if err := json.Unmarshal(latBody, &latData); err != nil {
		return templates.MetricsViewModel{}, fmt.Errorf("failed to unmarshal latency metrics: %w", err)
	}

	// Process last 24 hourly intervals
	counts := make([]int, 24)
	p50 := make([]int, 24)
	p95 := make([]int, 24)
	labels := make([]string, 24)

	now := time.Now()
	for i := 0; i < 24; i++ {
		t := now.Add(-time.Duration(23-i) * time.Hour)
		labels[i] = t.Format("15:00")
	}

	totalReqs := 0
	peakRate := 0
	errorCount := 0
	classHourVol := make(map[string][]int)

	if len(volData.TimeSeries) > 0 {
		for _, series := range volData.TimeSeries {
			rcClass := series.Metric.Labels["response_code_class"]
			isError := rcClass == "4xx" || rcClass == "5xx"
			if _, ok := classHourVol[rcClass]; !ok {
				classHourVol[rcClass] = make([]int, 24)
			}
			for _, pt := range series.Points {
				t, err := time.Parse(time.RFC3339, pt.Interval.EndTime)
				if err != nil {
					continue
				}
				hrIndex := 23 - int(now.Sub(t).Hours())
				if hrIndex >= 0 && hrIndex < 24 {
					val, _ := strconv.Atoi(pt.Value.Int64Value)
					counts[hrIndex] += val
					classHourVol[rcClass][hrIndex] += val
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

	// Aggregate latencies per hour weighted by series volume
	p95WeightedSum := make([]float64, 24)
	p95WeightTotal := make([]int, 24)
	p50WeightedSum := make([]float64, 24)
	p50WeightTotal := make([]int, 24)
	hasExplicitP50 := false

	if len(latData.TimeSeries) > 0 {
		for _, series := range latData.TimeSeries {
			rcClass := series.Metric.Labels["response_code_class"]
			pctLabel := series.Metric.Labels["percentile"]
			for _, pt := range series.Points {
				t, err := time.Parse(time.RFC3339, pt.Interval.EndTime)
				if err != nil {
					continue
				}
				hrIndex := 23 - int(now.Sub(t).Hours())
				if hrIndex >= 0 && hrIndex < 24 {
					val := pt.Value.DoubleValue
					if val > 0 {
						weight := 1
						if vols, ok := classHourVol[rcClass]; ok && vols[hrIndex] > 0 {
							weight = vols[hrIndex]
						}
						if pctLabel == "50" || pctLabel == "p50" {
							hasExplicitP50 = true
							p50WeightedSum[hrIndex] += val * float64(weight)
							p50WeightTotal[hrIndex] += weight
						} else {
							p95WeightedSum[hrIndex] += val * float64(weight)
							p95WeightTotal[hrIndex] += weight
						}
					}
				}
			}
		}
	}

	for i := 0; i < 24; i++ {
		if p95WeightTotal[i] > 0 {
			p95[i] = int(math.Round(p95WeightedSum[i] / float64(p95WeightTotal[i])))
		}
		if hasExplicitP50 {
			if p50WeightTotal[i] > 0 {
				p50[i] = int(math.Round(p50WeightedSum[i] / float64(p50WeightTotal[i])))
			}
		} else {
			p50[i] = p95[i]
		}
	}

	avgP95 := 0
	weightedLatencySum := 0
	totalLatencyWeight := 0
	unweightedSum := 0
	activeCount := 0
	for i, v := range p95 {
		if v > 0 {
			unweightedSum += v
			activeCount++
			if counts[i] > 0 {
				weightedLatencySum += v * counts[i]
				totalLatencyWeight += counts[i]
			}
		}
	}
	if totalLatencyWeight > 0 {
		avgP95 = int(math.Round(float64(weightedLatencySum) / float64(totalLatencyWeight)))
	} else if activeCount > 0 {
		avgP95 = unweightedSum / activeCount
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

// fetchCostAnalyticsData queries Cloud Logging logs or local telemetry logs.
func (dc *DashboardController) fetchCostAnalyticsData(ctx context.Context) (templates.CostsViewModel, error) {
	if os.Getenv("LOCAL_DEV") == "true" {
		return dc.fetchLocalLogsCosts()
	}
	if dc.ProjectID == "" || dc.ProjectID == "dev-project" || dc.TokenSource == nil {
		return templates.CostsViewModel{}, fmt.Errorf("google cloud logging credentials not initialized for project %q", dc.ProjectID)
	}

	serviceName := dc.resolveBackendServiceName()

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
		return templates.CostsViewModel{}, err
	}

	return dc.parseGCPCosts(respBody)
}

// parseGCPCosts parses the REST payload and aggregates spends.
func (dc *DashboardController) parseGCPCosts(payload []byte) (templates.CostsViewModel, error) {
	type GCPLogEntry struct {
		Timestamp   string `json:"timestamp"`
		InsertID    string `json:"insertId"`
		JSONPayload struct {
			ClientID    string `json:"client_id"`
			ModelRouted string `json:"model_routed"`
			BytesSent   int64  `json:"bytes_sent"`
			LatencyMs   int64  `json:"latency_ms"`
		} `json:"jsonPayload"`
	}
	type GCPLogsResponse struct {
		Entries []GCPLogEntry `json:"entries"`
	}

	var resp GCPLogsResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return templates.CostsViewModel{}, fmt.Errorf("failed to unmarshal cloud logging entries: %w", err)
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

		sessionID := entry.InsertID
		if len(sessionID) > 8 {
			sessionID = sessionID[:8]
		}

		tx := templates.CostTransaction{
			Time:          timeStr,
			SessionID:     sessionID,
			ClientID:      clientID,
			ModelRouted:   model,
			InputTokens:   inT,
			OutputTokens:  outT,
			EstimatedCost: cost,
			LatencyMs:     entry.JSONPayload.LatencyMs,
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

// ServeCosts handles cost breakouts and analytics.
func (dc *DashboardController) ServeCosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vm, err := dc.fetchCostAnalyticsData(ctx)
	if err != nil {
		log.Printf("[Costs] Error fetching analytics data: %v", err)
		http.Error(w, fmt.Sprintf("Failed to load cost analytics: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	content := templates.CostsTab(vm)
	_ = templates.Layout("Cost Analytics", "costs", content).Render(ctx, w)
}

// ServeMetrics renders the Cloud Monitoring charts tab.
func (dc *DashboardController) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vm, err := dc.fetchCloudMonitoringMetrics(ctx)
	if err != nil {
		log.Printf("[Monitoring] Error loading system monitoring metrics: %v", err)
		http.Error(w, fmt.Sprintf("Failed to load system metrics: %v", err), http.StatusInternalServerError)
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
		dc.renderFormError(w, r, "Application ID (Slug) cannot be empty.", http.StatusBadRequest)
		return
	}
	// Support email slugs (for Google service accounts) or standard alphanumeric slugs
	if strings.Contains(appID, "@") {
		if !strings.Contains(appID, ".") {
			dc.renderFormError(w, r, "Application ID email structure is invalid.", http.StatusBadRequest)
			return
		}
	} else {
		for _, char := range appID {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '.') {
				dc.renderFormError(w, r, "Application ID must be a valid slug (alphanumeric, hyphens, and dots only).", http.StatusBadRequest)
				return
			}
		}
	}

	if appName == "" {
		dc.renderFormError(w, r, "Application Name cannot be empty.", http.StatusBadRequest)
		return
	}

	if clientID == "" {
		dc.renderFormError(w, r, "Parent Client Organization selection is required.", http.StatusBadRequest)
		return
	}

	var parentClient config.Client
	if allClients, err := dc.Store.GetAllClients(ctx); err == nil {
		for _, c := range allClients {
			if c.ID == clientID {
				parentClient = c
				break
			}
		}
	}

	if priority == "" {
		if parentClient.Priority != "" {
			priority = parentClient.Priority
		} else {
			priority = "medium"
		}
	}

	var rpm int
	if rpmStr == "" {
		if parentClient.RPM > 0 {
			rpm = parentClient.RPM
		} else {
			rpm = 60
		}
	} else {
		var err error
		rpm, err = strconv.Atoi(rpmStr)
		if err != nil || rpm <= 0 {
			dc.renderFormError(w, r, "Requests Per Minute (RPM) limit must be a positive integer.", http.StatusBadRequest)
			return
		}
	}

	var tpm int
	if tpmStr == "" {
		if parentClient.TPM > 0 {
			tpm = parentClient.TPM
		} else {
			tpm = 40000
		}
	} else {
		var err error
		tpm, err = strconv.Atoi(tpmStr)
		if err != nil || tpm <= 0 {
			dc.renderFormError(w, r, "Tokens Per Minute (TPM) limit must be a positive integer.", http.StatusBadRequest)
			return
		}
	}

	optOutDynamicRouting := r.FormValue("opt_out_dynamic_routing") == "true"
	optOutTPM := r.FormValue("opt_out_tpm") == "true"

	existingApp, exists := dc.Store.LookupApp(appID)
	var complexity config.ComplexityRouting
	if exists {
		complexity = existingApp.Complexity
	} else {
		complexity = config.ComplexityRouting{
			Enabled:                false,
			AlwaysOverride:         false,
			SimpleModel:            "gemini-3.5-flash-lite",
			MediumModel:            "gemini-3.8-flash",
			ComplexModel:           "gemini-3.1-pro",
			SimpleCharLimit:        200,
			MediumCharLimit:        1000,
			ForceComplexMultimodal: true,
			ForceComplexTools:      true,
			UseLLMClassifier:       false,
			ClassifierModel:        "gemini-3.5-flash-lite",
		}
	}

	err := dc.Store.SaveApp(ctx, config.App{
		ID:                   appID,
		ClientID:             clientID,
		Name:                 appName,
		RPM:                  rpm,
		TPM:                  tpm,
		Priority:             priority,
		Complexity:           complexity,
		OptOutDynamicRouting: optOutDynamicRouting,
		OptOutTPM:            optOutTPM,
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

	if dc.Store != nil {
		if providers, err := dc.Store.GetAllProviders(ctx); err == nil {
			for _, p := range providers {
				decision.SyncDecisionProvidersFromLocalConfig(p)
			}
		}
	}

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
	engine := strings.TrimSpace(r.FormValue("engine"))
	geminiDecisionEndpoint := strings.TrimSpace(r.FormValue("gemini_decision_endpoint"))
	jevEndpoint := strings.TrimSpace(r.FormValue("jev_endpoint"))
	fastinoEndpoint := strings.TrimSpace(r.FormValue("fastino_endpoint"))
	clmEndpoint := strings.TrimSpace(r.FormValue("clm_endpoint"))
	secretRef := strings.TrimSpace(r.FormValue("secret_ref"))
	minConfidenceStr := strings.TrimSpace(r.FormValue("min_confidence"))
	simpleCriteria := strings.TrimSpace(r.FormValue("simple_criteria"))
	mediumCriteria := strings.TrimSpace(r.FormValue("medium_criteria"))
	complexCriteria := strings.TrimSpace(r.FormValue("complex_criteria"))
	apiKey := strings.TrimSpace(r.FormValue("api_key"))
	simpleModel := strings.TrimSpace(r.FormValue("simple_model"))
	mediumModel := strings.TrimSpace(r.FormValue("medium_model"))
	complexModel := strings.TrimSpace(r.FormValue("complex_model"))
	simpleCharLimitStr := strings.TrimSpace(r.FormValue("simple_char_limit"))
	mediumCharLimitStr := strings.TrimSpace(r.FormValue("medium_char_limit"))
	forceComplexMultimodal := r.FormValue("force_complex_multimodal") == "true"
	forceComplexTools := r.FormValue("force_complex_tools") == "true"
	useLLMClassifier := r.FormValue("use_llm_classifier") == "true"
	classifierModel := strings.TrimSpace(r.FormValue("classifier_model"))
	additionalInstructions := strings.TrimSpace(r.FormValue("additional_instructions"))

	if simpleModel == "" {
		simpleModel = "gemini-3.5-flash-lite"
	}
	if mediumModel == "" {
		mediumModel = "gemini-3.8-flash"
	}
	if complexModel == "" {
		complexModel = "gemini-3.1-pro"
	}
	if classifierModel == "" {
		classifierModel = "gemini-3.5-flash-lite"
	}

	if engine == "" {
		if useLLMClassifier {
			engine = "llm"
		} else {
			engine = "jev"
		}
	}

	if apiKey != "" {
		decision.SetEngineSecret(engine, apiKey)
		if secretRef != "" {
			decision.SetEngineSecret(secretRef, apiKey)
		}
	}

	simpleCharLimit := app.Complexity.SimpleCharLimit
	if simpleCharLimit <= 0 {
		simpleCharLimit = 200
	}
	if simpleCharLimitStr != "" {
		parsed, err := strconv.Atoi(simpleCharLimitStr)
		if err != nil || parsed < 0 {
			http.Error(w, "Simple text limit must be a non-negative integer.", http.StatusBadRequest)
			return
		}
		simpleCharLimit = parsed
	}

	mediumCharLimit := app.Complexity.MediumCharLimit
	if mediumCharLimit <= 0 {
		mediumCharLimit = 800
	}
	if mediumCharLimitStr != "" {
		parsed, err := strconv.Atoi(mediumCharLimitStr)
		if err != nil || parsed < 0 {
			http.Error(w, "Medium text limit must be a non-negative integer.", http.StatusBadRequest)
			return
		}
		mediumCharLimit = parsed
	}

	if simpleCharLimit > mediumCharLimit {
		http.Error(w, "Simple text character limit cannot be larger than Medium limit.", http.StatusBadRequest)
		return
	}

	minConfidence := app.Complexity.MinConfidence
	if minConfidenceStr != "" {
		mc, err := strconv.ParseFloat(minConfidenceStr, 64)
		if err != nil || mc < 0 || mc > 1 {
			http.Error(w, "Minimum confidence must be between 0.0 and 1.0.", http.StatusBadRequest)
			return
		}
		minConfidence = mc
	}

	taskMapping := app.Complexity.TaskMapping
	if taskMapping == nil {
		taskMapping = make(map[string]string)
	}
	taskKeys := map[string]string{
		"code":          strings.TrimSpace(r.FormValue("task_code")),
		"triage":        strings.TrimSpace(r.FormValue("task_triage")),
		"summarization": strings.TrimSpace(r.FormValue("task_summarization")),
	}
	for k, v := range taskKeys {
		if v != "" {
			taskMapping[k] = v
		} else {
			delete(taskMapping, k)
		}
	}

	// Update Complexity fields
	app.Complexity = config.ComplexityRouting{
		Enabled:                enabled,
		AlwaysOverride:         alwaysOverride,
		Engine:                 engine,
		GeminiDecisionEndpoint: geminiDecisionEndpoint,
		JevEndpoint:            jevEndpoint,
		FastinoEndpoint:        fastinoEndpoint,
		CLMEndpoint:            clmEndpoint,
		SecretRef:              secretRef,
		MinConfidence:          minConfidence,
		SimpleCriteria:         simpleCriteria,
		MediumCriteria:         mediumCriteria,
		ComplexCriteria:        complexCriteria,
		SimpleModel:            simpleModel,
		MediumModel:            mediumModel,
		ComplexModel:           complexModel,
		SimpleCharLimit:        simpleCharLimit,
		MediumCharLimit:        mediumCharLimit,
		ForceComplexMultimodal: forceComplexMultimodal,
		ForceComplexTools:      forceComplexTools,
		UseLLMClassifier:       useLLMClassifier,
		ClassifierModel:        classifierModel,
		AdditionalInstructions: additionalInstructions,
		TaskMapping:            taskMapping,
	}

	err := dc.Store.SaveApp(ctx, app)
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

// HandleComplexityKeys manages decision engine secret keys, endpoints, and Contrastive-LM cluster bindings.
func (dc *DashboardController) HandleComplexityKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		if dc.Store != nil {
			if providers, err := dc.Store.GetAllProviders(ctx); err == nil {
				for _, p := range providers {
					decision.SyncDecisionProvidersFromLocalConfig(p)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(decision.GetAllEngineSecretStatuses(""))
		return
	case http.MethodPost:
		engine := strings.TrimSpace(r.FormValue("engine"))
		apiKey := strings.TrimSpace(r.FormValue("api_key"))
		secretRef := strings.TrimSpace(r.FormValue("secret_ref"))
		endpoint := strings.TrimSpace(r.FormValue("endpoint"))
		connectionMode := strings.TrimSpace(r.FormValue("connection_mode"))
		clusterID := strings.TrimSpace(r.FormValue("cluster_id"))

		if engine == "" && secretRef != "" {
			engine = secretRef
		}
		if engine == "" || (apiKey == "" && endpoint == "" && connectionMode == "" && clusterID == "") {
			http.Error(w, "Both engine and api_key (or endpoint/cluster configuration) are required", http.StatusBadRequest)
			return
		}
		if apiKey != "" {
			decision.SetEngineSecret(engine, apiKey)
			if secretRef != "" {
				decision.SetEngineSecret(secretRef, apiKey)
			}
		}
		if connectionMode == "local" && endpoint == "" {
			decision.SetEngineEndpoint(engine, "")
			decision.SetEngineClusterConfig(engine, "", "local", clusterID)
		} else if endpoint != "" || connectionMode != "" || clusterID != "" {
			decision.SetEngineClusterConfig(engine, endpoint, connectionMode, clusterID)
		}

		// Persist cluster/endpoint config to local provider in store
		if dc.Store != nil && (endpoint != "" || connectionMode != "" || clusterID != "" || secretRef != "") {
			providers, _ := dc.Store.GetAllProviders(ctx)
			var localProv config.ProviderConfig
			found := false
			for _, p := range providers {
				if p.ID == config.ProviderLocal {
					localProv = p
					found = true
					break
				}
			}
			if !found {
				localProv = config.ProviderConfig{
					ID:      config.ProviderLocal,
					Enabled: true,
					Regions: make(map[string]config.RegionConfig),
				}
			}
			decision.ApplyDecisionProviderToLocalConfig(&localProv, engine, endpoint, connectionMode, clusterID, secretRef)
			_ = dc.Store.SaveProvider(ctx, localProv)
		}

		// Sync with backend API if using APIConfigStore
		if apiStore, ok := dc.Store.(*config.APIConfigStore); ok {
			_ = apiStore.SyncDecisionProvider(ctx, engine, apiKey, secretRef, endpoint, connectionMode, clusterID)
		}

		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/admin/complexity")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/admin/complexity", http.StatusSeeOther)
		return
	case http.MethodDelete:
		engine := strings.TrimSpace(r.URL.Query().Get("engine"))
		if engine == "" {
			engine = strings.TrimSpace(r.FormValue("engine"))
		}
		secretRef := strings.TrimSpace(r.URL.Query().Get("secret_ref"))
		if secretRef == "" {
			secretRef = strings.TrimSpace(r.FormValue("secret_ref"))
		}
		if engine != "" {
			decision.DeleteEngineSecret(engine)
		}
		if secretRef != "" {
			decision.DeleteEngineSecret(secretRef)
		}
		if apiStore, ok := dc.Store.(*config.APIConfigStore); ok {
			_ = apiStore.DeleteDecisionSecret(ctx, engine, secretRef)
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/admin/complexity")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleComplexityProviderProbe executes a live health & classification probe against a decision engine provider.
func (dc *DashboardController) HandleComplexityProviderProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	if dc.Store != nil {
		if providers, err := dc.Store.GetAllProviders(ctx); err == nil {
			for _, p := range providers {
				decision.SyncDecisionProvidersFromLocalConfig(p)
			}
		}
	}

	engine := strings.TrimSpace(r.FormValue("engine"))
	if engine == "" {
		engine = strings.TrimSpace(r.URL.Query().Get("engine"))
	}
	if engine == "" {
		engine = "contrastive-lm"
	}
	endpoint := strings.TrimSpace(r.FormValue("endpoint"))
	if endpoint == "" {
		endpoint = strings.TrimSpace(r.URL.Query().Get("endpoint"))
	}

	res, elapsed, target, probeErr := decision.ProbeDecisionProvider(ctx, engine, endpoint)
	_, mode, _ := decision.GetEngineClusterConfig(engine)
	if strings.Contains(r.Header.Get("Accept"), "application/json") || r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]interface{}{
			"ok":              probeErr == nil,
			"engine":          decision.NormalizeEngineName(engine),
			"connection_mode": mode,
			"endpoint":        target,
			"tier":            res.Tier,
			"confidence":      res.Confidence,
			"rationale":       res.Rationale,
			"latency_ms":      elapsed.Milliseconds(),
		}
		if probeErr != nil {
			payload["error"] = probeErr.Error()
		}
		_ = json.NewEncoder(w).Encode(payload)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	badgeClass := "bg-emerald-50 border-emerald-200 text-emerald-900"
	statusLabel := "Healthy"
	msg := res.Rationale
	if probeErr != nil {
		badgeClass = "bg-red-50 border-red-200 text-red-900"
		statusLabel = "Unreachable"
		msg = probeErr.Error()
	}
	if target == "" {
		target = "embedded-local-prototypes"
	}
	_, _ = fmt.Fprintf(w, `<div class="rounded-lg border p-3.5 text-xs %s flex flex-col sm:flex-row sm:items-center justify-between gap-2">
		<div>
			<span class="font-bold uppercase font-mono">%s</span>
			<span class="mx-1.5">&bull;</span>
			<span class="font-semibold">%s (%s)</span>
			<span class="mx-1.5">&bull;</span>
			<span class="font-mono">Tier: %s (%d ms)</span>
			<div class="text-[11px] mt-1 opacity-90">%s</div>
		</div>
		<span class="font-mono text-[11px] px-2 py-0.5 rounded bg-white/80 border border-current/20 shrink-0">%s</span>
	</div>`,
		badgeClass,
		decision.NormalizeEngineName(engine),
		statusLabel,
		mode,
		res.Tier,
		elapsed.Milliseconds(),
		msg,
		target,
	)
}

// HandleComplexityInspect tests prompt complexity against decision models (Jev, Fastino, CLM/Contrastive-LM, heuristics) and returns inspection explainability.
func (dc *DashboardController) HandleComplexityInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if dc.Store != nil {
		if providers, err := dc.Store.GetAllProviders(r.Context()); err == nil {
			for _, p := range providers {
				decision.SyncDecisionProvidersFromLocalConfig(p)
			}
		}
	}

	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		prompt = "What is 2+2?"
	}
	engine := strings.TrimSpace(r.FormValue("engine"))
	if engine == "" {
		engine = "jev"
	}
	task := strings.TrimSpace(r.FormValue("task"))

	simpleModel := strings.TrimSpace(r.FormValue("simple_model"))
	if simpleModel == "" {
		simpleModel = "gemini-3.5-flash-lite"
	}
	mediumModel := strings.TrimSpace(r.FormValue("medium_model"))
	if mediumModel == "" {
		mediumModel = "gemini-3.8-flash"
	}
	complexModel := strings.TrimSpace(r.FormValue("complex_model"))
	if complexModel == "" {
		complexModel = "gemini-3.1-pro"
	}

	complexityCfg := config.ComplexityRouting{
		Enabled:                true,
		Engine:                 engine,
		SimpleModel:            simpleModel,
		MediumModel:            mediumModel,
		ComplexModel:           complexModel,
		GeminiDecisionEndpoint: decision.GetEngineEndpoint("gemini-decision"),
		CLMEndpoint:            decision.GetEngineEndpoint("contrastive-lm"),
		JevEndpoint:            decision.GetEngineEndpoint("jev"),
		FastinoEndpoint:        decision.GetEngineEndpoint("fastino"),
	}

	var result decision.DecisionResult

	if task != "" {
		if model, tier, ok := decision.ResolveTaskModel(task, complexityCfg); ok {
			feats := decision.ExtractPromptFeatures(prompt, false, false)
			feats.ExtractedTask = task
			result = decision.DecisionResult{
				Tier:        tier,
				TargetModel: model,
				Engine:      "task_mapping",
				Confidence:  1.0,
				Rationale:   fmt.Sprintf("Matched specialized agent task '%s' -> routed to model tier %s (%s).", task, tier, model),
				Features:    feats,
			}
		}
	}

	if result.TargetModel == "" {
		switch strings.ToLower(engine) {
		case "gemini-decision", "gemini-decisions", "gemini-decision-1", "decisions":
			gemEng := decision.NewGeminiDecisionEngine(dc.TokenSource, dc.ProjectID)
			if dc.HTTPClient != nil {
				gemEng.HTTPClient = dc.HTTPClient
			}
			res, err := gemEng.Classify(r.Context(), prompt, complexityCfg, false, false)
			if err != nil {
				if strings.Contains(r.Header.Get("Accept"), "application/json") || r.URL.Query().Get("format") == "json" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadGateway)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
					return
				}
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			result = res
		case "fastino":
			fastino := decision.NewFastinoDecisionEngine()
			result = fastino.Classify(r.Context(), prompt, complexityCfg, false, false)
		case "clm", "contrastive-lm":
			clm := decision.NewCLMContrastiveEngine()
			result = clm.Classify(r.Context(), prompt, complexityCfg, false, false)
			if strings.ToLower(engine) == "contrastive-lm" {
				result.Engine = "contrastive-lm"
			}
		case "heuristics":
			feats := decision.ExtractPromptFeatures(prompt, false, false)
			tier := "simple"
			model := complexityCfg.SimpleModel
			if feats.Length > 800 {
				tier = "complex"
				model = complexityCfg.ComplexModel
			} else if feats.Length > 200 {
				tier = "medium"
				model = complexityCfg.MediumModel
			}
			result = decision.DecisionResult{
				Tier:        tier,
				TargetModel: model,
				Engine:      "heuristics",
				Confidence:  0.85,
				Rationale:   fmt.Sprintf("Static character boundary check: prompt length is %d characters (simple threshold <= 200, medium <= 800).", feats.Length),
				Features:    feats,
			}
		default: // "jev"
			jev := decision.NewJevDecisionEngine()
			result = jev.Classify(r.Context(), prompt, complexityCfg, false, false)
		}
	}

	if strings.Contains(r.Header.Get("Accept"), "application/json") || r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.ComplexityInspectResult(result).Render(r.Context(), w)
}

// ServeLocationsTestHelper is an exported test helper wrapper to execute fetchLocations in unit tests.
func (dc *DashboardController) ServeLocationsTestHelper(ctx context.Context) ([]templates.LocationInfo, error) {
	return dc.fetchLocations(ctx)
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
			AppID:          s.AppID,
			Model:          s.Model,
			Priority:       s.Priority,
			Tier:           s.Tier,
			Status:         s.Status,
			ClusterID:      s.ClusterID,
			AssignedNodeID: s.AssignedNodeID,
			QueueSource:    s.QueueSource,
			ArrivalTime:    s.ArrivalTime.Format(time.RFC3339),
			DurationMs:     s.DurationMs,
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
				{Title: "Local Clusters & Runners", Path: "admin/local-clusters.md"},
				{Title: "Metrics & Expenditure Analytics", Path: "admin/metrics-and-costs.md"},
			},
		},
		{
			Name: "Architecture",
			Items: []templates.DocItem{
				{Title: "System Overview", Path: "architecture/overview.md"},
				{Title: "Google GenAI SDK Coverage", Path: "architecture/google-genai-sdk-coverage.md"},
				{Title: "Local Clusters Plan", Path: "architecture/local-clusters-plan.md"},
				{Title: "Local Clusters Guide", Path: "architecture/local-clusters-guide.md"},
			},
		},
		{
			Name: "Development & Testing",
			Items: []templates.DocItem{
				{Title: "Test-Driven Development", Path: "approaches/tdd-feature-development.md"},
				{Title: "Model Version Compliance", Path: "approaches/model-compliance.md"},
				{Title: "Local Runners E2E Testing Plan", Path: "approaches/local-runners-testing.md"},
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

// ServeClusters renders the Local Clusters status tab.
func (dc *DashboardController) ServeClusters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	providers, err := dc.Store.GetAllProviders(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading providers for clusters: %v", err)
	}

	activeNodes, err := dc.Store.GetRegisteredRunners(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading registered runners: %v", err)
	}
	for i := range activeNodes {
		if !activeNodes[i].Node.LastHeartbeat.IsZero() && time.Since(activeNodes[i].Node.LastHeartbeat) > 30*time.Second && activeNodes[i].Node.Status == "online" {
			activeNodes[i].Node.Status = "offline"
		}
	}

	queueItems, _ := dc.Store.GetQueueStatus(ctx)

	w.Header().Set("Content-Type", "text/html")
	content := templates.ClustersTab(templates.ClustersViewModel{
		ActiveNodes: activeNodes,
		QueueDepth:  len(queueItems),
		Providers:   providers,
	})
	_ = templates.Layout("Local Clusters", "clusters", content).Render(ctx, w)
}

// PollClustersActive returns only the raw inner nodes component for dynamic HTMX replacement.
func (dc *DashboardController) PollClustersActive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	providers, err := dc.Store.GetAllProviders(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading providers: %v", err)
	}

	activeNodes, err := dc.Store.GetRegisteredRunners(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading registered runners: %v", err)
	}
	for i := range activeNodes {
		if !activeNodes[i].Node.LastHeartbeat.IsZero() && time.Since(activeNodes[i].Node.LastHeartbeat) > 30*time.Second && activeNodes[i].Node.Status == "online" {
			activeNodes[i].Node.Status = "offline"
		}
	}

	queueItems, _ := dc.Store.GetQueueStatus(ctx)

	w.Header().Set("Content-Type", "text/html")
	_ = templates.ClustersPoller(templates.ClustersViewModel{
		ActiveNodes: activeNodes,
		QueueDepth:  len(queueItems),
		Providers:   providers,
	}).Render(ctx, w)
}

// ServeClusterMappingsModal renders the attachments modal for a specific runner node.
func (dc *DashboardController) ServeClusterMappingsModal(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(r.URL.Query().Get("id"))
	if nodeID == "" {
		http.Error(w, "Missing node id", http.StatusBadRequest)
		return
	}
	dc.ServeClusterNodeModal(w, r)
}

// ServeClusterNodeModal renders the modal overlay for pre-registering a new cluster node or editing an existing runner node.
func (dc *DashboardController) ServeClusterNodeModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodeID := strings.TrimSpace(r.URL.Query().Get("id"))

	providers, err := dc.Store.GetAllProviders(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading providers for node modal: %v", err)
	}

	if nodeID == "" {
		w.Header().Set("Content-Type", "text/html")
		_ = templates.RunnerModal(config.Node{}, nil, providers).Render(ctx, w)
		return
	}

	runners, err := dc.Store.GetRegisteredRunners(ctx)
	if err != nil {
		http.Error(w, "Failed to fetch registered runners", http.StatusInternalServerError)
		return
	}

	var targetRunner *config.RunnerResponse
	for _, runner := range runners {
		if runner.Node.ID == nodeID {
			rCopy := runner
			targetRunner = &rCopy
			break
		}
	}

	if targetRunner == nil {
		http.Error(w, "Runner node not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_ = templates.RunnerModal(targetRunner.Node, targetRunner.AssignedClusters, providers).Render(ctx, w)
}

// ServeClusterEditModal renders the modal overlay for creating or editing a local cluster or direct endpoint.
func (dc *DashboardController) ServeClusterEditModal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resourceType := strings.TrimSpace(r.URL.Query().Get("type"))
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	providers, err := dc.Store.GetAllProviders(ctx)
	if err != nil {
		http.Error(w, "Failed to load providers", http.StatusInternalServerError)
		return
	}
	runners, _ := dc.Store.GetRegisteredRunners(ctx)

	var allClusters []config.LocalCluster
	var allEndpoints []config.DirectEndpoint
	seenClusters := make(map[string]int)
	for _, p := range providers {
		if p.ID == config.ProviderLocal {
			for _, reg := range p.Regions {
				if reg.LocalConfig != nil {
					for _, cl := range reg.LocalConfig.Clusters {
						seenClusters[cl.ID] = len(allClusters)
						allClusters = append(allClusters, cl)
					}
					allEndpoints = append(allEndpoints, reg.LocalConfig.DirectEndpoints...)
				}
			}
		}
	}
	for _, runner := range runners {
		for _, cid := range runner.AssignedClusters {
			trimmed := strings.TrimSpace(cid)
			if trimmed == "" {
				continue
			}
			if idx, ok := seenClusters[trimmed]; ok {
				if allClusters[idx].Nodes == nil {
					allClusters[idx].Nodes = make(map[string]config.Node)
				}
				if runner.Node.ID != "" {
					allClusters[idx].Nodes[runner.Node.ID] = runner.Node
				}
			} else {
				nodes := make(map[string]config.Node)
				if runner.Node.ID != "" {
					nodes[runner.Node.ID] = runner.Node
				}
				seenClusters[trimmed] = len(allClusters)
				allClusters = append(allClusters, config.LocalCluster{
					ID:          trimmed,
					Name:        trimmed,
					Description: "Auto-discovered from connected runner node(s).",
					QueueName:   "queue-" + trimmed,
					Nodes:       nodes,
					MaxQueueAge: 90,
				})
			}
		}
	}

	w.Header().Set("Content-Type", "text/html")
	if id == "" {
		if resourceType == "endpoint" || resourceType == "direct-http" {
			_ = templates.ClusterEditModal("endpoint", config.LocalCluster{}, config.DirectEndpoint{
				Engine: "vllm",
				Role:   "inference",
				Active: true,
			}, allClusters, runners).Render(ctx, w)
			return
		}
		_ = templates.ClusterEditModal("cluster", config.LocalCluster{
			MaxQueueAge: 90,
		}, config.DirectEndpoint{}, allClusters, runners).Render(ctx, w)
		return
	}

	if resourceType == "endpoint" || resourceType == "direct-http" {
		for _, ep := range allEndpoints {
			if ep.ID == id {
				_ = templates.ClusterEditModal("endpoint", config.LocalCluster{}, ep, allClusters, runners).Render(ctx, w)
				return
			}
		}
		http.Error(w, "Direct endpoint not found", http.StatusNotFound)
		return
	}

	for _, cl := range allClusters {
		if cl.ID == id {
			_ = templates.ClusterEditModal("cluster", cl, config.DirectEndpoint{}, allClusters, runners).Render(ctx, w)
			return
		}
	}
	http.Error(w, "Local cluster not found", http.StatusNotFound)
}

// CreateProviderCluster handles creating or updating a local cluster or direct endpoint in the "local" provider configuration.
func (dc *DashboardController) CreateProviderCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	entryType := strings.TrimSpace(r.FormValue("type"))
	id := strings.TrimSpace(r.FormValue("id"))
	name := strings.TrimSpace(r.FormValue("name"))
	target := strings.TrimSpace(r.FormValue("target"))

	if id == "" || name == "" {
		http.Error(w, "Cluster or endpoint ID and Name are required.", http.StatusBadRequest)
		return
	}

	providers, err := dc.Store.GetAllProviders(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading providers: %v", err)
		http.Error(w, "Failed to load providers", http.StatusInternalServerError)
		return
	}

	localProv := config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local Infrastructure",
		Enabled:     true,
		Regions:     make(map[string]config.RegionConfig),
	}
	for _, p := range providers {
		if p.ID == config.ProviderLocal {
			localProv = p
			break
		}
	}
	if localProv.Regions == nil {
		localProv.Regions = make(map[string]config.RegionConfig)
	}
	localReg, ok := localProv.Regions["local-cluster"]
	if !ok {
		localReg = config.RegionConfig{
			Code:        "local-cluster",
			Active:      true,
			LocalConfig: &config.LocalConfig{},
		}
	}
	if localReg.LocalConfig == nil {
		localReg.LocalConfig = &config.LocalConfig{}
	}

	var modifiedRunners []config.RunnerResponse

	switch entryType {
	case "endpoint", "direct-http":
		urlStr := strings.TrimSpace(r.FormValue("url"))
		if urlStr == "" {
			urlStr = target
		}
		modelStr := strings.TrimSpace(r.FormValue("model"))
		if modelStr == "" {
			modelStr = name
		}
		if urlStr == "" {
			http.Error(w, "Endpoint URL is required.", http.StatusBadRequest)
			return
		}
		engineStr := strings.TrimSpace(r.FormValue("engine"))
		roleStr := strings.TrimSpace(r.FormValue("role"))
		clusterIDStr := strings.TrimSpace(r.FormValue("cluster_id"))
		fallbackModel := strings.TrimSpace(r.FormValue("fallback_model"))
		fallbackLocation := strings.TrimSpace(r.FormValue("fallback_location"))
		if fallbackModel != "" && !config.IsValidModelName(config.StripLocationSuffix(fallbackModel)) {
			http.Error(w, "Invalid or legacy fallback_model: "+fallbackModel, http.StatusBadRequest)
			return
		}
		activeStr := strings.ToLower(strings.TrimSpace(r.FormValue("active")))

		activeVal := true
		if activeStr == "false" || activeStr == "0" || activeStr == "off" {
			activeVal = false
		}

		ep := config.DirectEndpoint{
			ID:               id,
			Name:             name,
			ModelName:        modelStr,
			URL:              urlStr,
			Engine:           engineStr,
			Role:             roleStr,
			ClusterID:        clusterIDStr,
			FallbackModel:    fallbackModel,
			FallbackLocation: fallbackLocation,
			Active:           activeVal,
		}
		lowerModel := strings.ToLower(modelStr)
		if lowerModel == "contrastive-lm" || lowerModel == "clm" || lowerModel == "jev" || lowerModel == "fastino" {
			if lowerModel == "clm" {
				lowerModel = "contrastive-lm"
			}
			if ep.Role == "" {
				ep.Role = "decision_engine"
			}
			if ep.Engine == "" {
				ep.Engine = lowerModel
			}
			ep.ConnectionMode = "cluster"
			if ep.ClusterID == "" {
				ep.ClusterID = id
			}
		}
		updated := false
		for i, existing := range localReg.LocalConfig.DirectEndpoints {
			if existing.ID == id {
				if ep.Engine == "" {
					ep.Engine = existing.Engine
				}
				if ep.Role == "" {
					ep.Role = existing.Role
				}
				if !r.Form.Has("cluster_id") && ep.ClusterID == "" {
					ep.ClusterID = existing.ClusterID
				}
				if ep.ConnectionMode == "" {
					ep.ConnectionMode = existing.ConnectionMode
				}
				if !r.Form.Has("active") {
					ep.Active = existing.Active
				}
				if !r.Form.Has("fallback_model") && ep.FallbackModel == "" {
					ep.FallbackModel = existing.FallbackModel
				}
				if !r.Form.Has("fallback_location") && ep.FallbackLocation == "" {
					ep.FallbackLocation = existing.FallbackLocation
				}
				localReg.LocalConfig.DirectEndpoints[i] = ep
				updated = true
				break
			}
		}
		if !updated {
			localReg.LocalConfig.DirectEndpoints = append(localReg.LocalConfig.DirectEndpoints, ep)
		}
	default: // "cluster" or "pull-queue"
		queueName := strings.TrimSpace(r.FormValue("queue_name"))
		if queueName == "" {
			queueName = target
		}
		description := strings.TrimSpace(r.FormValue("description"))
		fallbackModel := strings.TrimSpace(r.FormValue("fallback_model"))
		fallbackLocation := strings.TrimSpace(r.FormValue("fallback_location"))
		if fallbackModel != "" && !config.IsValidModelName(fallbackModel) {
			http.Error(w, "Invalid or legacy fallback_model: "+fallbackModel, http.StatusBadRequest)
			return
		}
		maxAgeStr := strings.TrimSpace(r.FormValue("max_queue_age"))
		var maxQueueAge time.Duration
		if maxAgeStr != "" {
			if parsedAge, parseErr := strconv.Atoi(maxAgeStr); parseErr == nil && parsedAge > 0 {
				maxQueueAge = time.Duration(parsedAge)
			}
		}

		selectedNodeIDs := r.Form["node_ids"]
		nodeSelectionPresent := r.FormValue("node_selection_present") == "1"

		clusterIdx := -1
		for i, existing := range localReg.LocalConfig.Clusters {
			if existing.ID == id {
				if queueName == "" {
					queueName = existing.QueueName
				}
				if queueName == "" {
					queueName = "queue-" + id
				}
				if !r.Form.Has("description") && description == "" {
					description = existing.Description
				}
				if !r.Form.Has("fallback_model") && fallbackModel == "" {
					fallbackModel = existing.FallbackModel
				}
				if !r.Form.Has("fallback_location") && fallbackLocation == "" {
					fallbackLocation = existing.FallbackLocation
				}
				if maxQueueAge == 0 {
					maxQueueAge = existing.MaxQueueAge
				}
				localReg.LocalConfig.Clusters[i] = config.LocalCluster{
					ID:               id,
					Name:             name,
					Description:      description,
					QueueName:        queueName,
					MaxQueueAge:      maxQueueAge,
					FallbackModel:    fallbackModel,
					FallbackLocation: fallbackLocation,
					Nodes:            existing.Nodes,
				}
				clusterIdx = i
				break
			}
		}
		if clusterIdx == -1 {
			if queueName == "" {
				queueName = "queue-" + id
			}
			if maxQueueAge == 0 {
				maxQueueAge = 90
			}
			cl := config.LocalCluster{
				ID:               id,
				Name:             name,
				Description:      description,
				QueueName:        queueName,
				MaxQueueAge:      maxQueueAge,
				FallbackModel:    fallbackModel,
				FallbackLocation: fallbackLocation,
			}
			clusterIdx = len(localReg.LocalConfig.Clusters)
			localReg.LocalConfig.Clusters = append(localReg.LocalConfig.Clusters, cl)
		}

		if len(selectedNodeIDs) > 0 || nodeSelectionPresent {
			selectedSet := make(map[string]bool, len(selectedNodeIDs))
			for _, nid := range selectedNodeIDs {
				trimmed := strings.TrimSpace(nid)
				if trimmed != "" {
					selectedSet[trimmed] = true
				}
			}
			attachedNodes := make(map[string]config.Node)
			if runners, runErr := dc.Store.GetRegisteredRunners(ctx); runErr == nil {
				for _, runner := range runners {
					hasPool := false
					for _, cid := range runner.AssignedClusters {
						if cid == id {
							hasPool = true
							break
						}
					}
					if selectedSet[runner.Node.ID] {
						attachedNodes[runner.Node.ID] = runner.Node
						if !hasPool {
							runner.AssignedClusters = append(runner.AssignedClusters, id)
							modifiedRunners = append(modifiedRunners, runner)
						}
					} else if nodeSelectionPresent && hasPool {
						filtered := make([]string, 0, len(runner.AssignedClusters))
						for _, cid := range runner.AssignedClusters {
							if cid != id {
								filtered = append(filtered, cid)
							}
						}
						runner.AssignedClusters = filtered
						modifiedRunners = append(modifiedRunners, runner)
					}
				}
			}
			localReg.LocalConfig.Clusters[clusterIdx].Nodes = attachedNodes
		}
	}

	localProv.Regions["local-cluster"] = localReg

	if err := dc.Store.SaveProvider(ctx, localProv); err != nil {
		log.Printf("[Dashboard] Error saving provider cluster/endpoint: %v", err)
		http.Error(w, "Failed to save provider configuration", http.StatusInternalServerError)
		return
	}
	for _, runner := range modifiedRunners {
		_ = dc.Store.SaveRegisteredRunner(ctx, runner)
		_ = dc.Store.AttachRunnerToClusters(ctx, runner.Node.ID, runner.AssignedClusters)
	}
	decision.SyncDecisionProvidersFromLocalConfig(localProv)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/clusters")
		dc.ServeClusters(w, r)
		return
	}
	http.Redirect(w, r, "/admin/clusters", http.StatusSeeOther)
}

// DeleteProviderCluster removes a local cluster or direct endpoint from the "local" provider configuration.
func (dc *DashboardController) DeleteProviderCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	entryType := strings.TrimSpace(r.URL.Query().Get("type"))
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "Missing cluster or endpoint id", http.StatusBadRequest)
		return
	}

	providers, err := dc.Store.GetAllProviders(ctx)
	if err != nil {
		log.Printf("[Dashboard] Error loading providers for deletion: %v", err)
		http.Error(w, "Failed to load providers", http.StatusInternalServerError)
		return
	}

	for _, p := range providers {
		if p.ID == config.ProviderLocal {
			localProv := p
			for regKey, reg := range localProv.Regions {
				if reg.LocalConfig == nil {
					continue
				}
				if entryType == "endpoint" || entryType == "direct-http" {
					var remaining []config.DirectEndpoint
					for _, ep := range reg.LocalConfig.DirectEndpoints {
						if ep.ID != id {
							remaining = append(remaining, ep)
						}
					}
					reg.LocalConfig.DirectEndpoints = remaining
				} else {
					var remaining []config.LocalCluster
					for _, cl := range reg.LocalConfig.Clusters {
						if cl.ID != id {
							remaining = append(remaining, cl)
						}
					}
					reg.LocalConfig.Clusters = remaining
				}
				localProv.Regions[regKey] = reg
			}
			if err := dc.Store.SaveProvider(ctx, localProv); err != nil {
				log.Printf("[Dashboard] Error deleting provider entry: %v", err)
				http.Error(w, "Failed to update provider configuration", http.StatusInternalServerError)
				return
			}
			break
		}
	}

	// If a pull-queue cluster was deleted, scrub its ID from any registered runner's AssignedClusters
	if entryType != "endpoint" && entryType != "direct-http" {
		if runners, runErr := dc.Store.GetRegisteredRunners(ctx); runErr == nil {
			for _, runner := range runners {
				filtered := []string{}
				changed := false
				for _, cid := range runner.AssignedClusters {
					if cid == id {
						changed = true
					} else {
						filtered = append(filtered, cid)
					}
				}
				if changed {
					runner.AssignedClusters = filtered
					_ = dc.Store.SaveRegisteredRunner(ctx, runner)
					_ = dc.Store.AttachRunnerToClusters(ctx, runner.Node.ID, filtered)
				}
			}
		}
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// SaveClusterMappings is an alias for AttachRunnerToClusters supporting /admin/clusters/mappings/save.
func (dc *DashboardController) SaveClusterMappings(w http.ResponseWriter, r *http.Request) {
	dc.AttachRunnerToClusters(w, r)
}

// SaveClusterNode handles pre-registering a new cluster node or updating an existing runner node and its cluster assignments.
func (dc *DashboardController) SaveClusterNode(w http.ResponseWriter, r *http.Request) {
	dc.AttachRunnerToClusters(w, r)
}

// AttachRunnerToClusters handles form submission mapping runner to selected clusters and creating or updating node capacity limits.
func (dc *DashboardController) AttachRunnerToClusters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form payload", http.StatusBadRequest)
		return
	}

	nodeID := strings.TrimSpace(r.FormValue("node_id"))
	clusterIDs := r.Form["cluster_ids"]
	if clusterIDs == nil {
		clusterIDs = []string{}
	}

	if nodeID == "" {
		http.Error(w, "Missing node ID", http.StatusBadRequest)
		return
	}

	runners, _ := dc.Store.GetRegisteredRunners(ctx)
	var targetRunner *config.RunnerResponse
	for _, runner := range runners {
		if runner.Node.ID == nodeID {
			rCopy := runner
			targetRunner = &rCopy
			break
		}
	}

	if targetRunner == nil {
		nodeName := strings.TrimSpace(r.FormValue("name"))
		if nodeName == "" {
			nodeName = nodeID
		}
		targetRunner = &config.RunnerResponse{
			Node: config.Node{
				ID:                nodeID,
				Name:              nodeName,
				Status:            "pre-registered",
				MemoryAllocatedGB: 64,
				ComputeGPUCores:   60,
				MaxModelSizeGB:    32,
				MaxConcurrent:     2,
			},
			AssignedClusters: clusterIDs,
		}
	} else {
		targetRunner.AssignedClusters = clusterIDs
		if nodeName := strings.TrimSpace(r.FormValue("name")); nodeName != "" {
			targetRunner.Node.Name = nodeName
		}
	}

	if memStr := strings.TrimSpace(r.FormValue("memory_allocated_gb")); memStr != "" {
		if val, parseErr := strconv.Atoi(memStr); parseErr == nil && val > 0 {
			targetRunner.Node.MemoryAllocatedGB = val
		}
	}
	if gpuStr := strings.TrimSpace(r.FormValue("compute_gpu_cores")); gpuStr != "" {
		if val, parseErr := strconv.Atoi(gpuStr); parseErr == nil && val >= 0 {
			targetRunner.Node.ComputeGPUCores = val
		}
	}
	if maxSizeStr := strings.TrimSpace(r.FormValue("max_model_size_gb")); maxSizeStr != "" {
		if val, parseErr := strconv.Atoi(maxSizeStr); parseErr == nil && val >= 0 {
			targetRunner.Node.MaxModelSizeGB = val
		}
	}
	if maxConcStr := strings.TrimSpace(r.FormValue("max_concurrent")); maxConcStr != "" {
		if val, parseErr := strconv.Atoi(maxConcStr); parseErr == nil && val >= 0 {
			targetRunner.Node.MaxConcurrent = val
		}
	}
	if r.Form.Has("supported_models") {
		rawModels := strings.Split(r.FormValue("supported_models"), ",")
		var parsedModels []string
		for _, m := range rawModels {
			trimmed := strings.TrimSpace(m)
			if trimmed != "" {
				parsedModels = append(parsedModels, trimmed)
			}
		}
		targetRunner.Node.SupportedModels = parsedModels
	}

	if err := dc.Store.SaveRegisteredRunner(ctx, *targetRunner); err != nil {
		log.Printf("[Dashboard] Error saving registered runner: %v", err)
		http.Error(w, "Failed to save runner node", http.StatusInternalServerError)
		return
	}

	if err := dc.Store.AttachRunnerToClusters(ctx, nodeID, clusterIDs); err != nil {
		log.Printf("[Dashboard] Error saving attachments: %v", err)
	}

	// Redirect back to full clusters view
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin/clusters")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/admin/clusters", http.StatusSeeOther)
}

// DeRegisterClusterNode removes a host from active clusters
func (dc *DashboardController) DeRegisterClusterNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing runner id", http.StatusBadRequest)
		return
	}

	_ = dc.Store.DeregisterRunner(ctx, id)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(""))
}
