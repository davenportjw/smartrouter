package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	store "geminirouter/backend/config"
	"geminirouter/pkg/config"
)

func genThrowawayKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("failed generating random test key: %v", err)
	}
	return "gr_test_" + hex.EncodeToString(b)
}

// TestTDD_GeminiDecisionsEngine_InitialSemanticRouting_LocalAndCloud verifies end-to-end through
// RouterProxy.ServeHTTP that the new Gemini Decisions API (gemini-decision-1) performs single-pass
// semantic routing and automatically resolves bare local models (gemma-4-4b) to an active LocalCluster
// while routing complex prompts to cloud Vertex AI (gemini-4-argon@global).
func TestTDD_GeminiDecisionsEngine_InitialSemanticRouting_LocalAndCloud(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	cfgStore, err := store.NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	// Mock Gemini Decisions API (/v1beta/decisions)
	decisionsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/decisions" {
			t.Errorf("expected /v1beta/decisions path, got %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var parsedReq struct {
			Input interface{} `json:"input"`
		}
		_ = json.Unmarshal(raw, &parsedReq)
		inputJSON, _ := json.Marshal(parsedReq.Input)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(inputJSON), "race condition") {
			_, _ = w.Write([]byte(`{
				"answers": {
					"complexity": {"type":"choice","choice":"complex","probabilities":{"complex":0.93,"medium":0.06,"simple":0.01},"confidence":0.93},
					"task_kind": {"type":"choice","choice":"debugging","probabilities":{"debugging":0.91,"code_generation":0.06,"reasoning":0.03},"confidence":0.91},
					"local_eligible": {"type":"noul","noul":0.04,"confidence":0.96}
				},
				"usage": {"total_input_tokens": 112, "total_output_tokens": 0, "total_tokens": 112}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"answers": {
				"complexity": {"type":"choice","choice":"simple","probabilities":{"complex":0.01,"medium":0.03,"simple":0.96},"confidence":0.96},
				"task_kind": {"type":"choice","choice":"fast_query","probabilities":{"fast_query":0.95,"general":0.05},"confidence":0.95},
				"local_eligible": {"type":"noul","noul":0.97,"confidence":0.97}
			},
			"usage": {"total_input_tokens": 84, "total_output_tokens": 0, "total_tokens": 84}
		}`))
	}))
	defer decisionsSrv.Close()

	// Mock Vertex AI Cloud Backend
	var cloudPathSeen string
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudPathSeen = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Fixed Go worker pool mutex race."}]}}],"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":25,"totalTokenCount":65}}`))
	}))
	defer cloudSrv.Close()

	// Register Local Cluster ("mac-studio-pool") + online runner serving bare "gemma-4-4b"
	_ = cfgStore.SaveProvider(ctx, config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local Compute",
		Enabled:     true,
		Regions: map[string]config.RegionConfig{
			"local-cluster": {
				Code:   "local-cluster",
				Active: true,
				LocalConfig: &config.LocalConfig{
					Clusters: []config.LocalCluster{
						{
							ID:          "mac-studio-pool",
							Name:        "Mac Studio M4 Max Pool",
							QueueName:   "mac-studio-q",
							MaxQueueAge: 5 * time.Second,
						},
					},
				},
			},
		},
	})
	_ = cfgStore.SaveRegisteredRunner(ctx, config.RunnerResponse{
		Node: config.Node{
			ID:                "runner-m4-1",
			Name:              "Mac Studio M4",
			Status:            "online",
			LastHeartbeat:     time.Now(),
			MemoryAllocatedGB: 64,
			ComputeGPUCores:   40,
			SupportedModels:   []string{"gemma-4-4b"},
			MaxModelSizeGB:    32,
			MaxConcurrent:     4,
		},
		AssignedClusters: []string{"mac-studio-pool"},
	})

	apiKey := genThrowawayKey(t)
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "cl-hybrid", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{
		ID:       "app-hybrid",
		ClientID: "cl-hybrid",
		Name:     "Hybrid Router App",
		Priority: "high",
		RPM:      100,
		TPM:      100000,
		Complexity: config.ComplexityRouting{
			Enabled:                true,
			Engine:                 "gemini-decision",
			GeminiDecisionEndpoint: decisionsSrv.URL + "/v1beta/decisions",
			SimpleModel:            "gemma-4-4b", // bare local model name (no @mac-studio-pool suffix!)
			MediumModel:            "gemini-3.8-flash",
			ComplexModel:           "gemini-4-argon",
		},
	})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey(apiKey), AppID: "app-hybrid", ClientID: "cl-hybrid", Status: "active"})

	q := NewClusterQueue(10, 5*time.Second)
	defer q.Close()
	rp, err := NewRouterProxy(cfgStore, "test-project", "us-central1")
	if err != nil {
		t.Fatalf("failed to init proxy: %v", err)
	}
	rp.Queue = q
	rp.TokenSource = &mockTokenSource{}
	rp.GeminiDecisionEngine.AllowLoopbackForTesting = true
	cloudURL, _ := url.Parse(cloudSrv.URL)
	rp.Target = cloudURL
	rp.Proxy.Transport = &mockRoundTripper{Target: cloudURL}

	// Case 1: Simple query -> Gemini Decisions API -> bare gemma-4-4b auto-resolves to "mac-studio-pool"!
	reqSimple := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"Convert 5 miles to km"}]}]}`))
	reqSimple.Header.Set("x-goog-api-key", apiKey)
	rrSimple := httptest.NewRecorder()
	doneSimple := make(chan struct{})
	go func() {
		rp.ServeHTTP(rrSimple, reqSimple)
		close(doneSimple)
	}()

	var polledJob *config.QueueJob
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := q.Poll([]string{"gemma-4-4b"}, []string{"mac-studio-pool"}); ok && j != nil {
			polledJob = j
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if polledJob == nil {
		t.Fatalf("expected simple query to auto-resolve bare gemma-4-4b to mac-studio-pool queue (status=%d body=%s)", rrSimple.Code, rrSimple.Body.String())
	}
	q.Resolve(polledJob.ID, config.QueueResult{
		StatusCode: http.StatusOK,
		Payload:    []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"8.05 km"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":4,"totalTokenCount":16}}`),
	})
	<-doneSimple

	if rrSimple.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for simple local route, got %d: %s", rrSimple.Code, rrSimple.Body.String())
	}
	if rrSimple.Header().Get("X-Router-Engine-Used") != "gemini-decision" {
		t.Errorf("expected X-Router-Engine-Used='gemini-decision', got %q", rrSimple.Header().Get("X-Router-Engine-Used"))
	}
	if rrSimple.Header().Get("X-Routed-Model") != "gemma-4-4b" || rrSimple.Header().Get("X-Routed-Model-Location") != "mac-studio-pool" {
		t.Errorf("expected gemma-4-4b @ mac-studio-pool, got %q @ %q", rrSimple.Header().Get("X-Routed-Model"), rrSimple.Header().Get("X-Routed-Model-Location"))
	}
	if rrSimple.Header().Get("X-Router-Confidence") != "0.96" {
		t.Errorf("expected X-Router-Confidence='0.96', got %q", rrSimple.Header().Get("X-Router-Confidence"))
	}

	// Case 2: Complex query -> Gemini Decisions API -> Cloud Vertex AI gemini-4-argon@global!
	reqComplex := httptest.NewRequest("POST", "/v1/models/gemini-dynamic:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"Debug this race condition in our Go worker pool mutex"}]}]}`))
	reqComplex.Header.Set("x-goog-api-key", apiKey)
	rrComplex := httptest.NewRecorder()
	rp.ServeHTTP(rrComplex, reqComplex)

	if rrComplex.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for complex cloud route, got %d: %s", rrComplex.Code, rrComplex.Body.String())
	}
	if rrComplex.Header().Get("X-Routed-Model") != "gemini-4-argon" {
		t.Errorf("expected X-Routed-Model='gemini-4-argon', got %q", rrComplex.Header().Get("X-Routed-Model"))
	}
	if !strings.Contains(cloudPathSeen, "/locations/global/publishers/google/models/gemini-4-argon:generateContent") {
		t.Errorf("expected upstream cloud path for gemini-4-argon@global, got %q", cloudPathSeen)
	}
}

