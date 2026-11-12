package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"geminirouter/pkg/config"
)

func TestExtractPromptFeatures(t *testing.T) {
	promptSimple := "Hi, what is the capital of France?"
	featSimple := ExtractPromptFeatures(promptSimple, false, false)
	if featSimple.HasCode || featSimple.HasMath || featSimple.ExtractedTask != "fast_query" {
		t.Fatalf("unexpected features for simple prompt: %+v", featSimple)
	}

	promptCode := "```go\nfunc add(a, b int) int { return a + b }\n```\nCan you optimize this algorithm with O(N) complexity?"
	featCode := ExtractPromptFeatures(promptCode, false, false)
	if !featCode.HasCode || !featCode.HasMath || featCode.ExtractedTask != "code" {
		t.Fatalf("unexpected features for code prompt: %+v", featCode)
	}

	promptMulti := "Step 1: Parse the logs.\nStep 2: Compare and contrast tradeoffs."
	featMulti := ExtractPromptFeatures(promptMulti, false, false)
	if !featMulti.HasMultiStep || !featMulti.HasReasoningKeywords {
		t.Fatalf("unexpected features for multi-step prompt: %+v", featMulti)
	}
}

func TestJevDecisionEngine_LocalScoring(t *testing.T) {
	engine := NewJevDecisionEngine()
	ctx := context.Background()
	c := config.ComplexityRouting{
		SimpleModel:     "gemini-3.5-flash-lite",
		MediumModel:     "gemini-3.8-flash",
		ComplexModel:    "gemini-3.1-pro",
		SimpleCharLimit: 200,
		MediumCharLimit: 1000,
	}

	// 1. Simple greeting
	resSimple := engine.Classify(ctx, "Hello there!", c, false, false)
	if resSimple.Tier != "simple" || resSimple.TargetModel != "gemini-3.5-flash-lite" {
		t.Errorf("expected simple tier, got %s (%s)", resSimple.Tier, resSimple.TargetModel)
	}

	// 2. Complex coding
	resComplex := engine.Classify(ctx, "Write a distributed Raft consensus algorithm in Go with mutual exclusion:\nfunc RunElection() {\n    return\n}", c, false, false)
	if resComplex.Tier != "complex" || resComplex.TargetModel != "gemini-3.1-pro" {
		t.Errorf("expected complex tier, got %s (%s)", resComplex.Tier, resComplex.TargetModel)
	}

	// 3. Force multimodal bypass
	cForce := c
	cForce.ForceComplexMultimodal = true
	resMultimodal := engine.Classify(ctx, "Short query", cForce, false, true)
	if resMultimodal.Tier != "complex" {
		t.Errorf("expected forced complex tier for multimodal, got %s", resMultimodal.Tier)
	}
}

func TestJevDecisionEngine_RemoteEndpoint(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	// Mock remote Jev decision server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"tier": "complex", "confidence": 0.95, "rationale": "Remote Jev detected high complexity", "scores": {"complex": 0.95}}`))
	}))
	defer srv.Close()

	engine := NewJevDecisionEngine()
	c := config.ComplexityRouting{
		JevEndpoint:  srv.URL,
		ComplexModel: "gemini-3.1-pro",
	}

	res := engine.Classify(context.Background(), "Any prompt", c, false, false)
	if res.Tier != "complex" || res.Confidence != 0.95 || res.TargetModel != "gemini-3.1-pro" {
		t.Errorf("unexpected remote Jev result: %+v", res)
	}
}

func TestCLMContrastiveEngine(t *testing.T) {
	engine := NewCLMContrastiveEngine()
	ctx := context.Background()
	c := config.ComplexityRouting{
		SimpleModel:  "gemini-3.5-flash-lite",
		MediumModel:  "gemini-3.8-flash",
		ComplexModel: "gemini-3.1-pro",
	}

	// 1. Simple prompt
	resSimple := engine.Classify(ctx, "What is 2 + 2?", c, false, false)
	if resSimple.Tier != "simple" {
		t.Errorf("expected simple tier from CLM, got %s", resSimple.Tier)
	}

	// 2. Complex prompt with code and algorithmic reasoning
	resComplex := engine.Classify(ctx, "```go\ntype Node struct {}\n```\nProve the asymptotic upper bound O(N log N) for this algorithm and write optimized tests.", c, false, false)
	if resComplex.Tier != "complex" || resComplex.TargetModel != "gemini-3.1-pro" {
		t.Errorf("expected complex tier from CLM, got %s (%s)", resComplex.Tier, resComplex.TargetModel)
	}
}

