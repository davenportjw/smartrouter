package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

// Client represents a router consumer client.
type Client struct {
	ID       string `firestore:"id" json:"id"`
	Name     string `firestore:"name" json:"name"`
	Tier     string `firestore:"tier" json:"tier"` // "free", "standard", "premium"
	RPM      int    `firestore:"rpm" json:"rpm"`
	TPM      int    `firestore:"tpm" json:"tpm"`
	Priority string `firestore:"priority" json:"priority"` // "high", "medium", "low"
}

// ComplexityRouting defines configurations and thresholds for dynamic routing.
type ComplexityRouting struct {
	Enabled                bool   `firestore:"enabled" json:"enabled"`
	AlwaysOverride         bool   `firestore:"always_override" json:"always_override"`
	SimpleModel            string `firestore:"simple_model" json:"simple_model"`
	MediumModel            string `firestore:"medium_model" json:"medium_model"`
	ComplexModel           string `firestore:"complex_model" json:"complex_model"`
	SimpleCharLimit        int    `firestore:"simple_char_limit" json:"simple_char_limit"`
	MediumCharLimit        int    `firestore:"medium_char_limit" json:"medium_char_limit"`
	ForceComplexMultimodal bool   `firestore:"force_complex_multimodal" json:"force_complex_multimodal"`
	ForceComplexTools      bool   `firestore:"force_complex_tools" json:"force_complex_tools"`
	UseLLMClassifier       bool   `firestore:"use_llm_classifier" json:"use_llm_classifier"`
	ClassifierModel        string `firestore:"classifier_model" json:"classifier_model"`
}

// App represents an explicit application belonging to a client.
type App struct {
	ID         string            `firestore:"id" json:"id"`
	ClientID   string            `firestore:"client_id" json:"client_id"`
	Name       string            `firestore:"name" json:"name"`
	RPM        int               `firestore:"rpm" json:"rpm"`
	TPM        int               `firestore:"tpm" json:"tpm"`
	Priority   string            `firestore:"priority" json:"priority"` // "high", "medium", "low"
	Complexity ComplexityRouting `firestore:"complexity" json:"complexity"`
}

// APIKey represents an authorized router API key mapping to a client and app.
type APIKey struct {
	KeyHash  string `firestore:"key_hash" json:"key_hash"`
	ClientID string `firestore:"client_id" json:"client_id"` // keep for backwards-compatibility
	AppID    string `firestore:"app_id" json:"app_id"`
	Status   string `firestore:"status" json:"status"` // "active", "revoked"
}

// RoutingRule defines dynamic model rewrites and targets.
type RoutingRule struct {
	ID             string `firestore:"id" json:"id"`
	AppID          string `firestore:"app_id" json:"app_id"`               // bound app boundary
	ModelPattern   string `firestore:"model_pattern" json:"model_pattern"` // regex or exact match
	ClientTier     string `firestore:"client_tier" json:"client_tier"`     // "all" or specific tier
	HeaderName     string `firestore:"header_name" json:"header_name"`     // optional header filter
	HeaderValue    string `firestore:"header_value" json:"header_value"`   // optional value pattern to match
	TargetModel    string `firestore:"target_model" json:"target_model"`
	TargetLocation string `firestore:"target_location" json:"target_location"` // E.g. "us-central1" for Vertex AI
	FallbackModel  string `firestore:"fallback_model" json:"fallback_model"`
	PriorityWeight int    `firestore:"priority_weight" json:"priority_weight"`
}

// CustomHeader defines client-provided custom header verification rules.
type CustomHeader struct {
	ID           string `firestore:"id" json:"id"`
	AppID        string `firestore:"app_id" json:"app_id"`                 // bound app boundary
	Name         string `firestore:"name" json:"name"`                     // e.g. "X-Client-App-ID"
	Description  string `firestore:"description" json:"description"`       // e.g. "Identifies the calling application"
	Required     bool   `firestore:"required" json:"required"`             // Whether the header is mandatory
	Validation   string `firestore:"validation" json:"validation"`         // "non-empty", "regex", "enum"
	ValuePattern string `firestore:"value_pattern" json:"value_pattern"`   // Regex format or comma-separated enum values
}

