package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"geminirouter/pkg/config"
)

// isOpenAIChatCompletionsPath returns true if the request path targets an OpenAI-compatible chat completions endpoint.
func isOpenAIChatCompletionsPath(path string) bool {
	clean := strings.TrimSuffix(strings.TrimSpace(path), "/")
	switch clean {
	case "/v1/chat/completions",
		"/v1beta/openai/chat/completions",
		"/v1/openai/chat/completions",
		"/chat/completions":
		return true
	default:
		return false
	}
}

// parseOpenAIModelsPath checks if a GET request targets the OpenAI/Gemini models list or single model retrieve endpoint.
func parseOpenAIModelsPath(method, path string) (isList bool, modelID string, matched bool) {
	if method != http.MethodGet {
		return false, "", false
	}
	clean := strings.TrimSuffix(strings.TrimSpace(path), "/")
	switch clean {
	case "/v1/models", "/v1beta/models", "/v1beta/openai/models", "/v1/openai/models":
		return true, "", true
	}

	prefixes := []string{
		"/v1beta/openai/models/",
		"/v1/openai/models/",
		"/v1/models/",
		"/v1beta/models/",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(clean, prefix) {
			rest := strings.TrimPrefix(clean, prefix)
			if rest != "" && !strings.Contains(rest, ":") && !strings.Contains(rest, "/") {
				return false, rest, true
			}
		}
	}
	return false, "", false
}

type openAIModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Name    string `json:"name,omitempty"`
}

