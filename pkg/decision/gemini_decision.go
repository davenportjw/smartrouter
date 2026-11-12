package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"geminirouter/pkg/config"
)

const (
	// DefaultGeminiDecisionEndpoint is the official Google Gemini Decisions API v1beta endpoint.
	DefaultGeminiDecisionEndpoint = "https://generativelanguage.googleapis.com/v1beta/decisions"
	// GeminiDecisionModel is the dedicated zero-output-token calibrated classifier model.
	GeminiDecisionModel = "gemini-decision-1"
)

// DecisionInputBlock represents a single multimodal or text content block in a Gemini Decisions API request.
type DecisionInputBlock struct {
	Type       string `json:"type"`                 // "text", "image", "audio", "video", "document"
	Text       string `json:"text,omitempty"`       // For type="text"
	Data       string `json:"data,omitempty"`       // Base64-encoded bytes for inline media
	URI        string `json:"uri,omitempty"`        // Files API or HTTPS URI (no gs:// or YouTube URLs)
	MimeType   string `json:"mime_type,omitempty"`  // Required when Data is populated
	Resolution string `json:"resolution,omitempty"` // Optional "low", "medium", "high"
}

// GeminiDecisionEngine performs single-forward-pass semantic routing via Google's Gemini Decisions API
// (POST /v1beta/decisions, model "gemini-decision-1") using keyless Google Cloud ADC credentials.
type GeminiDecisionEngine struct {
	TokenSource             oauth2.TokenSource
	ProjectID               string
	DefaultEndpoint         string
	APIKeysBaseURL          string
	HTTPClient              *http.Client
	AllowLoopbackForTesting bool

	mu                  sync.Mutex
	cachedAuthKey       string
	cachedAuthKeyExpiry time.Time
}

// NewGeminiDecisionEngine constructs a GeminiDecisionEngine backed by Google Cloud Application Default Credentials.
func NewGeminiDecisionEngine(ts oauth2.TokenSource, projectID string) *GeminiDecisionEngine {
	return &GeminiDecisionEngine{
		TokenSource:     ts,
		ProjectID:       strings.TrimSpace(projectID),
		DefaultEndpoint: DefaultGeminiDecisionEndpoint,
		APIKeysBaseURL:  "https://apikeys.googleapis.com",
		HTTPClient: &http.Client{
			Timeout: 3500 * time.Millisecond,
		},
	}
}

// ResolveEndpoint returns the active Gemini Decisions API endpoint URL for a ComplexityRouting config.
func (g *GeminiDecisionEngine) ResolveEndpoint(c config.ComplexityRouting) string {
	if ep := strings.TrimSpace(c.GeminiDecisionEndpoint); ep != "" {
		return ep
	}
	if ep := strings.TrimSpace(GetEngineEndpoint("gemini-decision")); ep != "" && ep != DefaultGeminiDecisionEndpoint {
		return ep
	}
	if ep := strings.TrimSpace(os.Getenv("GEMINI_DECISION_ENDPOINT")); ep != "" {
		return ep
	}
	if g != nil && strings.TrimSpace(g.DefaultEndpoint) != "" {
		return strings.TrimSpace(g.DefaultEndpoint)
	}
	return DefaultGeminiDecisionEndpoint
}

func validateDecisionEndpoint(endpoint string, allowLoopback bool) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("invalid Gemini Decisions endpoint URL: %q", endpoint)
	}
	h := strings.ToLower(u.Hostname())
	if h == "metadata.google.internal" || h == "169.254.169.254" || h == "fd00:ec2::254" || h == "metadata" {
		return fmt.Errorf("metadata endpoints are forbidden for Gemini Decisions API")
	}
	if !allowLoopback && os.Getenv("LOCAL_DEV") != "true" {
		if h == "localhost" {
			return fmt.Errorf("loopback host forbidden for Gemini Decisions API in production: %s", h)
		}
		if ip := net.ParseIP(h); ip != nil {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.String() == "fd00:ec2::254" {
				return fmt.Errorf("loopback/link-local/unspecified IP forbidden for Gemini Decisions API in production: %s", ip.String())
			}
			return nil
		}
		ips, err := net.LookupIP(h)
		if err != nil || len(ips) == 0 {
			if isClusterInternalHost(h) {
				return nil
			}
			return fmt.Errorf("failed to resolve Gemini Decisions endpoint host: %w", err)
		}
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.String() == "fd00:ec2::254" {
				return fmt.Errorf("loopback/link-local/unspecified IP forbidden for Gemini Decisions API in production: %s", ip.String())
			}
		}
	}
	return nil
}