// ConfigStore manages Firestore connections and a fast in-memory configuration cache.
type ConfigStore struct {
	Client      *firestore.Client
	isLocalDev  bool
	localDBPath string

	// Thread-safe in-memory cache
	mu      sync.RWMutex
	keys    map[string]APIKey // Hex-encoded KeyHash -> APIKey
	clients map[string]Client // ClientID -> Client
	apps    map[string]App    // AppID -> App
	rules   []RoutingRule
	headers []CustomHeader

	// Precompiled regex caches
	ruleRegexes   map[string]*regexp.Regexp
	headerRegexes map[string]*regexp.Regexp
}

// LocalDB represents the JSON schema for the local development database.
type LocalDB struct {
	Clients       map[string]Client `json:"clients"`
	Apps          map[string]App    `json:"apps"`
	APIKeys       map[string]APIKey `json:"api_keys"`
	RoutingRules  []RoutingRule     `json:"routing_rules"`
	CustomHeaders []CustomHeader    `json:"custom_headers"`
}

// NewConfigStore initializes a Firestore connection and caches configuration, or uses local file for dev.
func NewConfigStore(ctx context.Context, projectID string) (*ConfigStore, error) {
	isLocalDev := os.Getenv("LOCAL_DEV") == "true"

	store := &ConfigStore{
		isLocalDev:    isLocalDev,
		keys:          make(map[string]APIKey),
		clients:       make(map[string]Client),
		apps:          make(map[string]App),
		ruleRegexes:   make(map[string]*regexp.Regexp),
		headerRegexes: make(map[string]*regexp.Regexp),
	}

	if isLocalDev {
		if err := store.initLocalDB(); err != nil {
			return nil, fmt.Errorf("failed to initialize local dev database: %w", err)
		}
		return store, nil
	}

	client, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create firestore client: %w", err)
	}
	store.Client = client

	return store, nil
}