func TestResolveTaskModel(t *testing.T) {
	c := config.ComplexityRouting{
		SimpleModel:  "gemini-3.5-flash-lite",
		MediumModel:  "gemini-3.8-flash",
		ComplexModel: "gemini-3.1-pro",
		TaskMapping: map[string]string{
			"custom_eval": "custom-eval-model",
		},
	}

	// Custom mapping
	model, tier, ok := ResolveTaskModel("custom_eval", c)
	if !ok || model != "custom-eval-model" || tier != "custom_task" {
		t.Errorf("custom task mapping failed: %s, %s, %v", model, tier, ok)
	}

	// Standard code task
	modelCode, tierCode, okCode := ResolveTaskModel("code", c)
	if !okCode || modelCode != "gemini-3.1-pro" || tierCode != "complex" {
		t.Errorf("standard code task failed: %s, %s, %v", modelCode, tierCode, okCode)
	}

	// Standard summary task
	modelSumm, tierSumm, okSumm := ResolveTaskModel("summarization", c)
	if !okSumm || modelSumm != "gemini-3.8-flash" || tierSumm != "medium" {
		t.Errorf("standard summary task failed: %s, %s, %v", modelSumm, tierSumm, okSumm)
	}

	// Standard triage task
	modelTriage, tierTriage, okTriage := ResolveTaskModel("triage", c)
	if !okTriage || modelTriage != "gemini-3.5-flash-lite" || tierTriage != "simple" {
		t.Errorf("standard triage task failed: %s, %s, %v", modelTriage, tierTriage, okTriage)
	}

	// Unknown task
	_, _, okUnknown := ResolveTaskModel("unknown_xyz", c)
	if okUnknown {
		t.Errorf("expected unknown task to return false")
	}
}

func TestJevDecisionEngine_SSRFProtection(t *testing.T) {
	engine := NewJevDecisionEngine()
	ctx := context.Background()

	// 1. Metadata server must always be rejected even in LOCAL_DEV
	t.Setenv("LOCAL_DEV", "true")
	if _, err := engine.callRemoteJev(ctx, "http://169.254.169.254/computeMetadata/v1/", "hi", PromptFeatures{}); err == nil {
		t.Errorf("expected SSRF error for 169.254.169.254")
	}
	if _, err := engine.callRemoteJev(ctx, "http://metadata.google.internal/computeMetadata/v1/", "hi", PromptFeatures{}); err == nil {
		t.Errorf("expected SSRF error for metadata.google.internal")
	}
	if _, err := engine.callRemoteJev(ctx, "file:///etc/passwd", "hi", PromptFeatures{}); err == nil {
		t.Errorf("expected error for non-http scheme")
	}

	// 2. Loopback, link-local, and unspecified IPs must be rejected when LOCAL_DEV != "true"
	t.Setenv("LOCAL_DEV", "false")
	if _, err := engine.callRemoteJev(ctx, "http://127.0.0.1:8080/jev", "hi", PromptFeatures{}); err == nil {
		t.Errorf("expected SSRF error for loopback IP when LOCAL_DEV=false")
	}
	if err := validateRemoteJevURL("http://localhost:8080/jev"); err == nil {
		t.Errorf("expected SSRF error for localhost when LOCAL_DEV=false")
	}
	if err := validateRemoteJevURL("http://169.254.10.20:8080/jev"); err == nil {
		t.Errorf("expected SSRF error for link-local IP when LOCAL_DEV=false")
	}
	if err := validateRemoteJevURL("http://0.0.0.0:8080/jev"); err == nil {
		t.Errorf("expected SSRF error for unspecified IP when LOCAL_DEV=false")
	}

	// 3. RFC1918 private VPC/cluster IPs and in-cluster DNS hostnames are allowed for internal cluster runners
	for _, allowedClusterURL := range []string{
		"http://10.128.0.5:8080/rank",
		"http://172.16.0.12:8080/rank",
		"http://192.168.1.50:8080/rank",
		"http://clm-runner.default.svc.cluster.local:8080/rank",
		"http://clm-cluster.internal:8080/rank",
	} {
		if err := validateRemoteJevURL(allowedClusterURL); err != nil {
			t.Errorf("expected cluster endpoint %s to be permitted by SSRF validator, got: %v", allowedClusterURL, err)
		}
	}
}

