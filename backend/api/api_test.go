package api

import (
	"context"
	"encoding/json"
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

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "smartrouter-api-test-*")
	if err == nil {
		os.Setenv("LOCAL_DATA_DIR", tmpDir)
	}
	code := m.Run()
	if tmpDir != "" {
		os.RemoveAll(tmpDir)
	}
	os.Exit(code)
}

func TestAPIConfigStoreAndBackendAPIIntegration(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize backend database: %v", err)
	}

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

		// 0. Verify cluster endpoints enforce authentication when LOCAL_DEV != "true"
		t.Setenv("LOCAL_DEV", "false")
		for _, ep := range []string{
			"/api/v1/cluster/runners/register",
			"/api/v1/cluster/runners/heartbeat",
			"/api/v1/cluster/queue/poll",
			"/api/v1/cluster/queue/resolve",
		} {
			unauthReq := httptest.NewRequest("POST", ep, strings.NewReader(`{}`))
			unauthRR := httptest.NewRecorder()
			mux.ServeHTTP(unauthRR, unauthReq)
			if unauthRR.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 Unauthorized on %s without credentials, got %d", ep, unauthRR.Code)
			}
		}
		t.Setenv("LOCAL_DEV", "true")

		// 1. Node dynamic registration
		reqReg := httptest.NewRequest("POST", "/api/v1/cluster/runners/register", strings.NewReader(`{
			"id": "runner-mac-1",
			"cluster_id": "local-cluster-1",
			"hostname": "Jasons-MacBook-Pro",
			"platform": "darwin",
			"status": "online",
			"total_ram_bytes": 17179869184,
			"supported_models": ["gemma-4-4b"]
		}`))
		reqReg.Header.Set("X-Shared-Secret", sharedSecret)
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
		reqHb.Header.Set("X-Shared-Secret", sharedSecret)
		rrHb := httptest.NewRecorder()
		mux.ServeHTTP(rrHb, reqHb)
		if rrHb.Code != http.StatusOK {
			t.Errorf("expected heartbeat status 200, got %d: %s", rrHb.Code, rrHb.Body.String())
		}

		// 3. Poll queue when empty -> should return 204 No Content
		reqPollEmpty := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{
			"supported_models": ["gemma-4-4b"]
		}`))
		reqPollEmpty.Header.Set("X-Shared-Secret", sharedSecret)
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
			Model:     "gemma-4-4b",
			Priority:  "high",
			Payload:   []byte(`{"prompt": "hello"}`),
			CreatedAt: time.Now(),
		}
		resChan, err := cq.Enqueue(mockJob)
		if err != nil {
			t.Fatalf("failed to enqueue mock job: %v", err)
		}

		reqPoll := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{
			"supported_models": ["gemma-4-4b"]
		}`))
		reqPoll.Header.Set("X-Shared-Secret", sharedSecret)
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
		reqResolve.Header.Set("X-Shared-Secret", sharedSecret)
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

		// 6. Test /api/models/refresh endpoint
		reqRefresh := httptest.NewRequest("POST", "/api/models/refresh", nil)
		reqRefresh.Header.Set("X-Shared-Secret", sharedSecret)
		rrRefresh := httptest.NewRecorder()
		mux.ServeHTTP(rrRefresh, reqRefresh)
		if rrRefresh.Code != http.StatusOK {
			t.Errorf("expected 200 OK for /api/models/refresh, got %d: %s", rrRefresh.Code, rrRefresh.Body.String())
		}
		if !strings.Contains(rrRefresh.Body.String(), `"status":"refreshed"`) {
			t.Errorf("expected status refreshed in /api/models/refresh response, got: %s", rrRefresh.Body.String())
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
		reqReg.Header.Set("X-Shared-Secret", sharedSecret)
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
		reqPoll1.Header.Set("X-Shared-Secret", sharedSecret)
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
		reqPoll2.Header.Set("X-Shared-Secret", sharedSecret)
		rrPoll2 := httptest.NewRecorder()
		mux.ServeHTTP(rrPoll2, reqPoll2)
		if rrPoll2.Code != http.StatusOK {
			t.Errorf("expected 200 OK after mapping runner to cluster-beta, got %d: %s", rrPoll2.Code, rrPoll2.Body.String())
		}
		if !strings.Contains(rrPoll2.Body.String(), "job-beta") {
			t.Errorf("expected enqueued job-beta to be polled, got: %s", rrPoll2.Body.String())
		}

		// 5. Update runner capacity limits (max_model_size_gb & max_concurrent) via /api/v1/admin/runners/attach
		reqAttachLimits := httptest.NewRequest("POST", "/api/v1/admin/runners/attach", strings.NewReader(`{
			"node_id": "runner-mac-test",
			"cluster_ids": ["cluster-alpha"],
			"max_model_size_gb": 32,
			"max_concurrent": 6
		}`))
		reqAttachLimits.Header.Set("X-Shared-Secret", sharedSecret)
		rrAttachLimits := httptest.NewRecorder()
		mux.ServeHTTP(rrAttachLimits, reqAttachLimits)
		if rrAttachLimits.Code != http.StatusOK {
			t.Fatalf("expected 200 OK updating runner limits on attach, got %d: %s", rrAttachLimits.Code, rrAttachLimits.Body.String())
		}

		// Verify heartbeat returns updated max_model_size_gb (32) and max_concurrent (6)
		reqHbUpdated := httptest.NewRequest("POST", "/api/v1/cluster/runners/heartbeat", strings.NewReader(`{
			"node_id": "runner-mac-test"
		}`))
		reqHbUpdated.Header.Set("X-Shared-Secret", sharedSecret)
		rrHbUpdated := httptest.NewRecorder()
		mux.ServeHTTP(rrHbUpdated, reqHbUpdated)
		if rrHbUpdated.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on heartbeat after limit update, got %d: %s", rrHbUpdated.Code, rrHbUpdated.Body.String())
		}
		if !strings.Contains(rrHbUpdated.Body.String(), `"max_model_size_gb":32`) {
			t.Errorf("expected heartbeat to reflect updated max_model_size_gb:32, got: %s", rrHbUpdated.Body.String())
		}
		if !strings.Contains(rrHbUpdated.Body.String(), `"max_concurrent":6`) {
			t.Errorf("expected heartbeat to reflect updated max_concurrent:6, got: %s", rrHbUpdated.Body.String())
		}

		// 6. Verify APIConfigStore.SaveRegisteredRunner persists updated runner via REST API
		err = clientStore.SaveRegisteredRunner(ctx, config.RunnerResponse{
			Node: config.Node{
				ID:                "runner-mac-test",
				Name:              "Developer MacBook Pro M3 Max",
				Status:            "online",
				MemoryAllocatedGB: 64,
				ComputeGPUCores:   30,
				SupportedModels:   []string{"gemma-4-26b", "gemma-4-4b"},
				MaxModelSizeGB:    48,
				MaxConcurrent:     8,
			},
			AssignedClusters: []string{"cluster-alpha", "cluster-beta"},
		})
		if err != nil {
			t.Fatalf("clientStore.SaveRegisteredRunner failed: %v", err)
		}

		runnersAfterSave, err := clientStore.GetRegisteredRunners(ctx)
		if err != nil {
			t.Fatalf("clientStore.GetRegisteredRunners failed: %v", err)
		}
		foundSavedRunner := false
		for _, r := range runnersAfterSave {
			if r.Node.ID == "runner-mac-test" {
				foundSavedRunner = true
				if r.Node.MaxModelSizeGB != 48 || r.Node.MaxConcurrent != 8 {
					t.Errorf("expected saved runner limits 48GB / 8 concurrent, got %dGB / %d concurrent", r.Node.MaxModelSizeGB, r.Node.MaxConcurrent)
				}
				break
			}
		}
		if !foundSavedRunner {
			t.Errorf("expected runner-mac-test in GetRegisteredRunners after SaveRegisteredRunner")
		}
	})
}

