package decision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"geminirouter/pkg/config"
)

type staticTokenSource struct {
	token string
}

func (s *staticTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: s.token}, nil
}

func randomHexToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("failed generating random token: %v", err)
	}
	return hex.EncodeToString(buf)
}

func TestGeminiDecisionEngine_OfficialContractAndMultimodal(t *testing.T) {
	if !config.IsValidModelName("gemini-decision-1") {
		t.Fatalf("expected gemini-decision-1 to be recognized as a valid model name")
	}
	if NormalizeEngineName("gemini-decisions") != "gemini-decision" || NormalizeEngineName("gemini-decision-1") != "gemini-decision" {
		t.Fatalf("expected gemini-decisions / gemini-decision-1 aliases to normalize to gemini-decision")
	}

	adcToken := "adc-" + randomHexToken(t)
	var seenAuth, seenProject string
	var seenBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/decisions" {
			t.Errorf("expected path /v1beta/decisions, got %q", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		seenProject = r.Header.Get("x-goog-user-project")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seenBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"answers": {
				"complexity": {
					"type": "choice",
					"choice": "simple",
					"probabilities": {
						"complex": 0.02,
						"medium": 0.04,
						"simple": 0.94
					},
					"confidence": 0.94
				},
				"task_kind": {
					"type": "choice",
					"choice": "fast_query",
					"probabilities": {
						"code_generation": 0.01,
						"debugging": 0.01,
						"fast_query": 0.92,
						"general": 0.03,
						"reasoning": 0.01,
						"summarization": 0.01,
						"translation": 0.01
					},
					"confidence": 0.92
				},
				"local_eligible": {
					"type": "noul",
					"noul": 0.96,
					"confidence": 0.96
				}
			},
			"usage": {
				"total_input_tokens": 148,
				"input_tokens_by_modality": [{"modality": "text", "tokens": 148}],
				"total_output_tokens": 0,
				"total_tokens": 148
			}
		}`))
	}))
	defer srv.Close()

	eng := NewGeminiDecisionEngine(&staticTokenSource{token: adcToken}, "davenport-boutique")
	eng.AllowLoopbackForTesting = true

	cfg := config.ComplexityRouting{
		Enabled:                true,
		Engine:                 "gemini-decision",
		GeminiDecisionEndpoint: srv.URL + "/v1beta/decisions",
		SimpleModel:            "gemma-4-4b",
		MediumModel:            "gemini-3.8-flash",
		ComplexModel:           "gemini-4-argon",
		SimpleCriteria:         "Short greetings, unit conversions, and local triage.",
	}

	res, err := eng.ClassifyWithInput(context.Background(), []DecisionInputBlock{
		{Type: "image", Data: "aGVsbG8=", MimeType: "image/png"},
		{Type: "text", Text: "Convert 12 km to miles."},
	}, cfg, false, true)
	if err != nil {
		t.Fatalf("unexpected error from GeminiDecisionEngine: %v", err)
	}

	if seenAuth != "Bearer "+adcToken {
		t.Errorf("expected Authorization Bearer ADC token, got %q", seenAuth)
	}
	if seenProject != "davenport-boutique" {
		t.Errorf("expected x-goog-user-project='davenport-boutique', got %q", seenProject)
	}
	if gotModel, _ := seenBody["model"].(string); gotModel != "gemini-decision-1" {
		t.Errorf("expected model='gemini-decision-1', got %q", gotModel)
	}

	questions, ok := seenBody["questions"].(map[string]interface{})
	if !ok || questions["complexity"] == nil || questions["task_kind"] == nil || questions["local_eligible"] == nil {
		t.Fatalf("expected complexity, task_kind, and local_eligible questions in single forward pass, got: %#v", seenBody["questions"])
	}

	if res.Tier != "simple" {
		t.Errorf("expected Tier='simple', got %q", res.Tier)
	}
	if res.TargetModel != "gemma-4-4b" {
		t.Errorf("expected TargetModel='gemma-4-4b', got %q", res.TargetModel)
	}
	if res.Confidence != 0.94 {
		t.Errorf("expected Confidence=0.94, got %v", res.Confidence)
	}
	if res.LocalEligibleProb != 0.96 {
		t.Errorf("expected LocalEligibleProb=0.96, got %v", res.LocalEligibleProb)
	}
	if res.InputTokens != 148 {
		t.Errorf("expected InputTokens=148, got %d", res.InputTokens)
	}
	if res.Features.ExtractedTask != "fast_query" {
		t.Errorf("expected ExtractedTask='fast_query', got %q", res.Features.ExtractedTask)
	}
	if res.Scores["simple"] != 0.94 {
		t.Errorf("expected Scores['simple']=0.94, got %v", res.Scores["simple"])
	}
}

func TestGeminiDecisionEngine_NeverMockErrorSurfacing(t *testing.T) {
	adcToken := "adc-" + randomHexToken(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"safety","message":"Request blocked due to safety violations"}}`))
	}))
	defer srv.Close()

	eng := NewGeminiDecisionEngine(&staticTokenSource{token: adcToken}, "davenport-boutique")
	eng.AllowLoopbackForTesting = true

	cfg := config.ComplexityRouting{
		Enabled:                true,
		Engine:                 "gemini-decision",
		GeminiDecisionEndpoint: srv.URL + "/v1beta/decisions",
	}

	_, err := eng.ClassifyWithInput(context.Background(), []DecisionInputBlock{{Type: "text", Text: "blocked"}}, cfg, false, false)
	if err == nil || !strings.Contains(err.Error(), "safety") {
		t.Fatalf("expected real upstream safety error to surface without mock fallback, got: %v", err)
	}
}

