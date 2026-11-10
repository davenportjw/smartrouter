package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var (
	routerURL    string
	roundCounter uint64
	clientKeys   = map[string]string{
		"premium":  "gr_key_enterprise_123456789",
		"standard": "gr_key_standard_987654321",
		"free":     "gr_key_free_555555555",
	}
	prompts = []string{
		"What is the average distance to the moon?",
		"Give me a 3-step recipe for making chocolate chip cookies.",
		"Explain the difference between synchronous and asynchronous execution.",
		"Solve this math riddle: I am an odd number. Take away one letter and I become even. What number am I?",
		"Write a one-sentence tagline for a smart coffee mug.",
		"How many planets are in the solar system?",
		"What is the capital of France?",
		"Recommend a good science fiction book from the 1960s.",
		"Tell me a coding joke.",
		"List the primary colors of light.",
		"Write a short haiku about computer networks.",
	}
)

type GeminiPayload struct {
	Contents []Content `json:"contents"`
}

type Content struct {
	Parts []Part `json:"parts"`
}

type Part struct {
	Text string `json:"text"`
}

func main() {
	routerURL = os.Getenv("ROUTER_URL")
	if routerURL == "" {
		routerURL = "http://localhost:8080"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// 1. Start background continuous traffic simulation loop
	go runTrafficLoop()

	// 2. Start HTTP Server for health checks and manual triggers
	http.HandleFunc("/", handleTrigger)
	http.HandleFunc("/trigger", handleTrigger)

	log.Printf("Traffic Generator listening on port %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func runTrafficLoop() {
	log.Println("Background traffic simulation loop active.")

	// Wait a few seconds before initial round to let service settle
	time.Sleep(5 * time.Second)

	for {
		log.Println("[Background Loop] Starting periodic simulation round...")
		summary, err := executeSimulationRound()
		if err != nil {
			log.Printf("[Background Loop] Error executing round: %v", err)
		} else {
			log.Printf("[Background Loop] Round completed successfully: %s", summary)
		}

		// Deterministic interval cycle between 90 seconds and 240 seconds
		step := int(atomic.LoadUint64(&roundCounter))
		sleepSec := 90 + ((step * 37) % 150)
		log.Printf("[Background Loop] Sleeping for %d seconds...", sleepSec)
		time.Sleep(time.Duration(sleepSec) * time.Second)
	}
}

func handleTrigger(w http.ResponseWriter, r *http.Request) {
	// Support simple GET / or GET /healthz for health check probes
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "healthy",
			"message": "Gemini Smart Router Traffic Generator background worker is active.",
		})
		return
	}

	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Println("[Manual Trigger] HTTP request received. Triggering round...")
	summary, err := executeSimulationRound()

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"summary": summary,
	})
}

// executeSimulationRound selects a deterministic client tier, model, headers, and sends requests
func executeSimulationRound() (string, error) {
	step := int(atomic.AddUint64(&roundCounter, 1))

	// 1. Choose Client Tier deterministically (20-step cycle: 12 premium, 5 standard, 2 free, 1 invalid)
	clientTier := "premium"
	tierBucket := step % 20
	if tierBucket == 0 {
		clientTier = "invalid"
	} else if tierBucket <= 2 {
		clientTier = "free"
	} else if tierBucket <= 7 {
		clientTier = "standard"
	}

	// 2. Choose Model and Prompt deterministically
	var model string
	var prompt string
	var customHeaders map[string]string

	modelBucket := step % 20
	if modelBucket < 6 {
		model = "gemini-dynamic"
		// Vary prompt length to exercise simple/medium/complex complexity routing bands
		switch step % 3 {
		case 0:
			prompt = "Hi" // simple
		case 1:
			prompt = "Explain the core difference between sync and async coding." // medium
		default:
			prompt = "Write a highly complex concurrent thread-safe scheduler pipeline in Go, providing robust comments and analysis." // complex
		}
	} else if modelBucket < 9 {
		model = "gemini-3.1-pro"
		prompt = prompts[step%len(prompts)]
		// Set rule header on alternate steps to trigger the custom VIP rule
		if step%2 == 0 {
			customHeaders = map[string]string{"X-Route-Priority": "gold"}
		}
	} else if modelBucket < 17 {
		model = "gemini-3.8-flash"
		prompt = prompts[step%len(prompts)]
	} else {
		model = "gemini-3.1-pro"
		prompt = prompts[step%len(prompts)]
	}

	if clientTier == "invalid" {
		// Unauthorized call (Invalid API Key)
		return sendRequest("gr_key_bad_invalid_key", model, prompt, "script-runner-1", false, customHeaders)
	} else if clientTier == "free" {
		// Test Free Tier client
		// Every 3rd free tier round, trigger a rate limit (429) by sending 6 requests in a quick burst!
		if step%3 == 0 {
			log.Println("[Simulate] Simulating burst on Free Tier to trigger rate limits...")
			var burstSummary string
			for i := 0; i < 6; i++ {
				s, e := sendRequest(clientKeys["free"], model, prompt, "mobile-android", false, customHeaders)
				if e != nil {
					burstSummary += fmt.Sprintf("Request %d Error: %v; ", i+1, e)
				} else {
					burstSummary += fmt.Sprintf("Request %d: %s; ", i+1, s)
				}
				time.Sleep(50 * time.Millisecond)
			}
			return "Burst results: " + burstSummary, nil
		}
		return sendRequest(clientKeys["free"], model, prompt, "mobile-android", false, customHeaders)
	}

	// Premium or Standard
	// Deterministic cycle for custom header validation:
	// - 8/10: Valid header
	// - 1/10: Invalid header value (triggers regex failure)
	// - 1/10: Missing custom header (triggers required failure)
	headerBucket := step % 10
	key := clientKeys[clientTier]
	appID := "prod-app-main"
	if clientTier == "standard" {
		appID = "stage-web-ui"
	}

	if headerBucket < 8 {
		return sendRequest(key, model, prompt, appID, false, customHeaders)
	} else if headerBucket == 8 {
		// Regex is ^[a-zA-Z0-9-]+$
		invalidAppID := "app_with_underscores!"
		return sendRequest(key, model, prompt, invalidAppID, false, customHeaders)
	}

	// Miss header entirely
	return sendRequest(key, model, prompt, "", true, customHeaders)
}

func sendRequest(apiKey, model, prompt, appIDHeader string, missHeader bool, customHeaders map[string]string) (string, error) {
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", routerURL, model)

	payload := GeminiPayload{
		Contents: []Content{
			{
				Parts: []Part{
					{Text: prompt},
				},
			},
		},
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
	}

	if !missHeader {
		req.Header.Set("X-Client-App-ID", appIDHeader)
	}

	for k, v := range customHeaders {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	startTime := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute HTTP call: %w", err)
	}
	defer resp.Body.Close()

	latency := time.Since(startTime).Milliseconds()
	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	// Audit headers returned by the router
	routedModel := resp.Header.Get("X-Routed-Model")
	clientTier := resp.Header.Get("X-Client-Tier")

	if len(bodyStr) > 100 {
		bodyStr = bodyStr[:100] + "..."
	}

	summary := fmt.Sprintf("Model=%s (Routed=%s), Tier=%s, Status=%d, Latency=%dms, Response=%s", model, routedModel, clientTier, resp.StatusCode, latency, bodyStr)
	log.Printf("[Simulate] %s", summary)

	return summary, nil
}
