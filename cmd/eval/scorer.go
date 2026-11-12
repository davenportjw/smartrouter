package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"geminirouter/pkg/decision"
)

// CaseObservation captures a single engine's live classification outcome on an EvalCase.
type CaseObservation struct {
	Case              EvalCase           `json:"case"`
	PredictedTier     string             `json:"predicted_tier"`
	TargetModel       string             `json:"target_model"`
	ExecutionMode     string             `json:"execution_mode,omitempty"`
	Confidence        float64            `json:"confidence"`
	Scores            map[string]float64 `json:"scores,omitempty"`
	LocalEligibleProb float64            `json:"local_eligible_prob,omitempty"`
	HasLocalSignal    bool               `json:"has_local_signal,omitempty"`
	InputTokens       int                `json:"input_tokens,omitempty"`
	OutputTokens      int                `json:"output_tokens,omitempty"`
	LatencyMs         int64              `json:"latency_ms"`
	Rationale         string             `json:"rationale,omitempty"`
	Error             string             `json:"error,omitempty"`
}

// SkippedEngine documents an external routing provider omitted from head-to-head benchmarking
// because its remote endpoint or API key was not configured on the target deployment.
type SkippedEngine struct {
	Engine string `json:"engine"`
	Reason string `json:"reason"`
}

// IsExternalProviderBenchmarkable verifies that an external third-party or cluster decision provider
// (jev, fastino, contrastive-lm) has a real remote/cluster endpoint (and API key where required)
// configured, preventing built-in offline fallback stubs from being misrepresented as live provider benchmarks.
func IsExternalProviderBenchmarkable(st decision.EngineSecretStatus) (bool, string) {
	eng := strings.ToLower(strings.TrimSpace(st.Engine))
	ep := strings.TrimSpace(st.Endpoint)
	hasLiveEndpoint := (ep != "" && ep != "local-embedded (in-process)") || strings.TrimSpace(st.ClusterID) != ""

	switch eng {
	case "jev":
		if !st.Configured || !hasLiveEndpoint {
			return false, "Skipped: JEV_API_KEY and remote JEV_ENDPOINT are not configured (built-in offline heuristic fallback excluded)"
		}
		return true, ""
	case "fastino":
		if !st.Configured || !hasLiveEndpoint {
			return false, "Skipped: FASTINO_API_KEY and remote FASTINO_ENDPOINT are not configured (built-in offline heuristic fallback excluded)"
		}
		return true, ""
	case "clm", "contrastive-lm":
		if !hasLiveEndpoint {
			return false, "Skipped: CLM_ENDPOINT / cluster runner is not configured (built-in offline 5D anchor fallback excluded)"
		}
		return true, ""
	default:
		return true, ""
	}
}