// HashKey returns the hex-encoded SHA-256 hash of an API key.
func HashKey(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

// StartListeners starts real-time Firestore listener routines, or simulates them in local dev.
func (cs *ConfigStore) StartListeners(ctx context.Context) {
	if cs.isLocalDev {
		log.Println("[Local Dev Cache] Simulated real-time configuration synchronizer active.")
		return
	}

	go cs.listenKeys(ctx)
	go cs.listenClients(ctx)
	go cs.listenApps(ctx)
	go cs.listenRules(ctx)
	go cs.listenHeaders(ctx)
}

// LookupKey finds a matching active API key in the local cache.
func (cs *ConfigStore) LookupKey(apiKey string) (APIKey, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	hashed := HashKey(apiKey)
	key, ok := cs.keys[hashed]
	if ok && key.Status == "active" {
		return key, true
	}
	return APIKey{}, false
}

// LookupClient gets client details from the local cache by ID.
func (cs *ConfigStore) LookupClient(clientID string) (Client, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	client, ok := cs.clients[clientID]
	return client, ok
}

// LookupApp gets app details from the local cache by ID.
func (cs *ConfigStore) LookupApp(appID string) (App, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	app, ok := cs.apps[appID]
	return app, ok
}

// sortRulesLocked sorts the cached routing rules slice descending by PriorityWeight.
// The caller MUST hold the write lock cs.mu.
func (cs *ConfigStore) sortRulesLocked() {
	sort.Slice(cs.rules, func(i, j int) bool {
		if cs.rules[i].PriorityWeight == cs.rules[j].PriorityWeight {
			return cs.rules[i].ID < cs.rules[j].ID
		}
		return cs.rules[i].PriorityWeight > cs.rules[j].PriorityWeight
	})
}

// compileRegexesLocked compiles routing rule regexes and custom header regexes in the cached memory store.
// The caller MUST hold the write lock cs.mu.
func (cs *ConfigStore) compileRegexesLocked() {
	cs.ruleRegexes = make(map[string]*regexp.Regexp)
	for _, rule := range cs.rules {
		if rule.HeaderName != "" && rule.HeaderValue != "" {
			if strings.HasPrefix(rule.HeaderValue, "/") && strings.HasSuffix(rule.HeaderValue, "/") {
				pattern := rule.HeaderValue[1 : len(rule.HeaderValue)-1]
				re, err := regexp.Compile(pattern)
				if err != nil {
					log.Printf("[ConfigStore] Invalid regex pattern in rule %s: %v", rule.ID, err)
					continue
				}
				cs.ruleRegexes[rule.ID] = re
			}
		}
	}

	cs.headerRegexes = make(map[string]*regexp.Regexp)
	for _, h := range cs.headers {
		if h.Validation == "regex" && h.ValuePattern != "" {
			re, err := regexp.Compile(h.ValuePattern)
			if err != nil {
				log.Printf("[ConfigStore] Invalid regex pattern in custom header %s: %v", h.ID, err)
				continue
			}
			cs.headerRegexes[h.ID] = re
		}
	}
}

// MatchHeaderRegex verifies a custom header's value using the precompiled regex cache.
func (cs *ConfigStore) MatchHeaderRegex(headerID, val string) bool {
	cs.mu.RLock()
	re, exists := cs.headerRegexes[headerID]
	cs.mu.RUnlock()

	if exists {
		return re.MatchString(val)
	}

	// Fallback if not precompiled
	cs.mu.RLock()
	var pattern string
	for _, h := range cs.headers {
		if h.ID == headerID {
			pattern = h.ValuePattern
			break
		}
	}
	cs.mu.RUnlock()

	if pattern == "" {
		return false
	}

	reCompiled, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return reCompiled.MatchString(val)
}

// MatchRule evaluates rules to find the best matching target for model, tier, and specific app ID.
func (cs *ConfigStore) MatchRule(modelName, clientTier, appID string, headers map[string]string) (RoutingRule, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	for _, rule := range cs.rules {
		modelMatch := rule.ModelPattern == "*" || rule.ModelPattern == modelName
		tierMatch := rule.ClientTier == "all" || rule.ClientTier == clientTier
		appMatch := rule.AppID == "" || rule.AppID == "all" || rule.AppID == appID

		headerMatch := true
		if rule.HeaderName != "" {
			val, exists := headers[rule.HeaderName]
			if !exists {
				headerMatch = false
			} else if rule.HeaderValue != "" {
				if strings.HasPrefix(rule.HeaderValue, "/") && strings.HasSuffix(rule.HeaderValue, "/") {
					// Try from precompiled regex cache
					if re, cached := cs.ruleRegexes[rule.ID]; cached {
						headerMatch = re.MatchString(val)
					} else {
						pattern := rule.HeaderValue[1 : len(rule.HeaderValue)-1]
						matched, err := regexp.MatchString(pattern, val)
						headerMatch = (err == nil && matched)
					}
				} else {
					// Exact matching
					headerMatch = (val == rule.HeaderValue)
				}
			}
		}

		if modelMatch && tierMatch && appMatch && headerMatch {
			return rule, true
		}
	}
	return RoutingRule{}, false
}

// Real-time listeners using Firestore Snapshots

func (cs *ConfigStore) listenKeys(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("api_keys").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				backoff = 1 * time.Second // Reset backoff on success

				cs.mu.Lock()
				cs.keys = make(map[string]APIKey)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						log.Printf("[Firestore] Error reading key document snapshot: %v", err)
						continue
					}
					var key APIKey
					if err := doc.DataTo(&key); err != nil {
						log.Printf("[Firestore] DataTo error mapping APIKey: %v", err)
						continue
					}
					cs.keys[key.KeyHash] = key
				}
				cs.mu.Unlock()
				log.Printf("[Firestore Cache] Synchronized %d API keys", len(cs.keys))
			}
		}()

		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("[Firestore] Keys listener error: %v. Reconnecting in %v...", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

func (cs *ConfigStore) listenClients(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("clients").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				backoff = 1 * time.Second // Reset backoff on success

				cs.mu.Lock()
				cs.clients = make(map[string]Client)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						log.Printf("[Firestore] Error reading client document snapshot: %v", err)
						continue
					}
					var client Client
					if err := doc.DataTo(&client); err != nil {
						log.Printf("[Firestore] DataTo error mapping Client: %v", err)
						continue
					}
					cs.clients[client.ID] = client
				}
				cs.mu.Unlock()
				log.Printf("[Firestore Cache] Synchronized %d clients", len(cs.clients))
			}
		}()

		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("[Firestore] Clients listener error: %v. Reconnecting in %v...", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

func (cs *ConfigStore) listenRules(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("routing_rules").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				backoff = 1 * time.Second // Reset backoff on success

				cs.mu.Lock()
				cs.rules = nil
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						log.Printf("[Firestore] Error reading rule document snapshot: %v", err)
						continue
					}
					var rule RoutingRule
					if err := doc.DataTo(&rule); err != nil {
						log.Printf("[Firestore] DataTo error mapping RoutingRule: %v", err)
						continue
					}
					cs.rules = append(cs.rules, rule)
				}
				cs.sortRulesLocked()
				cs.compileRegexesLocked()
				cs.mu.Unlock()
				log.Printf("[Firestore Cache] Synchronized %d routing rules", len(cs.rules))
			}
		}()

		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("[Firestore] Rules listener error: %v. Reconnecting in %v...", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

func (cs *ConfigStore) listenApps(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("apps").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				backoff = 1 * time.Second // Reset backoff on success

				cs.mu.Lock()
				cs.apps = make(map[string]App)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						log.Printf("[Firestore] Error reading app document snapshot: %v", err)
						continue
					}
					var app App
					if err := doc.DataTo(&app); err != nil {
						log.Printf("[Firestore] DataTo error mapping App: %v", err)
						continue
					}
					cs.apps[app.ID] = app
				}
				cs.mu.Unlock()
				log.Printf("[Firestore Cache] Synchronized %d apps", len(cs.apps))
			}
		}()

		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("[Firestore] Apps listener error: %v. Reconnecting in %v...", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

// initLocalDB prepares local JSON db directory, loads data, and seeds if file does not exist.
func (cs *ConfigStore) initLocalDB() error {
	cs.localDBPath = "data/local_db.json"

	// Ensure data/ directory exists
	if err := os.MkdirAll("data", 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Check if local db file exists
	if _, err := os.Stat(cs.localDBPath); os.IsNotExist(err) {
		log.Printf("[Local Dev] Database file %s not found. Creating pre-seeded development database...", cs.localDBPath)

		// Seed default values
		devRule := RoutingRule{
			ID:             "rule-1",
			ModelPattern:   "*",
			ClientTier:     "all",
			TargetModel:    "gemini-2.5-flash",
			TargetLocation: "us-central1",
			FallbackModel:  "gemini-2.5-pro",
			PriorityWeight: 1,
		}

		db := LocalDB{
			Clients:      make(map[string]Client),
			Apps:         make(map[string]App),
			APIKeys:      make(map[string]APIKey),
			RoutingRules: []RoutingRule{devRule},
			CustomHeaders: []CustomHeader{
				{
					ID:           "header-1",
					Name:         "X-Client-App-ID",
					Description:  "Identifies the calling client application",
					Required:     true,
					Validation:   "regex",
					ValuePattern: "^[a-zA-Z0-9-]+$",
				},
			},
		}

		data, err := json.MarshalIndent(db, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal seeded db: %w", err)
		}

		if err := os.WriteFile(cs.localDBPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write seeded db: %w", err)
		}

		log.Println("[Local Dev] Seeded successfully.")
		log.Println("[Local Dev] ------------------------------------------------------------")
		log.Println("[Local Dev] Pre-seeded Router Rule: All requests (*) -> gemini-2.5-flash")
		log.Println("[Local Dev] Pre-seeded Custom Header: X-Client-App-ID (regex validation)")
		log.Println("[Local Dev] Please generate API Credentials via dashboard: http://localhost:8080/admin/keys")
		log.Println("[Local Dev] ------------------------------------------------------------")
	}

	// Read local db file
	data, err := os.ReadFile(cs.localDBPath)
	if err != nil {
		return fmt.Errorf("failed to read database file: %w", err)
	}

	var db LocalDB
	if err := json.Unmarshal(data, &db); err != nil {
		return fmt.Errorf("failed to unmarshal database JSON: %w", err)
	}

	if db.Apps == nil {
		db.Apps = make(map[string]App)
	}

	// Auto-migrate existing clients and keys to Apps
	dirty := false
	for keyHash, key := range db.APIKeys {
		if key.AppID == "" {
			defaultAppID := "app-" + key.ClientID
			key.AppID = defaultAppID
			db.APIKeys[keyHash] = key
			dirty = true

			if _, ok := db.Apps[defaultAppID]; !ok {
				cName := "Default Application"
				cRPM, cTPM, cPriority := 60, 40000, "medium"
				if client, exists := db.Clients[key.ClientID]; exists {
					cRPM = client.RPM
					cTPM = client.TPM
					cPriority = client.Priority
					cName = client.Name + " App"
				}
				db.Apps[defaultAppID] = App{
					ID:       defaultAppID,
					ClientID: key.ClientID,
					Name:     cName,
					RPM:      cRPM,
					TPM:      cTPM,
					Priority: cPriority,
					Complexity: ComplexityRouting{
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
					},
				}
			}
		}
	}

	// Migrate existing apps that don't have Complexity structure populated
	for id, app := range db.Apps {
		if app.Complexity.SimpleModel == "" {
			app.Complexity = ComplexityRouting{
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
			db.Apps[id] = app
			dirty = true
		}
	}

	if dirty {
		log.Println("[Local Dev] Migrating existing database schemas to explicit App-Centric model...")
		mdata, err := json.MarshalIndent(db, "", "  ")
		if err == nil {
			_ = os.WriteFile(cs.localDBPath, mdata, 0644)
		}
	}

	cs.mu.Lock()
	cs.clients = db.Clients
	cs.apps = db.Apps
	cs.keys = db.APIKeys
	cs.rules = db.RoutingRules
	cs.headers = db.CustomHeaders
	cs.sortRulesLocked()
	cs.compileRegexesLocked()
	cs.mu.Unlock()

	log.Printf("[Local Dev Cache] Loaded %d clients, %d apps, %d API keys, %d rules, %d headers.", len(cs.clients), len(cs.apps), len(cs.keys), len(cs.rules), len(cs.headers))
	return nil
}

// saveLocalDB flushes memory cache changes to local_db.json.
func (cs *ConfigStore) saveLocalDB() error {
	cs.mu.RLock()
	db := LocalDB{
		Clients:       cs.clients,
		Apps:          cs.apps,
		APIKeys:       cs.keys,
		RoutingRules:  cs.rules,
		CustomHeaders: cs.headers,
	}
	cs.mu.RUnlock()

	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal local db: %w", err)
	}

	if err := os.WriteFile(cs.localDBPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write local db: %w", err)
	}

	return nil
}

// GetAllKeys retrieves all API keys from the local file or active Firestore.
func (cs *ConfigStore) GetAllKeys(ctx context.Context) ([]APIKey, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []APIKey
		for _, k := range cs.keys {
			list = append(list, k)
		}
		return list, nil
	}

	keyDocs, err := cs.Client.Collection("api_keys").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []APIKey
	for _, doc := range keyDocs {
		var key APIKey
		if err := doc.DataTo(&key); err == nil {
			list = append(list, key)
		}
	}
	return list, nil
}

// GetAllClients retrieves all clients from the local file or active Firestore.
func (cs *ConfigStore) GetAllClients(ctx context.Context) ([]Client, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []Client
		for _, c := range cs.clients {
			list = append(list, c)
		}
		return list, nil
	}

	clientDocs, err := cs.Client.Collection("clients").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []Client
	for _, doc := range clientDocs {
		var client Client
		if err := doc.DataTo(&client); err == nil {
			list = append(list, client)
		}
	}
	return list, nil
}

