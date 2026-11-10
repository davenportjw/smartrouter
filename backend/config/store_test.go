package store

import (
	"context"
	"testing"

	"geminirouter/pkg/config"
)

func TestIsValidModelName(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		expected  bool
	}{
		// Valid Gemini 2.5+ Models
		{"Gemini 2.5 Flash", "gemini-2.5-flash", true},
		{"Gemini 2.5 Pro", "gemini-2.5-pro", true},
		{"Gemini 2.5 Flash-Lite", "gemini-2.5-flash-lite", true},
		{"Gemini 3.0 Flash", "gemini-3.0-flash", true},
		{"Gemini 3.1 Pro", "gemini-3.1-pro", true},
		{"Gemini 3.5 Flash-Lite", "gemini-3.5-flash-lite", true},

		// Valid Preview Models (2.5+ / 3.x+)
		{"Gemini 3.1 Pro Preview", "gemini-3.1-pro-preview", true},
		{"Gemini 3.1 Flash-Lite Preview", "gemini-3.1-flash-lite-preview", true},
		{"Gemini 2.5 Flash Preview Date-Suffix", "gemini-2.5-flash-preview-09-2025", true},
		{"Gemini 3.0 Pro Preview", "gemini-3-pro-preview", true},

		
		// Valid Embeddings and Virtual Router
		{"Text Embedding 004", "text-embedding-004", true},
		{"Multimodal Embedding 001", "multimodal-embedding-001", true},
		{"Gemini Dynamic Virtual Router", "gemini-dynamic", true},

		// Valid Custom/Tuned Model and Endpoint paths
		{"Valid Tuned Path", "projects/my-project/locations/us-central1/models/my-custom-model", true},
		{"Valid Endpoint Path", "projects/my-project/locations/us-central1/endpoints/my-endpoint-1", true},

		// Deprecated/Legacy models (pre-2.5 series must be blocked)
		{"Gemini 1.5 Flash (Legacy)", "gemini-1.5-flash", false},
		{"Gemini 1.5 Pro (Legacy)", "gemini-1.5-pro", false},
		{"Gemini 1.0 Pro (Legacy)", "gemini-1.0-pro", false},
		{"Gemini 2.0 Flash (Legacy)", "gemini-2.0-flash", false},
		{"Gemini 1.5 Pro Preview (Legacy)", "gemini-1.5-pro-preview", false},
		{"Gemini 2.0 Flash Preview (Legacy)", "gemini-2.0-flash-preview", false},


		// Invalid names
		{"Arbitrary string", "some-random-model", false},
		{"Empty name", "", false},
		{"Incomplete path", "projects/my-project/locations/us-central1/models/", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := config.IsValidModelName(tt.modelName)
			if actual != tt.expected {
				t.Errorf("IsValidModelName(%q) = %t; expected %t", tt.modelName, actual, tt.expected)
			}
		})
	}
}

func TestFirstStartModelSeedingEmpty(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	models, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to retrieve seeded models: %v", err)
	}

	if len(models) != 0 {
		t.Errorf("expected 0 seeded models on first start before discovery, got %d", len(models))
	}
}

func TestConfigStoreClientsCRUD(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// 1. Verify initial state
	clients, err := store.GetAllClients(ctx)
	if err != nil {
		t.Fatalf("failed to get clients: %v", err)
	}
	initialCount := len(clients)

	// 2. Save new client
	newClient := config.Client{
		ID:       "test-crud-client",
		Name:     "Test CRUD Client",
		Tier:     "premium",
		RPM:      150,
		TPM:      200000,
		Priority: "high",
	}

	if err := store.SaveClient(ctx, newClient); err != nil {
		t.Fatalf("failed to save client: %v", err)
	}

	// 3. GetAllClients and find new client
	clients, err = store.GetAllClients(ctx)
	if err != nil {
		t.Fatalf("failed to get clients: %v", err)
	}
	if len(clients) != initialCount+1 {
		t.Errorf("expected %d clients, got %d", initialCount+1, len(clients))
	}

	found := false
	for _, c := range clients {
		if c.ID == newClient.ID {
			found = true
			if c.Name != newClient.Name || c.Tier != newClient.Tier || c.RPM != newClient.RPM || c.TPM != newClient.TPM || c.Priority != newClient.Priority {
				t.Errorf("saved client details mismatch: %+v", c)
			}
			break
		}
	}
	if !found {
		t.Errorf("saved client not found in GetAllClients")
	}

	// 4. Delete client
	if err := store.DeleteClient(ctx, newClient.ID); err != nil {
		t.Fatalf("failed to delete client: %v", err)
	}

	// 5. Verify client was deleted
	clients, err = store.GetAllClients(ctx)
	if err != nil {
		t.Fatalf("failed to get clients: %v", err)
	}
	if len(clients) != initialCount {
		t.Errorf("expected %d clients after delete, got %d", initialCount, len(clients))
	}
}