// ScenarioResult records an end-to-end live proxy stress scenario through RouterProxy.ServeHTTP.
type ScenarioResult struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Category    string            `json:"category"`
	Passed      bool              `json:"passed"`
	LatencyMs   int64             `json:"latency_ms"`
	RoutedModel string            `json:"routed_model,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Detail      string            `json:"detail"`
}

// CategoryScore summarizes accuracy and error modes inside one adversarial category.
type CategoryScore struct {
	Category   string  `json:"category"`
	Total      int     `json:"total"`
	Correct    int     `json:"correct"`
	Accuracy   float64 `json:"accuracy"`
	UnderRoute int     `json:"under_route"`
	OverRoute  int     `json:"over_route"`
}

// EngineScorecard holds all quantitative metrics and weighted rubric grades for one routing engine.
type EngineScorecard struct {
	Engine                 string          `json:"engine"`
	TotalCases             int             `json:"total_cases"`
	CorrectCases           int             `json:"correct_cases"`
	Accuracy               float64         `json:"accuracy"`
	CriticalUnderRouteRate float64         `json:"critical_under_route_rate"` // complex -> simple (2-tier drop)
	MinorUnderRouteRate    float64         `json:"minor_under_route_rate"`    // 1-tier drop (complex->medium or medium->simple)
	OverProvisionRate      float64         `json:"over_provision_rate"`       // predicted > expected
	BrierScore             float64         `json:"brier_score"`               // Multi-class Brier score (0=perfect, 0.67=uniform)
	HasProbabilities       bool            `json:"has_probabilities"`
	LocalEligibleAccuracy  float64         `json:"local_eligible_accuracy,omitempty"`
	MeanConfidenceCorrect  float64         `json:"mean_confidence_correct"`
	MeanConfidenceWrong    float64         `json:"mean_confidence_wrong"`
	ExecutionMode          string          `json:"execution_mode,omitempty"`
	TotalInputTokens       int             `json:"total_input_tokens,omitempty"`
	TotalOutputTokens      int             `json:"total_output_tokens"`
	EstCostPer1kRoutesUSD  float64         `json:"est_cost_per_1k_routes_usd"`
	LatencyMeanMs          float64         `json:"latency_mean_ms"`
	LatencyP50Ms           int64           `json:"latency_p50_ms"`
	LatencyP95Ms           int64           `json:"latency_p95_ms"`
	AccuracyPoints         float64         `json:"accuracy_points"`    // out of 35
	SafetyPoints           float64         `json:"safety_points"`      // out of 25
	CalibrationPoints      float64         `json:"calibration_points"` // out of 15
	PolicyPoints           float64         `json:"policy_points"`      // out of 15
	LatencyPoints          float64         `json:"latency_points"`     // out of 10
	CompositeScore         float64         `json:"composite_score"`    // out of 100
	LetterGrade            string          `json:"letter_grade"`
	Categories             []CategoryScore `json:"categories"`
	Misses                 []string        `json:"misses,omitempty"`
}

// EvalReport bundles the complete comparative engine matrix and live proxy stress results.
type EvalReport struct {
	GeneratedAt    string                       `json:"generated_at"`
	TargetURL      string                       `json:"target_url"`
	ProjectID      string                       `json:"project_id"`
	PrimaryEngine  string                       `json:"primary_engine"`
	Engines        []EngineScorecard            `json:"engines"`
	SkippedEngines []SkippedEngine              `json:"skipped_engines,omitempty"`
	Scenarios      []ScenarioResult             `json:"scenarios"`
	Observations   map[string][]CaseObservation `json:"observations,omitempty"`
}

func tierRank(t string) int {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "simple":
		return 0
	case "medium":
		return 1
	case "complex":
		return 2
	default:
		return -1
	}
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func letterGradeFor(score float64) string {
	switch {
	case score >= 95:
		return "A+"
	case score >= 90:
		return "A"
	case score >= 85:
		return "A-"
	case score >= 80:
		return "B+"
	case score >= 75:
		return "B"
	case score >= 70:
		return "B-"
	case score >= 65:
		return "C+"
	case score >= 60:
		return "C"
	case score >= 50:
		return "D"
	default:
		return "F"
	}
}

// ScoreEngine computes accuracy, under/over-routing safety, multi-class Brier calibration,
// latency percentiles, and a transparent 100-point composite rating for an engine.
func ScoreEngine(engine string, obs []CaseObservation, scenarios []ScenarioResult) EngineScorecard {
	sc := EngineScorecard{
		Engine:     engine,
		TotalCases: len(obs),
	}
	if len(obs) == 0 {
		return sc
	}

	catMap := map[string]*CategoryScore{}
	catOrder := []string{}
	latencies := make([]int64, 0, len(obs))

	var criticalUnder, minorUnder, overProv int
	var brierSum float64
	var probCount int
	var localCorrect, localCount int
	var confCorrectSum, confWrongSum float64
	var confCorrectN, confWrongN int

	for _, o := range obs {
		cs, exists := catMap[o.Case.Category]
		if !exists {
			cs = &CategoryScore{Category: o.Case.Category}
			catMap[o.Case.Category] = cs
			catOrder = append(catOrder, o.Case.Category)
		}
		cs.Total++
		latencies = append(latencies, o.LatencyMs)
		sc.TotalInputTokens += o.InputTokens
		sc.TotalOutputTokens += o.OutputTokens
		if sc.ExecutionMode == "" && o.ExecutionMode != "" {
			sc.ExecutionMode = o.ExecutionMode
		}

		expRank := tierRank(o.Case.ExpectedTier)
		gotRank := tierRank(o.PredictedTier)

		if o.Error == "" && gotRank == expRank {
			sc.CorrectCases++
			cs.Correct++
			if o.Confidence > 0 {
				confCorrectSum += o.Confidence
				confCorrectN++
			}
		} else {
			if o.Confidence > 0 {
				confWrongSum += o.Confidence
				confWrongN++
			}
			sc.Misses = append(sc.Misses, fmt.Sprintf("%s [%s]: expected=%s got=%s (conf=%.2f, chars=%d)",
				o.Case.ID, o.Case.Category, o.Case.ExpectedTier, o.PredictedTier, o.Confidence, len([]rune(o.Case.Prompt))))
			if gotRank >= 0 && expRank >= 0 {
				diff := expRank - gotRank
				if diff >= 2 {
					criticalUnder++
					cs.UnderRoute++
				} else if diff == 1 {
					minorUnder++
					cs.UnderRoute++
				} else if diff < 0 {
					overProv++
					cs.OverRoute++
				}
			} else {
				criticalUnder++
				cs.UnderRoute++
			}
		}

		// Multi-class Brier score over {simple, medium, complex}
		var probs map[string]float64
		if len(o.Scores) > 0 {
			sSimple := o.Scores["simple"]
			sMed := o.Scores["medium"]
			sComp := o.Scores["complex"]
			sum := sSimple + sMed + sComp
			if sum > 0.95 && sum < 1.05 {
				probs = map[string]float64{"simple": sSimple, "medium": sMed, "complex": sComp}
				sc.HasProbabilities = true
			}
		}
		if probs == nil && o.Confidence > 0 && gotRank >= 0 {
			c := clampFloat(o.Confidence, 0.0, 1.0)
			rem := (1.0 - c) / 2.0
			probs = map[string]float64{"simple": rem, "medium": rem, "complex": rem}
			probs[strings.ToLower(o.PredictedTier)] = c
		}
		if probs != nil {
			for _, label := range []string{"simple", "medium", "complex"} {
				y := 0.0
				if strings.EqualFold(o.Case.ExpectedTier, label) {
					y = 1.0
				}
				d := probs[label] - y
				brierSum += d * d
			}
			probCount++
		}

		if o.HasLocalSignal {
			localCount++
			predLocal := o.LocalEligibleProb >= 0.50
			if predLocal == o.Case.ExpectedLocalEligible {
				localCorrect++
			}
		}
	}

	n := float64(len(obs))
	sc.Accuracy = float64(sc.CorrectCases) / n
	sc.CriticalUnderRouteRate = float64(criticalUnder) / n
	sc.MinorUnderRouteRate = float64(minorUnder) / n
	sc.OverProvisionRate = float64(overProv) / n

	if probCount > 0 {
		sc.BrierScore = brierSum / float64(probCount)
	} else {
		sc.BrierScore = 0.6667
	}
	if localCount > 0 {
		sc.LocalEligibleAccuracy = float64(localCorrect) / float64(localCount)
	}
	if confCorrectN > 0 {
		sc.MeanConfidenceCorrect = confCorrectSum / float64(confCorrectN)
	}
	if confWrongN > 0 {
		sc.MeanConfidenceWrong = confWrongSum / float64(confWrongN)
	}

	// Cost per 1,000 routing decisions:
	// - gemini-decision-1: $0.10 / 1M input tokens, $0.00 output tokens
	// - llm (gemini-3.5-flash-lite): $0.05 / 1M input tokens + $0.20 / 1M output/thought tokens
	lowerEng := strings.ToLower(engine)
	if strings.HasPrefix(lowerEng, "gemini-decision") && sc.TotalInputTokens > 0 {
		avgInputTokens := float64(sc.TotalInputTokens) / n
		sc.EstCostPer1kRoutesUSD = (avgInputTokens * 1000.0 / 1_000_000.0) * 0.10
	} else if strings.HasPrefix(lowerEng, "llm") && (sc.TotalInputTokens > 0 || sc.TotalOutputTokens > 0) {
		avgIn := float64(sc.TotalInputTokens) / n
		avgOut := float64(sc.TotalOutputTokens) / n
		sc.EstCostPer1kRoutesUSD = ((avgIn * 1000.0 / 1_000_000.0) * 0.05) + ((avgOut * 1000.0 / 1_000_000.0) * 0.20)
	}

	// Latency percentiles
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var latSum int64
	for _, l := range latencies {
		latSum += l
	}
	sc.LatencyMeanMs = float64(latSum) / n
	sc.LatencyP50Ms = latencies[(len(latencies)-1)*50/100]
	sc.LatencyP95Ms = latencies[(len(latencies)-1)*95/100]

	for _, cName := range catOrder {
		cs := catMap[cName]
		if cs.Total > 0 {
			cs.Accuracy = float64(cs.Correct) / float64(cs.Total)
		}
		sc.Categories = append(sc.Categories, *cs)
	}

	// Weighted 100-Point Router Rating Rubric:
	// 1. Adversarial Tier Accuracy (35 pts)
	sc.AccuracyPoints = sc.Accuracy * 35.0

	// 2. Under-Routing Safety (25 pts): heavy penalty for 2-tier complex->simple drops, moderate for 1-tier drops, light for over-provisioning waste
	safetyPenalty := 2.0*sc.CriticalUnderRouteRate + 0.75*sc.MinorUnderRouteRate + 0.25*sc.OverProvisionRate
	sc.SafetyPoints = 25.0 * clampFloat(1.0-safetyPenalty, 0.0, 1.0)

	// 3. Calibration & Local-Eligibility Signal Quality (15 pts)
	brierSkill := clampFloat(1.0-(sc.BrierScore/0.6667), 0.0, 1.0)
	if sc.HasProbabilities && localCount > 0 {
		sc.CalibrationPoints = 10.0*brierSkill + 5.0*sc.LocalEligibleAccuracy
	} else {
		// Uncalibrated engines without true simplex distributions or local-eligibility signals cap at 9/15
		sc.CalibrationPoints = 9.0 * brierSkill
	}

	// 4. End-to-End Proxy Policy & Hybrid Failover Invariants (15 pts)
	if len(scenarios) > 0 {
		passed := 0
		for _, s := range scenarios {
			if s.Passed {
				passed++
			}
		}
		sc.PolicyPoints = 15.0 * (float64(passed) / float64(len(scenarios)))
	} else {
		sc.PolicyPoints = 15.0
	}

	// 5. Routing Overhead Tail Latency (10 pts): full credit <= 350ms p95, linear decay to 0 at >= 2000ms
	if sc.LatencyP95Ms <= 350 {
		sc.LatencyPoints = 10.0
	} else if sc.LatencyP95Ms >= 2000 {
		sc.LatencyPoints = 0.0
	} else {
		sc.LatencyPoints = 10.0 * (1.0 - float64(sc.LatencyP95Ms-350)/1650.0)
	}

	sc.CompositeScore = math.Round((sc.AccuracyPoints+sc.SafetyPoints+sc.CalibrationPoints+sc.PolicyPoints+sc.LatencyPoints)*10) / 10
	sc.LetterGrade = letterGradeFor(sc.CompositeScore)
	return sc
}

// RenderMarkdownReport renders a clean, publication-ready Markdown evaluation scorecard.
func RenderMarkdownReport(rep EvalReport) string {
	var b strings.Builder
	ts := rep.GeneratedAt
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	b.WriteString("# Smart Router Adversarial Routing Evaluation & Scorecard\n\n")
	b.WriteString(fmt.Sprintf("- **Target Endpoint**: `%s`\n", rep.TargetURL))
	b.WriteString(fmt.Sprintf("- **GCP Project**: `%s`\n", rep.ProjectID))
	b.WriteString(fmt.Sprintf("- **Evaluated At**: `%s`\n\n", ts))

	b.WriteString("## 1. Head-to-Head Decision Engine Scorecard (20 Adversarial Prompts)\n\n")
	b.WriteString("| Engine | Grade | Score (/100) | Tier Accuracy | Critical Under-Route | Over-Provision Waste | Brier Score | Out Tokens | p50 / p95 (ms) | Cost / 1k Routes |\n")
	b.WriteString("| --- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |\n")
	for _, e := range rep.Engines {
		costStr := "$0.0000"
		if e.EstCostPer1kRoutesUSD > 0 {
			costStr = fmt.Sprintf("$%.4f", e.EstCostPer1kRoutesUSD)
		}
		b.WriteString(fmt.Sprintf("| `%s` | **%s** | **%.1f** | %.1f%% (%d/%d) | %.1f%% | %.1f%% | %.3f | %d | %d / %d ms | %s |\n",
			e.Engine,
			e.LetterGrade,
			e.CompositeScore,
			e.Accuracy*100, e.CorrectCases, e.TotalCases,
			e.CriticalUnderRouteRate*100,
			e.OverProvisionRate*100,
			e.BrierScore,
			e.TotalOutputTokens,
			e.LatencyP50Ms, e.LatencyP95Ms,
			costStr,
		))
	}

	if len(rep.SkippedEngines) > 0 {
		b.WriteString("\n### Skipped Unconfigured External Providers\n\n")
		for _, sk := range rep.SkippedEngines {
			b.WriteString(fmt.Sprintf("- **`%s`**: %s\n", sk.Engine, sk.Reason))
		}
	}

	b.WriteString("\n## 2. Per-Category Adversarial Accuracy Breakdown\n\n")
	b.WriteString("| Engine | `deceptive_brevity` | `verbose_boilerplate` | `homograph_camouflage` | `jargon_in_trivial_task` | `nuanced_medium` |\n")
	b.WriteString("| --- | :---: | :---: | :---: | :---: | :---: |\n")
	for _, e := range rep.Engines {
		catAcc := map[string]string{}
		for _, c := range e.Categories {
			catAcc[c.Category] = fmt.Sprintf("%.0f%% (%d/%d)", c.Accuracy*100, c.Correct, c.Total)
		}
		b.WriteString(fmt.Sprintf("| `%s` | %s | %s | %s | %s | %s |\n",
			e.Engine,
			catAcc["deceptive_brevity"],
			catAcc["verbose_boilerplate"],
			catAcc["homograph_camouflage"],
			catAcc["jargon_in_trivial_task"],
			catAcc["nuanced_medium"],
		))
	}

	b.WriteString("\n## 3. Live End-to-End Proxy Policy & Hybrid Failover Stress Scenarios\n\n")
	b.WriteString("| Scenario | Category | Status | Routed Model | Latency (ms) | Verification Detail |\n")
	b.WriteString("| --- | --- | :---: | --- | :---: | --- |\n")
	for _, s := range rep.Scenarios {
		status := "PASS"
		if !s.Passed {
			status = "FAIL"
		}
		model := s.RoutedModel
		if model == "" {
			model = "—"
		}
		b.WriteString(fmt.Sprintf("| %s | `%s` | **%s** | `%s` | %d ms | %s |\n",
			s.Name, s.Category, status, model, s.LatencyMs, s.Detail))
	}

	b.WriteString("\n## 4. Misroute & Edge-Case Diagnostics\n\n")
	for _, e := range rep.Engines {
		b.WriteString(fmt.Sprintf("### `%s` (Score: %.1f / %s)\n", e.Engine, e.CompositeScore, e.LetterGrade))
		if len(e.Misses) == 0 {
			b.WriteString("- Zero misroutes across all 20 adversarial cases.\n\n")
			continue
		}
		for _, m := range e.Misses {
			b.WriteString(fmt.Sprintf("- %s\n", m))
		}
		b.WriteString("\n")
	}

	return b.String()
}
