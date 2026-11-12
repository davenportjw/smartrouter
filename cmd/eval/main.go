package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/firestore"
	"geminirouter/pkg/config"
	"geminirouter/pkg/decision"
)

func generateEphemeralEvalKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gr_eval_" + hex.EncodeToString(buf), nil
}

func hashEvalKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func mintAdminOIDCToken(ctx context.Context, audience, projectID string) (string, error) {
	runnerSA := fmt.Sprintf("gemini-router-runner@%s.iam.gserviceaccount.com", projectID)
	adcCmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	adcOut, adcErr := adcCmd.Output()
	if adcErr == nil {
		accessToken := strings.TrimSpace(string(adcOut))
		if accessToken != "" {
			iamURL := fmt.Sprintf("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateIdToken", runnerSA)
			reqBody, _ := json.Marshal(map[string]interface{}{
				"audience":     audience,
				"includeEmail": true,
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, iamURL, bytes.NewReader(reqBody))
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+accessToken)
				req.Header.Set("Content-Type", "application/json")
				if projectID != "" {
					req.Header.Set("x-goog-user-project", projectID)
				}
				resp, doErr := (&http.Client{Timeout: 10 * time.Second}).Do(req)
				if doErr == nil {
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						var parsed struct {
							Token string `json:"token"`
						}
						if json.Unmarshal(body, &parsed) == nil && parsed.Token != "" {
							return parsed.Token, nil
						}
					}
				}
			}
		}
	}

	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-identity-token",
		"--impersonate-service-account="+runnerSA,
		"--audiences="+audience)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func main() {
	serviceURLFlag := flag.String("url", "", "Target Smart Router URL (defaults to ROUTER_URL or localhost:8080)")
	projectIDFlag := flag.String("project", "", "GCP Project ID (defaults to GOOGLE_CLOUD_PROJECT)")
	outJSONFlag := flag.String("out-json", "", "Optional path to write JSON evaluation report")
	outMDFlag := flag.String("out-md", "", "Optional path to write Markdown evaluation report")
	flag.Parse()

	serviceURL := strings.TrimRight(strings.TrimSpace(*serviceURLFlag), "/")
	if serviceURL == "" {
		serviceURL = strings.TrimRight(strings.TrimSpace(os.Getenv("ROUTER_URL")), "/")
	}
	if serviceURL == "" {
		serviceURL = "http://localhost:8080"
	}

	projectID := strings.TrimSpace(*projectIDFlag)
	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}
	if projectID == "" {
		projectID = "davenport-boutique"
	}

	ctx := context.Background()
	log.Printf("[EVAL] Targeting Smart Router: %s (project=%s)", serviceURL, projectID)

	var oidcToken string
	if strings.Contains(serviceURL, ".run.app") {
		tok, err := mintAdminOIDCToken(ctx, serviceURL, projectID)
		if err != nil {
			log.Fatalf("[EVAL] Failed to mint OIDC token via ADC impersonation: %v", err)
		}
		oidcToken = tok
	}

	fsClient, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("[EVAL] Failed to create Firestore client: %v", err)
	}
	defer fsClient.Close()

	runSuffixBytes := make([]byte, 4)
	_, _ = rand.Read(runSuffixBytes)
	runSuffix := hex.EncodeToString(runSuffixBytes)

	evalClientID := "eval-client-" + runSuffix
	evalAppID := "eval-app-" + runSuffix
	evalGuardedAppID := "eval-app-guarded-" + runSuffix
	evalRuleID := "eval-rule-hybrid-" + runSuffix
	evalClusterID := "eval-cluster-" + runSuffix
	evalRunnerID := "eval-runner-" + runSuffix

	evalAPIKey, err := generateEphemeralEvalKey()
	if err != nil {
		log.Fatalf("[EVAL] Failed to generate ephemeral API key: %v", err)
	}
	evalKeyHash := hashEvalKey(evalAPIKey)

	var hadPriorLocal bool
	var priorLocal config.ProviderConfig
	if snap, sErr := fsClient.Collection("providers").Doc("local").Get(ctx); sErr == nil && snap.Exists() {
		if snap.DataTo(&priorLocal) == nil {
			if priorLocal.Regions != nil {
				if reg, ok := priorLocal.Regions["local-cluster"]; ok && reg.LocalConfig != nil {
					filtered := make([]config.LocalCluster, 0, len(reg.LocalConfig.Clusters))
					for _, cl := range reg.LocalConfig.Clusters {
						if !strings.HasPrefix(cl.ID, "cluster-verify-") && !strings.HasPrefix(cl.ID, "eval-cluster-") {
							filtered = append(filtered, cl)
						}
					}
					reg.LocalConfig.Clusters = filtered
					priorLocal.Regions["local-cluster"] = reg
				}
			}
			hadPriorLocal = true
		}
	}

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			log.Println("[EVAL] Cleaning up ephemeral Firestore evaluation fixtures...")
			cCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = fsClient.Collection("api_keys").Doc(evalKeyHash).Delete(cCtx)
			_, _ = fsClient.Collection("routing_rules").Doc(evalRuleID).Delete(cCtx)
			_, _ = fsClient.Collection("apps").Doc(evalAppID).Delete(cCtx)
			_, _ = fsClient.Collection("apps").Doc(evalGuardedAppID).Delete(cCtx)
			_, _ = fsClient.Collection("clients").Doc(evalClientID).Delete(cCtx)
			_, _ = fsClient.Collection("runners").Doc(evalRunnerID).Delete(cCtx)
			_, _ = fsClient.Collection("models").Doc("gemma-4-26b@local-cluster").Delete(cCtx)
			if hadPriorLocal {
				_, _ = fsClient.Collection("providers").Doc("local").Set(cCtx, priorLocal)
			} else {
				_, _ = fsClient.Collection("providers").Doc("local").Delete(cCtx)
			}
			log.Println("[EVAL] Teardown complete.")
		})
	}
	defer cleanup()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(1)
	}()

	// Seed ephemeral eval fixtures
	log.Println("[EVAL] Seeding ephemeral client, app, hybrid local cluster, and conditional rule...")
	if _, err := fsClient.Collection("clients").Doc(evalClientID).Set(ctx, config.Client{
		ID:       evalClientID,
		Name:     "Adversarial Eval Harness Client",
		Tier:     "premium",
		RPM:      600,
		TPM:      500000,
		Priority: "high",
	}); err != nil {
		log.Fatalf("[EVAL] Failed to seed eval client: %v", err)
	}

	if _, err := fsClient.Collection("apps").Doc(evalAppID).Set(ctx, config.App{
		ID:       evalAppID,
		ClientID: evalClientID,
		Name:     "Adversarial Eval App",
		RPM:      600,
		TPM:      500000,
		Priority: "high",
		Complexity: config.ComplexityRouting{
			Enabled:                true,
			AlwaysOverride:         false,
			Engine:                 "gemini-decision",
			SimpleModel:            "gemini-3.5-flash-lite",
			MediumModel:            "gemini-3.8-flash",
			ComplexModel:           "gemini-4-argon",
			SimpleCharLimit:        80,
			MediumCharLimit:        240,
			ForceComplexTools:      true,
			ForceComplexMultimodal: true,
		},
	}); err != nil {
		log.Fatalf("[EVAL] Failed to seed eval app: %v", err)
	}

	if _, err := fsClient.Collection("apps").Doc(evalGuardedAppID).Set(ctx, config.App{
		ID:       evalGuardedAppID,
		ClientID: evalClientID,
		Name:     "Adversarial Eval App (MinConfidence=0.70)",
		RPM:      600,
		TPM:      500000,
		Priority: "high",
		Complexity: config.ComplexityRouting{
			Enabled:                true,
			AlwaysOverride:         false,
			Engine:                 "gemini-decision",
			MinConfidence:          0.70,
			SimpleModel:            "gemini-3.5-flash-lite",
			MediumModel:            "gemini-3.8-flash",
			ComplexModel:           "gemini-4-argon",
			SimpleCharLimit:        80,
			MediumCharLimit:        240,
			ForceComplexTools:      true,
			ForceComplexMultimodal: true,
		},
	}); err != nil {
		log.Fatalf("[EVAL] Failed to seed guarded eval app: %v", err)
	}

	if _, err := fsClient.Collection("api_keys").Doc(evalKeyHash).Set(ctx, config.APIKey{
		KeyHash:  evalKeyHash,
		ClientID: evalClientID,
		AppID:    evalAppID,
		Status:   "active",
	}); err != nil {
		log.Fatalf("[EVAL] Failed to seed eval API key: %v", err)
	}

	offlineNode := config.Node{
		ID:              evalRunnerID,
		Name:            "Eval Offline Gemma 4 Runner",
		Status:          "offline",
		LastHeartbeat:   time.Now().Add(-5 * time.Minute),
		SupportedModels: []string{"gemma-4-26b"},
	}
	localProv := priorLocal
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
		ID:               evalClusterID,
		Name:             "Eval Hybrid Spillover Pool",
		QueueName:        evalClusterID,
		MaxQueueAge:      15 * time.Second,
		FallbackModel:    "gemini-3.8-flash",
		FallbackLocation: "global",
		Nodes:            map[string]config.Node{evalRunnerID: offlineNode},
	})
	localProv.Regions["local-cluster"] = localReg
	if _, err := fsClient.Collection("providers").Doc("local").Set(ctx, localProv); err != nil {
		log.Fatalf("[EVAL] Failed to seed local provider: %v", err)
	}
	if _, err := fsClient.Collection("runners").Doc(evalRunnerID).Set(ctx, config.RunnerResponse{
		Node:             offlineNode,
		AssignedClusters: []string{evalClusterID},
	}); err != nil {
		log.Fatalf("[EVAL] Failed to seed offline runner: %v", err)
	}
	if _, err := fsClient.Collection("models").Doc("gemma-4-26b@local-cluster").Set(ctx, config.ModelConfig{
		ID:          "gemma-4-26b",
		DisplayName: "Gemma 4 26B",
		Location:    "local-cluster",
		Type:        "local",
		Active:      true,
	}); err != nil {
		log.Fatalf("[EVAL] Failed to seed gemma-4-26b model: %v", err)
	}

	// Stage-4 classifier-conditional hybrid rule: simple high-confidence prompts -> local gemma-4-26b cluster (spilling <1ms to cloud if offline)
	if _, err := fsClient.Collection("routing_rules").Doc(evalRuleID).Set(ctx, config.RoutingRule{
		ID:             evalRuleID,
		AppID:          evalAppID,
		ModelPattern:   "gemini-hybrid-eval",
		ClientTier:     "premium",
		DecisionEngine: "gemini-decision",
		ComplexityTier: "simple",
		MinConfidence:  0.65,
		TargetModel:    "gemma-4-26b",
		TargetLocation: evalClusterID,
		FallbackModel:  "gemini-3.8-flash",
		PriorityWeight: 20,
	}); err != nil {
		log.Fatalf("[EVAL] Failed to seed conditional hybrid rule: %v", err)
	}

	log.Println("[EVAL] Waiting 5s for real-time Firestore listener sync...")
	time.Sleep(5 * time.Second)

	httpClient := &http.Client{Timeout: 90 * time.Second}
	sharedSecret := strings.TrimSpace(os.Getenv("BACKEND_SHARED_SECRET"))

	attachAdminAuth := func(r *http.Request) {
		if oidcToken != "" {
			r.Header.Set("Authorization", "Bearer "+oidcToken)
		} else if sharedSecret != "" {
			r.Header.Set("X-Shared-Secret", sharedSecret)
		}
	}
	attachProxyAuth := func(r *http.Request) {
		r.Header.Set("x-goog-api-key", evalAPIKey)
		r.Header.Set("X-Client-App-ID", evalAppID)
		if oidcToken != "" {
			r.Header.Set("Authorization", "Bearer "+oidcToken)
		}
	}

	// Warm up ADC authorization key cache on gemini-decision before steady-state latency benchmarking
	warmPayload, _ := json.Marshal(map[string]interface{}{
		"app_id": evalAppID,
		"prompt": "Warmup ping",
		"engine": "gemini-decision",
	})
	warmReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, serviceURL+"/api/v1/complexity/inspect", bytes.NewReader(warmPayload))
	warmReq.Header.Set("Content-Type", "application/json")
	attachAdminAuth(warmReq)
	if warmResp, wErr := httpClient.Do(warmReq); wErr == nil {
		_, _ = io.Copy(io.Discard, warmResp.Body)
		warmResp.Body.Close()
	}

	corpus := BuildAdversarialCorpus()
	type engineSpec struct {
		Label  string
		Engine string
		AppID  string
	}
	engineSpecs := []engineSpec{
		{Label: "gemini-decision", Engine: "gemini-decision", AppID: evalAppID},
		{Label: "gemini-decision (min_conf=0.70)", Engine: "gemini-decision", AppID: evalGuardedAppID},
		{Label: "llm (gemini-3.5-flash-lite JSON)", Engine: "llm", AppID: evalAppID},
		{Label: "heuristics (char-bounds)", Engine: "heuristics", AppID: evalAppID},
	}

	// Probe live provider & secret status on the target router so unconfigured external services
	// (e.g. Jev without JEV_API_KEY, Fastino without FASTINO_API_KEY, or CLM without a cluster URL)
	// are never unfairly benchmarked via their offline local fallback stubs.
	var skippedEngines []SkippedEngine
	keysReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL+"/api/v1/complexity/keys", nil)
	attachAdminAuth(keysReq)
	if keysResp, kErr := httpClient.Do(keysReq); kErr == nil {
		kb, _ := io.ReadAll(keysResp.Body)
		keysResp.Body.Close()
		var parsedKeys struct {
			Providers []decision.EngineSecretStatus `json:"providers"`
		}
		if json.Unmarshal(kb, &parsedKeys) == nil {
			for _, st := range parsedKeys.Providers {
				engName := strings.ToLower(strings.TrimSpace(st.Engine))
				if engName == "gemini-decision" || engName == "" {
					continue
				}
				if ok, reason := IsExternalProviderBenchmarkable(st); ok {
					engineSpecs = append(engineSpecs, engineSpec{
						Label:  engName,
						Engine: engName,
						AppID:  evalAppID,
					})
				} else {
					log.Printf("[EVAL] %s (%s)", st.Engine, reason)
					skippedEngines = append(skippedEngines, SkippedEngine{
						Engine: st.Engine,
						Reason: reason,
					})
				}
			}
		}
	}
	engines := make([]string, 0, len(engineSpecs))
	for _, es := range engineSpecs {
		engines = append(engines, es.Label)
	}
	allObs := make(map[string][]CaseObservation, len(engineSpecs))

	log.Printf("[EVAL] Running %d adversarial prompts across %d engine configs (%d live classifications)...",
		len(corpus), len(engineSpecs), len(corpus)*len(engineSpecs))

	for _, es := range engineSpecs {
		obsList := make([]CaseObservation, 0, len(corpus))
		for _, tc := range corpus {
			reqBody, _ := json.Marshal(map[string]interface{}{
				"app_id":         es.AppID,
				"prompt":         tc.Prompt,
				"engine":         es.Engine,
				"has_tools":      tc.HasTools,
				"has_multimodal": tc.HasMultimodal,
			})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, serviceURL+"/api/v1/complexity/inspect", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			attachAdminAuth(req)

			start := time.Now()
			resp, doErr := httpClient.Do(req)
			elapsed := time.Since(start).Milliseconds()

			obs := CaseObservation{
				Case:      tc,
				LatencyMs: elapsed,
			}
			if doErr != nil {
				obs.Error = doErr.Error()
			} else {
				rawBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					obs.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawBody)))
				} else {
					var dr decision.DecisionResult
					if uErr := json.Unmarshal(rawBody, &dr); uErr != nil {
						obs.Error = uErr.Error()
					} else {
						obs.PredictedTier = dr.Tier
						obs.TargetModel = dr.TargetModel
						obs.ExecutionMode = dr.ExecutionMode
						obs.Confidence = dr.Confidence
						obs.Scores = dr.Scores
						obs.LocalEligibleProb = dr.LocalEligibleProb
						obs.HasLocalSignal = es.Engine == "gemini-decision"
						obs.InputTokens = dr.InputTokens
						obs.OutputTokens = dr.OutputTokens
						obs.Rationale = dr.Rationale
					}
				}
			}
			obsList = append(obsList, obs)
		}
		allObs[es.Label] = obsList
	}

	// Execute 7 Live End-to-End Proxy Stress Scenarios
	log.Println("[EVAL] Executing 7 live end-to-end proxy stress & hybrid failover scenarios...")
	scenarios := make([]ScenarioResult, 0, 7)

	runGeminiProxyCall := func(modelPath string, body map[string]interface{}, extraHeaders map[string]string) (*http.Response, []byte, int64, error) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s%s", serviceURL, modelPath), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		attachProxyAuth(req)
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		t0 := time.Now()
		resp, err := httpClient.Do(req)
		ms := time.Since(t0).Milliseconds()
		if err != nil {
			return nil, nil, ms, err
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, rb, ms, nil
	}

	// Scenario 1: Deceptive Brevity End-to-End (66-char 2PC crash question -> must route to gemini-4-argon)
	{
		resp, rb, ms, err := runGeminiProxyCall("/v1/models/gemini-dynamic:generateContent", map[string]interface{}{
			"contents": []interface{}{
				map[string]interface{}{
					"role":  "user",
					"parts": []interface{}{map[string]interface{}{"text": "Find a blocking state in 2PC if coordinator dies right after prepare. Reply in one sentence."}},
				},
			},
		}, map[string]string{"X-Router-Engine": "gemini-decision"})
		passed := err == nil && resp.StatusCode == http.StatusOK && resp.Header.Get("X-Routed-Model") == "gemini-4-argon" && resp.Header.Get("X-Router-Engine-Used") == "gemini-decision"
		detail := ""
		routed := ""
		if resp != nil {
			routed = resp.Header.Get("X-Routed-Model")
			detail = fmt.Sprintf("tier=%s conf=%s local_elig=%s", resp.Header.Get("X-Router-Complexity-Tier"), resp.Header.Get("X-Router-Confidence"), resp.Header.Get("X-Router-Local-Eligible"))
			if !passed {
				detail += " body=" + truncateStr(string(rb), 120)
			}
		} else if err != nil {
			detail = err.Error()
		}
		scenarios = append(scenarios, ScenarioResult{
			ID: "s1-deceptive-brevity-e2e", Name: "Deceptive Brevity E2E Route", Category: "adversarial_e2e",
			Passed: passed, LatencyMs: ms, RoutedModel: routed, Detail: detail,
		})
	}

	// Scenario 2: Verbose Boilerplate Cost Protection (620+ chars -> must route to gemini-3.5-flash-lite)
	{
		resp, rb, ms, err := runGeminiProxyCall("/v1/models/gemini-dynamic:generateContent", map[string]interface{}{
			"contents": []interface{}{
				map[string]interface{}{
					"role":  "user",
					"parts": []interface{}{map[string]interface{}{"text": corpus[4].Prompt}},
				},
			},
		}, map[string]string{"X-Router-Engine": "gemini-decision"})
		passed := err == nil && resp.StatusCode == http.StatusOK && resp.Header.Get("X-Routed-Model") == "gemini-3.5-flash-lite"
		routed := ""
		detail := ""
		if resp != nil {
			routed = resp.Header.Get("X-Routed-Model")
			detail = fmt.Sprintf("tier=%s conf=%s local_elig=%s", resp.Header.Get("X-Router-Complexity-Tier"), resp.Header.Get("X-Router-Confidence"), resp.Header.Get("X-Router-Local-Eligible"))
			if !passed {
				detail += " body=" + truncateStr(string(rb), 120)
			}
		} else if err != nil {
			detail = err.Error()
		}
		scenarios = append(scenarios, ScenarioResult{
			ID: "s2-verbose-boilerplate-e2e", Name: "Verbose Boilerplate Cost Guard", Category: "cost_efficiency",
			Passed: passed, LatencyMs: ms, RoutedModel: routed, Detail: detail,
		})
	}

	// Scenario 3: Tool-Call Forced Escalation on 16-char prompt
	{
		resp, rb, ms, err := runGeminiProxyCall("/v1/models/gemini-dynamic:generateContent", map[string]interface{}{
			"contents": []interface{}{
				map[string]interface{}{
					"role":  "user",
					"parts": []interface{}{map[string]interface{}{"text": "Check prod quota"}},
				},
			},
			"tools": []interface{}{
				map[string]interface{}{
					"functionDeclarations": []interface{}{
						map[string]interface{}{
							"name":        "get_quota_status",
							"description": "Returns active global RPM/TPM quota usage",
							"parameters": map[string]interface{}{
								"type":       "OBJECT",
								"properties": map[string]interface{}{},
							},
						},
					},
				},
			},
		}, map[string]string{"X-Router-Engine": "gemini-decision"})
		passed := err == nil && resp.StatusCode == http.StatusOK && resp.Header.Get("X-Routed-Model") == "gemini-4-argon"
		routed := ""
		detail := ""
		if resp != nil {
			routed = resp.Header.Get("X-Routed-Model")
			detail = fmt.Sprintf("ForceComplexTools -> tier=%s conf=%s", resp.Header.Get("X-Router-Complexity-Tier"), resp.Header.Get("X-Router-Confidence"))
			if !passed {
				detail += " body=" + truncateStr(string(rb), 120)
			}
		} else if err != nil {
			detail = err.Error()
		}
		scenarios = append(scenarios, ScenarioResult{
			ID: "s3-tool-call-escalation", Name: "16-Char Tool-Call Forced Escalation", Category: "policy_guardrail",
			Passed: passed, LatencyMs: ms, RoutedModel: routed, Detail: detail,
		})
	}

	// Scenario 4: Multimodal Inline PNG Escalation
	{
		// Valid 1x1 transparent PNG base64
		tinyPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z/C/HgAGgwJ/lK3Q6wAAAABJRU5ErkJggg=="
		resp, rb, ms, err := runGeminiProxyCall("/v1/models/gemini-dynamic:generateContent", map[string]interface{}{
			"contents": []interface{}{
				map[string]interface{}{
					"role": "user",
					"parts": []interface{}{
						map[string]interface{}{"text": "What color pixel is this? One word."},
						map[string]interface{}{"inlineData": map[string]interface{}{"mimeType": "image/png", "data": tinyPNG}},
					},
				},
			},
		}, map[string]string{"X-Router-Engine": "gemini-decision"})
		passed := err == nil && resp.StatusCode == http.StatusOK && resp.Header.Get("X-Routed-Model") == "gemini-4-argon"
		routed := ""
		detail := ""
		if resp != nil {
			routed = resp.Header.Get("X-Routed-Model")
			detail = fmt.Sprintf("ForceComplexMultimodal -> tier=%s", resp.Header.Get("X-Router-Complexity-Tier"))
			if !passed {
				detail += " body=" + truncateStr(string(rb), 120)
			}
		}
		scenarios = append(scenarios, ScenarioResult{
			ID: "s4-multimodal-escalation", Name: "Multimodal Inline Image Escalation", Category: "policy_guardrail",
			Passed: passed, LatencyMs: ms, RoutedModel: routed, Detail: detail,
		})
	}

	// Scenario 5: Multi-Turn Sticky Session Lock + Mid-Session Agent Task Escalation
	{
		sessID := "sess-eval-" + runSuffix
		t0 := time.Now()
		r1, _, _, e1 := runGeminiProxyCall("/v1/models/gemini-dynamic:generateContent", map[string]interface{}{
			"contents":         []interface{}{map[string]interface{}{"role": "user", "parts": []interface{}{map[string]interface{}{"text": "Hello! Reply in one word."}}}},
			"generationConfig": map[string]interface{}{"maxOutputTokens": 64},
		}, map[string]string{"X-Session-ID": sessID, "X-Session-Policy": "lock", "X-Router-Engine": "gemini-decision"})

		r2, _, _, e2 := runGeminiProxyCall("/v1/models/gemini-dynamic:generateContent", map[string]interface{}{
			"contents":         []interface{}{map[string]interface{}{"role": "user", "parts": []interface{}{map[string]interface{}{"text": "Audit auth headers in one sentence."}}}},
			"generationConfig": map[string]interface{}{"maxOutputTokens": 96},
		}, map[string]string{"X-Session-ID": sessID, "X-Agent-Task": "security_audit"})

		r3, _, _, e3 := runGeminiProxyCall("/v1/models/gemini-dynamic:generateContent", map[string]interface{}{
			"contents":         []interface{}{map[string]interface{}{"role": "user", "parts": []interface{}{map[string]interface{}{"text": "Acknowledge in one word."}}}},
			"generationConfig": map[string]interface{}{"maxOutputTokens": 64},
		}, map[string]string{"X-Session-ID": sessID})
		ms := time.Since(t0).Milliseconds()

		m1, m2, m3 := "", "", ""
		if r1 != nil {
			m1 = r1.Header.Get("X-Routed-Model")
		}
		if r2 != nil {
			m2 = r2.Header.Get("X-Routed-Model")
		}
		if r3 != nil {
			m3 = r3.Header.Get("X-Routed-Model")
		}
		passed := e1 == nil && e2 == nil && e3 == nil &&
			r1.StatusCode == http.StatusOK && r2.StatusCode == http.StatusOK && r3.StatusCode == http.StatusOK &&
			m1 == "gemini-3.5-flash-lite" && m2 == "gemini-4-argon" && m3 == "gemini-4-argon"
		detail := fmt.Sprintf("turn1=%s -> turn2(security_audit)=%s -> turn3(locked)=%s", m1, m2, m3)
		if !passed {
			detail += fmt.Sprintf(" (err1=%v err2=%v err3=%v)", e1, e2, e3)
		}
		scenarios = append(scenarios, ScenarioResult{
			ID: "s5-session-lock-task-switch", Name: "Sticky Session + Mid-Session Task Escalation", Category: "stateful_routing",
			Passed: passed, LatencyMs: ms, RoutedModel: m3,
			Detail: detail,
		})
	}

	// Scenario 6: Stage-4 Classifier-Conditional Hybrid Local Rule + <1ms Zero-Runner Cloud Spillover
	{
		resp, rb, ms, err := runGeminiProxyCall("/v1/models/gemini-hybrid-eval:generateContent", map[string]interface{}{
			"contents": []interface{}{
				map[string]interface{}{
					"role":  "user",
					"parts": []interface{}{map[string]interface{}{"text": "What is 2 + 2? Reply with digits only."}},
				},
			},
		}, nil)
		routed := ""
		fbReason := ""
		if resp != nil {
			routed = resp.Header.Get("X-Routed-Model")
			fbReason = resp.Header.Get("X-Cluster-Fallback-Reason")
		}
		passed := err == nil && resp != nil && resp.StatusCode == http.StatusOK &&
			routed == "gemini-3.8-flash" && fbReason == "no_online_runners"
		detail := fmt.Sprintf("gemini-decision matched simple>=0.65 -> gemma-4-26b@%s -> spillover=%s (%s)", evalClusterID, routed, fbReason)
		if !passed && len(rb) > 0 {
			detail += " body=" + truncateStr(string(rb), 120)
		}
		scenarios = append(scenarios, ScenarioResult{
			ID: "s6-hybrid-conditional-spillover", Name: "Conditional Local Gemma 4 + <1ms Cloud Spillover", Category: "hybrid_failover",
			Passed: passed, LatencyMs: ms, RoutedModel: routed, Detail: detail,
		})
	}

	// Scenario 7: Native POST /v1beta/decisions Zero-Output-Token & Simplex Calibration Contract
	{
		resp, rb, ms, err := runGeminiProxyCall("/v1beta/decisions", map[string]interface{}{
			"model": "gemini-decision-1",
			"input": []interface{}{
				map[string]interface{}{"type": "text", "text": "Where do I return a damaged package from Tuesday's pottery class?"},
			},
			"questions": map[string]interface{}{
				"complexity": map[string]interface{}{
					"type":         "choice",
					"instructions": "Classify the prompt into the most appropriate complexity tier.",
					"criteria": map[string]string{
						"simple":  "Everyday FAQ, lookup, formatting, or greeting",
						"medium":  "Multi-sentence explanation, translation, or summary",
						"complex": "Deep systems architecture, formal proof, or security vulnerability audit",
					},
				},
				"local_eligible": map[string]interface{}{
					"type":         "noul",
					"instructions": "Can a compact open-weights Gemma 4 model answer this accurately without frontier reasoning?",
				},
			},
		}, nil)
		var parsed struct {
			Answers map[string]struct {
				Choice        string             `json:"choice"`
				Confidence    float64            `json:"confidence"`
				Probabilities map[string]float64 `json:"probabilities"`
			} `json:"answers"`
			Usage struct {
				TotalInputTokens  int `json:"total_input_tokens"`
				TotalOutputTokens int `json:"total_output_tokens"`
			} `json:"usage"`
		}
		jsonErr := json.Unmarshal(rb, &parsed)
		probSum := 0.0
		for _, p := range parsed.Answers["complexity"].Probabilities {
			probSum += p
		}
		passed := err == nil && resp != nil && resp.StatusCode == http.StatusOK && jsonErr == nil &&
			parsed.Usage.TotalOutputTokens == 0 && parsed.Usage.TotalInputTokens > 0 &&
			math.Abs(probSum-1.0) < 0.01 && parsed.Answers["complexity"].Choice == "simple"
		scenarios = append(scenarios, ScenarioResult{
			ID: "s7-native-decisions-contract", Name: "Native /v1beta/decisions 0-Output-Token Simplex", Category: "decisions_contract",
			Passed: passed, LatencyMs: ms, RoutedModel: "gemini-decision-1",
			Detail: fmt.Sprintf("choice=%s (conf=%.3f, sum=%.3f), in_tok=%d, out_tok=%d",
				parsed.Answers["complexity"].Choice, parsed.Answers["complexity"].Confidence, probSum,
				parsed.Usage.TotalInputTokens, parsed.Usage.TotalOutputTokens),
		})
	}

	scorecards := make([]EngineScorecard, 0, len(engines))
	for _, eng := range engines {
		scorecards = append(scorecards, ScoreEngine(eng, allObs[eng], scenarios))
	}

	report := EvalReport{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		TargetURL:      serviceURL,
		ProjectID:      projectID,
		PrimaryEngine:  "gemini-decision",
		Engines:        scorecards,
		SkippedEngines: skippedEngines,
		Scenarios:      scenarios,
		Observations:   allObs,
	}

	mdOut := RenderMarkdownReport(report)
	fmt.Println("\n" + mdOut)

	if *outJSONFlag != "" {
		jb, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*outJSONFlag, jb, 0o644); err != nil {
			log.Fatalf("[EVAL] Failed to write JSON report: %v", err)
		}
		log.Printf("[EVAL] Wrote JSON report to %s", *outJSONFlag)
	}
	if *outMDFlag != "" {
		if err := os.WriteFile(*outMDFlag, []byte(mdOut), 0o644); err != nil {
			log.Fatalf("[EVAL] Failed to write Markdown report: %v", err)
		}
		log.Printf("[EVAL] Wrote Markdown report to %s", *outMDFlag)
	}
}

func truncateStr(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
