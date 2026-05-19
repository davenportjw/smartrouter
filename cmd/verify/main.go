package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/firestore"
	"geminirouter/pkg/config"
)

const (
	testClientID = "test-verify-client"
	testAppID    = "app-verify-client"
	testRuleID   = "rule-verify-high-priority"
	testAPIKey   = "gr_verify_post_deploy_key"
)

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func main() {
	log.Println("============================================================")
	log.Println("   S M A R T   R O U T E R   -   P O S T - D E P L O Y   T E S T")
	log.Println("============================================================")

	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		log.Fatalf("[FAIL] GOOGLE_CLOUD_PROJECT environment variable is not set.")
	}

	serviceURL := os.Getenv("SERVICE_URL")
	if serviceURL == "" {
		if len(os.Args) > 1 {
			serviceURL = os.Args[1]
		} else {
			log.Fatalf("[FAIL] SERVICE_URL is not specified. Pass as argument or set SERVICE_URL env.")
		}
	}

	log.Printf("Targeting Smart Router URL: %s", serviceURL)
	log.Printf("Using GCP Project ID      : %s", projectID)

	ctx := context.Background()
	client, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("[FAIL] Failed to create Firestore client: %v", err)
	}
	defer client.Close()

	// Pre-calculate key hash
	keyHash := hashKey(testAPIKey)

	// Define cleanup function to run at the end of tests
	cleanup := func() {
		log.Println("[INFO] Starting database cleanup...")
		
		// Delete test key
		_, _ = client.Collection("api_keys").Doc(keyHash).Delete(ctx)
		// Delete test rule
		_, _ = client.Collection("routing_rules").Doc(testRuleID).Delete(ctx)
		// Delete test app
		_, _ = client.Collection("apps").Doc(testAppID).Delete(ctx)
		// Delete test client
		_, _ = client.Collection("clients").Doc(testClientID).Delete(ctx)

		log.Println("[INFO] Database cleanup complete.")
	}

	// Ensure we clean up database entries regardless of outcome
	defer cleanup()

	// 1. Seed Client configuration
	log.Println("[INFO] Seeding client configuration...")
	dbClient := config.Client{
		ID:       testClientID,
		Name:     "Post-Deployment Verification Client",
		Tier:     "premium",
		RPM:      60,
		TPM:      40000,
		Priority: "medium",
	}
	_, err = client.Collection("clients").Doc(testClientID).Set(ctx, dbClient)
	if err != nil {
		log.Fatalf("[FAIL] Failed to seed client: %v", err)
	}

	// 2. Seed App configuration
	log.Println("[INFO] Seeding application configuration...")
	dbApp := config.App{
		ID:       testAppID,
		ClientID: testClientID,
		Name:     "Verification App",
		RPM:      60,
		TPM:      40000,
		Priority: "medium",
	}
	_, err = client.Collection("apps").Doc(testAppID).Set(ctx, dbApp)
	if err != nil {
		log.Fatalf("[FAIL] Failed to seed app: %v", err)
	}

	// 3. Seed API Key mapping
	log.Println("[INFO] Seeding API Key mapping...")
	dbKey := config.APIKey{
		KeyHash:  keyHash,
		ClientID: testClientID,
		AppID:    testAppID,
		Status:   "active",
	}
	_, err = client.Collection("api_keys").Doc(keyHash).Set(ctx, dbKey)
	if err != nil {
		log.Fatalf("[FAIL] Failed to seed API Key: %v", err)
	}

	// 4. Seed Routing Rule
	log.Println("[INFO] Seeding high-priority routing rule...")
	dbRule := config.RoutingRule{
		ID:             testRuleID,
		AppID:          testAppID,
		ModelPattern:   "gemini-1.5-pro",
		ClientTier:     "premium",
		HeaderName:     "X-Route-Priority",
		HeaderValue:    "gold",
		TargetModel:    "gemini-2.5-pro",
		TargetLocation: "us-central1",
		FallbackModel:  "gemini-2.5-flash",
		PriorityWeight: 5,
	}
	_, err = client.Collection("routing_rules").Doc(testRuleID).Set(ctx, dbRule)
	if err != nil {
		log.Fatalf("[FAIL] Failed to seed routing rule: %v", err)
	}

	// Allow a brief delay for listener sync
	log.Println("[INFO] Syncing configuration store (waiting 2 seconds)...")
	time.Sleep(2 * time.Second)

	// Execute Test Cases
	httpClient := &http.Client{Timeout: 35 * time.Second}

	// Test Case A: Valid Standard Query (gemini-2.5-flash)
	log.Println("--- [TEST] Case A: Standard Request (Targeting gemini-2.5-flash) ---")
	payload := map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"role": "user",
				"parts": []interface{}{
					map[string]interface{}{"text": "Explain gravity in one short sentence."},
				},
			},
		},
	}
	payloadBytes, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-2.5-flash:generateContent", serviceURL), bytes.NewReader(payloadBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", testAPIKey)
	req.Header.Set("X-Client-App-ID", testAppID)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Fatalf("[FAIL] Case A HTTP Request failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("[FAIL] Case A expected Status 200, got %d. Response: %s", resp.StatusCode, string(bodyBytes))
	}

	routedModel := resp.Header.Get("X-Routed-Model")
	log.Printf("[PASS] Case A succeeded! HTTP Status: %d. Routed Model: %s", resp.StatusCode, routedModel)
	if routedModel != "gemini-2.5-flash" {
		log.Fatalf("[FAIL] Case A expected Routed Model 'gemini-2.5-flash', got '%s'", routedModel)
	}

	// Test Case B: Rules-Based Routing (Rewrite gemini-1.5-pro -> gemini-2.5-pro)
	log.Println("--- [TEST] Case B: Rules-Based Header Routing (gemini-1.5-pro -> gemini-2.5-pro) ---")
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-1.5-pro:generateContent", serviceURL), bytes.NewReader(payloadBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", testAPIKey)
	req.Header.Set("X-Route-Priority", "gold")
	req.Header.Set("X-Client-App-ID", testAppID)

	resp, err = httpClient.Do(req)
	if err != nil {
		log.Fatalf("[FAIL] Case B HTTP Request failed: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("[FAIL] Case B expected Status 200, got %d. Response: %s", resp.StatusCode, string(bodyBytes))
	}

	routedModel = resp.Header.Get("X-Routed-Model")
	log.Printf("[PASS] Case B succeeded! HTTP Status: %d. Routed Model: %s", resp.StatusCode, routedModel)
	if routedModel != "gemini-2.5-pro" {
		log.Fatalf("[FAIL] Case B expected Routed Model 'gemini-2.5-pro', got '%s'", routedModel)
	}

	// Test Case C: Unauthenticated Request (Missing Key)
	log.Println("--- [TEST] Case C: Unauthenticated Request (No Key) ---")
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-2.5-flash:generateContent", serviceURL), bytes.NewReader(payloadBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err = httpClient.Do(req)
	if err != nil {
		log.Fatalf("[FAIL] Case C HTTP Request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		log.Fatalf("[FAIL] Case C expected Status 401, got %d", resp.StatusCode)
	}
	log.Printf("[PASS] Case C succeeded! HTTP Status: %d", resp.StatusCode)

	log.Println("============================================================")
	log.Println("👉 SUCCESS: ALL POST-DEPLOYMENT VERIFICATION SCENARIOS PASSED!")
	log.Println("============================================================")
}