func TestAPISessionsAndExplainability(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	ctx := context.Background()

	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}

	sharedSecret := "test-secret-smart"
	apiCtrl := NewAPIController(dbStore, proxy.NewRequestScheduler(1000, 100), nil, sharedSecret)
	mux := http.NewServeMux()
	apiCtrl.RegisterRoutes(mux)

	// 1. POST /api/v1/sessions/task
	reqTask := httptest.NewRequest("POST", "/api/v1/sessions/task", strings.NewReader(`{
		"session_id": "sess-agent-007",
		"task": "code_generation",
		"policy": "lock"
	}`))
	reqTask.Header.Set("X-Shared-Secret", sharedSecret)
	rrTask := httptest.NewRecorder()
	mux.ServeHTTP(rrTask, reqTask)

	if rrTask.Code != http.StatusOK {
		t.Fatalf("expected 200 for session task init, got %d: %s", rrTask.Code, rrTask.Body.String())
	}
	if !strings.Contains(rrTask.Body.String(), "gemini-3.1-pro") {
		t.Errorf("expected resolved model gemini-3.1-pro for code task, got: %s", rrTask.Body.String())
	}

	// 2. GET /api/v1/sessions
	reqList := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	reqList.Header.Set("X-Shared-Secret", sharedSecret)
	rrList := httptest.NewRecorder()
	mux.ServeHTTP(rrList, reqList)

	if rrList.Code != http.StatusOK {
		t.Errorf("expected 200 for list sessions, got %d: %s", rrList.Code, rrList.Body.String())
	}
	if !strings.Contains(rrList.Body.String(), "sess-agent-007") {
		t.Errorf("expected session list to contain sess-agent-007, got: %s", rrList.Body.String())
	}

	// 3. GET /api/v1/sessions?id=sess-agent-007
	reqGet := httptest.NewRequest("GET", "/api/v1/sessions?id=sess-agent-007", nil)
	reqGet.Header.Set("X-Shared-Secret", sharedSecret)
	rrGet := httptest.NewRecorder()
	mux.ServeHTTP(rrGet, reqGet)

	if rrGet.Code != http.StatusOK {
		t.Errorf("expected 200 for get session, got %d: %s", rrGet.Code, rrGet.Body.String())
	}

	// 4. POST /api/v1/complexity/inspect (Jev Engine)
	reqInspectJev := httptest.NewRequest("POST", "/api/v1/complexity/inspect", strings.NewReader(`{
		"prompt": "func QuickSort(arr []int) { if len(arr) <= 1 { return } }",
		"engine": "jev"
	}`))
	reqInspectJev.Header.Set("X-Shared-Secret", sharedSecret)
	rrInspectJev := httptest.NewRecorder()
	mux.ServeHTTP(rrInspectJev, reqInspectJev)

	if rrInspectJev.Code != http.StatusOK {
		t.Fatalf("expected 200 for jev inspect, got %d: %s", rrInspectJev.Code, rrInspectJev.Body.String())
	}
	if !strings.Contains(rrInspectJev.Body.String(), `"tier":"complex"`) {
		t.Errorf("expected complex tier for Go quicksort prompt, got: %s", rrInspectJev.Body.String())
	}
	if !strings.Contains(rrInspectJev.Body.String(), "gemini-3.1-pro") {
		t.Errorf("expected gemini-3.1-pro for complex tier, got: %s", rrInspectJev.Body.String())
	}

	// 5. POST /api/v1/complexity/inspect (CLM Engine)
	reqInspectCLM := httptest.NewRequest("POST", "/api/v1/complexity/inspect", strings.NewReader(`{
		"prompt": "hi",
		"engine": "clm"
	}`))
	reqInspectCLM.Header.Set("X-Shared-Secret", sharedSecret)
	rrInspectCLM := httptest.NewRecorder()
	mux.ServeHTTP(rrInspectCLM, reqInspectCLM)

	if rrInspectCLM.Code != http.StatusOK {
		t.Fatalf("expected 200 for clm inspect, got %d: %s", rrInspectCLM.Code, rrInspectCLM.Body.String())
	}
	if !strings.Contains(rrInspectCLM.Body.String(), `"tier":"simple"`) {
		t.Errorf("expected simple tier for 'hi', got: %s", rrInspectCLM.Body.String())
	}
	if !strings.Contains(rrInspectCLM.Body.String(), "gemini-3.5-flash-lite") {
		t.Errorf("expected gemini-3.5-flash-lite for simple tier, got: %s", rrInspectCLM.Body.String())
	}

	// 6. POST /api/v1/sessions/reset
	reqReset := httptest.NewRequest("POST", "/api/v1/sessions/reset", strings.NewReader(`{
		"session_id": "sess-agent-007"
	}`))
	reqReset.Header.Set("X-Shared-Secret", sharedSecret)
	rrReset := httptest.NewRecorder()
	mux.ServeHTTP(rrReset, reqReset)

	if rrReset.Code != http.StatusOK {
		t.Errorf("expected 200 for session reset, got %d: %s", rrReset.Code, rrReset.Body.String())
	}

	// 7. GET after reset should be 404
	rrGetAfter := httptest.NewRecorder()
	mux.ServeHTTP(rrGetAfter, reqGet)
	if rrGetAfter.Code != http.StatusNotFound {
		t.Errorf("expected 404 after reset, got %d: %s", rrGetAfter.Code, rrGetAfter.Body.String())
	}
}

