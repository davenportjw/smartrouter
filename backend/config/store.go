package store

import (
	"context"
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

	"geminirouter/pkg/config"
)

// ConfigStore manages Firestore connections and a fast in-memory configuration cache.
type ConfigStore struct {
	Client      *firestore.Client
	isLocalDev  bool
	localDBPath string

	// Thread-safe in-memory cache
	mu      sync.RWMutex
	keys    map[string]config.APIKey      // Hex-encoded KeyHash -> APIKey
	clients map[string]config.Client      // ClientID -> Client
	apps    map[string]config.App         // AppID -> App
	models  map[string]config.ModelConfig // Model ID -> ModelConfig
	rules   []config.RoutingRule
	headers []config.CustomHeader

	// Precompiled regex caches
	ruleRegexes   map[string]*regexp.Regexp
	headerRegexes map[string]*regexp.Regexp
}

// NewConfigStore initializes a Firestore connection and caches configuration, or uses local file for dev.
func NewConfigStore(ctx context.Context, projectID string) (*ConfigStore, error) {
	isLocalDev := os.Getenv("LOCAL_DEV") == "true"

	cs := &ConfigStore{
		isLocalDev:    isLocalDev,
		keys:          make(map[string]config.APIKey),
		clients:       make(map[string]config.Client),
		apps:          make(map[string]config.App),
		models:        make(map[string]config.ModelConfig),
		ruleRegexes:   make(map[string]*regexp.Regexp),
		headerRegexes: make(map[string]*regexp.Regexp),
	}

	if isLocalDev {
		if err := cs.initLocalDB(); err != nil {
			return nil, fmt.Errorf("failed to initialize local dev database: %w", err)
		}
		return cs, nil
	}

	client, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create firestore client: %w", err)
	}
	cs.Client = client

	return cs, nil
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
	go cs.listenModels(ctx)
}

// Cache lookups and calculations

func (cs *ConfigStore) LookupKey(apiKey string) (config.APIKey, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	hashed := config.HashKey(apiKey)
	key, ok := cs.keys[hashed]
	if ok && key.Status == "active" {
		return key, true
	}
	return config.APIKey{}, false
}

func (cs *ConfigStore) LookupClient(clientID string) (config.Client, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	client, ok := cs.clients[clientID]
	return client, ok
}

func (cs *ConfigStore) LookupApp(appID string) (config.App, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	app, ok := cs.apps[appID]
	return app, ok
}

func (cs *ConfigStore) LookupActiveModel(modelID string, routerLoc string) (config.ModelConfig, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if routerLoc == "" {
		routerLoc = "us-central1"
	}

	checkActive := func(key string) (config.ModelConfig, bool) {
		if m, ok := cs.models[key]; ok && m.Active {
			return m, true
		}
		return config.ModelConfig{}, false
	}

	// 1. Check specific region
	if m, ok := checkActive(modelID + "@" + routerLoc); ok {
		return m, true
	}

	// 2. Check parent multi-region
	if parent := config.GetMultiRegionParent(routerLoc); parent != "" {
		if m, ok := checkActive(modelID + "@" + parent); ok {
			return m, true
		}
	}

	// 3. Check global
	if m, ok := checkActive(modelID + "@" + "global"); ok {
		return m, true
	}

	// 4. Check exact match (fallback for exact custom resource names or legacy keys)
	if m, ok := checkActive(modelID); ok {
		return m, true
	}

	return config.ModelConfig{}, false
}

func (cs *ConfigStore) sortRulesLocked() {
	sort.Slice(cs.rules, func(i, j int) bool {
		if cs.rules[i].PriorityWeight == cs.rules[j].PriorityWeight {
			return cs.rules[i].ID < cs.rules[j].ID
		}
		return cs.rules[i].PriorityWeight > cs.rules[j].PriorityWeight
	})
}

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

func (cs *ConfigStore) MatchHeaderRegex(headerID, val string) bool {
	cs.mu.RLock()
	re, exists := cs.headerRegexes[headerID]
	cs.mu.RUnlock()

	if exists {
		return re.MatchString(val)
	}

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

func (cs *ConfigStore) MatchRule(modelName, clientTier, appID string, headers map[string]string) (config.RoutingRule, bool) {
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
					if re, cached := cs.ruleRegexes[rule.ID]; cached {
						headerMatch = re.MatchString(val)
					} else {
						pattern := rule.HeaderValue[1 : len(rule.HeaderValue)-1]
						matched, err := regexp.MatchString(pattern, val)
						headerMatch = (err == nil && matched)
					}
				} else {
					headerMatch = (val == rule.HeaderValue)
				}
			}
		}

		if modelMatch && tierMatch && appMatch && headerMatch {
			return rule, true
		}
	}
	return config.RoutingRule{}, false
}