type geminiModelSummary struct {
	Name                       string   `json:"name"`
	BaseModelID                string   `json:"baseModelId,omitempty"`
	DisplayName                string   `json:"displayName,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
}

// handleOpenAIModels serves GET /v1/models, GET /v1beta/openai/models, and GET /v1/models/{model}.
func (rp *RouterProxy) handleOpenAIModels(w http.ResponseWriter, r *http.Request, isList bool, modelID string) {
	createdTs := int64(1735689600)
	if !isList {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(openAIModelObject{
			ID:      modelID,
			Object:  "model",
			Created: createdTs,
			OwnedBy: "google",
			Name:    "models/" + modelID,
		})
		return
	}

	seen := make(map[string]bool)
	var data []openAIModelObject
	var geminiModels []geminiModelSummary

	addModel := func(id, displayName, ownedBy string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		data = append(data, openAIModelObject{
			ID:      id,
			Object:  "model",
			Created: createdTs,
			OwnedBy: ownedBy,
		})
		geminiModels = append(geminiModels, geminiModelSummary{
			Name:                       "models/" + id,
			BaseModelID:                id,
			DisplayName:                displayName,
			SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
		})
	}

	// Always include the Smart Router virtual dynamic model first
	addModel("gemini-dynamic", "Gemini Smart Router Dynamic Model", "smartrouter")

	if rp.Store != nil {
		if allModels, err := rp.Store.GetAllModels(r.Context()); err == nil {
			for _, m := range allModels {
				if !m.Active {
					continue
				}
				baseID := config.StripLocationSuffix(m.ID)
				addModel(baseID, m.DisplayName, "google")
			}
		}
	}

	// Ensure standard baseline Gemini 4 / Decisions / 3.x / 2.5 / Gemma 4 models are listed
	addModel("gemini-4-argon", "Gemini 4 Argon", "google")
	addModel("gemini-decision-1", "Gemini Decision 1", "google")
	addModel("gemini-3.8-flash", "Gemini 3.8 Flash", "google")
	addModel("gemini-3.5-flash-lite", "Gemini 3.5 Flash Lite", "google")
	addModel("gemini-3.1-pro", "Gemini 3.1 Pro", "google")
	addModel("gemini-2.5-flash", "Gemini 2.5 Flash", "google")
	addModel("gemma-4-4b", "Gemma 4 4B", "google")
	addModel("gemma-4-26b", "Gemma 4 26B", "google")
	addModel("gemma-4-31b", "Gemma 4 31B", "google")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		Object string               `json:"object"`
		Data   []openAIModelObject  `json:"data"`
		Models []geminiModelSummary `json:"models"`
	}{
		Object: "list",
		Data:   data,
		Models: geminiModels,
	})
}

type openAIChatRequest struct {
	Model               string                `json:"model"`
	Messages            []openAIMessage       `json:"messages"`
	Temperature         *float64              `json:"temperature,omitempty"`
	TopP                *float64              `json:"top_p,omitempty"`
	MaxTokens           *int                  `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                  `json:"max_completion_tokens,omitempty"`
	Stop                interface{}           `json:"stop,omitempty"`
	Stream              bool                  `json:"stream,omitempty"`
	Tools               []openAITool          `json:"tools,omitempty"`
	ResponseFormat      *openAIResponseFormat `json:"response_format,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIFunctionSpec `json:"function"`
}

type openAIFunctionSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string                 `json:"name,omitempty"`
		Schema map[string]interface{} `json:"schema,omitempty"`
	} `json:"json_schema,omitempty"`
}

// translateOpenAIToGeminiRequest converts an OpenAI /v1/chat/completions payload into a Vertex AI Gemini generateContent payload.
func translateOpenAIToGeminiRequest(body []byte, complexityEnabled bool) (requestedModel string, isStream bool, geminiBody []byte, err error) {
	var oaiReq openAIChatRequest
	if err := json.Unmarshal(body, &oaiReq); err != nil {
		return "", false, nil, fmt.Errorf("invalid OpenAI chat completion request JSON: %w", err)
	}

	requestedModel = strings.TrimSpace(oaiReq.Model)
	requestedModel = strings.TrimPrefix(requestedModel, "models/")
	requestedModel = strings.TrimPrefix(requestedModel, "google/")
	requestedModel = strings.TrimPrefix(requestedModel, "vertex_ai/")
	if requestedModel == "" {
		if complexityEnabled {
			requestedModel = "gemini-dynamic"
		} else {
			requestedModel = "gemini-3.8-flash"
		}
	}

	var systemParts []map[string]interface{}
	var contents []map[string]interface{}

	for _, msg := range oaiReq.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		parts := parseOpenAIMessageParts(msg)

		switch role {
		case "system", "developer":
			for _, p := range parts {
				if txt, ok := p["text"].(string); ok && txt != "" {
					systemParts = append(systemParts, map[string]interface{}{"text": txt})
				}
			}
		case "assistant", "model":
			if len(parts) > 0 {
				contents = append(contents, map[string]interface{}{
					"role":  "model",
					"parts": parts,
				})
			}
		case "tool":
			textVal := extractRawText(msg.Content)
			fnName := msg.Name
			if fnName == "" {
				fnName = msg.ToolCallID
			}
			if fnName == "" {
				fnName = "tool_result"
			}
			contents = append(contents, map[string]interface{}{
				"role": "user",
				"parts": []map[string]interface{}{
					{
						"functionResponse": map[string]interface{}{
							"name": fnName,
							"response": map[string]interface{}{
								"content": textVal,
							},
						},
					},
				},
			})
		default: // "user"
			if len(parts) > 0 {
				contents = append(contents, map[string]interface{}{
					"role":  "user",
					"parts": parts,
				})
			}
		}
	}

	if len(contents) == 0 && len(systemParts) > 0 {
		contents = append(contents, map[string]interface{}{
			"role":  "user",
			"parts": systemParts,
		})
		systemParts = nil
	}
	if len(contents) == 0 {
		contents = append(contents, map[string]interface{}{
			"role":  "user",
			"parts": []map[string]interface{}{{"text": ""}},
		})
	}

	geminiMap := map[string]interface{}{
		"contents": contents,
	}
	if len(systemParts) > 0 {
		geminiMap["systemInstruction"] = map[string]interface{}{
			"parts": systemParts,
		}
	}

	genCfg := make(map[string]interface{})
	if oaiReq.Temperature != nil {
		genCfg["temperature"] = *oaiReq.Temperature
	}
	if oaiReq.TopP != nil {
		genCfg["topP"] = *oaiReq.TopP
	}
	if oaiReq.MaxCompletionTokens != nil && *oaiReq.MaxCompletionTokens > 0 {
		genCfg["maxOutputTokens"] = *oaiReq.MaxCompletionTokens
	} else if oaiReq.MaxTokens != nil && *oaiReq.MaxTokens > 0 {
		genCfg["maxOutputTokens"] = *oaiReq.MaxTokens
	}
	if oaiReq.Stop != nil {
		switch v := oaiReq.Stop.(type) {
		case string:
			if v != "" {
				genCfg["stopSequences"] = []string{v}
			}
		case []interface{}:
			var stops []string
			for _, item := range v {
				if s, ok := item.(string); ok && s != "" {
					stops = append(stops, s)
				}
			}
			if len(stops) > 0 {
				genCfg["stopSequences"] = stops
			}
		}
	}
	if oaiReq.ResponseFormat != nil {
		switch oaiReq.ResponseFormat.Type {
		case "json_object":
			genCfg["responseMimeType"] = "application/json"
		case "json_schema":
			genCfg["responseMimeType"] = "application/json"
			if oaiReq.ResponseFormat.JSONSchema != nil && oaiReq.ResponseFormat.JSONSchema.Schema != nil {
				genCfg["responseSchema"] = oaiReq.ResponseFormat.JSONSchema.Schema
			}
		}
	}
	if len(genCfg) > 0 {
		geminiMap["generationConfig"] = genCfg
	}

	if len(oaiReq.Tools) > 0 {
		var fnDecls []map[string]interface{}
		for _, t := range oaiReq.Tools {
			if t.Function.Name == "" {
				continue
			}
			decl := map[string]interface{}{
				"name": t.Function.Name,
			}
			if t.Function.Description != "" {
				decl["description"] = t.Function.Description
			}
			if len(t.Function.Parameters) > 0 {
				decl["parameters"] = t.Function.Parameters
			}
			fnDecls = append(fnDecls, decl)
		}
		if len(fnDecls) > 0 {
			geminiMap["tools"] = []map[string]interface{}{
				{"functionDeclarations": fnDecls},
			}
		}
	}

	outBytes, err := json.Marshal(geminiMap)
	if err != nil {
		return "", false, nil, err
	}
	return requestedModel, oaiReq.Stream, outBytes, nil
}

func extractRawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func parseOpenAIMessageParts(msg openAIMessage) []map[string]interface{} {
	var parts []map[string]interface{}
	raw := bytes.TrimSpace(msg.Content)
	if len(raw) > 0 && string(raw) != "null" {
		if raw[0] == '"' {
			var text string
			if err := json.Unmarshal(raw, &text); err == nil {
				parts = append(parts, map[string]interface{}{"text": text})
			}
		} else if raw[0] == '[' {
			var contentParts []openAIContentPart
			if err := json.Unmarshal(raw, &contentParts); err == nil {
				for _, cp := range contentParts {
					switch cp.Type {
					case "text":
						parts = append(parts, map[string]interface{}{"text": cp.Text})
					case "image_url":
						if cp.ImageURL != nil && cp.ImageURL.URL != "" {
							imgURL := cp.ImageURL.URL
							if strings.HasPrefix(imgURL, "data:") {
								// Format: data:image/png;base64,iVBOR...
								commaIdx := strings.Index(imgURL, ",")
								if commaIdx != -1 {
									header := imgURL[len("data:"):commaIdx]
									b64Data := imgURL[commaIdx+1:]
									mimeType := strings.Split(header, ";")[0]
									if mimeType == "" {
										mimeType = "image/jpeg"
									}
									parts = append(parts, map[string]interface{}{
										"inlineData": map[string]interface{}{
											"mimeType": mimeType,
											"data":     b64Data,
										},
									})
								}
							} else {
								parts = append(parts, map[string]interface{}{
									"fileData": map[string]interface{}{
										"mimeType": "image/jpeg",
										"fileUri":  imgURL,
									},
								})
							}
						}
					}
				}
			}
		}
	}

	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		var argsMap map[string]interface{}
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &argsMap)
		}
		if argsMap == nil {
			argsMap = make(map[string]interface{})
		}
		parts = append(parts, map[string]interface{}{
			"functionCall": map[string]interface{}{
				"name": tc.Function.Name,
				"args": argsMap,
			},
		})
	}
	return parts
}

type geminiGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text,omitempty"`
				FunctionCall *struct {
					Name string                 `json:"name"`
					Args map[string]interface{} `json:"args"`
				} `json:"functionCall,omitempty"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type openAIChatCompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
	Usage   openAIUsage        `json:"usage"`
}

type openAIChatChoice struct {
	Index        int                  `json:"index"`
	Message      openAIResponseMsg    `json:"message"`
	FinishReason string               `json:"finish_reason"`
}

type openAIResponseMsg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// translateGeminiToOpenAIResponse converts a Vertex AI Gemini generateContent response into an OpenAI chat.completion JSON payload.
func translateGeminiToOpenAIResponse(geminiBody []byte, routedModel string) ([]byte, error) {
	var gemResp geminiGenerateResponse
	if err := json.Unmarshal(geminiBody, &gemResp); err != nil {
		return nil, err
	}

	var textBuilder strings.Builder
	var toolCalls []openAIToolCall
	finishReason := "stop"

	if len(gemResp.Candidates) > 0 {
		cand := gemResp.Candidates[0]
		for i, part := range cand.Content.Parts {
			if part.Text != "" {
				textBuilder.WriteString(part.Text)
			}
			if part.FunctionCall != nil && part.FunctionCall.Name != "" {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				toolCalls = append(toolCalls, openAIToolCall{
					ID:   fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i),
					Type: "function",
					Function: openAIFunctionCall{
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}
		finishReason = mapGeminiFinishReason(cand.FinishReason, len(toolCalls) > 0)
	}

	promptTokens := gemResp.UsageMetadata.PromptTokenCount
	completionTokens := gemResp.UsageMetadata.CandidatesTokenCount
	totalTokens := gemResp.UsageMetadata.TotalTokenCount
	if totalTokens == 0 && (promptTokens > 0 || completionTokens > 0) {
		totalTokens = promptTokens + completionTokens
	}

	if routedModel == "" {
		routedModel = "gemini-3.8-flash"
	}

	out := openAIChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   routedModel,
		Choices: []openAIChatChoice{
			{
				Index: 0,
				Message: openAIResponseMsg{
					Role:      "assistant",
					Content:   textBuilder.String(),
					ToolCalls: toolCalls,
				},
				FinishReason: finishReason,
			},
		},
		Usage: openAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		},
	}
	return json.Marshal(out)
}

func mapGeminiFinishReason(reason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "stop"
	}
}

type openAIChatChunk struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []openAIChatChunkChoice `json:"choices"`
	Usage   *openAIUsage            `json:"usage,omitempty"`
}

type openAIChatChunkChoice struct {
	Index        int                 `json:"index"`
	Delta        openAIChatChunkDelta `json:"delta"`
	FinishReason *string             `json:"finish_reason"`
}

type openAIChatChunkDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIInterceptorWriter captures the upstream Gemini response (or SSE stream) and translates it to OpenAI format.
type openAIInterceptorWriter struct {
	target      http.ResponseWriter
	isStream    bool
	routedModel string
	statusCode  int
	header      http.Header
	buf         bytes.Buffer
	streamID    string
	created     int64
	sentRole    bool
}

func newOpenAIInterceptorWriter(target http.ResponseWriter, isStream bool, routedModel string) *openAIInterceptorWriter {
	return &openAIInterceptorWriter{
		target:      target,
		isStream:    isStream,
		routedModel: routedModel,
		statusCode:  http.StatusOK,
		header:      make(http.Header),
		streamID:    fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		created:     time.Now().Unix(),
	}
}

func (w *openAIInterceptorWriter) Header() http.Header {
	return w.header
}

func (w *openAIInterceptorWriter) WriteHeader(code int) {
	w.statusCode = code
}

func (w *openAIInterceptorWriter) Write(b []byte) (int, error) {
	return w.buf.Write(b)
}

func (w *openAIInterceptorWriter) Flush() {
	if flusher, ok := w.target.(http.Flusher); ok {
		flusher.Flush()
	}
}

// maybeGunzip decompresses gzip-encoded payloads (detected via Content-Encoding header or 0x1f 0x8b magic bytes).
func maybeGunzip(data []byte, contentEncoding string) []byte {
	isGzip := strings.Contains(strings.ToLower(contentEncoding), "gzip") ||
		(len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b)
	if !isGzip {
		return data
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return data
	}
	defer zr.Close()
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		return data
	}
	return decompressed
}

// Finalize writes the translated OpenAI response or SSE stream to the underlying ResponseWriter.
func (w *openAIInterceptorWriter) Finalize(finalRoutedModel string) {
	if finalRoutedModel != "" {
		w.routedModel = finalRoutedModel
	}
	if w.routedModel == "" {
		w.routedModel = "gemini-3.8-flash"
	}

	contentEncoding := w.header.Get("Content-Encoding")

	// Copy non-content-length / non-encoding headers from upstream/router to target
	for k, vals := range w.header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		for _, v := range vals {
			w.target.Header().Set(k, v)
		}
	}
	w.target.Header().Del("Content-Encoding")

	rawBytes := maybeGunzip(w.buf.Bytes(), contentEncoding)
	if w.statusCode >= 400 {
		w.target.Header().Set("Content-Type", "application/json")
		w.target.WriteHeader(w.statusCode)
		_, _ = w.target.Write(rawBytes)
		return
	}

	if w.isStream {
		w.target.Header().Set("Content-Type", "text/event-stream")
		w.target.Header().Set("Cache-Control", "no-cache")
		w.target.Header().Set("Connection", "keep-alive")
		w.target.WriteHeader(w.statusCode)

		ssePayload := translateGeminiStreamToOpenAISSE(rawBytes, w.routedModel, w.streamID, w.created)
		_, _ = w.target.Write(ssePayload)
		w.Flush()
		return
	}

	translated, err := translateGeminiToOpenAIResponse(rawBytes, w.routedModel)
	if err != nil {
		w.target.Header().Set("Content-Type", "application/json")
		w.target.WriteHeader(w.statusCode)
		_, _ = w.target.Write(rawBytes)
		return
	}
	w.target.Header().Set("Content-Type", "application/json")
	w.target.WriteHeader(w.statusCode)
	_, _ = w.target.Write(translated)
}

func translateGeminiStreamToOpenAISSE(raw []byte, routedModel, streamID string, created int64) []byte {
	var out bytes.Buffer
	lines := strings.Split(string(raw), "\n")
	sentRole := false
	sentAnyChunk := false

	emitChunk := func(gemChunk geminiGenerateResponse) {
		if len(gemChunk.Candidates) == 0 {
			return
		}
		cand := gemChunk.Candidates[0]
		var textBuilder strings.Builder
		var toolCalls []openAIToolCall
		for i, part := range cand.Content.Parts {
			if part.Text != "" {
				textBuilder.WriteString(part.Text)
			}
			if part.FunctionCall != nil && part.FunctionCall.Name != "" {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				toolCalls = append(toolCalls, openAIToolCall{
					ID:   fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i),
					Type: "function",
					Function: openAIFunctionCall{
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}

		delta := openAIChatChunkDelta{
			Content:   textBuilder.String(),
			ToolCalls: toolCalls,
		}
		if !sentRole {
			delta.Role = "assistant"
			sentRole = true
		}

		var finishPtr *string
		if cand.FinishReason != "" {
			fr := mapGeminiFinishReason(cand.FinishReason, len(toolCalls) > 0)
			finishPtr = &fr
		}

		chunk := openAIChatChunk{
			ID:      streamID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   routedModel,
			Choices: []openAIChatChunkChoice{
				{
					Index:        0,
					Delta:        delta,
					FinishReason: finishPtr,
				},
			},
		}
		if gemChunk.UsageMetadata.TotalTokenCount > 0 {
			chunk.Usage = &openAIUsage{
				PromptTokens:     gemChunk.UsageMetadata.PromptTokenCount,
				CompletionTokens: gemChunk.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      gemChunk.UsageMetadata.TotalTokenCount,
			}
		}
		b, err := json.Marshal(chunk)
		if err == nil {
			out.WriteString("data: ")
			out.Write(b)
			out.WriteString("\n\n")
			sentAnyChunk = true
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			dataStr := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if dataStr == "" || dataStr == "[DONE]" {
				continue
			}
			var gemChunk geminiGenerateResponse
			if err := json.Unmarshal([]byte(dataStr), &gemChunk); err == nil {
				emitChunk(gemChunk)
			}
		}
	}

	if !sentAnyChunk && len(bytes.TrimSpace(raw)) > 0 {
		var singleGem geminiGenerateResponse
		if err := json.Unmarshal(raw, &singleGem); err == nil {
			emitChunk(singleGem)
		}
	}

	out.WriteString("data: [DONE]\n\n")
	return out.Bytes()
}
