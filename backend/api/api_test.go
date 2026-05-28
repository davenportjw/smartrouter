package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	store "geminirouter/backend/config"
	"geminirouter/backend/proxy"
	"geminirouter/pkg/config"
)

func TestAPIConfigStoreAndBackendAPIIntegration(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend database: %v", err)
	}

	// Clear databases from previous local dev states
	os.Remove("data/local_db.json")
	dbStore, _ = store.NewConfigStore(ctx, "test-project")

	sharedSecret := "super-secret-bypass-token"
	apiController := NewAPIController(dbStore, proxy.NewRequestScheduler(1000, 100), sharedSecret)

	mux := http.NewServeMux()
	apiController.RegisterRoutes(mux)

	// Start local test server
	server := httptest.NewServer(mux)
	defer server.Close()

	// 1. Instantiate API client config store pointing to the REST server
	clientStore := config.NewAPIConfigStore(server.URL, sharedSecret)

	// --- TEST APPS CRUD ---
	t.Run("Apps API CRUD Integration", func(t *testing.T) {
		// Fetch initially (should be empty or seed rule apps)
		apps, err := clientStore.GetAllApps(ctx)
		if err != nil {
			t.Fatalf("GetAllApps failed: %v", err)
		}
		initialCount := len(apps)

		// Create dynamic app
		newApp := config.App{
			ID:       "app-test-api",
			ClientID: "client-test-api",
			Name:     "REST API Verification App",
			RPM:      250,
			TPM:      150000,
			Priority: "high",
		}

		err = clientStore.SaveApp(ctx, newApp)
		if err != nil {
			t.Fatalf("SaveApp failed: %v", err)
		}

		// Fetch again and verify
		apps, err = clientStore.GetAllApps(ctx)
		if err != nil {
			t.Fatalf("GetAllApps failed: %v", err)
		}
		if len(apps) != initialCount+1 {
			t.Errorf("expected %d apps, got %d", initialCount+1, len(apps))
		}

		// Lookup App
		app, ok := clientStore.LookupApp("app-test-api")
		if !ok {
			t.Errorf("LookupApp failed to find saved app")
		}
		if app.Name != newApp.Name {
			t.Errorf("expected app name %q, got %q", newApp.Name, app.Name)
		}

		// Delete App
		err = clientStore.DeleteApp(ctx, "app-test-api")
		if err != nil {
			t.Fatalf("DeleteApp failed: %v", err)
		}

		// Verify deletion
		_, ok = clientStore.LookupApp("app-test-api")
		if ok {
			t.Errorf("App was not deleted successfully")
		}
	})

	// --- TEST KEYS CRUD ---
	t.Run("Keys API CRUD Integration", func(t *testing.T) {
		newKey := config.APIKey{
			KeyHash:  config.HashKey("raw-key-token-value"),
			AppID:    "app-test-api-2",
			ClientID: "client-test-api-2",
			Status:   "active",
		}

		err := clientStore.SaveKey(ctx, newKey)
		if err != nil {
			t.Fatalf("SaveKey failed: %v", err)
		}

		// Fetch and verify
		keys, err := clientStore.GetAllKeys(ctx)
		if err != nil {
			t.Fatalf("GetAllKeys failed: %v", err)
		}

		found := false
		for _, k := range keys {
			if k.KeyHash == newKey.KeyHash {
				found = true
				if k.Status != "active" {
					t.Errorf("expected key status to be 'active', got %q", k.Status)
				}
				break
			}
		}
		if !found {
			t.Errorf("expected to find created key hash in keys list")
		}

		// Revoke Key
		err = clientStore.RevokeKey(ctx, newKey.KeyHash)
		if err != nil {
			t.Fatalf("RevokeKey failed: %v", err)
		}

		// Verify revoked
		keys, err = clientStore.GetAllKeys(ctx)
		if err == nil {
			for _, k := range keys {
				if k.KeyHash == newKey.KeyHash {
					if k.Status != "revoked" {
						t.Errorf("expected key status to be 'revoked' after revocation, got %q", k.Status)
					}
					break
				}
			}
		}
	})

	// --- TEST ROUTING RULES CRUD ---
	t.Run("Routing Rules CRUD Integration", func(t *testing.T) {
		newRule := config.RoutingRule{
			ID:             "rule-test-api",
			AppID:          "app-test-api-3",
			ModelPattern:   "gemini-3.5-pro",
			ClientTier:     "premium",
			TargetModel:    "gemini-2.5-pro",
			TargetLocation: "us-central1",
			PriorityWeight: 10,
		}

		err := clientStore.SaveRule(ctx, newRule)
		if err != nil {
			t.Fatalf("SaveRule failed: %v", err)
		}

		// Fetch rules
		rules, err := clientStore.GetAllRules(ctx)
		if err != nil {
			t.Fatalf("GetAllRules failed: %v", err)
		}

		found := false
		for _, r := range rules {
			if r.ID == newRule.ID {
				found = true
				if r.TargetModel != newRule.TargetModel {
					t.Errorf("expected target model %q, got %q", newRule.TargetModel, r.TargetModel)
				}
				break
			}
		}
		if !found {
			t.Errorf("expected to find rule in rules list")
		}

		// Delete rule
		err = clientStore.DeleteRule(ctx, newRule.ID)
		if err != nil {
			t.Fatalf("DeleteRule failed: %v", err)
		}

		// Verify deleted
		rules, _ = clientStore.GetAllRules(ctx)
		for _, r := range rules {
			if r.ID == newRule.ID {
				t.Errorf("expected rule to be deleted from list")
			}
		}
	})

	// --- TEST AUTHENTICATION SECURITY BLOCK ---
	t.Run("Auth Security Middleware Enforcements", func(t *testing.T) {
		// Instantiate client store with an INVALID bypass secret
		unauthorizedClientStore := config.NewAPIConfigStore(server.URL, "wrong-secret")

		// Attempt GetAllApps call (should fail with unauthorized status error)
		_, err := unauthorizedClientStore.GetAllApps(ctx)
		if err == nil {
			t.Errorf("expected API call to fail with unauthorized status, but got success")
		}
	})

	// --- TEST PROVIDERS CRUD ---
	t.Run("Providers API CRUD Integration", func(t *testing.T) {
		// Google provider should be present by default
		providers, err := clientStore.GetAllProviders(ctx)
		if err != nil {
			t.Fatalf("GetAllProviders failed: %v", err)
		}
		foundGoogle := false
		for _, p := range providers {
			if p.ID == config.ProviderGoogle {
				foundGoogle = true
				if !p.Enabled {
					t.Errorf("expected google provider to be enabled by default")
				}
				break
			}
		}
		if !foundGoogle {
			t.Errorf("expected to find pre-seeded google provider configuration")
		}

		// Create and save a new Local cluster provider configuration
		newProvider := config.ProviderConfig{
			ID:          config.ProviderLocal,
			DisplayName: "On-Premises Local Cluster",
			Enabled:     true,
			Regions: map[string]config.RegionConfig{
				"local-cluster": {
					Code:            "local-cluster",
					Active:          true,
					EnabledServices: []string{"llama-3-8b"},
					LocalConfig: &config.LocalConfig{
						Clusters: []config.LocalCluster{
							{
								ID:        "cluster-alpha",
								Name:      "Alpha GPU Node Pool",
								QueueName: "alpha-queue",
							},
						},
					},
				},
			},
		}

		err = clientStore.SaveProvider(ctx, newProvider)
		if err != nil {
			t.Fatalf("SaveProvider failed: %v", err)
		}

		// Fetch again and verify creation
		providers, err = clientStore.GetAllProviders(ctx)
		if err != nil {
			t.Fatalf("GetAllProviders failed: %v", err)
		}
		foundLocal := false
		for _, p := range providers {
			if p.ID == config.ProviderLocal {
				foundLocal = true
				if p.DisplayName != newProvider.DisplayName {
					t.Errorf("expected DisplayName %q, got %q", newProvider.DisplayName, p.DisplayName)
				}
				reg, ok := p.Regions["local-cluster"]
				if !ok {
					t.Errorf("expected to find 'local-cluster' region in saved local provider")
				} else if len(reg.LocalConfig.Clusters) == 0 || reg.LocalConfig.Clusters[0].ID != "cluster-alpha" {
					t.Errorf("expected to find cluster-alpha inside region configs")
				}
				break
			}
		}
		if !foundLocal {
			t.Errorf("expected to find saved local cluster provider in list")
		}

		// Delete local provider
		err = clientStore.DeleteProvider(ctx, string(config.ProviderLocal))
		if err != nil {
			t.Fatalf("DeleteProvider failed: %v", err)
		}

		// Verify deletion
		providers, _ = clientStore.GetAllProviders(ctx)
		for _, p := range providers {
			if p.ID == config.ProviderLocal {
				t.Errorf("expected local provider to be deleted from list")
			}
		}
	})

	os.RemoveAll("data/local_db.json")
}