func TestStickyTurnSessionPolicy(t *testing.T) {
	reg := config.NewSessionRegistry(0)
	sess := reg.GetOrCreateSession("sess-sticky-1", "app-1", "sticky-turn")

	// Turn 1: simple -> locks to simple
	m, tier, upgraded := sess.ApplyStickyTurn("gemini-3.5-flash-lite", "simple")
	if m != "gemini-3.5-flash-lite" || tier != "simple" || upgraded {
		t.Fatalf("Turn 1 unexpected: model=%s tier=%s upgraded=%v", m, tier, upgraded)
	}

	// Turn 2: complex -> immediate upgrade to complex
	s2, _ := reg.GetSession("sess-sticky-1")
	m, tier, upgraded = s2.ApplyStickyTurn("gemini-3.1-pro", "complex")
	if m != "gemini-3.1-pro" || tier != "complex" || !upgraded {
		t.Fatalf("Turn 2 expected immediate upgrade to complex, got model=%s tier=%s upgraded=%v", m, tier, upgraded)
	}

	// Turn 3, 4, 5: simple prompts should remain on complex until 3 turns since upgrade have elapsed
	for turnIdx := 1; turnIdx <= 3; turnIdx++ {
		st, _ := reg.GetSession("sess-sticky-1")
		m, tier, upgraded = st.ApplyStickyTurn("gemini-3.5-flash-lite", "simple")
		if m != "gemini-3.1-pro" || tier != "complex" || upgraded {
			t.Fatalf("Turn +%d after upgrade expected to stay on complex, got model=%s tier=%s", turnIdx, m, tier)
		}
	}

	// Turn 6 (after 3 turns at complex): simple prompt now downgrades to simple
	s6, _ := reg.GetSession("sess-sticky-1")
	m, tier, upgraded = s6.ApplyStickyTurn("gemini-3.5-flash-lite", "simple")
	if m != "gemini-3.5-flash-lite" || tier != "simple" || upgraded {
		t.Fatalf("Turn +4 expected downgrade to simple after 3 turns since upgrade, got model=%s tier=%s", m, tier)
	}
}

