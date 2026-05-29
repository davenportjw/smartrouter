package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	apiController := NewAPIController(dbStore, proxy.NewRequestScheduler(1000, 100), nil, sharedSecret)

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

	// --- TEST LOCAL CLUSTER REST ENDPOINTS ---
	t.Run("Local Cluster REST API Endpoints", func(t *testing.T) {
		// Instantiate a cluster queue and set it on the controller
		cq := proxy.NewClusterQueue(10, 5*time.Second)
		apiController.Queue = cq

		// 1. Node dynamic registration
		reqReg := httptest.NewRequest("POST", "/api/v1/cluster/runners/register", strings.NewReader(`{
			"id": "runner-mac-1",
			"cluster_id": "local-cluster-1",
			"hostname": "Jasons-MacBook-Pro",
			"platform": "darwin",
			"status": "online",
			"total_ram_bytes": 17179869184,
			"supported_models": ["gemma2:2b"]
		}`))
		rrReg := httptest.NewRecorder()
		mux.ServeHTTP(rrReg, reqReg)
		if rrReg.Code != http.StatusOK {
			t.Errorf("expected runner registration status 200, got %d: %s", rrReg.Code, rrReg.Body.String())
		}
		if !strings.Contains(rrReg.Body.String(), `"registered"`) {
			t.Errorf("expected response to confirm registration status, got: %s", rrReg.Body.String())
		}

		// 2. Node dynamic heartbeat
		reqHb := httptest.NewRequest("POST", "/api/v1/cluster/runners/heartbeat", strings.NewReader(`{
			"node_id": "runner-mac-1",
			"cluster_id": "local-cluster-1"
		}`))
		rrHb := httptest.NewRecorder()
		mux.ServeHTTP(rrHb, reqHb)
		if rrHb.Code != http.StatusOK {
			t.Errorf("expected heartbeat status 200, got %d: %s", rrHb.Code, rrHb.Body.String())
		}

		// 3. Poll queue when empty -> should return 204 No Content
		reqPollEmpty := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{
			"supported_models": ["gemma2:2b"]
		}`))
		rrPollEmpty := httptest.NewRecorder()
		mux.ServeHTTP(rrPollEmpty, reqPollEmpty)
		if rrPollEmpty.Code != http.StatusNoContent {
			t.Errorf("expected empty queue poll status 204, got %d", rrPollEmpty.Code)
		}

		// 4. Enqueue a job and poll it
		mockJob := &config.QueueJob{
			ID:        "job-123",
			ClusterID: "local-cluster-1",
			AppID:     "app-1",
			Model:     "gemma2:2b",
			Priority:  "high",
			Payload:   []byte(`{"prompt": "hello"}`),
			CreatedAt: time.Now(),
		}
		resChan, err := cq.Enqueue(mockJob)
		if err != nil {
			t.Fatalf("failed to enqueue mock job: %v", err)
		}

		reqPoll := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{
			"supported_models": ["gemma2:2b"]
		}`))
		rrPoll := httptest.NewRecorder()
		mux.ServeHTTP(rrPoll, reqPoll)
		if rrPoll.Code != http.StatusOK {
			t.Fatalf("expected poll status 200, got %d: %s", rrPoll.Code, rrPoll.Body.String())
		}
		if !strings.Contains(rrPoll.Body.String(), "job-123") {
			t.Errorf("expected polled job to have ID 'job-123', got: %s", rrPoll.Body.String())
		}

		// 5. Resolve the enqueued job
		reqResolve := httptest.NewRequest("POST", "/api/v1/cluster/queue/resolve", strings.NewReader(`{
			"job_id": "job-123",
			"payload": "eyJyZXNwb25zZSI6ICJIZWxsbyBvdmVyIFJFU1QhIn0=",
			"status_code": 200
		}`))
		rrResolve := httptest.NewRecorder()
		mux.ServeHTTP(rrResolve, reqResolve)
		if rrResolve.Code != http.StatusOK {
			t.Errorf("expected resolve status 200, got %d: %s", rrResolve.Code, rrResolve.Body.String())
		}

		// Assert enqueued channel resolves successfully
		select {
		case res := <-resChan:
			if res.StatusCode != 200 {
				t.Errorf("expected resolved status 200, got %d", res.StatusCode)
			}
			if string(res.Payload) != `{"response": "Hello over REST!"}` {
				t.Errorf("expected resolved payload 'Hello over REST!', got: %s", string(res.Payload))
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("timed out waiting for job resolution channel propagation")
		}
	})

	// --- TEST DYNAMIC RUNNERS & CLUSTER ATTACHMENTS ---
	t.Run("Dynamic Runners and Cluster Attachments", func(t *testing.T) {
		// 1. Register a runner with 16GB RAM
		reqReg := httptest.NewRequest("POST", "/api/v1/cluster/runners/register", strings.NewReader(`{
			"id": "runner-mac-test",
			"name": "Developer MacBook",
			"memory_allocated_gb": 16,
			"compute_gpu_cores": 8,
			"supported_models": ["llama3:8b"]
		}`))
		rrReg := httptest.NewRecorder()
		mux.ServeHTTP(rrReg, reqReg)
		if rrReg.Code != http.StatusOK {
			t.Fatalf("failed to register runner: %s", rrReg.Body.String())
		}

		// 2. Verify runner lists via admin endpoint and holds the correct relative maxes
		reqList := httptest.NewRequest("GET", "/api/v1/admin/runners", nil)
		reqList.Header.Set("X-Shared-Secret", sharedSecret)
		rrList := httptest.NewRecorder()
		mux.ServeHTTP(rrList, reqList)
		if rrList.Code != http.StatusOK {
			t.Fatalf("failed to list runners: %s", rrList.Body.String())
		}

		bodyStr := rrList.Body.String()
		if !strings.Contains(bodyStr, `"id":"runner-mac-test"`) {
			t.Errorf("expected runner-mac-test in list, got: %s", bodyStr)
		}
		// Memory based calculations: 16GB memory -> 8GB max model size, 2 max concurrency
		if !strings.Contains(bodyStr, `"max_model_size_gb":8`) {
			t.Errorf("expected max_model_size_gb: 8, got: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, `"max_concurrent":2`) {
			t.Errorf("expected max_concurrent: 2, got: %s", bodyStr)
		}

		// 3. Administrative cluster mapping attachment
		reqAttach := httptest.NewRequest("POST", "/api/v1/admin/runners/attach", strings.NewReader(`{
			"node_id": "runner-mac-test",
			"cluster_ids": ["cluster-alpha"]
		}`))
		reqAttach.Header.Set("X-Shared-Secret", sharedSecret)
		rrAttach := httptest.NewRecorder()
		mux.ServeHTTP(rrAttach, reqAttach)
		if rrAttach.Code != http.StatusOK {
			t.Errorf("failed to attach runner to cluster: %s", rrAttach.Body.String())
		}

		// Verify attachment in list
		rrList2 := httptest.NewRecorder()
		mux.ServeHTTP(rrList2, reqList)
		if !strings.Contains(rrList2.Body.String(), `"assigned_clusters":["cluster-alpha"]`) {
			t.Errorf("expected runner-mac-test to be attached to cluster-alpha, got: %s", rrList2.Body.String())
		}

		// 4. Verify multi-cluster poll filtering
		// Clean queue first
		cq := proxy.NewClusterQueue(10, 5*time.Second)
		apiController.Queue = cq

		// Enqueue job targetting "cluster-beta"
		jobBeta := &config.QueueJob{
			ID:        "job-beta",
			ClusterID: "cluster-beta",
			Model:     "llama3:8b",
			Priority:  "high",
		}
		_, _ = cq.Enqueue(jobBeta)

		// Poll from queue for runner-mac-test (which is bound ONLY to cluster-alpha) -> should receive 204 (no match)
		reqPoll1 := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{
			"node_id": "runner-mac-test",
			"supported_models": ["llama3:8b"]
		}`))
		rrPoll1 := httptest.NewRecorder()
		mux.ServeHTTP(rrPoll1, reqPoll1)
		if rrPoll1.Code != http.StatusNoContent {
			t.Errorf("expected 204 NoContent (runner is bound to cluster-alpha but job targets cluster-beta), got status %d", rrPoll1.Code)
		}

		// Attach runner to cluster-beta as well
		reqAttach2 := httptest.NewRequest("POST", "/api/v1/admin/runners/attach", strings.NewReader(`{
			"node_id": "runner-mac-test",
			"cluster_ids": ["cluster-alpha", "cluster-beta"]
		}`))
		reqAttach2.Header.Set("X-Shared-Secret", sharedSecret)
		rrAttach2 := httptest.NewRecorder()
		mux.ServeHTTP(rrAttach2, reqAttach2)

		// Poll again -> should succeed and return job-beta!
		reqPoll2 := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{
			"node_id": "runner-mac-test",
			"supported_models": ["llama3:8b"]
		}`))
		rrPoll2 := httptest.NewRecorder()
		mux.ServeHTTP(rrPoll2, reqPoll2)
		if rrPoll2.Code != http.StatusOK {
			t.Errorf("expected 200 OK after mapping runner to cluster-beta, got %d: %s", rrPoll2.Code, rrPoll2.Body.String())
		}
		if !strings.Contains(rrPoll2.Body.String(), "job-beta") {
			t.Errorf("expected enqueued job-beta to be polled, got: %s", rrPoll2.Body.String())
		}
	})

	os.RemoveAll("data/local_db.json")
}