// Real-time Firestore listener routines

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
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.keys = make(map[string]config.APIKey)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var key config.APIKey
					if err := doc.DataTo(&key); err == nil {
						cs.keys[key.KeyHash] = key
					}
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
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.clients = make(map[string]config.Client)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var client config.Client
					if err := doc.DataTo(&client); err == nil {
						cs.clients[client.ID] = client
					}
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
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.rules = nil
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var rule config.RoutingRule
					if err := doc.DataTo(&rule); err == nil {
						cs.rules = append(cs.rules, rule)
					}
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
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.apps = make(map[string]config.App)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var app config.App
					if err := doc.DataTo(&app); err == nil {
						cs.apps[app.ID] = app
					}
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
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

func (cs *ConfigStore) listenModels(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("models").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.models = make(map[string]config.ModelConfig)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var m config.ModelConfig
					if err := doc.DataTo(&m); err == nil {
						cs.models[m.ID] = m
					}
				}
				cs.mu.Unlock()
				log.Printf("[Firestore Cache] Synchronized %d models", len(cs.models))
			}
		}()

		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

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
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.headers = nil
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var h config.CustomHeader
					if err := doc.DataTo(&h); err == nil {
						cs.headers = append(cs.headers, h)
					}
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
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 1*time.Minute {
				backoff = 1 * time.Minute
			}
		}
	}
}

// localJSON DB Logic