// GetAllRules retrieves all dynamic rules from the local file or active Firestore.
func (cs *ConfigStore) GetAllRules(ctx context.Context) ([]RoutingRule, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		return cs.rules, nil
	}

	ruleDocs, err := cs.Client.Collection("routing_rules").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []RoutingRule
	for _, doc := range ruleDocs {
		var rule RoutingRule
		if err := doc.DataTo(&rule); err == nil {
			list = append(list, rule)
		}
	}
	return list, nil
}

// SaveClient persists a client's details locally or to live Firestore.
func (cs *ConfigStore) SaveClient(ctx context.Context, client Client) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		cs.clients[client.ID] = client
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("clients").Doc(client.ID).Set(ctx, client)
	return err
}

// SaveKey persists an API Key mapping locally or to live Firestore.
func (cs *ConfigStore) SaveKey(ctx context.Context, key APIKey) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if key.AppID == "" {
		key.AppID = "app-" + key.ClientID
	}

	if isDev {
		cs.mu.Lock()
		cs.keys[key.KeyHash] = key
		if _, exists := cs.apps[key.AppID]; !exists {
			cName := "Default Application"
			cRPM, cTPM, cPriority := 60, 40000, "medium"
			if client, existsClient := cs.clients[key.ClientID]; existsClient {
				cRPM = client.RPM
				cTPM = client.TPM
				cPriority = client.Priority
				cName = client.Name + " App"
			}
			cs.apps[key.AppID] = App{
				ID:       key.AppID,
				ClientID: key.ClientID,
				Name:     cName,
				RPM:      cRPM,
				TPM:      cTPM,
				Priority: cPriority,
				Complexity: ComplexityRouting{
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
				},
			}
		}
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("api_keys").Doc(key.KeyHash).Set(ctx, key)
	return err
}

