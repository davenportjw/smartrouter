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
	"os/exec"
	"strings"
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

// getGCPIDToken queries gcloud to fetch an OIDC identity token for the targeted backend audience.
// If standard user token generation fails, it automatically attempts service account impersonation.
func getGCPIDToken(ctx context.Context, audience string, projectID string) (string, error) {
	// 1. Try standard direct user identity token generation first
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-identity-token", fmt.Sprintf("--audiences=%s", audience))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil {
		return strings.TrimSpace(out.String()), nil
	}

	// 2. If standard direct call fails, fetch the OAuth2 access token from Application Default Credentials (ADC)
	log.Println("[INFO] Standard direct OIDC fetch failed. Fetching OAuth2 access token via ADC...")
	cmdAdc := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	var outAdc bytes.Buffer
	cmdAdc.Stdout = &outAdc
	if err := cmdAdc.Run(); err != nil {
		// Fallback to standard gcloud print-access-token
		outAdc.Reset()
		cmdAdc = exec.CommandContext(ctx, "gcloud", "auth", "print-access-token")
		cmdAdc.Stdout = &outAdc
		if err := cmdAdc.Run(); err != nil {
			return "", fmt.Errorf("failed to get gcloud access token: %w", err)
		}
	}

	accessToken := strings.TrimSpace(outAdc.String())

	// 3. Call the Google IAM Credentials API generateIdToken REST endpoint to get a signed OIDC ID token for the runner service account
	saEmail := fmt.Sprintf("gemini-router-runner@%s.iam.gserviceaccount.com", projectID)
	log.Printf("[INFO] Generating OIDC Identity token for Service Account: %s", saEmail)

	apiURL := fmt.Sprintf("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateIdToken", saEmail)
	reqBody, err := json.Marshal(map[string]interface{}{
		"audience":     audience,
		"includeEmail": true,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IAM Credentials API returned status %s: %s", resp.Status, string(respBody))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}

	return result.Token, nil
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

	var oidcToken string
	if os.Getenv("LOCAL_DEV") != "true" && strings.Contains(serviceURL, ".run.app") {
		log.Println("[INFO] Fetching Google OIDC Identity Token for Cloud Run IAM...")
		token, err := getGCPIDToken(ctx, serviceURL, projectID)
		if err != nil {
			log.Printf("[Warning] Failed to fetch identity token via gcloud: %v. Proceeding without it.", err)
		} else {
			oidcToken = token
			log.Println("[INFO] Successfully retrieved OIDC Identity Token.")
		}
	}
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

	// 2. Seed App configuration with Complexity Routing enabled
	log.Println("[INFO] Seeding application configuration...")
	dbApp := config.App{
		ID:       testAppID,
		ClientID: testClientID,
		Name:     "Verification App",
		RPM:      60,
		TPM:      40000,
		Priority: "medium",
		Complexity: config.ComplexityRouting{
			Enabled:         true,
			AlwaysOverride:  false,
			SimpleModel:     "gemini-2.5-flash-lite",
			MediumModel:     "gemini-2.5-flash",
			ComplexModel:    "gemini-2.5-pro",
			SimpleCharLimit: 50,  // under 50 characters is "simple" -> gemini-2.5-flash-lite
			MediumCharLimit: 200, // under 200 characters is "medium" -> gemini-2.5-flash
		},
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
	log.Println("[INFO] Syncing configuration store (waiting 5 seconds)...")
	time.Sleep(5 * time.Second)

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
	if oidcToken != "" {
		req.Header.Set("Authorization", "Bearer "+oidcToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Fatalf("[FAIL] Case A HTTP Request failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden {
			log.Fatalf("[FAIL] Case A expected Status 200, got 403 Forbidden. This means your local gcloud client lacks permission to invoke the Cloud Run backend. If deploying/testing locally, run 'gcloud auth application-default login' and ensure your email/domain is in ALLOWED_EMAIL_DOMAINS.")
		}
		log.Fatalf("[FAIL] Case A expected Status 200, got %d. Response: %s", resp.StatusCode, string(bodyBytes))
	}

	routedModel := resp.Header.Get("X-Routed-Model")
	log.Printf("[PASS] Case A succeeded! HTTP Status: %d. Routed Model: %s", resp.StatusCode, routedModel)
	if routedModel != "gemini-2.5-flash" {
		log.Fatalf("[FAIL] Case A expected Routed Model 'gemini-2.5-flash', got '%s'", routedModel)
	}

	// Parse and log Case A response text
	var geminiRespA struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(bodyBytes, &geminiRespA); err == nil && len(geminiRespA.Candidates) > 0 && len(geminiRespA.Candidates[0].Content.Parts) > 0 {
		log.Printf("[RESPONSE A] Generated Text: %s", strings.TrimSpace(geminiRespA.Candidates[0].Content.Parts[0].Text))
	} else {
		log.Printf("[RESPONSE A] Raw JSON: %s", string(bodyBytes))
	}

	// Test Case B: Rules-Based Routing (Rewrite gemini-1.5-pro -> gemini-2.5-pro)
	log.Println("--- [TEST] Case B: Rules-Based Header Routing (gemini-1.5-pro -> gemini-2.5-pro) ---")
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-1.5-pro:generateContent", serviceURL), bytes.NewReader(payloadBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", testAPIKey)
	req.Header.Set("X-Route-Priority", "gold")
	req.Header.Set("X-Client-App-ID", testAppID)
	if oidcToken != "" {
		req.Header.Set("Authorization", "Bearer "+oidcToken)
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		log.Fatalf("[FAIL] Case B HTTP Request failed: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden {
			log.Fatalf("[FAIL] Case B expected Status 200, got 403 Forbidden. This means your local gcloud client lacks permission to invoke the Cloud Run backend.")
		}
		log.Fatalf("[FAIL] Case B expected Status 200, got %d. Response: %s", resp.StatusCode, string(bodyBytes))
	}

	routedModel = resp.Header.Get("X-Routed-Model")
	log.Printf("[PASS] Case B succeeded! HTTP Status: %d. Routed Model: %s", resp.StatusCode, routedModel)
	if routedModel != "gemini-2.5-pro" {
		log.Fatalf("[FAIL] Case B expected Routed Model 'gemini-2.5-pro', got '%s'", routedModel)
	}

	// Parse and log Case B response text
	var geminiRespB struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(bodyBytes, &geminiRespB); err == nil && len(geminiRespB.Candidates) > 0 && len(geminiRespB.Candidates[0].Content.Parts) > 0 {
		log.Printf("[RESPONSE B] Generated Text: %s", strings.TrimSpace(geminiRespB.Candidates[0].Content.Parts[0].Text))
	} else {
		log.Printf("[RESPONSE B] Raw JSON: %s", string(bodyBytes))
	}

	// Test Case C: Unauthenticated Request (Missing Key)
	log.Println("--- [TEST] Case C: Unauthenticated Request (No Key) ---")
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-2.5-flash:generateContent", serviceURL), bytes.NewReader(payloadBytes))
	req.Header.Set("Content-Type", "application/json")
	if oidcToken != "" {
		req.Header.Set("Authorization", "Bearer "+oidcToken)
	}

	resp, err = httpClient.Do(req)
	if err != nil {
		log.Fatalf("[FAIL] Case C HTTP Request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		log.Fatalf("[FAIL] Case C expected Status 401, got %d", resp.StatusCode)
	}
	log.Printf("[PASS] Case C succeeded! HTTP Status: %d", resp.StatusCode)

	// Test Case D: Complexity-Based Dynamic Routing (gemini-dynamic)
	log.Println("--- [TEST] Case D: Complexity-Based Dynamic Routing (gemini-dynamic) ---")

	// Case D1: Short Prompt (under 50 characters) should target gemini-2.5-flash-lite (SimpleModel)
	log.Println("[INFO] Case D1: Executing simple query...")
	payloadD1 := map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"role": "user",
				"parts": []interface{}{
					map[string]interface{}{"text": "Hi!"},
				},
			},
		},
	}
	payloadBytesD1, _ := json.Marshal(payloadD1)

	reqD1, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-dynamic:generateContent", serviceURL), bytes.NewReader(payloadBytesD1))
	reqD1.Header.Set("Content-Type", "application/json")
	reqD1.Header.Set("x-goog-api-key", testAPIKey)
	reqD1.Header.Set("X-Client-App-ID", testAppID)
	if oidcToken != "" {
		reqD1.Header.Set("Authorization", "Bearer "+oidcToken)
	}

	respD1, err := httpClient.Do(reqD1)
	if err != nil {
		log.Fatalf("[FAIL] Case D1 HTTP Request failed: %v", err)
	}
	bodyBytesD1, _ := io.ReadAll(respD1.Body)
	respD1.Body.Close()

	if respD1.StatusCode != http.StatusOK {
		log.Fatalf("[FAIL] Case D1 expected Status 200, got %d. Response: %s", respD1.StatusCode, string(bodyBytesD1))
	}

	routedModelD1 := respD1.Header.Get("X-Routed-Model")
	log.Printf("[PASS] Case D1 succeeded! HTTP Status: %d. Routed Model: %s", respD1.StatusCode, routedModelD1)
	if routedModelD1 != "gemini-2.5-flash-lite" {
		log.Fatalf("[FAIL] Case D1 expected Routed Model 'gemini-2.5-flash-lite', got '%s'", routedModelD1)
	}

	var geminiRespD1 struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(bodyBytesD1, &geminiRespD1); err == nil && len(geminiRespD1.Candidates) > 0 && len(geminiRespD1.Candidates[0].Content.Parts) > 0 {
		log.Printf("[RESPONSE D1] Generated Text: %s", strings.TrimSpace(geminiRespD1.Candidates[0].Content.Parts[0].Text))
	}

	// Case D2: Medium Prompt (over 50 but under 200 characters) should target gemini-2.5-flash (MediumModel)
	log.Println("[INFO] Case D2: Executing medium query...")
	payloadD2 := map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"role": "user",
				"parts": []interface{}{
					map[string]interface{}{"text": "Please explain what a solar eclipse is in exactly one sentence for a young student."},
				},
			},
		},
	}
	payloadBytesD2, _ := json.Marshal(payloadD2)

	reqD2, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-dynamic:generateContent", serviceURL), bytes.NewReader(payloadBytesD2))
	reqD2.Header.Set("Content-Type", "application/json")
	reqD2.Header.Set("x-goog-api-key", testAPIKey)
	reqD2.Header.Set("X-Client-App-ID", testAppID)
	if oidcToken != "" {
		reqD2.Header.Set("Authorization", "Bearer "+oidcToken)
	}

	respD2, err := httpClient.Do(reqD2)
	if err != nil {
		log.Fatalf("[FAIL] Case D2 HTTP Request failed: %v", err)
	}
	bodyBytesD2, _ := io.ReadAll(respD2.Body)
	respD2.Body.Close()

	if respD2.StatusCode != http.StatusOK {
		log.Fatalf("[FAIL] Case D2 expected Status 200, got %d. Response: %s", respD2.StatusCode, string(bodyBytesD2))
	}

	routedModelD2 := respD2.Header.Get("X-Routed-Model")
	log.Printf("[PASS] Case D2 succeeded! HTTP Status: %d. Routed Model: %s", respD2.StatusCode, routedModelD2)
	if routedModelD2 != "gemini-2.5-flash" {
		log.Fatalf("[FAIL] Case D2 expected Routed Model 'gemini-2.5-flash', got '%s'", routedModelD2)
	}

	var geminiRespD2 struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(bodyBytesD2, &geminiRespD2); err == nil && len(geminiRespD2.Candidates) > 0 && len(geminiRespD2.Candidates[0].Content.Parts) > 0 {
		log.Printf("[RESPONSE D2] Generated Text: %s", strings.TrimSpace(geminiRespD2.Candidates[0].Content.Parts[0].Text))
	}

	log.Println("============================================================")
	log.Println("👉 SUCCESS: ALL POST-DEPLOYMENT VERIFICATION SCENARIOS PASSED!")
	log.Println("============================================================")
}