func TestFastinoDecisionEngine_LocalAndRemote(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	ctx := context.Background()
	engine := NewFastinoDecisionEngine()

	cfg := config.ComplexityRouting{
		Enabled:      true,
		Engine:       "fastino",
		SimpleModel:  "gemini-3.5-flash-lite",
		MediumModel:  "gemini-3.8-flash",
		ComplexModel: "gemini-3.1-pro",
	}

	// 1. Local simple prompt
	resSimple := engine.Classify(ctx, "Hi there! What time is it?", cfg, false, false)
	if resSimple.Tier != "simple" || resSimple.Engine != "fastino" || resSimple.TargetModel != "gemini-3.5-flash-lite" {
		t.Errorf("expected simple tier from Fastino local, got %+v", resSimple)
	}
	if !strings.Contains(resSimple.Rationale, "fastino-local") {
		t.Errorf("expected fastino-local rationale prefix, got %s", resSimple.Rationale)
	}

	// 2. Local complex prompt with code & math
	resComplex := engine.Classify(ctx, "```go\nfunc SolveLP(matrix [][]float64) []float64 { return nil }\n```\nProve convergence in O(N^3) time step by step.", cfg, false, false)
	if resComplex.Tier != "complex" || resComplex.TargetModel != "gemini-3.1-pro" {
		t.Errorf("expected complex tier from Fastino local, got %+v", resComplex)
	}

	// 3. Custom criteria alignment boost
	cfgCriteria := cfg
	cfgCriteria.ComplexCriteria = "hipaa compliance medical audit phi regulation"
	resCriteria := engine.Classify(ctx, "Check hipaa compliance medical audit phi regulation rules", cfgCriteria, false, false)
	if resCriteria.Tier != "complex" {
		t.Errorf("expected custom ComplexCriteria keyword overlap to boost tier to complex, got %s (%s)", resCriteria.Tier, resCriteria.Rationale)
	}

	// 4. Remote Fastino /v1/systemone endpoint with secret auth
	SetEngineSecret("fastino", "sk-fastino-live-secret-9999")
	defer DeleteEngineSecret("fastino")

	var capturedAuth string
	var capturedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		var reqMap map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&reqMap)
		if m, ok := reqMap["model"].(string); ok {
			capturedModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {
				"complexity": {
					"choice": "complex",
					"confidence": 0.94,
					"probabilities": {"simple": 0.01, "medium": 0.05, "complex": 0.94},
					"rationale": "GLiDE zero-shot span classifier detected multi-hop architecture synthesis"
				}
			}
		}`))
	}))
	defer srv.Close()

	cfgRemote := cfg
	cfgRemote.FastinoEndpoint = srv.URL
	resRemote := engine.Classify(ctx, "Hello", cfgRemote, false, false)
	if resRemote.Tier != "complex" || resRemote.TargetModel != "gemini-3.1-pro" || resRemote.Confidence != 0.94 {
		t.Errorf("expected remote Fastino complex 0.94, got %+v", resRemote)
	}
	if capturedAuth != "Bearer sk-fastino-live-secret-9999" {
		t.Errorf("expected Bearer auth header from vault, got %q", capturedAuth)
	}
	if capturedModel != "fastino/GLiDE" {
		t.Errorf("expected model fastino/GLiDE, got %q", capturedModel)
	}
	if !strings.Contains(resRemote.Rationale, "fastino-remote") {
		t.Errorf("expected fastino-remote in rationale, got %s", resRemote.Rationale)
	}
}

func TestCLMContrastiveEngine_RemoteRankAndSystemOne(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	ctx := context.Background()
	engine := NewCLMContrastiveEngine()

	SetEngineSecret("contrastive-lm", "clm-secret-key-123456")
	defer DeleteEngineSecret("contrastive-lm")

	var capturedAuth string
	srvRank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"rank": 1, "candidate": "complex: deep reasoning and code", "prob": 0.97, "score": 14.2},
			{"rank": 2, "candidate": "medium: standard summarization", "prob": 0.02, "score": 3.1},
			{"rank": 3, "candidate": "simple: short greeting", "prob": 0.01, "score": 0.5}
		]`))
	}))
	defer srvRank.Close()

	cfg := config.ComplexityRouting{
		Enabled:      true,
		Engine:       "contrastive-lm",
		CLMEndpoint:  srvRank.URL,
		SimpleModel:  "gemini-3.5-flash-lite",
		MediumModel:  "gemini-3.8-flash",
		ComplexModel: "gemini-3.1-pro",
	}

	res := engine.Classify(ctx, "Hi", cfg, false, false)
	if res.Tier != "complex" || res.TargetModel != "gemini-3.1-pro" || res.Engine != "contrastive-lm" {
		t.Errorf("expected complex tier with engine contrastive-lm from /rank endpoint, got %+v", res)
	}
	if capturedAuth != "Bearer clm-secret-key-123456" {
		t.Errorf("expected Bearer clm-secret-key-123456, got %q", capturedAuth)
	}
	if !strings.Contains(res.Rationale, "clm-remote") {
		t.Errorf("expected clm-remote rationale prefix, got %s", res.Rationale)
	}
}

