package main

import (
	"math"
	"strings"
	"testing"

	"geminirouter/pkg/config"
	"geminirouter/pkg/decision"
)

func TestAdversarialCorpusIntegrity(t *testing.T) {
	cases := BuildAdversarialCorpus()
	if len(cases) < 20 {
		t.Fatalf("expected at least 20 adversarial routing cases, got %d", len(cases))
	}

	catCounts := map[string]int{}
	seenIDs := map[string]bool{}
	for _, tc := range cases {
		if tc.ID == "" || seenIDs[tc.ID] {
			t.Fatalf("duplicate or empty case ID: %q", tc.ID)
		}
		seenIDs[tc.ID] = true
		catCounts[tc.Category]++

		if tc.ExpectedTier != "simple" && tc.ExpectedTier != "medium" && tc.ExpectedTier != "complex" {
			t.Fatalf("case %s has invalid ExpectedTier %q", tc.ID, tc.ExpectedTier)
		}
		if tc.Category == "deceptive_brevity" && len([]rune(tc.Prompt)) > 90 {
			t.Errorf("deceptive_brevity case %s should be <= 90 chars to trap char-limit heuristics, got %d chars", tc.ID, len([]rune(tc.Prompt)))
		}
		if tc.Category == "verbose_boilerplate" && len([]rune(tc.Prompt)) < 450 {
			t.Errorf("verbose_boilerplate case %s should be >= 450 chars to trap char-limit heuristics, got %d chars", tc.ID, len([]rune(tc.Prompt)))
		}
	}

	requiredCats := []string{
		"deceptive_brevity",
		"verbose_boilerplate",
		"homograph_camouflage",
		"jargon_in_trivial_task",
		"nuanced_medium",
	}
	for _, c := range requiredCats {
		if catCounts[c] < 3 {
			t.Errorf("expected at least 3 cases in category %q, got %d", c, catCounts[c])
		}
	}

	// Ensure default eval models strictly satisfy Gemini 2.5+ / Gemma 4 compliance
	for _, m := range []string{"gemini-3.5-flash-lite", "gemini-3.8-flash", "gemini-3.1-pro", "gemini-4-argon", "gemma-4-26b", "gemini-decision-1"} {
		if !config.IsValidModelName(m) {
			t.Fatalf("eval model %q failed IsValidModelName", m)
		}
	}
}

func TestBrierScoreAndEngineGrading(t *testing.T) {
	// Perfect calibrated engine vs. brittle heuristic engine
	calibratedObs := []CaseObservation{
		{
			Case:              EvalCase{ID: "c1", Category: "deceptive_brevity", ExpectedTier: "complex", ExpectedLocalEligible: false},
			PredictedTier:     "complex",
			Confidence:        0.95,
			Scores:            map[string]float64{"simple": 0.01, "medium": 0.04, "complex": 0.95},
			LocalEligibleProb: 0.05,
			HasLocalSignal:    true,
			LatencyMs:         92,
		},
		{
			Case:              EvalCase{ID: "c2", Category: "verbose_boilerplate", ExpectedTier: "simple", ExpectedLocalEligible: true},
			PredictedTier:     "simple",
			Confidence:        0.94,
			Scores:            map[string]float64{"simple": 0.94, "medium": 0.05, "complex": 0.01},
			LocalEligibleProb: 0.96,
			HasLocalSignal:    true,
			LatencyMs:         88,
		},
		{
			Case:              EvalCase{ID: "c3", Category: "nuanced_medium", ExpectedTier: "medium", ExpectedLocalEligible: true},
			PredictedTier:     "medium",
			Confidence:        0.89,
			Scores:            map[string]float64{"simple": 0.06, "medium": 0.89, "complex": 0.05},
			LocalEligibleProb: 0.82,
			HasLocalSignal:    true,
			LatencyMs:         104,
		},
	}

	brittleObs := []CaseObservation{
		{
			Case:          EvalCase{ID: "c1", Category: "deceptive_brevity", ExpectedTier: "complex", ExpectedLocalEligible: false},
			PredictedTier: "simple", // Critical 2-tier under-route!
			Confidence:    0.80,
			LatencyMs:     2,
		},
		{
			Case:          EvalCase{ID: "c2", Category: "verbose_boilerplate", ExpectedTier: "simple", ExpectedLocalEligible: true},
			PredictedTier: "complex", // Severe 2-tier over-provision waste!
			Confidence:    0.80,
			LatencyMs:     2,
		},
		{
			Case:          EvalCase{ID: "c3", Category: "nuanced_medium", ExpectedTier: "medium", ExpectedLocalEligible: true},
			PredictedTier: "simple", // Minor under-route
			Confidence:    0.80,
			LatencyMs:     2,
		},
	}

	liveScenarios := []ScenarioResult{
		{ID: "s1", Name: "Zero-Runner Spillover", Passed: true, LatencyMs: 110},
		{ID: "s2", Name: "Decisions Zero Output Tokens", Passed: true, LatencyMs: 85},
	}

	calScore := ScoreEngine("gemini-decision", calibratedObs, liveScenarios)
	britScore := ScoreEngine("heuristics", brittleObs, liveScenarios)

	if math.Abs(calScore.Accuracy-1.0) > 1e-6 {
		t.Fatalf("expected calibrated accuracy 1.0, got %.4f", calScore.Accuracy)
	}
	if calScore.BrierScore > 0.02 {
		t.Fatalf("expected low Brier score (<0.02) for well-calibrated probabilities, got %.4f", calScore.BrierScore)
	}
	if calScore.CriticalUnderRouteRate != 0 {
		t.Fatalf("expected 0 critical under-route rate, got %.4f", calScore.CriticalUnderRouteRate)
	}
	if calScore.CompositeScore < 94.0 || !strings.HasPrefix(calScore.LetterGrade, "A") {
		t.Fatalf("expected A/A+ grade (>=94) for calibrated engine, got %.2f (%s)", calScore.CompositeScore, calScore.LetterGrade)
	}

	if britScore.CriticalUnderRouteRate < 0.33 {
		t.Fatalf("expected brittle engine critical under-route rate >= 0.33, got %.4f", britScore.CriticalUnderRouteRate)
	}
	if britScore.OverProvisionRate < 0.33 {
		t.Fatalf("expected brittle engine over-provision rate >= 0.33, got %.4f", britScore.OverProvisionRate)
	}
	if britScore.CompositeScore >= 55.0 {
		t.Fatalf("expected brittle engine with 0%% accuracy and critical under-routing to score < 55, got %.2f (%s)", britScore.CompositeScore, britScore.LetterGrade)
	}

	md := RenderMarkdownReport(EvalReport{
		TargetURL:     "https://gemini-smart-router-txgsracloq-uc.a.run.app",
		ProjectID:     "davenport-boutique",
		PrimaryEngine: "gemini-decision",
		Engines:       []EngineScorecard{calScore, britScore},
		Scenarios:     liveScenarios,
	})
	if !strings.Contains(md, "gemini-decision") || !strings.Contains(md, "Critical Under-Route") {
		t.Fatalf("markdown report missing expected sections:\n%s", md)
	}
}