// TestTDD_LocalClusterZeroOnlineRunners_InstantCloudSpillover verifies that when a LocalCluster has
// FallbackModel configured and 0 online capable runners (e.g. runner heartbeat is stale >30s),
// requests spill over to cloud immediately instead of stalling for MaxQueueAge.
func TestTDD_LocalClusterZeroOnlineRunners_InstantCloudSpillover(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")

	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Instant cloud spillover succeeded"}]}}]}`))
	}))
	defer cloudSrv.Close()

	_ = cfgStore.SaveProvider(ctx, config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local Compute",
		Enabled:     true,
		Regions: map[string]config.RegionConfig{
			"local-cluster": {
				Code:   "local-cluster",
				Active: true,
				LocalConfig: &config.LocalConfig{
					Clusters: []config.LocalCluster{
						{
							ID:               "sleeping-laptop-pool",
							Name:             "Sleeping Laptop Pool",
							QueueName:        "laptop-q",
							MaxQueueAge:      10 * time.Second, // Would stall 10s without instant spillover!
							FallbackModel:    "gemini-3.8-flash",
							FallbackLocation: "global",
						},
					},
				},
			},
		},
	})
	// Runner last heartbeated 2 minutes ago (offline!)
	_ = cfgStore.SaveRegisteredRunner(ctx, config.RunnerResponse{
		Node: config.Node{
			ID:              "runner-stale",
			Name:            "Closed MacBook",
			Status:          "online",
			LastHeartbeat:   time.Now().Add(-2 * time.Minute),
			SupportedModels: []string{"gemma-4-4b"},
			MaxModelSizeGB:  16,
		},
		AssignedClusters: []string{"sleeping-laptop-pool"},
	})

	apiKey := genThrowawayKey(t)
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "cl-spill", Tier: "standard"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-spill", ClientID: "cl-spill", Priority: "high", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey(apiKey), AppID: "app-spill", ClientID: "cl-spill", Status: "active"})
	_ = cfgStore.SaveRule(ctx, config.RoutingRule{
		ID:             "rule-spill",
		ModelPattern:   "gemma-4-4b",
		ClientTier:     "all",
		TargetModel:    "gemma-4-4b",
		TargetLocation: "sleeping-laptop-pool",
		PriorityWeight: 10,
	})

	q := NewClusterQueue(10, 10*time.Second)
	defer q.Close()
	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.Queue = q
	rp.TokenSource = &mockTokenSource{}
	cloudURL, _ := url.Parse(cloudSrv.URL)
	rp.Target = cloudURL
	rp.Proxy.Transport = &mockRoundTripper{Target: cloudURL}

	start := time.Now()
	req := httptest.NewRequest("POST", "/v1/models/gemma-4-4b:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"Hello"}]}]}`))
	req.Header.Set("x-goog-api-key", apiKey)
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK via instant cloud spillover, got %d: %s", rr.Code, rr.Body.String())
	}
	if elapsed > 1*time.Second {
		t.Fatalf("expected instant cloud spillover (<1s) instead of waiting MaxQueueAge, took %v", elapsed)
	}
	if rr.Header().Get("X-Cluster-Fallback") != "true" {
		t.Errorf("expected X-Cluster-Fallback='true', got %q", rr.Header().Get("X-Cluster-Fallback"))
	}
	if rr.Header().Get("X-Cluster-Fallback-Reason") != "no_online_runners" {
		t.Errorf("expected X-Cluster-Fallback-Reason='no_online_runners', got %q", rr.Header().Get("X-Cluster-Fallback-Reason"))
	}
	if rr.Header().Get("X-Routed-Model") != "gemini-3.8-flash" {
		t.Errorf("expected fallback model gemini-3.8-flash, got %q", rr.Header().Get("X-Routed-Model"))
	}
}