func TestRunnerRuntimeManagementEnforcement(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	dbStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init config store: %v", err)
	}

	scheduler := proxy.NewRequestScheduler(1000, 100)
	queue := proxy.NewClusterQueue(100, 30*time.Second)
	defer queue.Close()
	sharedSecret := "test-secret-123"
	ac := NewAPIController(dbStore, scheduler, queue, sharedSecret)

	mux := http.NewServeMux()
	ac.RegisterRoutes(mux)

	// 1. Register a runner with explicit MaxModelSizeGB=28, MaxConcurrent=3, and ?cluster_id=cluster-alpha
	regPayload := `{
		"id": "runner-m4-01",
		"name": "M4 Pro Initial",
		"memory_allocated_gb": 32,
		"compute_gpu_cores": 20,
		"max_model_size_gb": 28,
		"max_concurrent": 3,
		"supported_models": ["gemma-4-4b", "gemma-4-26b"]
	}`
	reqReg := httptest.NewRequest("POST", "/api/v1/cluster/runners/register?cluster_id=cluster-alpha", strings.NewReader(regPayload))
	reqReg.Header.Set("X-Shared-Secret", sharedSecret)
	rrReg := httptest.NewRecorder()
	mux.ServeHTTP(rrReg, reqReg)
	if rrReg.Code != http.StatusOK {
		t.Fatalf("expected 200 on register, got %d: %s", rrReg.Code, rrReg.Body.String())
	}

	// Verify explicit MaxModelSizeGB (28, not 16) and MaxConcurrent (3, not 2) and cluster_id ("cluster-alpha") were honored
	runners, err := dbStore.GetRegisteredRunners(ctx)
	if err != nil || len(runners) != 1 {
		t.Fatalf("expected 1 registered runner, got %d (err=%v)", len(runners), err)
	}
	if runners[0].Node.MaxModelSizeGB != 28 || runners[0].Node.MaxConcurrent != 3 {
		t.Fatalf("expected explicit MaxModelSizeGB=28 and MaxConcurrent=3 to be preserved, got %d and %d", runners[0].Node.MaxModelSizeGB, runners[0].Node.MaxConcurrent)
	}
	if len(runners[0].AssignedClusters) != 1 || runners[0].AssignedClusters[0] != "cluster-alpha" {
		t.Fatalf("expected AssignedClusters=['cluster-alpha'], got %v", runners[0].AssignedClusters)
	}

	// 2. Admin edits the runner in the dashboard (via SaveRegisteredRunner):
	//    - Changes Name to "M4 Pro Managed"
	//    - Changes MemoryAllocatedGB to 64, ComputeGPUCores to 40
	//    - Caps MaxModelSizeGB to 12 (so gemma-4-26b [26GB] is blocked, gemma-4-4b [4GB] is allowed)
	//    - Sets MaxConcurrent to 1
	//    - Sets SupportedModels to ["gemma-4-4b", "gemma-4-26b"]
	//    - Reassigns AssignedClusters to ["cluster-beta"] (detaching cluster-alpha and NOT attached to local-cluster)
	adminUpdated := config.RunnerResponse{
		Node: config.Node{
			ID:                "runner-m4-01",
			Name:              "M4 Pro Managed",
			Status:            "online",
			MemoryAllocatedGB: 64,
			ComputeGPUCores:   40,
			MaxModelSizeGB:    12,
			MaxConcurrent:     1,
			SupportedModels:   []string{"gemma-4-4b", "gemma-4-26b"},
		},
		AssignedClusters: []string{"cluster-beta"},
	}
	if err := dbStore.SaveRegisteredRunner(ctx, adminUpdated); err != nil {
		t.Fatalf("failed to save admin updated runner: %v", err)
	}

	// 3. Runner sends a heartbeat claiming cluster_id="local-cluster"
	//    Heartbeat MUST NOT re-attach "local-cluster" to an existing runner, and must return updated limits & supported_models!
	reqHB := httptest.NewRequest("POST", "/api/v1/cluster/runners/heartbeat", strings.NewReader(`{"node_id":"runner-m4-01","cluster_id":"local-cluster"}`))
	reqHB.Header.Set("X-Shared-Secret", sharedSecret)
	rrHB := httptest.NewRecorder()
	mux.ServeHTTP(rrHB, reqHB)
	if rrHB.Code != http.StatusOK {
		t.Fatalf("expected 200 on heartbeat, got %d: %s", rrHB.Code, rrHB.Body.String())
	}

	var hbResp struct {
		Status           string   `json:"status"`
		MaxModelSizeGB   int      `json:"max_model_size_gb"`
		MaxConcurrent    int      `json:"max_concurrent"`
		SupportedModels  []string `json:"supported_models"`
		AssignedClusters []string `json:"assigned_clusters"`
	}
	if err := json.Unmarshal(rrHB.Body.Bytes(), &hbResp); err != nil {
		t.Fatalf("failed to decode heartbeat response: %v", err)
	}
	if hbResp.MaxModelSizeGB != 12 || hbResp.MaxConcurrent != 1 {
		t.Errorf("expected heartbeat to return MaxModelSizeGB=12, MaxConcurrent=1, got %+v", hbResp)
	}
	if len(hbResp.AssignedClusters) != 1 || hbResp.AssignedClusters[0] != "cluster-beta" {
		t.Errorf("expected heartbeat NOT to re-attach local-cluster; want ['cluster-beta'], got %v", hbResp.AssignedClusters)
	}
	if len(hbResp.SupportedModels) != 2 {
		t.Errorf("expected heartbeat to return supported_models, got %v", hbResp.SupportedModels)
	}

	// Also verify GET /api/v1/admin/runners returns the admin-edited Name, MemoryAllocatedGB, ComputeGPUCores
	reqList := httptest.NewRequest("GET", "/api/v1/admin/runners", nil)
	reqList.Header.Set("X-Shared-Secret", sharedSecret)
	rrList := httptest.NewRecorder()
	mux.ServeHTTP(rrList, reqList)
	var listResp []config.RunnerResponse
	_ = json.Unmarshal(rrList.Body.Bytes(), &listResp)
	if len(listResp) != 1 || listResp[0].Node.Name != "M4 Pro Managed" || listResp[0].Node.MemoryAllocatedGB != 64 || listResp[0].Node.ComputeGPUCores != 40 {
		t.Errorf("expected GET /api/v1/admin/runners to reflect admin-edited hardware/name, got %+v", listResp)
	}

	// 4. Enqueue a job for gemma-4-26b (26GB > MaxModelSizeGB 12GB) in cluster-beta
	_, _ = queue.Enqueue(&config.QueueJob{
		ID:        "job-26b-too-large",
		ClusterID: "cluster-beta",
		Model:     "gemma-4-26b",
		Priority:  "high",
		Payload:   []byte(`{"prompt":"too large"}`),
		CreatedAt: time.Now(),
	})

	// Runner polls claiming it supports gemma-4-26b -> must get 204 No Content because MaxModelSizeGB=12 filters out 26b!
	reqPoll26b := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{"node_id":"runner-m4-01","supported_models":["gemma-4-26b"]}`))
	reqPoll26b.Header.Set("X-Shared-Secret", sharedSecret)
	rrPoll26b := httptest.NewRecorder()
	mux.ServeHTTP(rrPoll26b, reqPoll26b)
	if rrPoll26b.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for gemma-4-26b exceeding MaxModelSizeGB=12, got %d: %s", rrPoll26b.Code, rrPoll26b.Body.String())
	}

	// 5. Enqueue two jobs for gemma-4-4b (4GB <= 12GB) in cluster-beta
	_, _ = queue.Enqueue(&config.QueueJob{
		ID:        "job-4b-1",
		ClusterID: "cluster-beta",
		Model:     "gemma-4-4b",
		Priority:  "high",
		Payload:   []byte(`{"prompt":"first"}`),
		CreatedAt: time.Now(),
	})
	_, _ = queue.Enqueue(&config.QueueJob{
		ID:        "job-4b-2",
		ClusterID: "cluster-beta",
		Model:     "gemma-4-4b",
		Priority:  "high",
		Payload:   []byte(`{"prompt":"second"}`),
		CreatedAt: time.Now(),
	})

	// First poll for gemma-4-4b succeeds (1 in-flight == MaxConcurrent 1)
	reqPoll1 := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{"node_id":"runner-m4-01","supported_models":["gemma-4-4b"]}`))
	reqPoll1.Header.Set("X-Shared-Secret", sharedSecret)
	rrPoll1 := httptest.NewRecorder()
	mux.ServeHTTP(rrPoll1, reqPoll1)
	if rrPoll1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on first poll for gemma-4-4b, got %d", rrPoll1.Code)
	}
	var polledJob1 config.QueueJob
	_ = json.Unmarshal(rrPoll1.Body.Bytes(), &polledJob1)
	if polledJob1.ID != "job-4b-1" {
		t.Errorf("expected job-4b-1, got %q", polledJob1.ID)
	}

	// Second poll while job-4b-1 is still in-flight MUST return 204 No Content because MaxConcurrent=1!
	reqPoll2 := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{"node_id":"runner-m4-01","supported_models":["gemma-4-4b"]}`))
	reqPoll2.Header.Set("X-Shared-Secret", sharedSecret)
	rrPoll2 := httptest.NewRecorder()
	mux.ServeHTTP(rrPoll2, reqPoll2)
	if rrPoll2.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content on second poll while 1 job is in-flight (MaxConcurrent=1), got %d: %s", rrPoll2.Code, rrPoll2.Body.String())
	}

	// Resolve job-4b-1 -> frees the concurrency slot
	reqResolve := httptest.NewRequest("POST", "/api/v1/cluster/queue/resolve", strings.NewReader(`{"job_id":"job-4b-1","status_code":200,"payload":"e30="}`))
	reqResolve.Header.Set("X-Shared-Secret", sharedSecret)
	rrResolve := httptest.NewRecorder()
	mux.ServeHTTP(rrResolve, reqResolve)
	if rrResolve.Code != http.StatusOK {
		t.Fatalf("expected 200 OK resolving job-4b-1, got %d", rrResolve.Code)
	}

	// Third poll now succeeds and pops job-4b-2!
	reqPoll3 := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{"node_id":"runner-m4-01","supported_models":["gemma-4-4b"]}`))
	reqPoll3.Header.Set("X-Shared-Secret", sharedSecret)
	rrPoll3 := httptest.NewRecorder()
	mux.ServeHTTP(rrPoll3, reqPoll3)
	if rrPoll3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on poll after resolving first job, got %d", rrPoll3.Code)
	}
	var polledJob2 config.QueueJob
	_ = json.Unmarshal(rrPoll3.Body.Bytes(), &polledJob2)
	if polledJob2.ID != "job-4b-2" {
		t.Errorf("expected job-4b-2, got %q", polledJob2.ID)
	}
}

