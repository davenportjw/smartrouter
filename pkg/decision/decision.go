package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"geminirouter/pkg/config"
)

// PromptFeatures contains the parsed semantic, structural, and syntactic properties of a prompt.
type PromptFeatures struct {
	Length               int     `json:"length"`
	WordCount            int     `json:"word_count"`
	HasCode              bool    `json:"has_code"`
	CodeLines            int     `json:"code_lines"`
	HasMath              bool    `json:"has_math"`
	HasMultiStep         bool    `json:"has_multi_step"`
	HasReasoningKeywords bool    `json:"has_reasoning_keywords"`
	HasTools             bool    `json:"has_tools"`
	HasMultimodal        bool    `json:"has_multimodal"`
	ExtractedTask        string  `json:"extracted_task"`
	CodeRatio            float64 `json:"code_ratio"`
	ComplexityScore      float64 `json:"complexity_score"`
}

// DecisionResult represents the outcome of prompt complexity classification or routing.
type DecisionResult struct {
	Tier        string             `json:"tier"` // "simple", "medium", "complex"
	TargetModel string             `json:"target_model"`
	Engine      string             `json:"engine"` // "jev", "fastino", "clm", "contrastive-lm", "llm", "heuristics", "task_mapping", "session_lock", "prompt_override"
	Confidence  float64            `json:"confidence"`
	Rationale   string             `json:"rationale"`
	Scores      map[string]float64 `json:"scores,omitempty"`
	Features    PromptFeatures     `json:"features"`
}

var (
	codePattern      = regexp.MustCompile("(?m)(```[a-zA-Z0-9_-]*|\\b(func|def|class|import|return|SELECT|FROM|WHERE|INSERT|package|struct|impl|async|await)\\b)")
	mathPattern      = regexp.MustCompile("(?i)(\\\\int|\\\\sum|O\\([n1klog ]+\\)|algorithm|derivative|integral|eigen|matrix|tensor|asymptotic|proof|combinatorics)")
	multiStepPattern = regexp.MustCompile("(?i)(step\\s+[0-9]+|first[,\\s]+then|break\\s+down|pipeline|phase\\s+[0-9]+|multi-step|workflow)")
	reasoningPattern = regexp.MustCompile("(?i)(why\\s+does|explain\\s+how|compare\\s+and\\s+contrast|tradeoffs|edge\\s+cases|optimize|architect|analyze|root\\s+cause)")
	triagePattern    = regexp.MustCompile("(?i)^(hi|hello|hey|what\\s+is|who\\s+is|when\\s+was|where\\s+is|define\\b|convert\\b|fact\\s+check)")
)

// ExtractPromptFeatures extracts structural and syntactic features from the input prompt.
func ExtractPromptFeatures(text string, hasTools, hasMultimodal bool) PromptFeatures {
	trimmed := strings.TrimSpace(text)
	charLen := len(trimmed)
	words := strings.Fields(trimmed)
	wordCount := len(words)

	hasCode := codePattern.MatchString(trimmed)
	hasMath := mathPattern.MatchString(trimmed)
	hasMultiStep := multiStepPattern.MatchString(trimmed)
	hasReasoning := reasoningPattern.MatchString(trimmed)

	codeLines := 0
	lines := strings.Split(trimmed, "\n")
	for _, l := range lines {
		tl := strings.TrimSpace(l)
		if strings.HasPrefix(tl, "```") || strings.HasPrefix(tl, "//") || strings.HasPrefix(tl, "#") ||
			strings.HasPrefix(tl, "func ") || strings.HasPrefix(tl, "def ") || strings.HasPrefix(tl, "class ") ||
			strings.HasPrefix(tl, "import ") || strings.HasPrefix(tl, "return ") {
			codeLines++
		}
	}

	codeRatio := 0.0
	if len(lines) > 0 && codeLines > 0 {
		codeRatio = float64(codeLines) / float64(len(lines))
	}

	// Task category deduction
	task := "general"
	if hasCode || codeLines > 0 {
		task = "code"
	} else if hasMath || hasReasoning {
		task = "reasoning"
	} else if strings.Contains(strings.ToLower(trimmed), "summar") {
		task = "summarization"
	} else if strings.Contains(strings.ToLower(trimmed), "translat") {
		task = "translation"
	} else if triagePattern.MatchString(trimmed) || (wordCount <= 15 && !hasCode && !hasMath) {
		task = "fast_query"
	}

	// Calculate a composite normalized complexity index [0.0 - 1.0]
	score := 0.0
	if charLen > 1000 {
		score += 0.35
	} else if charLen > 300 {
		score += 0.15
	}
	if hasCode {
		score += 0.40
	}
	if hasMath {
		score += 0.35
	}
	if hasMultiStep {
		score += 0.25
	}
	if hasReasoning {
		score += 0.20
	}
	if hasTools {
		score += 0.50
	}
	if hasMultimodal {
		score += 0.50
	}
	if score > 1.0 {
		score = 1.0
	}

	return PromptFeatures{
		Length:               charLen,
		WordCount:            wordCount,
		HasCode:              hasCode,
		CodeLines:            codeLines,
		HasMath:              hasMath,
		HasMultiStep:         hasMultiStep,
		HasReasoningKeywords: hasReasoning,
		HasTools:             hasTools,
		HasMultimodal:        hasMultimodal,
		ExtractedTask:        task,
		CodeRatio:            codeRatio,
		ComplexityScore:      score,
	}
}

