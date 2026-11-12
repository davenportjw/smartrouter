package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoreRunnerEngineExecutionLoop(t *testing.T) {
	// 1. Start mock local llama.cpp server
	var llamaCalled int32
	llamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llamaCalled, 1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("expected llama-server path to be /v1/chat/completions, got %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"choices": [
				{
					"message": {
						"role": "assistant",
						"content": "Completed text from mock llama.cpp!"
					}
				}
			]
		}`))
	}))
	defer llamaServer.Close()

	// 2. Start mock Router Backend Control Plane
	var heartbeatCount int32
	var pollCount int32
	var resolveCount int32
	var receivedPayload string

	routerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cluster/runners/register":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"registered"}`))
		case "/api/v1/cluster/runners/heartbeat":
			atomic.AddInt32(&heartbeatCount, 1)
			w.WriteHeader(http.StatusOK)
		case "/api/v1/cluster/queue/poll":
			count := atomic.AddInt32(&pollCount, 1)
			if count == 1 {
				// Return a mock queued job with Gemini GenAI format
				job := pollJob{
					ID:      "job-test-uuid",
					Model:   "gemma-4-4b",
					Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"who are you?"}]}]}`),
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(job)
			} else {
				w.WriteHeader(http.StatusNoContent) // subsequent poll attempts return 204
			}
		case "/api/v1/cluster/queue/resolve":
			atomic.AddInt32(&resolveCount, 1)
			var res struct {
				JobID      string `json:"job_id"`
				Payload    []byte `json:"payload"`
				StatusCode int    `json:"status_code"`
			}
			bodyBytes, _ := io.ReadAll(r.Body)
			json.Unmarshal(bodyBytes, &res)
			receivedPayload = string(res.Payload)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer routerServer.Close()

	// 3. Instantiate core runner engine
	runner := NewCoreRunnerEngine(routerServer.URL, []string{"gemma-4-4b"})
	runner.LlamaServerURL = llamaServer.URL

	// Start the polling/heartbeat loop in a background goroutine
	go runner.StartLoop()

	// Allow loop to run for a brief moment
	time.Sleep(1500 * time.Millisecond)

	// Stop the engine loop
	runner.Stop()

	// 4. Run Assertions
	if atomic.LoadInt32(&heartbeatCount) == 0 {
		t.Errorf("expected heartbeat requests to be fired by runner")
	}
	if atomic.LoadInt32(&pollCount) == 0 {
		t.Errorf("expected poll requests to be fired by runner")
	}
	if atomic.LoadInt32(&llamaCalled) == 0 {
		t.Errorf("expected runner to forward prompt to llama-server")
	}
	if atomic.LoadInt32(&resolveCount) == 0 {
		t.Errorf("expected runner to post resolution back to router control plane")
	}

	// Verify resolution body content contains translated Gemini candidate schema output!
	if !strings.Contains(receivedPayload, "Completed text from mock llama.cpp!") {
		t.Errorf("expected resolved payload to contain llama.cpp output text, got: %s", receivedPayload)
	}
	if !strings.Contains(receivedPayload, "candidates") {
		t.Errorf("expected output to be translated back to Gemini candidate structure, got: %s", receivedPayload)
	}

	// Verify runner stats updated
	stats := runner.GetStats()
	if stats.ProcessedCount != 1 {
		t.Errorf("expected stats.ProcessedCount to be 1, got %d", stats.ProcessedCount)
	}
	if stats.Status != "offline" {
		t.Errorf("expected final status to be 'offline', got %q", stats.Status)
	}
}

func TestCoreRunnerEngineFailureSurfaces502Error(t *testing.T) {
	t.Setenv("BACKEND_SHARED_SECRET", "runner-secret-xyz")
	t.Setenv("LOCAL_DEV", "true")

	var resolvedStatus int32
	var resolvedErr string
	var authHeaderSeen int32

	routerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Shared-Secret") == "runner-secret-xyz" {
			atomic.AddInt32(&authHeaderSeen, 1)
		}
		if r.URL.Path == "/api/v1/cluster/queue/resolve" {
			var res struct {
				JobID      string `json:"job_id"`
				StatusCode int    `json:"status_code"`
				Error      string `json:"error"`
			}
			bodyBytes, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(bodyBytes, &res)
			atomic.StoreInt32(&resolvedStatus, int32(res.StatusCode))
			resolvedErr = res.Error
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/v1/cluster/queue/poll" {
			job := pollJob{
				ID:      "job-fail-502",
				Model:   "gemma-4-4b",
				Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(job)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer routerServer.Close()

	runner := NewCoreRunnerEngine(routerServer.URL, []string{"gemma-4-4b"})
	// Point LlamaServerURL to a closed local port so local inference fails immediately
	closedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := closedServer.URL
	closedServer.Close()
	runner.LlamaServerURL = closedURL

	runner.pollAndProcess()

	if atomic.LoadInt32(&authHeaderSeen) < 2 {
		t.Fatalf("expected X-Shared-Secret header on poll and resolve calls, got %d", atomic.LoadInt32(&authHeaderSeen))
	}
	if atomic.LoadInt32(&resolvedStatus) != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway when llama-server and fallback fail, got %d", atomic.LoadInt32(&resolvedStatus))
	}
	if resolvedErr == "" || strings.Contains(resolvedErr, "Greetings!") {
		t.Fatalf("expected real error message instead of fake greeting, got %q", resolvedErr)
	}
}

func TestCoreRunnerEngineHeartbeatDynamicSyncAndConcurrency(t *testing.T) {
	routerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cluster/runners/heartbeat":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "alive",
				"max_model_size_gb": 24,
				"max_concurrent": 3,
				"supported_models": ["gemma-4-4b", "gemma-4-26b"],
				"assigned_clusters": ["cluster-prod-gpu"]
			}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer routerServer.Close()

	runner := NewCoreRunnerEngine(routerServer.URL, []string{"gemma-4-4b"})
	runner.sendHeartbeat()

	runner.mu.RLock()
	gotMaxModelSize := runner.MaxModelSizeGB
	gotMaxConcurrent := runner.MaxConcurrent
	gotClusterID := runner.ClusterID
	gotModels := append([]string(nil), runner.SupportedModels...)
	runner.mu.RUnlock()

	if gotMaxModelSize != 24 || gotMaxConcurrent != 3 {
		t.Fatalf("expected heartbeat to sync MaxModelSizeGB=24 and MaxConcurrent=3, got %d and %d", gotMaxModelSize, gotMaxConcurrent)
	}
	stats := runner.GetStats()
	if stats.MaxModelSizeGB != 24 || stats.MaxConcurrent != 3 {
		t.Fatalf("expected EngineStats to sync MaxModelSizeGB=24 and MaxConcurrent=3, got %d and %d", stats.MaxModelSizeGB, stats.MaxConcurrent)
	}
	if gotClusterID != "cluster-prod-gpu" {
		t.Fatalf("expected heartbeat to sync ClusterID='cluster-prod-gpu', got %q", gotClusterID)
	}
	if len(gotModels) != 2 || gotModels[1] != "gemma-4-26b" {
		t.Fatalf("expected heartbeat to sync SupportedModels=['gemma-4-4b', 'gemma-4-26b'], got %v", gotModels)
	}
}

func TestCoreRunnerEnginePreservesUsageMetadataAndSystemInstruction(t *testing.T) {
	var seenMessages []map[string]string
	llamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]string `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		seenMessages = req.Messages

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "8.05 kilometers"}}],
			"usage": {"prompt_tokens": 19, "completion_tokens": 4, "total_tokens": 23}
		}`))
	}))
	defer llamaServer.Close()

	runner := NewCoreRunnerEngine("http://localhost:8080", []string{"gemma-4-4b"})
	runner.LlamaServerURL = llamaServer.URL

	geminiPayload := []byte(`{
		"systemInstruction": {"parts": [{"text": "Reply concisely."}]},
		"contents": [{"role": "user", "parts": [{"text": "Convert 5 miles to km"}]}]
	}`)
	out, code, err := runner.forwardToLlamaCpp("gemma-4-4b", geminiPayload)
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected 200 OK from forwardToLlamaCpp, got code=%d err=%v", code, err)
	}
	if len(seenMessages) != 2 || seenMessages[0]["role"] != "system" || seenMessages[0]["content"] != "Reply concisely." {
		t.Fatalf("expected systemInstruction to map to OpenAI system message, got: %#v", seenMessages)
	}

	var parsed struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("failed unmarshalling translated Gemini response: %v", err)
	}
	if parsed.UsageMetadata.PromptTokenCount != 19 || parsed.UsageMetadata.CandidatesTokenCount != 4 || parsed.UsageMetadata.TotalTokenCount != 23 {
		t.Fatalf("expected usageMetadata (19/4/23) to be preserved from llama.cpp usage, got: %+v", parsed.UsageMetadata)
	}
	if len(parsed.Candidates) == 0 || parsed.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("expected finishReason='STOP', got: %+v", parsed.Candidates)
	}
}