func TestMinConfidenceFallbackAndSecretVault(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	ctx := context.Background()

	// 1. Test Secret Vault & Masking
	SetEngineSecret("jev", "sk-typesafe-jev-key-abcdef1234")
	defer DeleteEngineSecret("jev")

	status := GetEngineSecretStatus("jev", "")
	if !status.Configured || status.Source != "memory" {
		t.Errorf("expected jev secret configured from memory, got %+v", status)
	}
	if status.MaskedPreview == "sk-typesafe-jev-key-abcdef1234" || !strings.Contains(status.MaskedPreview, "...") {
		t.Errorf("expected masked preview, got %q", status.MaskedPreview)
	}

	// 2. Test MinConfidence upward safety escalation (never downgrades complex; escalates simple->medium and upward-leaning medium->complex)
	srvLowConfSimple := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {
				"complexity": {
					"choice": "simple",
					"confidence": 0.52,
					"probabilities": {"simple": 0.52, "medium": 0.42, "complex": 0.06}
				}
			}
		}`))
	}))
	defer srvLowConfSimple.Close()

	jev := NewJevDecisionEngine()
	cfg := config.ComplexityRouting{
		Enabled:       true,
		Engine:        "jev",
		JevEndpoint:   srvLowConfSimple.URL,
		MinConfidence: 0.80,
		SimpleModel:   "gemini-3.5-flash-lite",
		MediumModel:   "gemini-3.8-flash",
		ComplexModel:  "gemini-4-argon",
	}

	resSimple := jev.Classify(ctx, "Ambiguous simple request", cfg, false, false)
	if resSimple.Tier != "medium" || resSimple.TargetModel != "gemini-3.8-flash" {
		t.Errorf("expected upward escalation simple -> medium (gemini-3.8-flash) when confidence 0.52 < MinConfidence 0.80, got tier=%s model=%s", resSimple.Tier, resSimple.TargetModel)
	}
	if !strings.Contains(resSimple.Rationale, "min_confidence_fallback") {
		t.Errorf("expected min_confidence_fallback in rationale, got %s", resSimple.Rationale)
	}

	// 3. Low-confidence complex MUST NEVER be downgraded to medium
	resComplex := applyMinConfidenceFallback(DecisionResult{
		Tier:        "complex",
		TargetModel: "gemini-4-argon",
		Confidence:  0.52,
		Scores:      map[string]float64{"simple": 0.12, "medium": 0.36, "complex": 0.52},
		Rationale:   "gemini-decision-1: tier=complex (conf=0.52)",
	}, cfg)
	if resComplex.Tier != "complex" || resComplex.TargetModel != "gemini-4-argon" {
		t.Errorf("expected low-confidence complex to remain complex (never downgrade), got tier=%s model=%s", resComplex.Tier, resComplex.TargetModel)
	}

	// 4. Low-confidence medium with upward complex tail (p_complex > p_simple) escalates to complex
	resUpwardMedium := applyMinConfidenceFallback(DecisionResult{
		Tier:        "medium",
		TargetModel: "gemini-3.8-flash",
		Confidence:  0.47,
		Scores:      map[string]float64{"simple": 0.23, "medium": 0.47, "complex": 0.30},
		Rationale:   "gemini-decision-1: tier=medium (conf=0.47)",
	}, cfg)
	if resUpwardMedium.Tier != "complex" || resUpwardMedium.TargetModel != "gemini-4-argon" {
		t.Errorf("expected upward-leaning uncertain medium to escalate to complex (gemini-4-argon), got tier=%s model=%s", resUpwardMedium.Tier, resUpwardMedium.TargetModel)
	}

	// 5. Low-confidence medium where cumulative (simple+medium >= MinConfidence) and p_simple >= p_complex stays medium
	resDownwardMedium := applyMinConfidenceFallback(DecisionResult{
		Tier:        "medium",
		TargetModel: "gemini-3.8-flash",
		Confidence:  0.50,
		Scores:      map[string]float64{"simple": 0.47, "medium": 0.50, "complex": 0.03},
		Rationale:   "gemini-decision-1: tier=medium (conf=0.50)",
	}, cfg)
	if resDownwardMedium.Tier != "medium" || resDownwardMedium.TargetModel != "gemini-3.8-flash" {
		t.Errorf("expected downward-leaning medium (p_simple+p_medium=0.97 >= 0.80) to stay medium, got tier=%s model=%s", resDownwardMedium.Tier, resDownwardMedium.TargetModel)
	}
}

func TestLoadDotEnvAndSecretManagerSources(t *testing.T) {
	DeleteEngineSecret("jev")
	DeleteEngineSecret("fastino")
	DeleteEngineSecret("contrastive-lm")

	t.Setenv("K_SERVICE", "")
	t.Setenv("JEV_API_KEY", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("FASTINO_API_KEY", "")
	t.Setenv("PIONEER_API_KEY", "")
	t.Setenv("CLM_API_KEY", "")
	t.Setenv("CONTRASTIVE_LM_API_KEY", "")
	_ = os.Unsetenv("JEV_API_KEY")
	_ = os.Unsetenv("FASTINO_API_KEY")
	_ = os.Unsetenv("CLM_API_KEY")

	tmpDir := t.TempDir()
	dotenvPath := filepath.Join(tmpDir, ".env")
	dotenvContent := `# Test .env file