// RevokeKey updates the status of an API Key to revoked locally or in Firestore.
func (cs *ConfigStore) RevokeKey(ctx context.Context, hash string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		key, ok := cs.keys[hash]
		if ok {
			key.Status = "revoked"
			cs.keys[hash] = key
		}
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("api_keys").Doc(hash).Update(ctx, []firestore.Update{
		{Path: "status", Value: "revoked"},
	})
	return err
}

// GetHeaders returns all cached custom headers.
func (cs *ConfigStore) GetHeaders() []CustomHeader {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	h := make([]CustomHeader, len(cs.headers))
	copy(h, cs.headers)
	return h
}

// GetAllHeaders returns custom headers from the local cache or Firestore.
func (cs *ConfigStore) GetAllHeaders(ctx context.Context) ([]CustomHeader, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		return cs.GetHeaders(), nil
	}

	headerDocs, err := cs.Client.Collection("custom_headers").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []CustomHeader
	for _, doc := range headerDocs {
		var h CustomHeader
		if err := doc.DataTo(&h); err == nil {
			list = append(list, h)
		}
	}
	return list, nil
}

// SaveHeader persists a custom header rule.
func (cs *ConfigStore) SaveHeader(ctx context.Context, h CustomHeader) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		found := false
		for idx, existing := range cs.headers {
			if existing.ID == h.ID {
				cs.headers[idx] = h
				found = true
				break
			}
		}
		if !found {
			cs.headers = append(cs.headers, h)
		}
		cs.compileRegexesLocked()
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("custom_headers").Doc(h.ID).Set(ctx, h)
	return err
}