func TestExternalProviderReadinessGateAndLLMOutputTokens(t *testing.T) {
	// 1. Unconfigured external providers (no API key or in-process fallback endpoint) MUST NOT be benchmarked as live providers
	okJevUnconf, reasonJev := IsExternalProviderBenchmarkable(decision.EngineSecretStatus{
		Engine:         "jev",
		Configured:     false,
		Endpoint:       "local-embedded (in-process)",
		ConnectionMode: "local",
	})
	if okJevUnconf || !strings.Contains(reasonJev, "JEV_API_KEY") {
		t.Fatalf("expected unconfigured jev to be excluded with JEV_API_KEY reason, got ok=%v reason=%q", okJevUnconf, reasonJev)
	}

	okFastinoKeyOnlyNoURL, _ := IsExternalProviderBenchmarkable(decision.EngineSecretStatus{
		Engine:         "fastino",
		Configured:     true,
		Endpoint:       "local-embedded (in-process)",
		ConnectionMode: "local",
	})
	if okFastinoKeyOnlyNoURL {
		t.Fatalf("expected fastino without remote endpoint URL to be excluded from live provider benchmark")
	}

	okCLMUnconf, _ := IsExternalProviderBenchmarkable(decision.EngineSecretStatus{
		Engine:         "contrastive-lm",
		Configured:     false,
		Endpoint:       "local-embedded (in-process)",
		ConnectionMode: "local",
	})
	if okCLMUnconf {
		t.Fatalf("expected contrastive-lm without cluster/remote endpoint to be excluded from live provider benchmark")
	}

	// 2. Properly configured remote/cluster providers ARE benchmarkable
	okJevLive, _ := IsExternalProviderBenchmarkable(decision.EngineSecretStatus{
		Engine:         "jev",
		Configured:     true,
		Endpoint:       "https://api.jev.internal/v1/systemone",
		ConnectionMode: "remote",
	})
	if !okJevLive {
		t.Fatalf("expected configured remote jev provider to be benchmarkable")
	}

	okCLMLive, _ := IsExternalProviderBenchmarkable(decision.EngineSecretStatus{
		Engine:         "contrastive-lm",
		Configured:     false,
		Endpoint:       "http://clm-runner.default.svc.cluster.local:8080/rank",
		ConnectionMode: "cluster",
	})
	if !okCLMLive {
		t.Fatalf("expected cluster-internal contrastive-lm runner to be benchmarkable")
	}

	// 3. Generative LLM JSON classifier tracks both InputTokens and OutputTokens + SkippedEngines renders in Markdown
	llmObs := []CaseObservation{
		{
			Case:          EvalCase{ID: "c1", Category: "deceptive_brevity", ExpectedTier: "complex"},
			PredictedTier: "complex",
			InputTokens:   200,
			OutputTokens:  12,
			LatencyMs:     420,
		},
	}
	llmScore := ScoreEngine("llm (gemini-3.5-flash-lite JSON)", llmObs, nil)
	if llmScore.TotalInputTokens != 200 || llmScore.TotalOutputTokens != 12 {
		t.Fatalf("expected TotalInputTokens=200 TotalOutputTokens=12, got in=%d out=%d", llmScore.TotalInputTokens, llmScore.TotalOutputTokens)
	}
	if llmScore.EstCostPer1kRoutesUSD <= 0 {
		t.Fatalf("expected positive cost per 1k routes for llm JSON classifier, got %.6f", llmScore.EstCostPer1kRoutesUSD)
	}

	md := RenderMarkdownReport(EvalReport{
		TargetURL:     "https://gemini-smart-router-txgsracloq-uc.a.run.app",
		ProjectID:     "davenport-boutique",
		PrimaryEngine: "gemini-decision",
		Engines:       []EngineScorecard{llmScore},
		SkippedEngines: []SkippedEngine{
			{Engine: "jev", Reason: reasonJev},
		},
	})
	if !strings.Contains(md, "Skipped Unconfigured External Providers") || !strings.Contains(md, "jev") {
		t.Fatalf("expected Skipped Unconfigured External Providers section in markdown report, got:\n%s", md)
	}
}
