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
					Model:   "gemma2:2b",
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
	runner := NewCoreRunnerEngine(routerServer.URL, []string{"gemma2:2b"})
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