// TestTDD_DirectEndpoint_CloudFailoverAndSSEStreaming verifies DirectEndpoint cloud failover when
// an unmanaged local server is unreachable AND real SSE streaming when healthy.
func TestTDD_DirectEndpoint_CloudFailoverAndSSEStreaming(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")

	// Closed server for unreachable DirectEndpoint failover test
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close()

	// Live streaming DirectEndpoint server
	streamDirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if streamFlag, _ := body["stream"].(bool); streamFlag {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"}}]}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"local stream!\"},\"finish_reason\":\"stop\"}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Non-stream ok"}}]}`))
	}))
	defer streamDirectSrv.Close()

	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Cloud rescued dead direct endpoint"}]}}]}`))
	}))
	defer cloudSrv.Close()

	_ = cfgStore.SaveProvider(ctx, config.ProviderConfig{
		ID:          config.ProviderLocal,
		DisplayName: "Local Compute",
		Enabled:     true,
		Regions: map[string]config.RegionConfig{
			"local-cluster": {
				Code:   "local-cluster",
				Active: true,
				LocalConfig: &config.LocalConfig{
					DirectEndpoints: []config.DirectEndpoint{
						{
							ID:               "dead-mlx-ep",
							Name:             "Unreachable MLX Box",
							ModelName:        "gemma-4-31b",
							URL:              deadURL,
							Engine:           "mlx",
							Active:           true,
							FallbackModel:    "gemini-3.8-flash",
							FallbackLocation: "global",
						},
						{
							ID:        "live-vllm-stream",
							Name:      "Live Streaming vLLM",
							ModelName: "gemma-4-26b",
							URL:       streamDirectSrv.URL,
							Engine:    "vllm",
							Active:    true,
						},
					},
				},
			},
		},
	})

	apiKey := genThrowawayKey(t)
	_ = cfgStore.DeleteRule(ctx, "rule-1")
	_ = cfgStore.DeleteHeader(ctx, "header-1")
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "cl-ep", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-ep", ClientID: "cl-ep", Priority: "high", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey(apiKey), AppID: "app-ep", ClientID: "cl-ep", Status: "active"})

	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	cloudURL, _ := url.Parse(cloudSrv.URL)
	rp.Target = cloudURL
	rp.Proxy.Transport = &mockRoundTripper{Target: cloudURL}

	// Subtest A: Unreachable DirectEndpoint for bare gemma-4-31b fails over to Cloud gemini-3.8-flash!
	reqFailover := httptest.NewRequest("POST", "/v1/models/gemma-4-31b:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"Test failover"}]}]}`))
	reqFailover.Header.Set("x-goog-api-key", apiKey)
	rrFailover := httptest.NewRecorder()
	rp.ServeHTTP(rrFailover, reqFailover)
	if rrFailover.Code != http.StatusOK {
		t.Fatalf("expected 200 OK via DirectEndpoint cloud failover, got %d: %s", rrFailover.Code, rrFailover.Body.String())
	}
	if rrFailover.Header().Get("X-Cluster-Fallback") != "true" {
		t.Errorf("expected X-Cluster-Fallback='true' on DirectEndpoint failover")
	}
	if !strings.Contains(rrFailover.Body.String(), "Cloud rescued dead direct endpoint") {
		t.Errorf("unexpected failover body: %s", rrFailover.Body.String())
	}

	// Subtest B1: OpenAI SSE streaming through DirectEndpoint
	reqStreamOAI := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemma-4-26b","stream":true,"messages":[{"role":"user","content":"Stream please"}]}`))
	reqStreamOAI.Header.Set("Authorization", "Bearer "+apiKey)
	rrStreamOAI := httptest.NewRecorder()
	rp.ServeHTTP(rrStreamOAI, reqStreamOAI)
	if rrStreamOAI.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for OpenAI SSE stream on DirectEndpoint, got %d: %s", rrStreamOAI.Code, rrStreamOAI.Body.String())
	}
	if !strings.Contains(rrStreamOAI.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("expected text/event-stream Content-Type, got %q", rrStreamOAI.Header().Get("Content-Type"))
	}
	if !strings.Contains(rrStreamOAI.Body.String(), "local stream!") || !strings.Contains(rrStreamOAI.Body.String(), "[DONE]") {
		t.Errorf("expected streamed OpenAI chunks + [DONE], got: %s", rrStreamOAI.Body.String())
	}

	// Subtest B2: Gemini :streamGenerateContent?alt=sse streaming through DirectEndpoint
	reqStreamGem := httptest.NewRequest("POST", "/v1/models/gemma-4-26b:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[{"parts":[{"text":"Stream Gemini"}]}]}`))
	reqStreamGem.Header.Set("x-goog-api-key", apiKey)
	rrStreamGem := httptest.NewRecorder()
	rp.ServeHTTP(rrStreamGem, reqStreamGem)
	if rrStreamGem.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for Gemini SSE stream on DirectEndpoint, got %d: %s", rrStreamGem.Code, rrStreamGem.Body.String())
	}
	if !strings.Contains(rrStreamGem.Body.String(), `"candidates"`) || !strings.Contains(rrStreamGem.Body.String(), "local stream!") {
		t.Errorf("expected translated Gemini SSE candidate chunks from DirectEndpoint stream, got: %s", rrStreamGem.Body.String())
	}
}