// criteriaOverlapBoost calculates keyword overlap between the prompt and custom tier criteria.
func criteriaOverlapBoost(text, criteria string) float64 {
	if strings.TrimSpace(criteria) == "" || strings.TrimSpace(text) == "" {
		return 0.0
	}
	lowerText := strings.ToLower(text)
	tokens := regexp.MustCompile(`[a-zA-Z0-9_-]{3,}`).FindAllString(strings.ToLower(criteria), -1)
	if len(tokens) == 0 {
		return 0.0
	}
	stopWords := map[string]bool{
		"and": true, "the": true, "for": true, "with": true, "that": true,
		"this": true, "from": true, "into": true, "are": true, "queries": true,
		"prompts": true, "questions": true, "tasks": true,
	}
	matches := 0
	valid := 0
	for _, tok := range tokens {
		if stopWords[tok] {
			continue
		}
		valid++
		if strings.Contains(lowerText, tok) {
			matches++
		}
	}
	if valid == 0 || matches == 0 {
		return 0.0
	}
	return math.Min(float64(matches)*1.6, 4.0)
}

// applyMinConfidenceFallback downgrades/normalizes uncertain predictions to the "medium" tier
// when c.MinConfidence > 0 and the classifier's confidence is below that threshold.
func applyMinConfidenceFallback(res DecisionResult, c config.ComplexityRouting) DecisionResult {
	if c.MinConfidence > 0 && res.Confidence < c.MinConfidence && res.Tier != "medium" {
		origTier := res.Tier
		res.Tier = "medium"
		res.TargetModel = resolveModelByTier("medium", c)
		res.Rationale = fmt.Sprintf("%s [min_confidence_fallback: %s@%.2f < %.2f -> medium]", res.Rationale, origTier, res.Confidence, c.MinConfidence)
	}
	return res
}

// buildSystemOnePayload creates a request payload compatible with both /v1/systemone and flat classifier endpoints.
func buildSystemOnePayload(engineName, text string, f PromptFeatures, c config.ComplexityRouting) map[string]interface{} {
	simpleDesc := c.SimpleCriteria
	if simpleDesc == "" {
		simpleDesc = "simple: Short greetings, factual lookups, single-step formatting, or trivial translations."
	}
	mediumDesc := c.MediumCriteria
	if mediumDesc == "" {
		mediumDesc = "medium: Standard summarization, multi-paragraph drafting, moderate explanations, or general Q&A."
	}
	complexDesc := c.ComplexCriteria
	if complexDesc == "" {
		complexDesc = "complex: Multi-step reasoning, code generation/debugging, mathematical proofs, or system architecture."
	}

	modelID := "jev/systemone"
	switch NormalizeEngineName(engineName) {
	case "fastino":
		modelID = "fastino/GLiDE"
	case "contrastive-lm":
		modelID = "contrastive-lm/rank"
	}

	return map[string]interface{}{
		"model":    modelID,
		"prompt":   text,
		"features": f,
		"context": []map[string]string{
			{"role": "user", "content": text},
		},
		"questions": map[string]interface{}{
			"complexity": map[string]interface{}{
				"question": "Classify the routing complexity tier of this user request.",
				"type":     "choice",
				"choices":  []string{"simple", "medium", "complex"},
				"criteria": map[string]string{
					"simple":  simpleDesc,
					"medium":  mediumDesc,
					"complex": complexDesc,
				},
			},
		},
		// Also include contrastive /rank fields so a single request structure works across endpoints
		"query": text,
		"candidates": []string{
			"simple: " + simpleDesc,
			"medium: " + mediumDesc,
			"complex: " + complexDesc,
		},
		"top_k": 3,
	}
}