// DeleteHeader deletes a custom header rule by ID.
func (cs *ConfigStore) DeleteHeader(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		var updated []CustomHeader
		for _, h := range cs.headers {
			if h.ID != id {
				updated = append(updated, h)
			}
		}
		cs.headers = updated
		cs.compileRegexesLocked()
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("custom_headers").Doc(id).Delete(ctx)
	return err
}

// SaveRule persists a routing rule.
func (cs *ConfigStore) SaveRule(ctx context.Context, rule RoutingRule) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		found := false
		for idx, existing := range cs.rules {
			if existing.ID == rule.ID {
				cs.rules[idx] = rule
				found = true
				break
			}
		}
		if !found {
			cs.rules = append(cs.rules, rule)
		}
		cs.sortRulesLocked()
		cs.compileRegexesLocked()
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("routing_rules").Doc(rule.ID).Set(ctx, rule)
	return err
}

// DeleteRule deletes a routing rule by ID.
func (cs *ConfigStore) DeleteRule(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		var updated []RoutingRule
		for _, r := range cs.rules {
			if r.ID != id {
				updated = append(updated, r)
			}
		}
		cs.rules = updated
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("routing_rules").Doc(id).Delete(ctx)
	return err
}

// listenHeaders streams live updates for custom headers from Firestore.
func (cs *ConfigStore) listenHeaders(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("custom_headers").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				backoff = 1 * time.Second // Reset backoff on success

				cs.mu.Lock()
				cs.headers = nil
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						log.Printf("[Firestore] Error reading header document snapshot: %v", err)
						continue
					}
					var h CustomHeader
					if err := doc.DataTo(&h); err != nil {
						log.Printf("[Firestore] DataTo error mapping CustomHeader: %v", err)
						continue
					}
					cs.headers = append(cs.headers, h)
				}
				cs.compileRegexesLocked()
				cs.mu.Unlock()
				log.Printf("[Firestore Cache] Synchronized %d custom headers", len(cs.headers))
			}
		}()

		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("[Firestore] Headers listener error: %v. Reconnecting in %v...", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

// GetAllApps retrieves all apps from local cache or Firestore.
func (cs *ConfigStore) GetAllApps(ctx context.Context) ([]App, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []App
		for _, a := range cs.apps {
			list = append(list, a)
		}
		return list, nil
	}

	appDocs, err := cs.Client.Collection("apps").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []App
	for _, doc := range appDocs {
		var app App
		if err := doc.DataTo(&app); err == nil {
			list = append(list, app)
		}
	}
	return list, nil
}

// SaveApp persists an application's details.
func (cs *ConfigStore) SaveApp(ctx context.Context, app App) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		cs.apps[app.ID] = app
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("apps").Doc(app.ID).Set(ctx, app)
	return err
}

// DeleteApp deletes an application by ID.
func (cs *ConfigStore) DeleteApp(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		delete(cs.apps, id)
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("apps").Doc(id).Delete(ctx)
	return err
}