// TestTDD_DecisionsAPIPassthroughProxy verifies that client applications using google-genai
// `client.decisions.create(model="gemini-decision-1", ...)` can call POST /v1beta/decisions directly
// through Smart Router with App API keys, rate limiting, and keyless upstream ADC forwarding.
func TestTDD_DecisionsAPIPassthroughProxy(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")

	var upstreamAuth, upstreamProject, leakedAPIKey string
	decisionsUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		upstreamProject = r.Header.Get("x-goog-user-project")
		leakedAPIKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answers":{"is_damaged":{"type":"noul","noul":0.99,"confidence":0.99}},"usage":{"total_input_tokens":64,"total_output_tokens":0,"total_tokens":64}}`))
	}))
	defer decisionsUpstream.Close()

	apiKey := genThrowawayKey(t)
	_ = cfgStore.DeleteHeader(ctx, "header-1")
	_ = cfgStore.SaveClient(ctx, config.Client{ID: "cl-dec", Tier: "premium"})
	_ = cfgStore.SaveApp(ctx, config.App{ID: "app-dec", ClientID: "cl-dec", Priority: "high", RPM: 100, TPM: 100000})
	_ = cfgStore.SaveKey(ctx, config.APIKey{KeyHash: config.HashKey(apiKey), AppID: "app-dec", ClientID: "cl-dec", Status: "active"})

	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	rp.GeminiDecisionEngine.AllowLoopbackForTesting = true
	rp.GeminiDecisionEngine.DefaultEndpoint = decisionsUpstream.URL + "/v1beta/decisions"

	payload := `{"model":"gemini-decision-1","input":"The blue mug arrived cracked.","questions":{"is_damaged":{"type":"noul","instructions":"Did the item arrive damaged?"}}}`
	req := httptest.NewRequest("POST", "/v1beta/decisions", strings.NewReader(payload))
	req.Header.Set("x-goog-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /v1beta/decisions proxy, got %d: %s", rr.Code, rr.Body.String())
	}
	if leakedAPIKey != "" {
		t.Errorf("expected client x-goog-api-key to be scrubbed before forwarding upstream, got %q", leakedAPIKey)
	}
	if !strings.HasPrefix(upstreamAuth, "Bearer ") || upstreamProject != "test-project" {
		t.Errorf("expected upstream ADC Bearer + x-goog-user-project='test-project', got auth=%q proj=%q", upstreamAuth, upstreamProject)
	}
	if rr.Header().Get("X-Routed-Model") != "gemini-decision-1" {
		t.Errorf("expected X-Routed-Model='gemini-decision-1', got %q", rr.Header().Get("X-Routed-Model"))
	}
	if !strings.Contains(rr.Body.String(), `"noul":0.99`) {
		t.Errorf("expected calibrated decision answer in response body, got: %s", rr.Body.String())
	}
}