// ValidatePassthroughEndpoint applies SSRF validation before forwarding a direct /v1beta/decisions request.
func (g *GeminiDecisionEngine) ValidatePassthroughEndpoint(endpoint string) error {
	allowLoopback := g != nil && g.AllowLoopbackForTesting
	return validateDecisionEndpoint(endpoint, allowLoopback)
}

// AttachUpstreamAuth attaches keyless Google Cloud ADC credentials to an outgoing Gemini Decisions API request.
// When targeting generativelanguage.googleapis.com (which rejects raw service-account OAuth2 Bearer tokens),
// it dynamically resolves and caches a project-managed Authorization Key restricted to generativelanguage.googleapis.com
// via Google Cloud API Keys v2 (apikeys.googleapis.com) using ADC.
func (g *GeminiDecisionEngine) AttachUpstreamAuth(ctx context.Context, req *http.Request, secretRef string) error {
	if explicitKey := ResolveEngineSecret("gemini-decision", secretRef); explicitKey != "" {
		req.Header.Set("x-goog-api-key", explicitKey)
		return nil
	}

	var ts oauth2.TokenSource
	if g != nil {
		ts = g.TokenSource
	}
	if ts == nil {
		var err error
		ts, err = google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return fmt.Errorf("gemini-decision ADC credentials unavailable: %w", err)
		}
	}

	tok, err := ts.Token()
	if err != nil {
		return fmt.Errorf("gemini-decision ADC token fetch failed: %w", err)
	}

	projID := ""
	if g != nil {
		projID = strings.TrimSpace(g.ProjectID)
	}
	if projID == "" {
		projID = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}
	if projID == "" {
		projID = strings.TrimSpace(os.Getenv("GCP_PROJECT"))
	}

	if req.URL != nil && strings.EqualFold(req.URL.Hostname(), "generativelanguage.googleapis.com") && projID != "" {
		authKey, err := g.resolveProjectAuthKeyViaADC(ctx, tok.AccessToken, projID)
		if err != nil {
			return fmt.Errorf("gemini-decision ADC project auth key lookup failed: %w", err)
		}
		req.Header.Set("x-goog-api-key", authKey)
		return nil
	}

	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if projID != "" {
		req.Header.Set("x-goog-user-project", projID)
	}
	return nil
}