func TestRunnerIsolationHeartbeatStalenessAndUnifiedQueueAPI(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())
	sharedSecret := "test-isolation-secret"
	t.Setenv("BACKEND_SHARED_SECRET", sharedSecret)

	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	cq := proxy.NewClusterQueue(20, 30*time.Second)
	defer cq.Close()
	apiCtrl := NewAPIController(cfgStore, nil, cq, sharedSecret)
	mux := http.NewServeMux()
	apiCtrl.RegisterRoutes(mux)

	// 1. Register via /api/v1/cluster/register alias
	regBody := `{"id":"node-iso-1","name":"Isolated Node","status":"online","supported_models":["gemma-4-4b"]}`
	reqReg := httptest.NewRequest("POST", "/api/v1/cluster/register?cluster_id=cluster-alpha", strings.NewReader(regBody))
	reqReg.Header.Set("X-Shared-Secret", sharedSecret)
	rrReg := httptest.NewRecorder()
	mux.ServeHTTP(rrReg, reqReg)
	if rrReg.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /api/v1/cluster/register alias, got %d", rrReg.Code)
	}

	// 2. Detach runner from all clusters (AssignedClusters = [])
	reqDetach := httptest.NewRequest("POST", "/api/v1/admin/runners/attach", strings.NewReader(`{"node_id":"node-iso-1","cluster_ids":[]}`))
	reqDetach.Header.Set("X-Shared-Secret", sharedSecret)
	rrDetach := httptest.NewRecorder()
	mux.ServeHTTP(rrDetach, reqDetach)
	if rrDetach.Code != http.StatusOK {
		t.Fatalf("expected 200 OK detaching runner, got %d", rrDetach.Code)
	}

	// 3. Re-registering the detached runner must NOT overwrite its empty AssignedClusters with default cluster!
	reqReReg := httptest.NewRequest("POST", "/api/v1/cluster/register?cluster_id=cluster-alpha", strings.NewReader(regBody))
	reqReReg.Header.Set("X-Shared-Secret", sharedSecret)
	rrReReg := httptest.NewRecorder()
	mux.ServeHTTP(rrReReg, reqReReg)

	// Enqueue a job on cluster-alpha and verify detached runner gets 204 No Content
	_, _ = cq.Enqueue(&config.QueueJob{
		ID:        "job-alpha-1",
		ClusterID: "cluster-alpha",
		AppID:     "app-1",
		Model:     "gemma-4-4b",
		Priority:  "high",
		CreatedAt: time.Now(),
	})
	reqPoll := httptest.NewRequest("POST", "/api/v1/cluster/queue/poll", strings.NewReader(`{"node_id":"node-iso-1","supported_models":["gemma-4-4b"]}`))
	reqPoll.Header.Set("X-Shared-Secret", sharedSecret)
	rrPoll := httptest.NewRecorder()
	mux.ServeHTTP(rrPoll, reqPoll)
	if rrPoll.Code != http.StatusNoContent {
		t.Fatalf("expected detached runner to receive 204 No Content on poll, got %d: %s", rrPoll.Code, rrPoll.Body.String())
	}

	// 4. Verify GET /api/v1/queue includes ClusterQueue snapshot items
	reqQueue := httptest.NewRequest("GET", "/api/v1/queue", nil)
	reqQueue.Header.Set("X-Shared-Secret", sharedSecret)
	rrQueue := httptest.NewRecorder()
	mux.ServeHTTP(rrQueue, reqQueue)
	if rrQueue.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/v1/queue, got %d", rrQueue.Code)
	}
	var qItems []config.QueueSnapshotItem
	_ = json.Unmarshal(rrQueue.Body.Bytes(), &qItems)
	if len(qItems) != 1 || qItems[0].ClusterID != "cluster-alpha" || qItems[0].QueueSource != "cluster" {
		t.Fatalf("expected /api/v1/queue to include cluster queue item for cluster-alpha, got %+v", qItems)
	}

	// 5. Verify 30s heartbeat staleness marks runner "offline" in GET /api/v1/admin/runners
	apiCtrl.runnersMu.Lock()
	staleNode := apiCtrl.registeredRunners["node-iso-1"]
	staleNode.LastHeartbeat = time.Now().Add(-45 * time.Second)
	staleNode.Status = "online"
	apiCtrl.registeredRunners["node-iso-1"] = staleNode
	apiCtrl.runnersMu.Unlock()

	reqList := httptest.NewRequest("GET", "/api/v1/admin/runners", nil)
	reqList.Header.Set("X-Shared-Secret", sharedSecret)
	rrList := httptest.NewRecorder()
	mux.ServeHTTP(rrList, reqList)
	var runners []config.RunnerResponse
	_ = json.Unmarshal(rrList.Body.Bytes(), &runners)
	foundOffline := false
	for _, r := range runners {
		if r.Node.ID == "node-iso-1" && r.Node.Status == "offline" {
			foundOffline = true
		}
	}
	if !foundOffline {
		t.Fatalf("expected runner with LastHeartbeat > 30s ago to be marked 'offline', got %+v", runners)
	}
}



