package main

import (
	"bytes"
	"context"
	"crypto/rand"
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
	testClientID  = "test-verify-client"
	testAppID     = "app-verify-client"
	testRuleID    = "rule-verify-high-priority"
	testClusterID = "cluster-verify-spillover"
	testRunnerID  = "runner-verify-offline"
)

func generateEphemeralVerifyKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gr_verify_" + hex.EncodeToString(buf), nil
}

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

	testAPIKey, err := generateEphemeralVerifyKey()
	if err != nil {
		log.Fatalf("[FAIL] Failed to generate ephemeral verification API key: %v", err)
	}

	// Pre-calculate key hash
	keyHash := hashKey(testAPIKey)

	// Snapshot existing providers/local doc (if any) so we can safely restore it after Case I
	var hadPriorLocalProvider bool
	var priorLocalProvider config.ProviderConfig
	if snap, snapErr := client.Collection("providers").Doc("local").Get(ctx); snapErr == nil && snap.Exists() {
		if dataErr := snap.DataTo(&priorLocalProvider); dataErr == nil {
			// Scrub any stale verification clusters from prior interrupted runs
			if priorLocalProvider.Regions != nil {
				if reg, ok := priorLocalProvider.Regions["local-cluster"]; ok && reg.LocalConfig != nil {
					filtered := make([]config.LocalCluster, 0, len(reg.LocalConfig.Clusters))
					for _, cl := range reg.LocalConfig.Clusters {
						if !strings.HasPrefix(cl.ID, "cluster-verify-") && !strings.HasPrefix(cl.ID, "eval-cluster-") {
							filtered = append(filtered, cl)
						}
					}
					reg.LocalConfig.Clusters = filtered
					priorLocalProvider.Regions["local-cluster"] = reg
				}
			}
			hadPriorLocalProvider = true
		}
	}

	// Define cleanup function to run at the end of tests
	cleanup := func() {
		log.Println("[INFO] Starting database cleanup...")

		// Restore or delete temporary local provider/runner fixture
		_, _ = client.Collection("runners").Doc(testRunnerID).Delete(ctx)
		if hadPriorLocalProvider {
			_, _ = client.Collection("providers").Doc("local").Set(ctx, priorLocalProvider)
		} else {
			_, _ = client.Collection("providers").Doc("local").Delete(ctx)
		}
		// Delete temporary gemma-4-26b model registration
		_, _ = client.Collection("models").Doc("gemma-4-26b@local-cluster").Delete(ctx)
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
	failf := func(format string, args ...interface{}) {
		cleanup()
		log.Fatalf(format, args...)
	}

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
			SimpleModel:     "gemini-3.5-flash-lite",
			MediumModel:     "gemini-3.8-flash",
			ComplexModel:    "gemini-4-argon",
			SimpleCharLimit: 50,  // under 50 characters is "simple" -> gemini-3.5-flash-lite
			MediumCharLimit: 200, // under 200 characters is "medium" -> gemini-3.8-flash
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
		ModelPattern:   "gemini-custom-rule",
		ClientTier:     "premium",
		HeaderName:     "X-Route-Priority",
		HeaderValue:    "gold",
		TargetModel:    "gemini-3.8-flash",
		TargetLocation: "global",
		FallbackModel:  "gemini-3.5-flash-lite",
		PriorityWeight: 5,
	}
	_, err = client.Collection("routing_rules").Doc(testRuleID).Set(ctx, dbRule)
	if err != nil {
		log.Fatalf("[FAIL] Failed to seed routing rule: %v", err)
	}

	// 5. Seed Hybrid Local Cluster + Offline Runner Fixture for Case I (Zero-Runner Spillover)
	log.Println("[INFO] Seeding hybrid local cluster spillover fixture...")
	offlineNode := config.Node{
		ID:              testRunnerID,
		Name:            "Verification Offline Apple Silicon Runner",
		Status:          "offline",
		LastHeartbeat:   time.Now().Add(-2 * time.Minute),
		SupportedModels: []string{"gemma-4-26b"},
	}
	localProv := priorLocalProvider
	localProv.ID = config.ProviderLocal
	if localProv.DisplayName == "" {
		localProv.DisplayName = "Local Open-Weights Runners"
	}
	localProv.Enabled = true
	if localProv.Regions == nil {
		localProv.Regions = make(map[string]config.RegionConfig)
	}
	localReg := localProv.Regions["local-cluster"]
	localReg.Code = "local-cluster"
	localReg.Active = true
	if localReg.LocalConfig == nil {
		localReg.LocalConfig = &config.LocalConfig{}
	}
	localReg.LocalConfig.Clusters = append(localReg.LocalConfig.Clusters, config.LocalCluster{
		ID:               testClusterID,
		Name:             "Verification Hybrid Spillover Cluster",
		QueueName:        testClusterID,
		MaxQueueAge:      15 * time.Second,
		FallbackModel:    "gemini-3.8-flash",
		FallbackLocation: "global",
		Nodes: map[string]config.Node{
			testRunnerID: offlineNode,
		},
	})
	localProv.Regions["local-cluster"] = localReg
	if _, err := client.Collection("providers").Doc("local").Set(ctx, localProv); err != nil {
		log.Fatalf("[FAIL] Failed to seed local provider fixture: %v", err)
	}
	if _, err := client.Collection("runners").Doc(testRunnerID).Set(ctx, config.RunnerResponse{
		Node:             offlineNode,
		AssignedClusters: []string{testClusterID},
	}); err != nil {
		log.Fatalf("[FAIL] Failed to seed offline runner fixture: %v", err)
	}
	if _, err := client.Collection("models").Doc("gemma-4-26b@local-cluster").Set(ctx, config.ModelConfig{
		ID:          "gemma-4-26b",
		DisplayName: "Gemma 4 26B (Hybrid Local/Cloud)",
		Location:    "local-cluster",
		Type:        "local",
		Active:      true,
	}); err != nil {
		log.Fatalf("[FAIL] Failed to seed gemma-4-26b model fixture: %v", err)
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

	// Test Case B: Rules-Based Routing (Rewrite gemini-custom-rule -> gemini-3.8-flash)
	log.Println("--- [TEST] Case B: Rules-Based Header Routing (gemini-custom-rule -> gemini-3.8-flash) ---")
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-custom-rule:generateContent", serviceURL), bytes.NewReader(payloadBytes))
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
	if routedModel != "gemini-3.8-flash" {
		log.Fatalf("[FAIL] Case B expected Routed Model 'gemini-3.8-flash', got '%s'", routedModel)
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

	// Case D1: Short Prompt (under 50 characters) should target gemini-3.5-flash-lite (SimpleModel)
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
	if routedModelD1 != "gemini-3.5-flash-lite" {
		log.Fatalf("[FAIL] Case D1 expected Routed Model 'gemini-3.5-flash-lite', got '%s'", routedModelD1)
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

	// Case D2: Medium Prompt (over 50 but under 200 characters) should target gemini-3.8-flash (MediumModel)
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
	if routedModelD2 != "gemini-3.8-flash" {
		log.Fatalf("[FAIL] Case D2 expected Routed Model 'gemini-3.8-flash', got '%s'", routedModelD2)
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

	// Test Case E: OpenAI Spec Chat Completions (/v1/chat/completions with gemini-dynamic)
	log.Println("--- [TEST] Case E: OpenAI Spec Chat Completions (/v1/chat/completions) ---")
	payloadE := map[string]interface{}{
		"model": "gemini-dynamic",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "Reply in one short sentence."},
			map[string]interface{}{"role": "user", "content": "Hello!"},
		},
	}
	payloadBytesE, _ := json.Marshal(payloadE)

	reqE, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/chat/completions", serviceURL), bytes.NewReader(payloadBytesE))
	reqE.Header.Set("Content-Type", "application/json")
	reqE.Header.Set("x-goog-api-key", testAPIKey)
	reqE.Header.Set("X-Client-App-ID", testAppID)
	if oidcToken != "" {
		reqE.Header.Set("Authorization", "Bearer "+oidcToken)
	} else {
		reqE.Header.Set("Authorization", "Bearer "+testAPIKey)
	}

	respE, err := httpClient.Do(reqE)
	if err != nil {
		log.Fatalf("[FAIL] Case E HTTP Request failed: %v", err)
	}
	bodyBytesE, _ := io.ReadAll(respE.Body)
	respE.Body.Close()

	if respE.StatusCode != http.StatusOK {
		log.Fatalf("[FAIL] Case E expected Status 200, got %d. Response: %s", respE.StatusCode, string(bodyBytesE))
	}

	var oaiRespE struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bodyBytesE, &oaiRespE); err != nil {
		log.Fatalf("[FAIL] Case E failed to parse OpenAI response JSON: %v (raw: %s)", err, string(bodyBytesE))
	}
	if oaiRespE.Object != "chat.completion" || len(oaiRespE.Choices) == 0 {
		log.Fatalf("[FAIL] Case E expected object='chat.completion' with non-empty choices, got: %s", string(bodyBytesE))
	}
	log.Printf("[PASS] Case E succeeded! OpenAI Model: %s, Assistant Reply: %s", oaiRespE.Model, strings.TrimSpace(oaiRespE.Choices[0].Message.Content))

	// Test Case F: OpenAI Spec Models List (GET /v1/models)
	log.Println("--- [TEST] Case F: OpenAI Spec Models List (GET /v1/models) ---")
	reqF, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/models", serviceURL), nil)
	reqF.Header.Set("x-goog-api-key", testAPIKey)
	if oidcToken != "" {
		reqF.Header.Set("Authorization", "Bearer "+oidcToken)
	} else {
		reqF.Header.Set("Authorization", "Bearer "+testAPIKey)
	}

	respF, err := httpClient.Do(reqF)
	if err != nil {
		log.Fatalf("[FAIL] Case F HTTP Request failed: %v", err)
	}
	bodyBytesF, _ := io.ReadAll(respF.Body)
	respF.Body.Close()

	if respF.StatusCode != http.StatusOK {
		log.Fatalf("[FAIL] Case F expected Status 200, got %d. Response: %s", respF.StatusCode, string(bodyBytesF))
	}
	var modelsRespF struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyBytesF, &modelsRespF); err != nil || modelsRespF.Object != "list" || len(modelsRespF.Data) == 0 {
		log.Fatalf("[FAIL] Case F invalid /v1/models payload: %s", string(bodyBytesF))
	}
	log.Printf("[PASS] Case F succeeded! Listed %d models via /v1/models", len(modelsRespF.Data))

	// Test Case G: Gemini Decisions API Semantic Routing (X-Router-Engine: gemini-decision)
	log.Println("--- [TEST] Case G: Gemini Decisions API Semantic Routing (gemini-decision-1) ---")
	startG1 := time.Now()
	reqG, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-dynamic:generateContent", serviceURL), bytes.NewReader(payloadBytesD1))
	reqG.Header.Set("Content-Type", "application/json")
	reqG.Header.Set("x-goog-api-key", testAPIKey)
	reqG.Header.Set("X-Client-App-ID", testAppID)
	reqG.Header.Set("X-Router-Engine", "gemini-decision")
	if oidcToken != "" {
		reqG.Header.Set("Authorization", "Bearer "+oidcToken)
	}

	respG, err := httpClient.Do(reqG)
	if err != nil {
		log.Fatalf("[FAIL] Case G HTTP Request failed: %v", err)
	}
	bodyBytesG, _ := io.ReadAll(respG.Body)
	respG.Body.Close()
	elapsedG1 := time.Since(startG1).Milliseconds()

	if respG.StatusCode != http.StatusOK {
		log.Fatalf("[FAIL] Case G expected Status 200, got %d. Response: %s", respG.StatusCode, string(bodyBytesG))
	}
	engineUsedG := respG.Header.Get("X-Router-Engine-Used")
	routedModelG := respG.Header.Get("X-Routed-Model")
	tierG := respG.Header.Get("X-Router-Complexity-Tier")
	confG := respG.Header.Get("X-Router-Confidence")
	localEligG := respG.Header.Get("X-Router-Local-Eligible")
	if engineUsedG != "gemini-decision" {
		log.Fatalf("[FAIL] Case G expected X-Router-Engine-Used 'gemini-decision', got '%s' (error=%s)", engineUsedG, respG.Header.Get("X-Router-Decision-Error"))
	}
	log.Printf("[PASS] Case G1 (Simple) succeeded in %dms! Engine: %s, Tier: %s, Routed Model: %s, Confidence: %s, LocalEligible: %s", elapsedG1, engineUsedG, tierG, routedModelG, confG, localEligG)

	// Case G2: Complex multi-step architecture prompt via gemini-decision-1
	payloadG2 := map[string]interface{}{
		"contents": []interface{}{
			map[string]interface{}{
				"role": "user",
				"parts": []interface{}{
					map[string]interface{}{
						"text": "Design a multi-region Byzantine-fault-tolerant consensus state machine in Go with formal safety invariants, Raft log compaction trade-offs, and tail-latency bounds under asymmetric network partitions. Reply in two concise sentences.",
					},
				},
			},
		},
	}
	payloadBytesG2, _ := json.Marshal(payloadG2)
	startG2 := time.Now()
	reqG2, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemini-dynamic:generateContent", serviceURL), bytes.NewReader(payloadBytesG2))
	reqG2.Header.Set("Content-Type", "application/json")
	reqG2.Header.Set("x-goog-api-key", testAPIKey)
	reqG2.Header.Set("X-Client-App-ID", testAppID)
	reqG2.Header.Set("X-Router-Engine", "gemini-decision")
	if oidcToken != "" {
		reqG2.Header.Set("Authorization", "Bearer "+oidcToken)
	}
	respG2, err := httpClient.Do(reqG2)
	if err != nil {
		log.Fatalf("[FAIL] Case G2 HTTP Request failed: %v", err)
	}
	bodyBytesG2, _ := io.ReadAll(respG2.Body)
	respG2.Body.Close()
	elapsedG2 := time.Since(startG2).Milliseconds()
	if respG2.StatusCode != http.StatusOK {
		failf("[FAIL] Case G2 expected Status 200, got %d. Response: %s", respG2.StatusCode, string(bodyBytesG2))
	}
	if respG2.Header.Get("X-Router-Engine-Used") != "gemini-decision" {
		failf("[FAIL] Case G2 expected X-Router-Engine-Used 'gemini-decision', got '%s' (error=%s)", respG2.Header.Get("X-Router-Engine-Used"), respG2.Header.Get("X-Router-Decision-Error"))
	}
	log.Printf("[PASS] Case G2 (Complex) succeeded in %dms! Engine: %s, Tier: %s, Routed Model: %s, Confidence: %s, LocalEligible: %s",
		elapsedG2,
		respG2.Header.Get("X-Router-Engine-Used"),
		respG2.Header.Get("X-Router-Complexity-Tier"),
		respG2.Header.Get("X-Routed-Model"),
		respG2.Header.Get("X-Router-Confidence"),
		respG2.Header.Get("X-Router-Local-Eligible"),
	)

	// Test Case H: Native Gemini Decisions API Pass-Through (POST /v1beta/decisions)
	log.Println("--- [TEST] Case H: Native Gemini Decisions API Pass-Through (POST /v1beta/decisions) ---")
	payloadH := map[string]interface{}{
		"model": "gemini-decision-1",
		"input": []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": "What is the capital of France?",
			},
		},
		"questions": map[string]interface{}{
			"complexity": map[string]interface{}{
				"type":         "choice",
				"instructions": "Classify the prompt into the most appropriate complexity tier.",
				"criteria": map[string]string{
					"simple":  "Short factual lookup or greeting",
					"medium":  "Standard explanation or summary",
					"complex": "Deep multi-step system architecture or formal proof",
				},
			},
		},
	}
	payloadBytesH, _ := json.Marshal(payloadH)

	startH := time.Now()
	reqH, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1beta/decisions", serviceURL), bytes.NewReader(payloadBytesH))
	reqH.Header.Set("Content-Type", "application/json")
	reqH.Header.Set("x-goog-api-key", testAPIKey)
	reqH.Header.Set("X-Client-App-ID", testAppID)
	if oidcToken != "" {
		reqH.Header.Set("Authorization", "Bearer "+oidcToken)
	}

	respH, err := httpClient.Do(reqH)
	if err != nil {
		failf("[FAIL] Case H HTTP Request failed: %v", err)
	}
	bodyBytesH, _ := io.ReadAll(respH.Body)
	respH.Body.Close()
	elapsedH := time.Since(startH).Milliseconds()

	if respH.StatusCode != http.StatusOK {
		failf("[FAIL] Case H expected Status 200, got %d. Response: %s", respH.StatusCode, string(bodyBytesH))
	}
	var decRespH struct {
		Answers map[string]interface{} `json:"answers"`
		Usage   struct {
			TotalTokens       int `json:"total_tokens"`
			TotalInputTokens  int `json:"total_input_tokens"`
			TotalOutputTokens int `json:"total_output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(bodyBytesH, &decRespH); err != nil || len(decRespH.Answers) == 0 {
		failf("[FAIL] Case H invalid /v1beta/decisions payload: %s", string(bodyBytesH))
	}
	if decRespH.Usage.TotalOutputTokens != 0 {
		failf("[FAIL] Case H expected 0 output tokens from gemini-decision-1, got %d", decRespH.Usage.TotalOutputTokens)
	}
	log.Printf("[PASS] Case H succeeded in %dms! InputTokens: %d, OutputTokens: %d, Answers: %v", elapsedH, decRespH.Usage.TotalInputTokens, decRespH.Usage.TotalOutputTokens, decRespH.Answers)

	// Test Case I: Bare Local Open-Weights Auto-Resolution + Zero-Runner Instant Cloud Spillover
	log.Println("--- [TEST] Case I: Bare gemma-4-26b Auto-Resolution + Zero-Runner Instant Cloud Spillover ---")
	startI := time.Now()
	reqI, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/models/gemma-4-26b:generateContent", serviceURL), bytes.NewReader(payloadBytesD1))
	reqI.Header.Set("Content-Type", "application/json")
	reqI.Header.Set("x-goog-api-key", testAPIKey)
	reqI.Header.Set("X-Client-App-ID", testAppID)
	if oidcToken != "" {
		reqI.Header.Set("Authorization", "Bearer "+oidcToken)
	}
	respI, err := httpClient.Do(reqI)
	if err != nil {
		failf("[FAIL] Case I HTTP Request failed: %v", err)
	}
	bodyBytesI, _ := io.ReadAll(respI.Body)
	respI.Body.Close()
	elapsedI := time.Since(startI).Milliseconds()
	if respI.StatusCode != http.StatusOK {
		failf("[FAIL] Case I expected Status 200 on cloud spillover, got %d. Response: %s", respI.StatusCode, string(bodyBytesI))
	}
	if respI.Header.Get("X-Cluster-Fallback-Reason") != "no_online_runners" || respI.Header.Get("X-Routed-Model") != "gemini-3.8-flash" {
		failf("[FAIL] Case I expected X-Cluster-Fallback-Reason=no_online_runners and X-Routed-Model=gemini-3.8-flash, got reason=%q model=%q",
			respI.Header.Get("X-Cluster-Fallback-Reason"), respI.Header.Get("X-Routed-Model"))
	}
	log.Printf("[PASS] Case I succeeded in %dms! Requested: gemma-4-26b -> Spillover Model: %s (Reason: %s)",
		elapsedI, respI.Header.Get("X-Routed-Model"), respI.Header.Get("X-Cluster-Fallback-Reason"))

	log.Println("============================================================")
	log.Println("👉 SUCCESS: ALL POST-DEPLOYMENT VERIFICATION SCENARIOS PASSED!")
	log.Println("============================================================")
}