func TestNewConfigStoreFirestore(t *testing.T) {
	t.Setenv("LOCAL_DEV", "false")
	t.Setenv("FIRESTORE_EMULATOR_HOST", "localhost:8888")

	ctx := context.Background()
	store, err := NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("expected to create firestore store with emulator host: %v", err)
	}
	defer store.Client.Close()

	if store.isLocalDev {
		t.Errorf("expected isLocalDev to be false")
	}
	if store.Client == nil {
		t.Errorf("expected firestore client to be instantiated")
	}
}

func TestMatchRuleCaseInsensitiveHeaderAndModelPatternRegex(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	store, err := NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	// Rule 1: lowercase header name and slash-delimited regex ModelPattern
	ruleRegex := config.RoutingRule{
		ID:             "rule-regex",
		ModelPattern:   `/^gemini-3\.[0-9]+-pro$/`,
		ClientTier:     "all",
		HeaderName:     "x-custom-env",
		HeaderValue:    "staging",
		TargetModel:    "gemini-3.8-flash",
		PriorityWeight: 100,
	}
	// Rule 2: glob ModelPattern
	ruleGlob := config.RoutingRule{
		ID:             "rule-glob",
		ModelPattern:   "gemini-2.5-*",
		ClientTier:     "all",
		HeaderName:     "X-ROUTING-TAG",
		HeaderValue:    "/^canary-[0-9]+$/",
		TargetModel:    "gemini-3.5-flash-lite",
		PriorityWeight: 90,
	}

	if err := store.SaveRule(ctx, ruleRegex); err != nil {
		t.Fatalf("failed to save regex rule: %v", err)
	}
	if err := store.SaveRule(ctx, ruleGlob); err != nil {
		t.Fatalf("failed to save glob rule: %v", err)
	}

	// Match Rule 1 with canonicalized header "X-Custom-Env" and model "gemini-3.1-pro"
	matched, ok := store.MatchRule("gemini-3.1-pro", "standard", "app-1", map[string]string{
		"X-Custom-Env": "staging",
	})
	if !ok || matched.ID != "rule-regex" {
		t.Fatalf("expected rule-regex to match case-insensitive header X-Custom-Env and regex ModelPattern, got ok=%v rule=%+v", ok, matched)
	}

	// Match Rule 2 with mixed-case header "X-Routing-Tag" and glob model "gemini-2.5-flash-lite"
	matched, ok = store.MatchRule("gemini-2.5-flash-lite", "standard", "app-1", map[string]string{
		"X-Routing-Tag": "canary-42",
	})
	if !ok || matched.ID != "rule-glob" {
		t.Fatalf("expected rule-glob to match case-insensitive header X-Routing-Tag and glob ModelPattern, got ok=%v rule=%+v", ok, matched)
	}
}

func TestConfigStoreRunnerPersistence(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	dataDir := t.TempDir()
	t.Setenv("LOCAL_DATA_DIR", dataDir)

	ctx := context.Background()
	store, err := NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	runner := config.RunnerResponse{
		Node: config.Node{
			ID:              "node-persist-1",
			Name:            "Apple Silicon Runner 1",
			Status:          "online",
			SupportedModels: []string{"gemma-4-4b"},
		},
		AssignedClusters: []string{},
	}

	if err := store.SaveRegisteredRunner(ctx, runner); err != nil {
		t.Fatalf("SaveRegisteredRunner failed: %v", err)
	}

	if err := store.AttachRunnerToClusters(ctx, "node-persist-1", []string{"cluster-a", "cluster-b"}); err != nil {
		t.Fatalf("AttachRunnerToClusters failed: %v", err)
	}

	// Re-instantiate store from same dataDir to verify persistence on disk
	storeReloaded, err := NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to reload config store: %v", err)
	}

	runners, err := storeReloaded.GetRegisteredRunners(ctx)
	if err != nil {
		t.Fatalf("GetRegisteredRunners failed: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("expected 1 persisted runner, got %d", len(runners))
	}
	if runners[0].Node.ID != "node-persist-1" || len(runners[0].AssignedClusters) != 2 {
		t.Errorf("unexpected persisted runner state: %+v", runners[0])
	}

	// Deregister and verify removal
	if err := storeReloaded.DeregisterRunner(ctx, "node-persist-1"); err != nil {
		t.Fatalf("DeregisterRunner failed: %v", err)
	}
	runnersAfter, err := storeReloaded.GetRegisteredRunners(ctx)
	if err != nil {
		t.Fatalf("GetRegisteredRunners after deregister failed: %v", err)
	}
	if len(runnersAfter) != 0 {
		t.Errorf("expected 0 runners after deregister, got %d", len(runnersAfter))
	}
}