func (g *GeminiDecisionEngine) resolveProjectAuthKeyViaADC(ctx context.Context, accessToken, projectID string) (string, error) {
	if g != nil {
		g.mu.Lock()
		if g.cachedAuthKey != "" && time.Now().Before(g.cachedAuthKeyExpiry) {
			cached := g.cachedAuthKey
			g.mu.Unlock()
			return cached, nil
		}
		g.mu.Unlock()
	}

	baseURL := "https://apikeys.googleapis.com"
	client := http.DefaultClient
	if g != nil {
		if strings.TrimSpace(g.APIKeysBaseURL) != "" {
			baseURL = strings.TrimRight(strings.TrimSpace(g.APIKeysBaseURL), "/")
		}
		if g.HTTPClient != nil {
			client = g.HTTPClient
		}
	}

	listURL := fmt.Sprintf("%s/v2/projects/%s/locations/global/keys?pageSize=300", baseURL, url.PathEscape(projectID))
	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return "", err
	}
	listReq.Header.Set("Authorization", "Bearer "+accessToken)
	listReq.Header.Set("x-goog-user-project", projectID)

	listResp, err := client.Do(listReq)
	if err != nil {
		return "", err
	}
	listBody, readErr := io.ReadAll(io.LimitReader(listResp.Body, 512*1024))
	listResp.Body.Close()
	if readErr != nil {
		return "", readErr
	}
	if listResp.StatusCode < 200 || listResp.StatusCode >= 300 {
		return "", fmt.Errorf("apikeys list HTTP %d: %s", listResp.StatusCode, strings.TrimSpace(string(listBody)))
	}

	var parsedList struct {
		Keys []struct {
			Name                string `json:"name"`
			ServiceAccountEmail string `json:"serviceAccountEmail"`
			Restrictions        struct {
				APITargets []struct {
					Service string `json:"service"`
				} `json:"apiTargets"`
			} `json:"restrictions"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(listBody, &parsedList); err != nil {
		return "", fmt.Errorf("invalid apikeys list JSON: %w", err)
	}

	selectedKeyName := ""
	fallbackKeyName := ""
	for _, k := range parsedList.Keys {
		targetsGenLang := false
		for _, t := range k.Restrictions.APITargets {
			if strings.EqualFold(strings.TrimSpace(t.Service), "generativelanguage.googleapis.com") {
				targetsGenLang = true
				break
			}
		}
		if !targetsGenLang || strings.TrimSpace(k.Name) == "" {
			continue
		}
		if strings.TrimSpace(k.ServiceAccountEmail) != "" {
			selectedKeyName = strings.TrimSpace(k.Name)
			break
		}
		if fallbackKeyName == "" {
			fallbackKeyName = strings.TrimSpace(k.Name)
		}
	}
	if selectedKeyName == "" {
		selectedKeyName = fallbackKeyName
	}
	if selectedKeyName == "" {
		return "", fmt.Errorf("no Google Cloud authorization key restricted to generativelanguage.googleapis.com found in project %s", projectID)
	}

	ksURL := fmt.Sprintf("%s/v2/%s/keyString", baseURL, strings.TrimLeft(selectedKeyName, "/"))
	ksReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ksURL, nil)
	if err != nil {
		return "", err
	}
	ksReq.Header.Set("Authorization", "Bearer "+accessToken)
	ksReq.Header.Set("x-goog-user-project", projectID)

	ksResp, err := client.Do(ksReq)
	if err != nil {
		return "", err
	}
	ksBody, ksReadErr := io.ReadAll(io.LimitReader(ksResp.Body, 64*1024))
	ksResp.Body.Close()
	if ksReadErr != nil {
		return "", ksReadErr
	}
	if ksResp.StatusCode < 200 || ksResp.StatusCode >= 300 {
		return "", fmt.Errorf("apikeys keyString HTTP %d: %s", ksResp.StatusCode, strings.TrimSpace(string(ksBody)))
	}

	var parsedKS struct {
		KeyString string `json:"keyString"`
	}
	if err := json.Unmarshal(ksBody, &parsedKS); err != nil || strings.TrimSpace(parsedKS.KeyString) == "" {
		return "", fmt.Errorf("apikeys keyString response missing keyString")
	}

	resolved := strings.TrimSpace(parsedKS.KeyString)
	if g != nil {
		g.mu.Lock()
		g.cachedAuthKey = resolved
		g.cachedAuthKeyExpiry = time.Now().Add(30 * time.Minute)
		g.mu.Unlock()
	}
	return resolved, nil
}

// Classify classifies plain text via ClassifyWithInput. Callers that need strict error propagation
// should call ClassifyWithInput directly.
func (g *GeminiDecisionEngine) Classify(ctx context.Context, text string, c config.ComplexityRouting, hasTools, hasMultimodal bool) (DecisionResult, error) {
	return g.ClassifyWithInput(ctx, []DecisionInputBlock{{Type: "text", Text: text}}, c, hasTools, hasMultimodal)
}

type geminiDecisionQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

type geminiDecisionRequest struct {
	Model     string                            `json:"model"`
	Input     interface{}                       `json:"input"`
	Questions map[string]geminiDecisionQuestion `json:"questions"`
}

type geminiDecisionAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type geminiDecisionUsage struct {
	TotalInputTokens  int `json:"total_input_tokens"`
	TotalOutputTokens int `json:"total_output_tokens"`
	TotalTokens       int `json:"total_tokens"`
}

type geminiDecisionResponse struct {
	Answers map[string]geminiDecisionAnswer `json:"answers"`
	Usage   geminiDecisionUsage             `json:"usage"`
	Error   *struct {
		Code    interface{} `json:"code"`
		Message string      `json:"message"`
		Status  string      `json:"status"`
	} `json:"error,omitempty"`
}

// ClassifyWithInput executes a single-forward-pass call against Google's Gemini Decisions API
// (model "gemini-decision-1"), evaluating complexity tier, primary task category, and local open-weights
// eligibility simultaneously with calibrated probabilities and 0 output tokens.
// Following the strict Never-Mock policy, upstream HTTP/safety/auth errors are returned directly without fake scores.
func (g *GeminiDecisionEngine) ClassifyWithInput(ctx context.Context, blocks []DecisionInputBlock, c config.ComplexityRouting, hasTools, hasMultimodal bool) (DecisionResult, error) {
	var textParts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			textParts = append(textParts, b.Text)
		} else if b.Type != "" && b.Type != "text" {
			hasMultimodal = true
		}
	}
	combinedText := strings.Join(textParts, "\n")
	features := ExtractPromptFeatures(combinedText, hasTools, hasMultimodal)

	if (c.ForceComplexMultimodal && hasMultimodal) || (c.ForceComplexTools && hasTools) {
		return DecisionResult{
			Tier:        "complex",
			TargetModel: ResolveModelByTier("complex", c),
			Engine:      "gemini-decision",
			Confidence:  1.0,
			Rationale:   "gemini-decision: forced complex tier due to multimodal/tools policy",
			Features:    features,
		}, nil
	}

	if len(blocks) == 0 {
		return DecisionResult{}, fmt.Errorf("gemini-decision requires non-empty input blocks")
	}

	endpoint := g.ResolveEndpoint(c)
	allowLoopback := g != nil && g.AllowLoopbackForTesting
	if err := validateDecisionEndpoint(endpoint, allowLoopback); err != nil {
		return DecisionResult{}, err
	}

	simpleDesc := strings.TrimSpace(c.SimpleCriteria)
	if simpleDesc == "" {
		simpleDesc = "Short greetings, single-fact lookups, unit conversions, trivial formatting, or quick triage."
	}
	mediumDesc := strings.TrimSpace(c.MediumCriteria)
	if mediumDesc == "" {
		mediumDesc = "Standard summarization, multi-paragraph drafting, moderate explanations, translation, or general Q&A."
	}
	complexDesc := strings.TrimSpace(c.ComplexCriteria)
	if complexDesc == "" {
		complexDesc = "Multi-step analytical reasoning, code generation, debugging race conditions or stack traces, math proofs, or system architecture."
	}

	compInstructions := "Which model complexity tier best matches the reasoning, coding, and domain difficulty required to answer this request?"
	if extra := strings.TrimSpace(c.AdditionalInstructions); extra != "" {
		compInstructions += " Additional routing guidance: " + extra
	}

	var inputPayload interface{} = blocks
	if len(blocks) == 1 && blocks[0].Type == "text" {
		inputPayload = blocks[0].Text
	}

	reqPayload := geminiDecisionRequest{
		Model: GeminiDecisionModel,
		Input: inputPayload,
		Questions: map[string]geminiDecisionQuestion{
			"complexity": {
				Type:         "choice",
				Instructions: compInstructions,
				Criteria: map[string]string{
					"simple":  simpleDesc,
					"medium":  mediumDesc,
					"complex": complexDesc,
				},
			},
			"task_kind": {
				Type:         "choice",
				Instructions: "Which primary task category best describes what the user is asking for?",
				Criteria: map[string]string{
					"code_generation": "Writing, refactoring, or synthesizing source code, scripts, SQL, or infrastructure configs.",
					"debugging":       "Diagnosing bugs, stack traces, race conditions, compiler errors, or failing tests.",
					"fast_query":      "Short greetings, unit conversions, single-fact lookups, or quick triage.",
					"general":         "Open-ended conversation, drafting, brainstorming, or standard Q&A.",
					"reasoning":       "Multi-step analytical reasoning, math proofs, system architecture, or tradeoff evaluation.",
					"summarization":   "Condensing, extracting key points, or summarizing documents and transcripts.",
					"translation":     "Translating text or idioms between natural languages.",
				},
			},
			"local_eligible": {
				Type:         "noul",
				Instructions: "Can a compact local open-weights model answer this request accurately without deep multi-step reasoning, frontier coding, or broad world-knowledge synthesis?",
				Criteria: map[string]string{
					"true_description":  "Self-contained formatting, short factual Q&A, unit conversion, simple classification, translation, or concise summarization suitable for a fast local model.",
					"false_description": "Complex multi-step reasoning, intricate debugging, system architecture, nuanced code generation, or tasks requiring frontier cloud intelligence.",
				},
			},
		},
	}

	rawBody, err := json.Marshal(reqPayload)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("failed marshalling gemini-decision request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		return DecisionResult{}, fmt.Errorf("failed building gemini-decision HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := g.AttachUpstreamAuth(ctx, httpReq, c.SecretRef); err != nil {
		return DecisionResult{}, err
	}

	httpClient := http.DefaultClient
	if g != nil && g.HTTPClient != nil {
		httpClient = g.HTTPClient
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("gemini-decision HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return DecisionResult{}, fmt.Errorf("failed reading gemini-decision response: %w", err)
	}

	var parsed geminiDecisionResponse
	_ = json.Unmarshal(respBytes, &parsed)

	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return DecisionResult{}, fmt.Errorf("gemini-decision upstream HTTP %d (code=%v): %s", resp.StatusCode, parsed.Error.Code, parsed.Error.Message)
		}
		return DecisionResult{}, fmt.Errorf("gemini-decision upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return DecisionResult{}, fmt.Errorf("gemini-decision error (code=%v): %s", parsed.Error.Code, parsed.Error.Message)
	}

	compAns, ok := parsed.Answers["complexity"]
	if !ok {
		return DecisionResult{}, fmt.Errorf("gemini-decision response missing 'complexity' answer")
	}

	tier := normalizeTierName(compAns.Choice)
	if tier == "" {
		return DecisionResult{}, fmt.Errorf("gemini-decision returned unrecognized complexity tier %q", compAns.Choice)
	}

	conf := compAns.Confidence
	if conf <= 0 && len(compAns.Probabilities) > 0 {
		if p, exists := compAns.Probabilities[tier]; exists {
			conf = p
		}
	}

	if taskAns, hasTask := parsed.Answers["task_kind"]; hasTask && strings.TrimSpace(taskAns.Choice) != "" {
		features.ExtractedTask = strings.TrimSpace(taskAns.Choice)
	}

	var localEligibleProb float64
	if localAns, hasLocal := parsed.Answers["local_eligible"]; hasLocal {
		localEligibleProb = localAns.Noul
	}

	targetModel := ResolveModelByTier(tier, c)
	if c.TaskMapping != nil && features.ExtractedTask != "" {
		if mapped, exists := c.TaskMapping[strings.ToLower(features.ExtractedTask)]; exists && strings.TrimSpace(mapped) != "" {
			targetModel = strings.TrimSpace(mapped)
		}
	}

	scores := make(map[string]float64, len(compAns.Probabilities))
	for k, v := range compAns.Probabilities {
		scores[k] = v
	}

	res := DecisionResult{
		Tier:              tier,
		TargetModel:       targetModel,
		Engine:            "gemini-decision",
		ExecutionMode:     "adc_remote_api",
		Confidence:        conf,
		LocalEligibleProb: localEligibleProb,
		InputTokens:       parsed.Usage.TotalInputTokens,
		OutputTokens:      parsed.Usage.TotalOutputTokens,
		Rationale:         fmt.Sprintf("gemini-decision-1: tier=%s (conf=%.2f) task=%s local_noul=%.2f in_tokens=%d", tier, conf, features.ExtractedTask, localEligibleProb, parsed.Usage.TotalInputTokens),
		Scores:            scores,
		Features:          features,
	}

	return applyMinConfidenceFallback(res, c), nil
}

// BuildDecisionInputBlocksFromGeminiBody extracts multimodal/text DecisionInputBlock items from a
// Gemini generateContent request body while safely converting unsupported gs:// URIs, YouTube URLs,
// and tool-call parts into text descriptors per official Gemini Decisions API constraints.
func BuildDecisionInputBlocksFromGeminiBody(body []byte) (blocks []DecisionInputBlock, hasTools bool, hasMultimodal bool) {
	type inlineMedia struct {
		MimeType      string `json:"mimeType"`
		MimeTypeSnake string `json:"mime_type"`
		Data          string `json:"data"`
	}
	type fileMedia struct {
		MimeType      string `json:"mimeType"`
		MimeTypeSnake string `json:"mime_type"`
		FileURI       string `json:"fileUri"`
		FileURISnake  string `json:"file_uri"`
	}
	type rawPart struct {
		Text             string          `json:"text"`
		InlineData       *inlineMedia    `json:"inlineData"`
		InlineDataSnake  *inlineMedia    `json:"inline_data"`
		FileData         *fileMedia      `json:"fileData"`
		FileDataSnake    *fileMedia      `json:"file_data"`
		FunctionCall     json.RawMessage `json:"functionCall"`
		FunctionResponse json.RawMessage `json:"functionResponse"`
	}
	var req struct {
		SystemInstruction *struct {
			Parts []rawPart `json:"parts"`
		} `json:"systemInstruction"`
		SystemInstructionSnake *struct {
			Parts []rawPart `json:"parts"`
		} `json:"system_instruction"`
		Contents []struct {
			Role  string    `json:"role"`
			Parts []rawPart `json:"parts"`
		} `json:"contents"`
		Tools []json.RawMessage `json:"tools"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" {
			return []DecisionInputBlock{{Type: "text", Text: trimmed}}, false, false
		}
		return nil, false, false
	}

	if len(req.Tools) > 0 {
		hasTools = true
	}

	mapMimeToDecisionType := func(mime string) string {
		m := strings.ToLower(strings.TrimSpace(mime))
		switch {
		case strings.HasPrefix(m, "image/"):
			return "image"
		case strings.HasPrefix(m, "audio/"):
			return "audio"
		case strings.HasPrefix(m, "video/"):
			return "video"
		case m == "application/pdf":
			return "document"
		default:
			return ""
		}
	}

	appendParts := func(parts []rawPart) {
		for _, p := range parts {
			if strings.TrimSpace(p.Text) != "" {
				blocks = append(blocks, DecisionInputBlock{Type: "text", Text: p.Text})
			}
			inl := p.InlineData
			if inl == nil {
				inl = p.InlineDataSnake
			}
			if inl != nil && inl.Data != "" {
				hasMultimodal = true
				mime := inl.MimeType
				if mime == "" {
					mime = inl.MimeTypeSnake
				}
				if bType := mapMimeToDecisionType(mime); bType != "" {
					blocks = append(blocks, DecisionInputBlock{
						Type:     bType,
						Data:     inl.Data,
						MimeType: mime,
					})
				} else {
					blocks = append(blocks, DecisionInputBlock{
						Type: "text",
						Text: fmt.Sprintf("[Attached inline media: %s]", mime),
					})
				}
			}
			fd := p.FileData
			if fd == nil {
				fd = p.FileDataSnake
			}
			if fd != nil {
				hasMultimodal = true
				uri := fd.FileURI
				if uri == "" {
					uri = fd.FileURISnake
				}
				mime := fd.MimeType
				if mime == "" {
					mime = fd.MimeTypeSnake
				}
				lowerURI := strings.ToLower(uri)
				bType := mapMimeToDecisionType(mime)
				// Gemini Decisions API rejects gs:// URIs and YouTube URLs; convert those to text descriptors.
				if bType != "" && (strings.HasPrefix(lowerURI, "https://") || strings.HasPrefix(lowerURI, "http://")) &&
					!strings.Contains(lowerURI, "youtube.com") && !strings.Contains(lowerURI, "youtu.be") {
					blocks = append(blocks, DecisionInputBlock{
						Type:     bType,
						URI:      uri,
						MimeType: mime,
					})
				} else if uri != "" {
					blocks = append(blocks, DecisionInputBlock{
						Type: "text",
						Text: fmt.Sprintf("[Referenced media file (%s): %s]", mime, uri),
					})
				}
			}
			if len(p.FunctionCall) > 0 || len(p.FunctionResponse) > 0 {
				hasTools = true
				blocks = append(blocks, DecisionInputBlock{
					Type: "text",
					Text: "[Conversation includes tool call / function execution context]",
				})
			}
		}
	}

	sysInst := req.SystemInstruction
	if sysInst == nil {
		sysInst = req.SystemInstructionSnake
	}
	if sysInst != nil {
		appendParts(sysInst.Parts)
	}
	for _, c := range req.Contents {
		appendParts(c.Parts)
	}

	return blocks, hasTools, hasMultimodal
}