func (cs *ConfigStore) initLocalDB() error {
	cs.localDBPath = "data/local_db.json"

	if err := os.MkdirAll("data", 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	if _, err := os.Stat(cs.localDBPath); os.IsNotExist(err) {
		log.Printf("[Local Dev] Database file %s not found. Creating pre-seeded development database...", cs.localDBPath)

		devRule := config.RoutingRule{
			ID:             "rule-1",
			ModelPattern:   "*",
			ClientTier:     "all",
			TargetModel:    "gemini-2.5-flash",
			TargetLocation: "us-central1",
			FallbackModel:  "gemini-2.5-pro",
			PriorityWeight: 1,
		}

		db := config.LocalDB{
			Clients:      make(map[string]config.Client),
			Apps:         make(map[string]config.App),
			APIKeys:      make(map[string]config.APIKey),
			RoutingRules: []config.RoutingRule{devRule},
			CustomHeaders: []config.CustomHeader{
				{
					ID:           "header-1",
					Name:         "X-Client-App-ID",
					Description:  "Identifies the calling client application",
					Required:     true,
					Validation:   "regex",
					ValuePattern: "^[a-zA-Z0-9-]+$",
				},
			},
			Models: []config.ModelConfig{},
		}

		data, err := json.MarshalIndent(db, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal seeded db: %w", err)
		}

		if err := os.WriteFile(cs.localDBPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write seeded db: %w", err)
		}
	}

	data, err := os.ReadFile(cs.localDBPath)
	if err != nil {
		return fmt.Errorf("failed to read database file: %w", err)
	}

	var db config.LocalDB
	if err := json.Unmarshal(data, &db); err != nil {
		return fmt.Errorf("failed to unmarshal database JSON: %w", err)
	}

	if db.Apps == nil {
		db.Apps = make(map[string]config.App)
	}

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
				db.Apps[defaultAppID] = config.App{
					ID:       defaultAppID,
					ClientID: key.ClientID,
					Name:     cName,
					RPM:      cRPM,
					TPM:      cTPM,
					Priority: cPriority,
					Complexity: config.ComplexityRouting{
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

	for id, app := range db.Apps {
		if app.Complexity.SimpleModel == "" {
			app.Complexity = config.ComplexityRouting{
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
	if len(db.Models) == 0 {
		db.Models = []config.ModelConfig{}
		dirty = true
	}

	if dirty {
		log.Println("[Local Dev] Migrating database schemas...")
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
	cs.models = make(map[string]config.ModelConfig)
	for _, m := range db.Models {
		cs.models[m.ID] = m
	}
	cs.sortRulesLocked()
	cs.compileRegexesLocked()
	cs.mu.Unlock()

	return nil
}

func (cs *ConfigStore) saveLocalDB() error {
	cs.mu.RLock()
	var modelsList []config.ModelConfig
	for _, m := range cs.models {
		modelsList = append(modelsList, m)
	}
	db := config.LocalDB{
		Clients:       cs.clients,
		Apps:          cs.apps,
		APIKeys:       cs.keys,
		RoutingRules:  cs.rules,
		CustomHeaders: cs.headers,
		Models:        modelsList,
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

// CRUD Interface Implementations satisfying config.AdminStore

func (cs *ConfigStore) GetAllKeys(ctx context.Context) ([]config.APIKey, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []config.APIKey
		for _, k := range cs.keys {
			list = append(list, k)
		}
		return list, nil
	}

	keyDocs, err := cs.Client.Collection("api_keys").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []config.APIKey
	for _, doc := range keyDocs {
		var key config.APIKey
		if err := doc.DataTo(&key); err == nil {
			list = append(list, key)
		}
	}
	return list, nil
}

func (cs *ConfigStore) GetAllClients(ctx context.Context) ([]config.Client, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []config.Client
		for _, c := range cs.clients {
			list = append(list, c)
		}
		return list, nil
	}

	clientDocs, err := cs.Client.Collection("clients").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []config.Client
	for _, doc := range clientDocs {
		var client config.Client
		if err := doc.DataTo(&client); err == nil {
			list = append(list, client)
		}
	}
	return list, nil
}

func (cs *ConfigStore) GetAllRules(ctx context.Context) ([]config.RoutingRule, error) {
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
	var list []config.RoutingRule
	for _, doc := range ruleDocs {
		var rule config.RoutingRule
		if err := doc.DataTo(&rule); err == nil {
			list = append(list, rule)
		}
	}
	return list, nil
}

func (cs *ConfigStore) SaveClient(ctx context.Context, client config.Client) error {
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

func (cs *ConfigStore) SaveKey(ctx context.Context, key config.APIKey) error {
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
			cs.apps[key.AppID] = config.App{
				ID:       key.AppID,
				ClientID: key.ClientID,
				Name:     cName,
				RPM:      cRPM,
				TPM:      cTPM,
				Priority: cPriority,
				Complexity: config.ComplexityRouting{
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

func (cs *ConfigStore) RevokeKey(ctx context.Context, hash string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		if key, ok := cs.keys[hash]; ok {
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

func (cs *ConfigStore) SaveRule(ctx context.Context, rule config.RoutingRule) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		found := false
		for i, r := range cs.rules {
			if r.ID == rule.ID {
				cs.rules[i] = rule
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

func (cs *ConfigStore) DeleteRule(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		var newRules []config.RoutingRule
		for _, r := range cs.rules {
			if r.ID != id {
				newRules = append(newRules, r)
			}
		}
		cs.rules = newRules
		cs.compileRegexesLocked()
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("routing_rules").Doc(id).Delete(ctx)
	return err
}

func (cs *ConfigStore) GetAllHeaders(ctx context.Context) ([]config.CustomHeader, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		return cs.headers, nil
	}

	headerDocs, err := cs.Client.Collection("custom_headers").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []config.CustomHeader
	for _, doc := range headerDocs {
		var h config.CustomHeader
		if err := doc.DataTo(&h); err == nil {
			list = append(list, h)
		}
	}
	return list, nil
}

func (cs *ConfigStore) GetHeaders() []config.CustomHeader {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	h := make([]config.CustomHeader, len(cs.headers))
	copy(h, cs.headers)
	return h
}

func (cs *ConfigStore) SaveHeader(ctx context.Context, header config.CustomHeader) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		found := false
		for i, h := range cs.headers {
			if h.ID == header.ID {
				cs.headers[i] = header
				found = true
				break
			}
		}
		if !found {
			cs.headers = append(cs.headers, header)
		}
		cs.compileRegexesLocked()
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("custom_headers").Doc(header.ID).Set(ctx, header)
	return err
}

func (cs *ConfigStore) DeleteHeader(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		var newHeaders []config.CustomHeader
		for _, h := range cs.headers {
			if h.ID != id {
				newHeaders = append(newHeaders, h)
			}
		}
		cs.headers = newHeaders
		cs.compileRegexesLocked()
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("custom_headers").Doc(id).Delete(ctx)
	return err
}

func (cs *ConfigStore) GetAllModels(ctx context.Context) ([]config.ModelConfig, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []config.ModelConfig
		for _, m := range cs.models {
			list = append(list, m)
		}
		return list, nil
	}

	modelDocs, err := cs.Client.Collection("models").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []config.ModelConfig
	for _, doc := range modelDocs {
		var m config.ModelConfig
		if err := doc.DataTo(&m); err == nil {
			list = append(list, m)
		}
	}
	return list, nil
}

func (cs *ConfigStore) SaveModel(ctx context.Context, model config.ModelConfig) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		cs.models[model.ID] = model
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("models").Doc(model.ID).Set(ctx, model)
	return err
}

func (cs *ConfigStore) DeleteModel(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		delete(cs.models, id)
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("models").Doc(id).Delete(ctx)
	return err
}

func (cs *ConfigStore) GetAllApps(ctx context.Context) ([]config.App, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []config.App
		for _, a := range cs.apps {
			list = append(list, a)
		}
		return list, nil
	}

	appDocs, err := cs.Client.Collection("apps").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []config.App
	for _, doc := range appDocs {
		var app config.App
		if err := doc.DataTo(&app); err == nil {
			list = append(list, app)
		}
	}
	return list, nil
}

func (cs *ConfigStore) SaveApp(ctx context.Context, app config.App) error {
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

func (cs *ConfigStore) GetQueueStatus(ctx context.Context) ([]config.QueueSnapshotItem, error) {
	return nil, nil
}