JEV_API_KEY="jev-dotenv-secret-1111"
export FASTINO_API_KEY='pio-dotenv-secret-2222'
CLM_API_KEY=clm-dotenv-secret-3333 # inline comment
`
	if err := os.WriteFile(dotenvPath, []byte(dotenvContent), 0600); err != nil {
		t.Fatalf("failed to write temp .env: %v", err)
	}

	if err := LoadDotEnv(dotenvPath); err != nil {
		t.Fatalf("LoadDotEnv failed: %v", err)
	}

	// Verify local .env source reporting when K_SERVICE is empty
	stJev := GetEngineSecretStatus("jev", "")
	if !stJev.Configured || stJev.Source != "dotenv" || stJev.MaskedPreview != "jev...1111" {
		t.Errorf("expected jev secret from dotenv with masked preview jev...1111, got %+v", stJev)
	}
	stFastino := GetEngineSecretStatus("fastino", "")
	if !stFastino.Configured || stFastino.Source != "dotenv" || stFastino.MaskedPreview != "pio...2222" {
		t.Errorf("expected fastino secret from dotenv with masked preview pio...2222, got %+v", stFastino)
	}
	stCLM := GetEngineSecretStatus("contrastive-lm", "")
	if !stCLM.Configured || stCLM.Source != "dotenv" || stCLM.MaskedPreview != "clm...3333" {
		t.Errorf("expected contrastive-lm secret from dotenv with masked preview clm...3333, got %+v", stCLM)
	}

	// Verify Cloud Run Secret Manager source reporting when K_SERVICE is set
	t.Setenv("K_SERVICE", "gemini-smart-router")
	stCloudRun := GetEngineSecretStatus("jev", "")
	if !stCloudRun.Configured || stCloudRun.Source != "secret_manager" {
		t.Errorf("expected source=secret_manager when K_SERVICE is set, got %+v", stCloudRun)
	}
}

func TestGlobalProviderClusterConfigAndProbe(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	ctx := context.Background()

	defer func() {
		SetEngineClusterConfig("contrastive-lm", "", "local", "")
		SetEngineEndpoint("fastino", "")
		SetEngineEndpoint("jev", "")
		DeleteEngineSecret("contrastive-lm")
	}()

	srvCluster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"rank": 1, "candidate": "complex: deep reasoning and code", "prob": 0.96, "score": 12.8},
			{"rank": 2, "candidate": "medium: standard summarization", "prob": 0.03, "score": 2.4},
			{"rank": 3, "candidate": "simple: short greeting", "prob": 0.01, "score": 0.2}
		]`))
	}))
	defer srvCluster.Close()

	// Configure global Contrastive-LM cluster runner
	SetEngineClusterConfig("contrastive-lm", srvCluster.URL, "cluster", "clm-cluster-default")
	SetEngineSecret("contrastive-lm", "clm-cluster-token-7777")

	st := GetEngineSecretStatus("contrastive-lm", "")
	if st.ConnectionMode != "cluster" || st.ClusterID != "clm-cluster-default" || st.Endpoint != srvCluster.URL {
		t.Fatalf("expected cluster config in EngineSecretStatus, got %+v", st)
	}
	if st.DisplayName == "" {
		t.Errorf("expected non-empty DisplayName for contrastive-lm")
	}

	// Verify CLMContrastiveEngine.Classify uses global cluster endpoint when c.CLMEndpoint is empty
	clmEngine := NewCLMContrastiveEngine()
	cfg := config.ComplexityRouting{
		Enabled:      true,
		Engine:       "contrastive-lm",
		SimpleModel:  "gemini-3.5-flash-lite",
		MediumModel:  "gemini-3.8-flash",
		ComplexModel: "gemini-3.1-pro",
	}
	res := clmEngine.Classify(ctx, "Hello", cfg, false, false)
	if res.Tier != "complex" || !strings.Contains(res.Rationale, "clm-remote") {
		t.Errorf("expected CLMContrastiveEngine to use global cluster endpoint and return complex/clm-remote, got %+v", res)
	}

	// Verify ProbeDecisionProvider hits the live cluster server
	probeRes, elapsed, target, err := ProbeDecisionProvider(ctx, "contrastive-lm", "")
	if err != nil {
		t.Fatalf("expected ProbeDecisionProvider to succeed against cluster server, got err: %v", err)
	}
	if probeRes.Tier != "complex" || target != srvCluster.URL || elapsed <= 0 {
		t.Errorf("unexpected ProbeDecisionProvider result: res=%+v target=%s elapsed=%v", probeRes, target, elapsed)
	}

	// Verify ProbeDecisionProvider surfaces real HTTP errors when endpoint fails
	if _, _, _, errFail := ProbeDecisionProvider(ctx, "contrastive-lm", "http://127.0.0.1:1/unreachable"); errFail == nil {
		t.Errorf("expected ProbeDecisionProvider to return real error for unreachable endpoint")
	}
}