// parseRemoteDecisionResponse decodes responses from:
// 1. /v1/systemone structured JSON (`answers.complexity.{choice, confidence, probabilities, rationale}`)
// 2. Flat classifier JSON (`{tier, confidence, rationale, scores}`)
// 3. Contrastive /rank JSON array (`[{rank, candidate, prob, score}, ...]`)
func parseRemoteDecisionResponse(body []byte, engineName, rationalePrefix string, f PromptFeatures, c config.ComplexityRouting) (DecisionResult, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return DecisionResult{}, fmt.Errorf("empty remote classifier response")
	}

	// Case 1: JSON Array from contrastive /rank endpoint
	if trimmed[0] == '[' {
		var rankItems []struct {
			Rank      int     `json:"rank"`
			Candidate string  `json:"candidate"`
			Prob      float64 `json:"prob"`
			Score     float64 `json:"score"`
		}
		if err := json.Unmarshal(trimmed, &rankItems); err != nil {
			return DecisionResult{}, err
		}
		if len(rankItems) == 0 {
			return DecisionResult{}, fmt.Errorf("empty rank array from remote contrastive endpoint")
		}
		scores := map[string]float64{}
		bestTier := "medium"
		bestConf := 0.0
		for i, item := range rankItems {
			tier := inferTierFromCandidate(item.Candidate, c)
			val := item.Prob
			if val <= 0 {
				val = item.Score
			}
			scores[tier] = math.Round(val*1000) / 1000
			if i == 0 || val > bestConf {
				bestTier = tier
				bestConf = val
			}
		}
		return DecisionResult{
			Tier:       bestTier,
			Engine:     engineName,
			Confidence: math.Round(bestConf*100) / 100,
			Rationale:  fmt.Sprintf("%s[rank top=%s prob=%.2f]", rationalePrefix, bestTier, bestConf),
			Scores:     scores,
			Features:   f,
		}, nil
	}

	// Case 2: JSON Object (/v1/systemone or flat format)
	var rawObj struct {
		// Flat fields
		Tier       string             `json:"tier"`
		Choice     string             `json:"choice"`
		Confidence float64            `json:"confidence"`
		Rationale  string             `json:"rationale"`
		Scores     map[string]float64 `json:"scores"`
		// /v1/systemone fields
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
			Rationale     string             `json:"rationale"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(trimmed, &rawObj); err != nil {
		return DecisionResult{}, err
	}

	if ans, ok := rawObj.Answers["complexity"]; ok && (ans.Choice != "" || len(ans.Probabilities) > 0) {
		tier := normalizeTierName(ans.Choice)
		conf := ans.Confidence
		if conf <= 0 && len(ans.Probabilities) > 0 {
			conf = ans.Probabilities[tier]
		}
		rat := ans.Rationale
		if rat == "" {
			rat = fmt.Sprintf("%s[systemone choice=%s conf=%.2f]", rationalePrefix, tier, conf)
		} else if !strings.Contains(rat, rationalePrefix) {
			rat = fmt.Sprintf("%s[%s]", rationalePrefix, rat)
		}
		return DecisionResult{
			Tier:       tier,
			Engine:     engineName,
			Confidence: conf,
			Rationale:  rat,
			Scores:     ans.Probabilities,
			Features:   f,
		}, nil
	}

	tier := normalizeTierName(rawObj.Tier)
	if tier == "" {
		tier = normalizeTierName(rawObj.Choice)
	}
	if tier == "" {
		return DecisionResult{}, fmt.Errorf("missing complexity tier in remote response")
	}
	rat := rawObj.Rationale
	if rat == "" {
		rat = fmt.Sprintf("%s[choice=%s conf=%.2f]", rationalePrefix, tier, rawObj.Confidence)
	} else if rationalePrefix != "jev-remote" && !strings.Contains(rat, rationalePrefix) {
		rat = fmt.Sprintf("%s[%s]", rationalePrefix, rat)
	}

	return DecisionResult{
		Tier:       tier,
		Engine:     engineName,
		Confidence: rawObj.Confidence,
		Rationale:  rat,
		Scores:     rawObj.Scores,
		Features:   f,
	}, nil
}

func normalizeTierName(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "simple", "low", "easy", "fast", "lite":
		return "simple"
	case "medium", "standard", "moderate", "default":
		return "medium"
	case "complex", "high", "hard", "pro", "deep", "reasoning":
		return "complex"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func inferTierFromCandidate(cand string, c config.ComplexityRouting) string {
	lc := strings.ToLower(strings.TrimSpace(cand))
	if strings.HasPrefix(lc, "simple") || (c.SimpleCriteria != "" && strings.Contains(lc, strings.ToLower(c.SimpleCriteria))) {
		return "simple"
	}
	if strings.HasPrefix(lc, "complex") || (c.ComplexCriteria != "" && strings.Contains(lc, strings.ToLower(c.ComplexCriteria))) {
		return "complex"
	}
	if strings.HasPrefix(lc, "medium") || (c.MediumCriteria != "" && strings.Contains(lc, strings.ToLower(c.MediumCriteria))) {
		return "medium"
	}
	if strings.Contains(lc, "proof") || strings.Contains(lc, "architect") || strings.Contains(lc, "code") || strings.Contains(lc, "complex") {
		return "complex"
	}
	if strings.Contains(lc, "greet") || strings.Contains(lc, "factual") || strings.Contains(lc, "simple") || strings.Contains(lc, "short") {
		return "simple"
	}
	return "medium"
}

// JevDecisionEngine implements fast typed Structured Decision Model (System One) routing.
type JevDecisionEngine struct {
	Client *http.Client
}

// NewJevDecisionEngine creates an instance of the Jev Structured Decision Engine.
func NewJevDecisionEngine() *JevDecisionEngine {
	return &JevDecisionEngine{
		Client: &http.Client{Timeout: 800 * time.Millisecond},
	}
}

// Classify classifies prompt complexity into simple, medium, or complex using calibrated decision weights.
func (j *JevDecisionEngine) Classify(ctx context.Context, text string, c config.ComplexityRouting, hasTools, hasMultimodal bool) DecisionResult {
	features := ExtractPromptFeatures(text, hasTools, hasMultimodal)

	// Bypass rules
	if hasTools && c.ForceComplexTools {
		return DecisionResult{
			Tier:        "complex",
			TargetModel: resolveModelByTier("complex", c),
			Engine:      "jev",
			Confidence:  0.99,
			Rationale:   "Forced complex tier due to presence of active tools/function calls.",
			Features:    features,
		}
	}
	if hasMultimodal && c.ForceComplexMultimodal {
		return DecisionResult{
			Tier:        "complex",
			TargetModel: resolveModelByTier("complex", c),
			Engine:      "jev",
			Confidence:  0.99,
			Rationale:   "Forced complex tier due to presence of multimodal audio/video/image parts.",
			Features:    features,
		}
	}

	// If remote Jev endpoint is specified (in per-app config, global provider config, or JEV_ENDPOINT env), attempt fast RPC call
	endpoint := strings.TrimSpace(c.JevEndpoint)
	if endpoint == "" {
		endpoint = GetEngineEndpoint("jev")
	}
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("JEV_ENDPOINT"))
	}
	if endpoint != "" {
		if res, err := j.callRemoteJevWithConfig(ctx, endpoint, text, features, c); err == nil {
			res.TargetModel = resolveModelByTier(res.Tier, c)
			return applyMinConfidenceFallback(res, c)
		}
	}

	// Local Jev Structured Decision Model (System One fast probability scoring)
	// Compute calibrated unnormalized log-odds (logits)
	logitSimple := 1.2 + criteriaOverlapBoost(text, c.SimpleCriteria)
	logitMedium := 1.0 + criteriaOverlapBoost(text, c.MediumCriteria)
	logitComplex := 0.5 + criteriaOverlapBoost(text, c.ComplexCriteria)

	if features.HasCode {
		logitComplex += 2.5
		logitSimple -= 2.0
	}
	if features.HasMath {
		logitComplex += 2.2
		logitSimple -= 1.8
	}
	if features.HasMultiStep {
		logitComplex += 1.4
		logitMedium += 1.0
		logitSimple -= 1.0
	}
	if features.HasReasoningKeywords {
		logitComplex += 1.2
		logitMedium += 0.8
	}
	simpleLimit := c.SimpleCharLimit
	if simpleLimit <= 0 {
		simpleLimit = 200
	}
	mediumLimit := c.MediumCharLimit
	if mediumLimit <= 0 {
		mediumLimit = 1000
	}

	if features.Length <= simpleLimit && !features.HasCode && !features.HasMath {
		logitSimple += 2.0
	} else if features.Length > mediumLimit {
		logitComplex += 1.5
		logitSimple -= 1.5
	} else {
		logitMedium += 1.2
	}

	// Softmax normalization
	maxLogit := math.Max(logitSimple, math.Max(logitMedium, logitComplex))
	expSimple := math.Exp(logitSimple - maxLogit)
	expMedium := math.Exp(logitMedium - maxLogit)
	expComplex := math.Exp(logitComplex - maxLogit)
	sumExp := expSimple + expMedium + expComplex

	pSimple := expSimple / sumExp
	pMedium := expMedium / sumExp
	pComplex := expComplex / sumExp

	scores := map[string]float64{
		"simple":  math.Round(pSimple*1000) / 1000,
		"medium":  math.Round(pMedium*1000) / 1000,
		"complex": math.Round(pComplex*1000) / 1000,
	}

	tier := "medium"
	conf := pMedium
	rationale := "Prompt contains balanced multi-step instructions suitable for standard generation."

	if pComplex >= pMedium && pComplex >= pSimple {
		tier = "complex"
		conf = pComplex
		reasons := []string{}
		if features.HasCode {
			reasons = append(reasons, "code constructs/syntax")
		}
		if features.HasMath {
			reasons = append(reasons, "mathematical/algorithmic formulations")
		}
		if features.HasMultiStep {
			reasons = append(reasons, "multi-step reasoning directives")
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "high token density and depth")
		}
		rationale = fmt.Sprintf("Jev Structured Decision Model classified as complex due to %s.", strings.Join(reasons, ", "))
	} else if pSimple >= pMedium && pSimple >= pComplex {
		tier = "simple"
		conf = pSimple
		rationale = "Jev Structured Decision Model classified as simple (short conversational or direct factual lookup)."
	}

	res := DecisionResult{
		Tier:        tier,
		TargetModel: resolveModelByTier(tier, c),
		Engine:      "jev",
		Confidence:  math.Round(conf*100) / 100,
		Rationale:   rationale,
		Scores:      scores,
		Features:    features,
	}
	return applyMinConfidenceFallback(res, c)
}

func (j *JevDecisionEngine) callRemoteJev(ctx context.Context, endpoint, text string, f PromptFeatures) (DecisionResult, error) {
	return j.callRemoteJevWithConfig(ctx, endpoint, text, f, config.ComplexityRouting{})
}

func (j *JevDecisionEngine) callRemoteJevWithConfig(ctx context.Context, endpoint, text string, f PromptFeatures, c config.ComplexityRouting) (DecisionResult, error) {
	return callRemoteDecisionEndpoint(ctx, j.Client, endpoint, "jev", "jev-remote", text, f, c)
}

func callRemoteDecisionEndpoint(ctx context.Context, client *http.Client, endpoint, engineName, rationalePrefix, text string, f PromptFeatures, c config.ComplexityRouting) (DecisionResult, error) {
	if err := validateRemoteJevURL(endpoint); err != nil {
		return DecisionResult{}, err
	}
	if client == nil {
		client = &http.Client{Timeout: 800 * time.Millisecond}
	}

	payload := buildSystemOnePayload(engineName, text, f, c)
	reqBody, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return DecisionResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	if secret := ResolveEngineSecret(engineName, c.SecretRef); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := client.Do(req)
	if err != nil {
		return DecisionResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return DecisionResult{}, fmt.Errorf("remote %s error: %d", engineName, resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return DecisionResult{}, err
	}

	return parseRemoteDecisionResponse(bodyBytes, engineName, rationalePrefix, f, c)
}

func isClusterInternalHost(h string) bool {
	if h == "metadata.google.internal" || h == "metadata" {
		return false
	}
	return strings.HasSuffix(h, ".svc.cluster.local") ||
		strings.HasSuffix(h, ".cluster.local") ||
		strings.HasSuffix(h, ".internal") ||
		strings.HasSuffix(h, ".local")
}

func validateRemoteJevURL(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("invalid remote Jev endpoint URL")
	}
	h := strings.ToLower(u.Hostname())
	if h == "metadata.google.internal" || h == "169.254.169.254" || h == "fd00:ec2::254" || h == "metadata" {
		return fmt.Errorf("metadata endpoints are forbidden for remote Jev")
	}
	if os.Getenv("LOCAL_DEV") != "true" {
		if h == "localhost" {
			return fmt.Errorf("loopback host forbidden for remote Jev in production: %s", h)
		}
		if ip := net.ParseIP(h); ip != nil {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.String() == "fd00:ec2::254" {
				return fmt.Errorf("loopback/link-local/unspecified IP forbidden for remote Jev in production: %s", ip.String())
			}
			// RFC1918 / private VPC cluster IPs (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) are permitted for internal cluster runners.
			return nil
		}
		ips, err := net.LookupIP(h)
		if err != nil || len(ips) == 0 {
			if isClusterInternalHost(h) {
				return nil
			}
			return fmt.Errorf("failed to resolve remote Jev endpoint host: %w", err)
		}
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.String() == "fd00:ec2::254" {
				return fmt.Errorf("loopback/link-local/unspecified IP forbidden for remote Jev in production: %s", ip.String())
			}
		}
	}
	return nil
}

// ProbeDecisionProvider executes a live verification probe against the specified decision engine
// ("jev", "fastino", or "contrastive-lm"). If a remote or cluster endpoint is configured (or passed
// via endpointOverride), it performs a real HTTP POST against that endpoint and surfaces any error
// directly. If no remote endpoint is configured, it runs the local embedded classifier.
func ProbeDecisionProvider(ctx context.Context, engine, endpointOverride string) (DecisionResult, time.Duration, string, error) {
	start := time.Now()
	canonical := NormalizeEngineName(engine)
	if canonical == "" {
		return DecisionResult{}, 0, "", fmt.Errorf("unsupported decision engine: %s", engine)
	}

	samplePrompt := "Implement a concurrent worker pool in Go with context cancellation and bounded error channels."
	features := ExtractPromptFeatures(samplePrompt, false, false)
	cfg := config.ComplexityRouting{
		Enabled:      true,
		Engine:       canonical,
		SimpleModel:  "gemini-3.5-flash-lite",
		MediumModel:  "gemini-3.8-flash",
		ComplexModel: "gemini-3.1-pro",
	}

	_, mode, _ := GetEngineClusterConfig(canonical)
	endpoint := strings.TrimSpace(endpointOverride)
	if endpoint == "" && mode != "local" {
		endpoint = GetEngineEndpoint(canonical)
	}
	if endpoint == "" && mode != "local" {
		switch canonical {
		case "jev":
			endpoint = strings.TrimSpace(os.Getenv("JEV_ENDPOINT"))
		case "fastino":
			endpoint = strings.TrimSpace(os.Getenv("FASTINO_ENDPOINT"))
		case "contrastive-lm":
			endpoint = strings.TrimSpace(os.Getenv("CLM_ENDPOINT"))
		}
	}

	if endpoint != "" {
		client := &http.Client{Timeout: 1500 * time.Millisecond}
		res, err := callRemoteDecisionEndpoint(ctx, client, endpoint, canonical, canonical+"-remote", samplePrompt, features, cfg)
		elapsed := time.Since(start)
		if err != nil {
			return DecisionResult{}, elapsed, endpoint, err
		}
		res.TargetModel = resolveModelByTier(res.Tier, cfg)
		return res, elapsed, endpoint, nil
	}

	var res DecisionResult
	switch canonical {
	case "fastino":
		res = NewFastinoDecisionEngine().Classify(ctx, samplePrompt, cfg, false, false)
	case "contrastive-lm":
		res = NewCLMContrastiveEngine().Classify(ctx, samplePrompt, cfg, false, false)
	default:
		res = NewJevDecisionEngine().Classify(ctx, samplePrompt, cfg, false, false)
	}
	return res, time.Since(start), "local-embedded", nil
}

// FastinoDecisionEngine implements ultra-low-latency System One / Fastino token & criteria classification.
type FastinoDecisionEngine struct {
	Client *http.Client
}

// NewFastinoDecisionEngine creates an instance of the Fastino Decision Engine.
func NewFastinoDecisionEngine() *FastinoDecisionEngine {
	return &FastinoDecisionEngine{
		Client: &http.Client{Timeout: 800 * time.Millisecond},
	}
}

// Classify evaluates prompt complexity via a remote Fastino endpoint or local calibrated multi-head discriminator.
func (fEngine *FastinoDecisionEngine) Classify(ctx context.Context, text string, c config.ComplexityRouting, hasTools, hasMultimodal bool) DecisionResult {
	features := ExtractPromptFeatures(text, hasTools, hasMultimodal)

	if hasTools && c.ForceComplexTools {
		return DecisionResult{
			Tier:        "complex",
			TargetModel: resolveModelByTier("complex", c),
			Engine:      "fastino",
			Confidence:  0.99,
			Rationale:   "fastino-local[Forced complex tier due to presence of active tools/function calls]",
			Features:    features,
		}
	}
	if hasMultimodal && c.ForceComplexMultimodal {
		return DecisionResult{
			Tier:        "complex",
			TargetModel: resolveModelByTier("complex", c),
			Engine:      "fastino",
			Confidence:  0.99,
			Rationale:   "fastino-local[Forced complex tier due to presence of multimodal parts]",
			Features:    features,
		}
	}

	endpoint := strings.TrimSpace(c.FastinoEndpoint)
	if endpoint == "" {
		endpoint = GetEngineEndpoint("fastino")
	}
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("FASTINO_ENDPOINT"))
	}
	if endpoint != "" {
		if res, err := callRemoteDecisionEndpoint(ctx, fEngine.Client, endpoint, "fastino", "fastino-remote", text, features, c); err == nil {
			res.TargetModel = resolveModelByTier(res.Tier, c)
			return applyMinConfidenceFallback(res, c)
		}
	}

	// Local Fastino linear discriminator with criteria awareness
	logitSimple := 1.35 + criteriaOverlapBoost(text, c.SimpleCriteria)
	logitMedium := 1.05 + criteriaOverlapBoost(text, c.MediumCriteria)
	logitComplex := 0.55 + criteriaOverlapBoost(text, c.ComplexCriteria)

	if features.HasCode {
		logitComplex += 2.6
		logitSimple -= 2.1
	}
	if features.HasMath {
		logitComplex += 2.3
		logitSimple -= 1.9
	}
	if features.HasMultiStep {
		logitComplex += 1.5
		logitMedium += 0.9
		logitSimple -= 1.1
	}
	if features.HasReasoningKeywords {
		logitComplex += 1.3
		logitMedium += 0.7
	}

	simpleLimit := c.SimpleCharLimit
	if simpleLimit <= 0 {
		simpleLimit = 200
	}
	mediumLimit := c.MediumCharLimit
	if mediumLimit <= 0 {
		mediumLimit = 1000
	}

	if features.Length <= simpleLimit && !features.HasCode && !features.HasMath {
		logitSimple += 2.1
	} else if features.Length > mediumLimit {
		logitComplex += 1.6
		logitSimple -= 1.5
	} else {
		logitMedium += 1.25
	}

	maxLogit := math.Max(logitSimple, math.Max(logitMedium, logitComplex))
	expSimple := math.Exp(logitSimple - maxLogit)
	expMedium := math.Exp(logitMedium - maxLogit)
	expComplex := math.Exp(logitComplex - maxLogit)
	sumExp := expSimple + expMedium + expComplex

	pSimple := expSimple / sumExp
	pMedium := expMedium / sumExp
	pComplex := expComplex / sumExp

	scores := map[string]float64{
		"simple":  math.Round(pSimple*1000) / 1000,
		"medium":  math.Round(pMedium*1000) / 1000,
		"complex": math.Round(pComplex*1000) / 1000,
	}

	tier := "medium"
	conf := pMedium
	if pComplex >= pMedium && pComplex >= pSimple {
		tier = "complex"
		conf = pComplex
	} else if pSimple >= pMedium && pSimple >= pComplex {
		tier = "simple"
		conf = pSimple
	}

	res := DecisionResult{
		Tier:        tier,
		TargetModel: resolveModelByTier(tier, c),
		Engine:      "fastino",
		Confidence:  math.Round(conf*100) / 100,
		Rationale:   fmt.Sprintf("fastino-local[tier=%s task=%s conf=%.2f]", tier, features.ExtractedTask, conf),
		Scores:      scores,
		Features:    features,
	}
	return applyMinConfidenceFallback(res, c)
}

// CLMContrastiveEngine implements Contrastive Language Model representation anchor matching.
type CLMContrastiveEngine struct {
	AnchorSimple  []float64
	AnchorMedium  []float64
	AnchorComplex []float64
	Client        *http.Client
}

// NewCLMContrastiveEngine creates a new Contrastive Language Model classifier with pre-calibrated anchor profiles.
func NewCLMContrastiveEngine() *CLMContrastiveEngine {
	return &CLMContrastiveEngine{
		// Anchors: [lengthNorm, codeWeight, mathWeight, stepWeight, densityScore]
		AnchorSimple:  []float64{0.10, 0.00, 0.00, 0.05, 0.15},
		AnchorMedium:  []float64{0.45, 0.10, 0.15, 0.40, 0.50},
		AnchorComplex: []float64{0.90, 0.95, 0.90, 0.85, 0.90},
		Client:        &http.Client{Timeout: 800 * time.Millisecond},
	}
}

// Classify evaluates prompt contrastive embedding similarity against tier anchor profiles or a remote CLM /rank endpoint.
func (cEngine *CLMContrastiveEngine) Classify(ctx context.Context, text string, c config.ComplexityRouting, hasTools, hasMultimodal bool) DecisionResult {
	features := ExtractPromptFeatures(text, hasTools, hasMultimodal)
	engineLabel := "clm"
	if strings.EqualFold(strings.TrimSpace(c.Engine), "contrastive-lm") {
		engineLabel = "contrastive-lm"
	}

	// Bypasses
	if hasTools && c.ForceComplexTools {
		return DecisionResult{
			Tier:        "complex",
			TargetModel: resolveModelByTier("complex", c),
			Engine:      engineLabel,
			Confidence:  0.99,
			Rationale:   "Forced complex tier due to presence of active tools/function calls.",
			Features:    features,
		}
	}
	if hasMultimodal && c.ForceComplexMultimodal {
		return DecisionResult{
			Tier:        "complex",
			TargetModel: resolveModelByTier("complex", c),
			Engine:      engineLabel,
			Confidence:  0.99,
			Rationale:   "Forced complex tier due to presence of multimodal audio/video/image parts.",
			Features:    features,
		}
	}

	endpoint := strings.TrimSpace(c.CLMEndpoint)
	if endpoint == "" {
		endpoint = GetEngineEndpoint("contrastive-lm")
	}
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("CLM_ENDPOINT"))
	}
	if endpoint != "" {
		if res, err := callRemoteDecisionEndpoint(ctx, cEngine.Client, endpoint, engineLabel, "clm-remote", text, features, c); err == nil {
			res.TargetModel = resolveModelByTier(res.Tier, c)
			return applyMinConfidenceFallback(res, c)
		}
	}

	// Build normalized feature vector v
	lenNorm := math.Min(float64(features.Length)/1500.0, 1.0)
	codeNorm := 0.0
	if features.HasCode {
		codeNorm = 0.7 + (features.CodeRatio * 0.3)
	}
	mathNorm := 0.0
	if features.HasMath {
		mathNorm = 0.9
	}
	stepNorm := 0.0
	if features.HasMultiStep {
		stepNorm = 0.8
	} else if features.HasReasoningKeywords {
		stepNorm = 0.5
	}
	densityNorm := features.ComplexityScore

	v := []float64{lenNorm, codeNorm, mathNorm, stepNorm, densityNorm}

	distSimple := euclideanDistance(v, cEngine.AnchorSimple)
	distMedium := euclideanDistance(v, cEngine.AnchorMedium)
	distComplex := euclideanDistance(v, cEngine.AnchorComplex)

	simSimple := (1.0 / (1.0 + distSimple)) + (criteriaOverlapBoost(text, c.SimpleCriteria) * 0.12)
	simMedium := (1.0 / (1.0 + distMedium)) + (criteriaOverlapBoost(text, c.MediumCriteria) * 0.12)
	simComplex := (1.0 / (1.0 + distComplex)) + (criteriaOverlapBoost(text, c.ComplexCriteria) * 0.12)

	if features.HasCode {
		simComplex += 0.28
		simSimple -= 0.18
	}
	if features.HasMath {
		simComplex += 0.25
		simSimple -= 0.16
	}
	if features.HasMultiStep {
		simComplex += 0.14
		simSimple -= 0.10
	}

	scores := map[string]float64{
		"simple":  math.Round(simSimple*1000) / 1000,
		"medium":  math.Round(simMedium*1000) / 1000,
		"complex": math.Round(simComplex*1000) / 1000,
	}

	tier := "medium"
	bestSim := simMedium
	rationale := "CLM Contrastive Anchor (clm-5d): prompt vector aligns most closely with standard generation tier."

	if simComplex >= simMedium && simComplex >= simSimple {
		tier = "complex"
		bestSim = simComplex
		margin := simComplex - math.Max(simMedium, simSimple)
		rationale = fmt.Sprintf("CLM Contrastive Anchor (clm-5d): prompt vector matches complex anchor profile with contrastive margin %.2f.", margin)
	} else if simSimple >= simMedium && simSimple >= simComplex {
		tier = "simple"
		bestSim = simSimple
		margin := simSimple - math.Max(simMedium, simComplex)
		rationale = fmt.Sprintf("CLM Contrastive Anchor (clm-5d): prompt vector matches simple/factual anchor profile with contrastive margin %.2f.", margin)
	}

	res := DecisionResult{
		Tier:        tier,
		TargetModel: resolveModelByTier(tier, c),
		Engine:      engineLabel,
		Confidence:  math.Round(math.Min(bestSim, 0.99)*100) / 100,
		Rationale:   rationale,
		Scores:      scores,
		Features:    features,
	}
	return applyMinConfidenceFallback(res, c)
}

func euclideanDistance(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		diff := a[i] - b[i]
		sum += diff * diff
	}
	return math.Sqrt(sum)
}

// ResolveTaskModel maps an agent task or kind to a configured model tier.
func ResolveTaskModel(task string, c config.ComplexityRouting) (model string, tier string, ok bool) {
	if task == "" {
		return "", "", false
	}
	cleanedTask := strings.TrimSpace(strings.ToLower(task))

	// 1. Check user custom TaskMapping
	if c.TaskMapping != nil {
		if mappedModel, exists := c.TaskMapping[cleanedTask]; exists && mappedModel != "" {
			return mappedModel, "custom_task", true
		}
	}

	// 2. Default intelligent task matrix
	switch cleanedTask {
	case "code", "coding", "code_generation", "debug", "debugging", "refactor", "architecture", "planning", "reasoning", "deep_analysis", "complex":
		return ResolveModelByTier("complex", c), "complex", true
	case "summarization", "summary", "translate", "translation", "draft", "drafting", "chat", "general", "medium":
		return ResolveModelByTier("medium", c), "medium", true
	case "triage", "fast_query", "fact_check", "lookup", "greeting", "classification", "simple":
		return ResolveModelByTier("simple", c), "simple", true
	}

	return "", "", false
}

// ResolveModelByTier returns the configured model for a given complexity tier.
func ResolveModelByTier(tier string, c config.ComplexityRouting) string {
	switch tier {
	case "simple":
		if c.SimpleModel != "" {
			return c.SimpleModel
		}
		return "gemini-3.5-flash-lite"
	case "medium":
		if c.MediumModel != "" {
			return c.MediumModel
		}
		return "gemini-3.8-flash"
	case "complex":
		if c.ComplexModel != "" {
			return c.ComplexModel
		}
		return "gemini-3.1-pro"
	default:
		return "gemini-3.8-flash"
	}
}

func resolveModelByTier(tier string, c config.ComplexityRouting) string {
	return ResolveModelByTier(tier, c)
}

// InferTierFromModel returns the tier ("simple", "medium", or "complex") corresponding to a given model name.
func InferTierFromModel(model string, c config.ComplexityRouting) string {
	cleanModel := config.StripLocationSuffix(model)
	complexModel := c.ComplexModel
	if complexModel == "" {
		complexModel = "gemini-3.1-pro"
	}
	simpleModel := c.SimpleModel
	if simpleModel == "" {
		simpleModel = "gemini-3.5-flash-lite"
	}
	if cleanModel == config.StripLocationSuffix(complexModel) || strings.Contains(cleanModel, "pro") {
		return "complex"
	}
	if cleanModel == config.StripLocationSuffix(simpleModel) || strings.Contains(cleanModel, "lite") {
		return "simple"
	}
	return "medium"
}