func TestGeminiDecisionEngine_ADCResolvesProjectAuthKeyForGenerativeLanguage(t *testing.T) {
	adcToken := "adc-" + randomHexToken(t)
	projectAuthKey := "authkey-" + randomHexToken(t)
	listCalls := 0
	keyStringCalls := 0

	apiKeysSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+adcToken {
			t.Errorf("expected ADC Bearer token on apikeys lookup, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-goog-user-project") != "davenport-boutique" {
			t.Errorf("expected x-goog-user-project on apikeys lookup, got %q", r.Header.Get("x-goog-user-project"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/projects/davenport-boutique/locations/global/keys":
			listCalls++
			_, _ = w.Write([]byte(`{
				"keys": [
					{
						"name": "projects/834476222725/locations/global/keys/firebase-key",
						"restrictions": {"apiTargets": [{"service": "identitytoolkit.googleapis.com"}]}
					},
					{
						"name": "projects/834476222725/locations/global/keys/genlang-key",
						"serviceAccountEmail": "smart-router-api-sa@davenport-boutique.iam.gserviceaccount.com",
						"restrictions": {"apiTargets": [{"service": "generativelanguage.googleapis.com"}]}
					}
				]
			}`))
		case "/v2/projects/834476222725/locations/global/keys/genlang-key/keyString":
			keyStringCalls++
			_, _ = w.Write([]byte(`{"keyString":"` + projectAuthKey + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiKeysSrv.Close()

	eng := NewGeminiDecisionEngine(&staticTokenSource{token: adcToken}, "davenport-boutique")
	eng.APIKeysBaseURL = apiKeysSrv.URL

	req1, _ := http.NewRequest(http.MethodPost, DefaultGeminiDecisionEndpoint, nil)
	if err := eng.AttachUpstreamAuth(context.Background(), req1, ""); err != nil {
		t.Fatalf("AttachUpstreamAuth failed: %v", err)
	}
	if got := req1.Header.Get("x-goog-api-key"); got != projectAuthKey {
		t.Fatalf("expected ADC-resolved project auth key %q on generativelanguage.googleapis.com, got %q", projectAuthKey, got)
	}
	if gotAuth := req1.Header.Get("Authorization"); gotAuth != "" {
		t.Fatalf("expected service-account Authorization header omitted when x-goog-api-key auth key is attached, got %q", gotAuth)
	}

	// Second call must hit in-memory cache without re-querying apikeys.googleapis.com
	req2, _ := http.NewRequest(http.MethodPost, DefaultGeminiDecisionEndpoint, nil)
	if err := eng.AttachUpstreamAuth(context.Background(), req2, ""); err != nil {
		t.Fatalf("second AttachUpstreamAuth failed: %v", err)
	}
	if listCalls != 1 || keyStringCalls != 1 {
		t.Fatalf("expected 1 cached apikeys lookup, got listCalls=%d keyStringCalls=%d", listCalls, keyStringCalls)
	}
}