// TestTDD_InspectLLMClassifier_UsageMetadataAndUncachedEvaluation verifies that InspectLLMClassifier
// invokes the generative JSON LLM classifier (gemini-3.5-flash-lite) without cache short-circuiting
// and captures exact prompt and generated/thought output token counts for apples-to-apples evaluation.
func TestTDD_InspectLLMClassifier_UsageMetadataAndUncachedEvaluation(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("LOCAL_DATA_DIR", t.TempDir())

	ctx := context.Background()
	cfgStore, _ := store.NewConfigStore(ctx, "test-project")

	calls := 0
	llmUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload struct {
			Contents []struct {
				Role string `json:"role"`
			} `json:"contents"`
			SystemInstruction struct {
				Role string `json:"role"`
			} `json:"systemInstruction"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Vertex AI generateContent rejects contents[] entries without role="user"
		if len(payload.Contents) == 0 || payload.Contents[0].Role != "user" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Please use a valid role: user, model.","status":"INVALID_ARGUMENT"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"candidates": [{"content": {"parts": [{"text": "{\"complexity\":\"complex\"}"}]}}],
			"usageMetadata": {"promptTokenCount": 142, "candidatesTokenCount": 7, "thoughtsTokenCount": 4, "totalTokenCount": 153}
		}`))
	}))
	defer llmUpstream.Close()

	rp, _ := NewRouterProxy(cfgStore, "test-project", "us-central1")
	rp.TokenSource = &mockTokenSource{}
	u, _ := url.Parse(llmUpstream.URL)
	rp.Target = u

	compCfg := config.ComplexityRouting{
		Enabled:         true,
		Engine:          "llm",
		ClassifierModel: "gemini-3.5-flash-lite",
		SimpleModel:     "gemini-3.5-flash-lite",
		MediumModel:     "gemini-3.8-flash",
		ComplexModel:    "gemini-4-argon",
	}

	res1, err := rp.InspectLLMClassifier(ctx, "Find a blocking state in 2PC if coordinator dies right after prepare.", compCfg, false, false)
	if err != nil {
		t.Fatalf("unexpected error from InspectLLMClassifier: %v", err)
	}
	if res1.Tier != "complex" || res1.TargetModel != "gemini-4-argon" || res1.Engine != "llm" {
		t.Errorf("unexpected LLM inspect result: %+v", res1)
	}
	if res1.InputTokens != 142 || res1.OutputTokens != 11 {
		t.Errorf("expected InputTokens=142 OutputTokens=11 (7+4), got in=%d out=%d", res1.InputTokens, res1.OutputTokens)
	}
	if res1.ExecutionMode != "vertex_generative_json" {
		t.Errorf("expected ExecutionMode='vertex_generative_json', got %q", res1.ExecutionMode)
	}

	// Second call must execute live (not cache-hit) so latency/token benchmarks remain authentic
	_, err = rp.InspectLLMClassifier(ctx, "Find a blocking state in 2PC if coordinator dies right after prepare.", compCfg, false, false)
	if err != nil || calls != 2 {
		t.Errorf("expected uncached second live call (calls=2), got calls=%d err=%v", calls, err)
	}
}
