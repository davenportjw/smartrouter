package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
)

var (
	routerURL   string
	clientKeys  = map[string]string{
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
		routerURL = "https://gemini-smart-router-txgsracloq-uc.a.run.app"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Seed random generator
	rand.Seed(time.Now().UnixNano())

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

		// Sleep for a randomized interval between 90 seconds and 240 seconds
		sleepSec := 90 + rand.Intn(150)
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

// executeSimulationRound selects a random client tier, model, headers, and sends requests
func executeSimulationRound() (string, error) {
	// 1. Choose Client Tier
	// Weighted choice: Premium (60%), Standard (25%), Free (10%), Invalid (5%)
	clientTier := "premium"
	roll := rand.Float64()
	if roll < 0.05 {
		clientTier = "invalid"
	} else if roll < 0.15 {
		clientTier = "free"
	} else if roll < 0.40 {
		clientTier = "standard"
	}

	// 2. Choose Model
	// Weighted choice: Flash (85%), Pro (15%)
	model := "gemini-2.5-flash"
	if rand.Float64() < 0.15 {
		model = "gemini-2.5-pro"
	}

	prompt := prompts[rand.Intn(len(prompts))]

	if clientTier == "invalid" {
		// Unauthorized call (Invalid API Key)
		return sendRequest("gr_key_bad_invalid_key", model, prompt, "script-runner-1", false)
	} else if clientTier == "free" {
		// Test Free Tier client
		// Occasionally (30% of the time) trigger a rate limit (429) by sending 6 requests in a quick burst!
		if rand.Float64() < 0.30 {
			log.Println("[Simulate] Simulating burst on Free Tier to trigger rate limits...")
			var burstSummary string
			for i := 0; i < 6; i++ {
				s, e := sendRequest(clientKeys["free"], model, prompt, "mobile-android", false)
				if e != nil {
					burstSummary += fmt.Sprintf("Request %d Error: %v; ", i+1, e)
				} else {
					burstSummary += fmt.Sprintf("Request %d: %s; ", i+1, s)
				}
				time.Sleep(50 * time.Millisecond)
			}
			return "Burst results: " + burstSummary, nil
		}
		return sendRequest(clientKeys["free"], model, prompt, "mobile-android", false)
	}

	// Premium or Standard
	// Roll for custom header validation:
	// - 80%: Valid header
	// - 10%: Invalid header value (triggers regex failure)
	// - 10%: Missing custom header (triggers required failure)
	headerRoll := rand.Float64()
	key := clientKeys[clientTier]
	appID := "prod-app-main"
	if clientTier == "standard" {
		appID = "stage-web-ui"
	}

	if headerRoll < 0.80 {
		return sendRequest(key, model, prompt, appID, false)
	} else if headerRoll < 0.90 {
		// Regex is ^[a-zA-Z0-9-]+$
		invalidAppID := "app_with_underscores!"
		return sendRequest(key, model, prompt, invalidAppID, false)
	}

	// Miss header entirely
	return sendRequest(key, model, prompt, "", true)
}

func sendRequest(apiKey, model, prompt, appIDHeader string, missHeader bool) (string, error) {
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

	if len(bodyStr) > 100 {
		bodyStr = bodyStr[:100] + "..."
	}

	summary := fmt.Sprintf("Model=%s, Status=%d, Latency=%dms, Response=%s", model, resp.StatusCode, latency, bodyStr)
	log.Printf("[Simulate] %s", summary)

	return summary, nil
}
