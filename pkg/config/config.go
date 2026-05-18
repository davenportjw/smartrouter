package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

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

// APIKey represents an authorized router API key mapping to a client.
type APIKey struct {
	KeyHash  string `firestore:"key_hash" json:"key_hash"`
	ClientID string `firestore:"client_id" json:"client_id"`
	Status   string `firestore:"status" json:"status"` // "active", "revoked"
}

// RoutingRule defines dynamic model rewrites and targets.
type RoutingRule struct {
	ID             string `firestore:"id" json:"id"`
	ModelPattern   string `firestore:"model_pattern" json:"model_pattern"` // regex or exact match
	ClientTier     string `firestore:"client_tier" json:"client_tier"`     // "all" or specific tier
	TargetModel    string `firestore:"target_model" json:"target_model"`
	TargetLocation string `firestore:"target_location" json:"target_location"` // E.g. "us-central1" for Vertex AI
	FallbackModel  string `firestore:"fallback_model" json:"fallback_model"`
	PriorityWeight int    `firestore:"priority_weight" json:"priority_weight"`
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
	rules   []RoutingRule
}

// LocalDB represents the JSON schema for the local development database.
type LocalDB struct {
	Clients      map[string]Client `json:"clients"`
	APIKeys      map[string]APIKey `json:"api_keys"`
	RoutingRules []RoutingRule     `json:"routing_rules"`
}

// NewConfigStore initializes a Firestore connection and caches configuration, or uses local file for dev.
func NewConfigStore(ctx context.Context, projectID string) (*ConfigStore, error) {
	isLocalDev := os.Getenv("LOCAL_DEV") == "true"

	store := &ConfigStore{
		isLocalDev: isLocalDev,
		keys:       make(map[string]APIKey),
		clients:    make(map[string]Client),
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
	go cs.listenRules(ctx)
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

// MatchRule evaluates rules to find the best matching target for model and client tier.
func (cs *ConfigStore) MatchRule(modelName, clientTier string) (RoutingRule, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	for _, rule := range cs.rules {
		// Simple exact matches for now; can support glob/regex patterns in the future
		modelMatch := rule.ModelPattern == "*" || rule.ModelPattern == modelName
		tierMatch := rule.ClientTier == "all" || rule.ClientTier == clientTier

		if modelMatch && tierMatch {
			return rule, true
		}
	}
	return RoutingRule{}, false
}

// Real-time listeners using Firestore Snapshots

func (cs *ConfigStore) listenKeys(ctx context.Context) {
	it := cs.Client.Collection("api_keys").Snapshots(ctx)
	for {
		snap, err := it.Next()
		if err != nil {
			log.Printf("[Firestore] Keys listener error: %v", err)
			return
		}

		cs.mu.Lock()
		// Clear old keys cache and reload all snapshots
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
}

func (cs *ConfigStore) listenClients(ctx context.Context) {
	it := cs.Client.Collection("clients").Snapshots(ctx)
	for {
		snap, err := it.Next()
		if err != nil {
			log.Printf("[Firestore] Clients listener error: %v", err)
			return
		}

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
}

func (cs *ConfigStore) listenRules(ctx context.Context) {
	it := cs.Client.Collection("routing_rules").Snapshots(ctx)
	for {
		snap, err := it.Next()
		if err != nil {
			log.Printf("[Firestore] Rules listener error: %v", err)
			return
		}

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
		cs.mu.Unlock()
		log.Printf("[Firestore Cache] Synchronized %d routing rules", len(cs.rules))
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
			TargetModel:    "gemini-1.5-flash",
			TargetLocation: "us-central1",
			FallbackModel:  "gemini-1.5-pro",
			PriorityWeight: 1,
		}

		db := LocalDB{
			Clients:      make(map[string]Client),
			APIKeys:      make(map[string]APIKey),
			RoutingRules: []RoutingRule{devRule},
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
		log.Println("[Local Dev] Pre-seeded Router Rule: All requests (*) -> gemini-1.5-flash")
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

	cs.mu.Lock()
	cs.clients = db.Clients
	cs.keys = db.APIKeys
	cs.rules = db.RoutingRules
	cs.mu.Unlock()

	log.Printf("[Local Dev Cache] Loaded %d clients, %d API keys, %d rules.", len(cs.clients), len(cs.keys), len(cs.rules))
	return nil
}

// saveLocalDB flushes memory cache changes to local_db.json.
func (cs *ConfigStore) saveLocalDB() error {
	cs.mu.RLock()
	db := LocalDB{
		Clients:      cs.clients,
		APIKeys:      cs.keys,
		RoutingRules: cs.rules,
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

	if isDev {
		cs.mu.Lock()
		cs.keys[key.KeyHash] = key
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
