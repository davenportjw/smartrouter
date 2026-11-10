package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"

	"geminirouter/pkg/config"
	"geminirouter/pkg/decision"
)

// resolveLocalDataPath locates the local data file using LOCAL_DATA_DIR if set,
// or by walking up from the current working directory to the repository root (go.mod).
func resolveLocalDataPath(filename string) string {
	if customDir := os.Getenv("LOCAL_DATA_DIR"); customDir != "" {
		return filepath.Join(customDir, filename)
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return filepath.Join(dir, "data", filename)
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return filepath.Join("data", filename)
}

// ConfigStore manages Firestore connections and a fast in-memory configuration cache.
type ConfigStore struct {
	Client      *firestore.Client
	isLocalDev  bool
	localDBPath string

	// Thread-safe in-memory cache
	mu        sync.RWMutex
	keys      map[string]config.APIKey         // Hex-encoded KeyHash -> APIKey
	clients   map[string]config.Client         // ClientID -> Client
	apps      map[string]config.App            // AppID -> App
	models    map[string]config.ModelConfig    // Model ID -> ModelConfig
	providers map[string]config.ProviderConfig // Provider ID -> ProviderConfig
	runners   map[string]config.RunnerResponse // Node ID -> RunnerResponse
	rules     []config.RoutingRule
	headers   []config.CustomHeader

	// Precompiled regex caches
	ruleRegexes   map[string]*regexp.Regexp
	modelRegexes  map[string]*regexp.Regexp
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
		providers:     make(map[string]config.ProviderConfig),
		runners:       make(map[string]config.RunnerResponse),
		ruleRegexes:   make(map[string]*regexp.Regexp),
		modelRegexes:  make(map[string]*regexp.Regexp),
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

// StartListeners starts real-time Firestore listener routines and waits up to 5s for the initial snapshot sync.
func (cs *ConfigStore) StartListeners(ctx context.Context) {
	if cs.isLocalDev {
		log.Println("[Local Dev Cache] Simulated real-time configuration synchronizer active.")
		return
	}

	listeners := []func(context.Context, func()){
		cs.listenKeys,
		cs.listenClients,
		cs.listenApps,
		cs.listenRules,
		cs.listenHeaders,
		cs.listenModels,
		cs.listenProviders,
		cs.listenRunners,
	}

	var wg sync.WaitGroup
	wg.Add(len(listeners))

	for _, fn := range listeners {
		listenerFn := fn
		var once sync.Once
		ready := func() {
			once.Do(wg.Done)
		}
		go listenerFn(ctx, ready)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	select {
	case <-doneCh:
		log.Println("[Firestore Cache] Initial cold-start snapshot synchronization complete.")
	case <-syncCtx.Done():
		log.Println("[Firestore Cache] Initial snapshot sync timed out or context cancelled; continuing in background.")
	}
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

func compileModelPatternRegex(pattern string) (*regexp.Regexp, bool) {
	if pattern == "" || pattern == "*" {
		return nil, false
	}
	if strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") && len(pattern) > 2 {
		re, err := regexp.Compile(pattern[1 : len(pattern)-1])
		return re, err == nil
	}
	if strings.Contains(pattern, "*") && !strings.Contains(pattern, ".*") {
		globRegex := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
		re, err := regexp.Compile(globRegex)
		return re, err == nil
	}
	if strings.HasPrefix(pattern, "^") || strings.HasSuffix(pattern, "$") || strings.ContainsAny(pattern, "[]()+?{}\\^$|") {
		re, err := regexp.Compile(pattern)
		return re, err == nil
	}
	return nil, false
}

func (cs *ConfigStore) compileRegexesLocked() {
	cs.ruleRegexes = make(map[string]*regexp.Regexp)
	cs.modelRegexes = make(map[string]*regexp.Regexp)
	for _, rule := range cs.rules {
		if rule.HeaderName != "" && rule.HeaderValue != "" {
			if strings.HasPrefix(rule.HeaderValue, "/") && strings.HasSuffix(rule.HeaderValue, "/") && len(rule.HeaderValue) > 2 {
				pattern := rule.HeaderValue[1 : len(rule.HeaderValue)-1]
				re, err := regexp.Compile(pattern)
				if err != nil {
					log.Printf("[ConfigStore] Invalid regex pattern in rule %s: %v", rule.ID, err)
				} else {
					cs.ruleRegexes[rule.ID] = re
				}
			}
		}
		if re, ok := compileModelPatternRegex(rule.ModelPattern); ok {
			cs.modelRegexes[rule.ID] = re
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
	return cs.MatchRuleWithContext(modelName, clientTier, appID, headers, "", "", 0.0, "")
}

// ClassifierRuleCandidateEngines returns distinct classifier engine names referenced by
// routing rules that match the given modelName, clientTier, and appID.
func (cs *ConfigStore) ClassifierRuleCandidateEngines(modelName, clientTier, appID string) []string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	seen := map[string]bool{}
	var engines []string
	for _, rule := range cs.rules {
		hasClassifierCondition := (rule.ComplexityTier != "" && rule.ComplexityTier != "all" && rule.ComplexityTier != "*") ||
			(rule.DecisionEngine != "" && rule.DecisionEngine != "all" && rule.DecisionEngine != "*") ||
			rule.MinConfidence > 0 ||
			(rule.TaskKind != "" && rule.TaskKind != "all" && rule.TaskKind != "*")
		if !hasClassifierCondition {
			continue
		}
		modelMatch := rule.ModelPattern == "*" || rule.ModelPattern == modelName
		if !modelMatch && rule.ModelPattern != "" {
			if re, cached := cs.modelRegexes[rule.ID]; cached {
				modelMatch = re.MatchString(modelName)
			} else if re, ok := compileModelPatternRegex(rule.ModelPattern); ok {
				modelMatch = re.MatchString(modelName)
			}
		}
		tierMatch := rule.ClientTier == "" || rule.ClientTier == "all" || rule.ClientTier == clientTier
		appMatch := rule.AppID == "" || rule.AppID == "all" || rule.AppID == appID
		if modelMatch && tierMatch && appMatch {
			eng := rule.DecisionEngine
			if eng == "" || eng == "all" || eng == "*" {
				eng = "jev"
			}
			norm := decision.NormalizeEngineName(eng)
			if !seen[norm] {
				seen[norm] = true
				engines = append(engines, eng)
			}
		}
	}
	return engines
}

func (cs *ConfigStore) MatchRuleWithContext(modelName, clientTier, appID string, headers map[string]string, evaluatedTier, engineUsed string, evaluatedConfidence float64, activeTask string) (config.RoutingRule, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	for _, rule := range cs.rules {
		modelMatch := rule.ModelPattern == "*" || rule.ModelPattern == modelName
		if !modelMatch && rule.ModelPattern != "" {
			if re, cached := cs.modelRegexes[rule.ID]; cached {
				modelMatch = re.MatchString(modelName)
			} else if re, ok := compileModelPatternRegex(rule.ModelPattern); ok {
				modelMatch = re.MatchString(modelName)
			}
		}
		tierMatch := rule.ClientTier == "" || rule.ClientTier == "all" || rule.ClientTier == clientTier
		appMatch := rule.AppID == "" || rule.AppID == "all" || rule.AppID == appID

		headerMatch := true
		if rule.HeaderName != "" {
			val, exists := headers[rule.HeaderName]
			if !exists {
				val, exists = headers[http.CanonicalHeaderKey(rule.HeaderName)]
			}
			if !exists {
				for k, v := range headers {
					if strings.EqualFold(k, rule.HeaderName) {
						val = v
						exists = true
						break
					}
				}
			}
			if !exists {
				headerMatch = false
			} else if rule.HeaderValue != "" {
				if strings.HasPrefix(rule.HeaderValue, "/") && strings.HasSuffix(rule.HeaderValue, "/") && len(rule.HeaderValue) > 2 {
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

		complexityMatch := true
		if rule.ComplexityTier != "" && rule.ComplexityTier != "all" && rule.ComplexityTier != "*" {
			complexityMatch = strings.EqualFold(rule.ComplexityTier, evaluatedTier)
		}

		engineMatch := true
		if rule.DecisionEngine != "" && rule.DecisionEngine != "all" && rule.DecisionEngine != "*" {
			engineMatch = decision.EngineMatches(rule.DecisionEngine, engineUsed)
		}

		confidenceMatch := true
		if rule.MinConfidence > 0 {
			confidenceMatch = evaluatedConfidence >= rule.MinConfidence
		}

		taskMatch := true
		if rule.TaskKind != "" && rule.TaskKind != "all" && rule.TaskKind != "*" {
			taskMatch = strings.EqualFold(rule.TaskKind, activeTask)
		}

		if modelMatch && tierMatch && appMatch && headerMatch && complexityMatch && engineMatch && confidenceMatch && taskMatch {
			return rule, true
		}
	}
	return config.RoutingRule{}, false
}

// Real-time Firestore listener routines

func (cs *ConfigStore) listenKeys(ctx context.Context, ready func()) {
	defer ready()
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
					ready()
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
				ready()
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

func (cs *ConfigStore) listenClients(ctx context.Context, ready func()) {
	defer ready()
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
					ready()
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
				ready()
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

func (cs *ConfigStore) listenRules(ctx context.Context, ready func()) {
	defer ready()
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
					ready()
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
				ready()
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

func (cs *ConfigStore) listenApps(ctx context.Context, ready func()) {
	defer ready()
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
					ready()
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
				ready()
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

func (cs *ConfigStore) listenModels(ctx context.Context, ready func()) {
	defer ready()
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
					ready()
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
				ready()
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

func (cs *ConfigStore) listenHeaders(ctx context.Context, ready func()) {
	defer ready()
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
					ready()
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
				ready()
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

func (cs *ConfigStore) listenProviders(ctx context.Context, ready func()) {
	defer ready()
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("providers").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					ready()
					return err
				}
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.providers = make(map[string]config.ProviderConfig)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var p config.ProviderConfig
					if err := doc.DataTo(&p); err == nil {
						cs.providers[string(p.ID)] = p
					}
				}
				cs.mu.Unlock()
				ready()
				log.Printf("[Firestore Cache] Synchronized %d providers", len(cs.providers))
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

func (cs *ConfigStore) listenRunners(ctx context.Context, ready func()) {
	defer ready()
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		it := cs.Client.Collection("runners").Snapshots(ctx)
		err := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					ready()
					return err
				}
				backoff = 1 * time.Second

				cs.mu.Lock()
				cs.runners = make(map[string]config.RunnerResponse)
				for {
					doc, err := snap.Documents.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						continue
					}
					var r config.RunnerResponse
					if err := doc.DataTo(&r); err == nil && r.Node.ID != "" {
						cs.runners[r.Node.ID] = r
					}
				}
				cs.mu.Unlock()
				ready()
				log.Printf("[Firestore Cache] Synchronized %d runners", len(cs.runners))
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
	cs.localDBPath = resolveLocalDataPath("local_db.json")

	if err := os.MkdirAll(filepath.Dir(cs.localDBPath), 0755); err != nil {
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
			Providers: map[string]config.ProviderConfig{
				"google": {
					ID:          config.ProviderGoogle,
					DisplayName: "Google Cloud",
					Enabled:     true,
					Regions: map[string]config.RegionConfig{
						"us-central1": {
							Code:            "us-central1",
							Active:          true,
							EnabledServices: []string{"gemini"},
						},
					},
				},
			},
			Runners: []config.RunnerResponse{},
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

	// Auto-migrate providers if empty or missing
	if db.Providers == nil || len(db.Providers) == 0 {
		db.Providers = map[string]config.ProviderConfig{
			"google": {
				ID:          config.ProviderGoogle,
				DisplayName: "Google Cloud",
				Enabled:     true,
				Regions: map[string]config.RegionConfig{
					"us-central1": {
						Code:            "us-central1",
						Active:          true,
						EnabledServices: []string{"gemini"},
					},
				},
			},
		}
		dirty = true
	}

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
						SimpleModel:            "gemini-3.5-flash-lite",
						MediumModel:            "gemini-3.8-flash",
						ComplexModel:           "gemini-3.1-pro",
						SimpleCharLimit:        200,
						MediumCharLimit:        1000,
						ForceComplexMultimodal: true,
						ForceComplexTools:      true,
						UseLLMClassifier:       false,
						ClassifierModel:        "gemini-3.5-flash-lite",
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
	cs.providers = db.Providers
	cs.runners = make(map[string]config.RunnerResponse)
	for _, r := range db.Runners {
		if r.Node.ID != "" {
			cs.runners[r.Node.ID] = r
		}
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
	var runnersList []config.RunnerResponse
	for _, r := range cs.runners {
		runnersList = append(runnersList, r)
	}
	db := config.LocalDB{
		Clients:       cs.clients,
		Apps:          cs.apps,
		APIKeys:       cs.keys,
		RoutingRules:  cs.rules,
		CustomHeaders: cs.headers,
		Models:        modelsList,
		Providers:     cs.providers,
		Runners:       runnersList,
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

func (cs *ConfigStore) DeleteClient(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		delete(cs.clients, id)
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("clients").Doc(id).Delete(ctx)
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
					SimpleModel:            "gemini-3.5-flash-lite",
					MediumModel:            "gemini-3.8-flash",
					ComplexModel:           "gemini-3.1-pro",
					SimpleCharLimit:        200,
					MediumCharLimit:        1000,
					ForceComplexMultimodal: true,
					ForceComplexTools:      true,
					UseLLMClassifier:       false,
					ClassifierModel:        "gemini-3.5-flash-lite",
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

func (cs *ConfigStore) GetAllProviders(ctx context.Context) ([]config.ProviderConfig, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		var list []config.ProviderConfig
		for _, p := range cs.providers {
			list = append(list, p)
		}
		return list, nil
	}

	providerDocs, err := cs.Client.Collection("providers").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	var list []config.ProviderConfig
	for _, doc := range providerDocs {
		var p config.ProviderConfig
		if err := doc.DataTo(&p); err == nil {
			list = append(list, p)
		}
	}
	return list, nil
}

func (cs *ConfigStore) SaveProvider(ctx context.Context, provider config.ProviderConfig) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		cs.providers[string(provider.ID)] = provider
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("providers").Doc(string(provider.ID)).Set(ctx, provider)
	return err
}

func (cs *ConfigStore) DeleteProvider(ctx context.Context, id string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		delete(cs.providers, id)
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("providers").Doc(id).Delete(ctx)
	return err
}

// Dynamic runner persistence implementations satisfying the AdminStore interface

func (cs *ConfigStore) syncRunnerClusterNodesLocked(node config.Node, assignedClusters []string, removeAll bool) {
	prov, ok := cs.providers["local"]
	if !ok || prov.Regions == nil {
		return
	}
	reg, ok := prov.Regions["local-cluster"]
	if !ok || reg.LocalConfig == nil {
		return
	}
	assignedSet := make(map[string]bool, len(assignedClusters))
	for _, cid := range assignedClusters {
		if cid != "" {
			assignedSet[cid] = true
		}
	}
	for i := range reg.LocalConfig.Clusters {
		c := &reg.LocalConfig.Clusters[i]
		if c.Nodes == nil {
			c.Nodes = make(map[string]config.Node)
		}
		if !removeAll && assignedSet[c.ID] {
			c.Nodes[node.ID] = node
		} else {
			delete(c.Nodes, node.ID)
		}
	}
	prov.Regions["local-cluster"] = reg
	cs.providers["local"] = prov
}

func (cs *ConfigStore) SaveRegisteredRunner(ctx context.Context, runner config.RunnerResponse) error {
	if runner.Node.ID == "" {
		return fmt.Errorf("runner node ID is required")
	}

	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if runner.AssignedClusters == nil {
		runner.AssignedClusters = []string{}
	}

	if isDev {
		cs.mu.Lock()
		cs.runners[runner.Node.ID] = runner
		cs.syncRunnerClusterNodesLocked(runner.Node, runner.AssignedClusters, false)
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("runners").Doc(runner.Node.ID).Set(ctx, runner)
	return err
}

func (cs *ConfigStore) GetRegisteredRunners(ctx context.Context) ([]config.RunnerResponse, error) {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.RLock()
		defer cs.mu.RUnlock()
		list := make([]config.RunnerResponse, 0, len(cs.runners))
		for _, r := range cs.runners {
			list = append(list, r)
		}
		return list, nil
	}

	runnerDocs, err := cs.Client.Collection("runners").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	list := make([]config.RunnerResponse, 0, len(runnerDocs))
	for _, doc := range runnerDocs {
		var r config.RunnerResponse
		if err := doc.DataTo(&r); err == nil {
			list = append(list, r)
		}
	}
	return list, nil
}

func (cs *ConfigStore) AttachRunnerToClusters(ctx context.Context, nodeID string, clusterIDs []string) error {
	if clusterIDs == nil {
		clusterIDs = []string{}
	}
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		runner, exists := cs.runners[nodeID]
		if !exists {
			runner = config.RunnerResponse{
				Node: config.Node{ID: nodeID},
			}
		}
		runner.AssignedClusters = clusterIDs
		cs.runners[nodeID] = runner
		cs.syncRunnerClusterNodesLocked(runner.Node, clusterIDs, false)
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	docRef := cs.Client.Collection("runners").Doc(nodeID)
	docSnap, err := docRef.Get(ctx)
	var runner config.RunnerResponse
	if err == nil && docSnap.Exists() {
		_ = docSnap.DataTo(&runner)
	} else {
		runner = config.RunnerResponse{
			Node: config.Node{ID: nodeID},
		}
	}
	runner.AssignedClusters = clusterIDs
	_, err = docRef.Set(ctx, runner)
	return err
}

func (cs *ConfigStore) DeregisterRunner(ctx context.Context, nodeID string) error {
	cs.mu.RLock()
	isDev := cs.isLocalDev
	cs.mu.RUnlock()

	if isDev {
		cs.mu.Lock()
		runner := cs.runners[nodeID]
		delete(cs.runners, nodeID)
		cs.syncRunnerClusterNodesLocked(config.Node{ID: nodeID}, runner.AssignedClusters, true)
		cs.mu.Unlock()
		return cs.saveLocalDB()
	}

	_, err := cs.Client.Collection("runners").Doc(nodeID).Delete(ctx)
	return err
}

