package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"geminirouter/pkg/config"
)

type EngineStats struct {
	NodeID         string `json:"node_id"`
	Status         string `json:"status"` // "idle", "busy", "offline"
	ProcessedCount int64  `json:"processed_count"`
	ErrorCount     int64  `json:"error_count"`
	MaxModelSizeGB int    `json:"max_model_size_gb"`
	MaxConcurrent  int    `json:"max_concurrent"`
}

type CoreRunnerEngine struct {
	mu                sync.RWMutex
	NodeID            string
	RouterURL         string
	ClusterID         string
	LlamaServerURL    string
	SupportedModels   []string
	MemoryAllocatedGB int
	ComputeGPUCores   int
	MaxModelSizeGB    int
	MaxConcurrent     int
	inFlight          int
	Active            bool
	Stats             EngineStats
	httpClient        *http.Client
	cancelChan        chan struct{}
}

func NewCoreRunnerEngine(routerURL string, supportedModels []string) *CoreRunnerEngine {
	return &CoreRunnerEngine{
		NodeID:          fmt.Sprintf("runner-%d", time.Now().UnixNano()),
		RouterURL:       routerURL,
		ClusterID:       "local-cluster-1",       // Default fallback
		LlamaServerURL:  "http://localhost:8080", // Default llama-server endpoint
		SupportedModels: supportedModels,
		httpClient: &http.Client{
			Timeout: 120 * time.Second, // local inference can take longer
		},
		cancelChan: make(chan struct{}),
		Stats: EngineStats{
			Status: "idle",
		},
	}
}

func (e *CoreRunnerEngine) StartLoop() {
	e.mu.Lock()
	e.Active = true
	e.Stats.Status = "idle"
	e.mu.Unlock()

	// Register dynamic capabilities on startup
	e.registerNode()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Println("[Runner Engine] Core polling and heartbeat loop active.")

	for {
		select {
		case <-e.cancelChan:
			return
		case <-ticker.C:
			e.sendHeartbeat()
			e.mu.RLock()
			maxConc := e.MaxConcurrent
			inFlight := e.inFlight
			e.mu.RUnlock()
			if maxConc > 1 {
				slots := maxConc - inFlight
				for i := 0; i < slots; i++ {
					go e.pollAndProcess()
				}
			} else {
				e.pollAndProcess()
			}
		}
	}
}

func (e *CoreRunnerEngine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Active {
		close(e.cancelChan)
		e.Active = false
		e.Stats.Status = "offline"
	}
}

func (e *CoreRunnerEngine) GetStats() EngineStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	stats := e.Stats
	stats.MaxModelSizeGB = e.MaxModelSizeGB
	stats.MaxConcurrent = e.MaxConcurrent
	return stats
}

func (e *CoreRunnerEngine) fetchOIDCToken(ctx context.Context) (string, error) {
	isLocalDev := os.Getenv("LOCAL_DEV") == "true"
	if isLocalDev || !strings.HasPrefix(e.RouterURL, "https://") {
		return "", nil
	}

	client := &http.Client{Timeout: 1 * time.Second}
	tokenURL := fmt.Sprintf("http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s", url.QueryEscape(e.RouterURL))
	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func (e *CoreRunnerEngine) attachAuthHeaders(ctx context.Context, req *http.Request) {
	if secret := os.Getenv("BACKEND_SHARED_SECRET"); secret != "" {
		req.Header.Set("X-Shared-Secret", secret)
	}
	if token, tokenErr := e.fetchOIDCToken(ctx); tokenErr == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (e *CoreRunnerEngine) sendHeartbeat() {
	e.mu.RLock()
	payload := map[string]string{
		"node_id":    e.NodeID,
		"cluster_id": e.ClusterID,
	}
	e.mu.RUnlock()

	bodyBytes, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", e.RouterURL+"/api/v1/cluster/runners/heartbeat", bytes.NewReader(bodyBytes))
	if err != nil {
		log.Printf("[Runner Warning] Failed to create heartbeat request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	e.attachAuthHeaders(ctx, req)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		log.Printf("[Runner Warning] Heartbeat POST failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyErr, _ := io.ReadAll(resp.Body)
		log.Printf("[Runner Warning] Heartbeat returned status %s: %s", resp.Status, string(bodyErr))
		return
	}

	var hbResp struct {
		MaxModelSizeGB   int      `json:"max_model_size_gb"`
		MaxConcurrent    int      `json:"max_concurrent"`
		SupportedModels  []string `json:"supported_models"`
		AssignedClusters []string `json:"assigned_clusters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err == nil {
		e.mu.Lock()
		if hbResp.MaxModelSizeGB > 0 {
			e.MaxModelSizeGB = hbResp.MaxModelSizeGB
			e.Stats.MaxModelSizeGB = hbResp.MaxModelSizeGB
		}
		if hbResp.MaxConcurrent > 0 {
			e.MaxConcurrent = hbResp.MaxConcurrent
			e.Stats.MaxConcurrent = hbResp.MaxConcurrent
		}
		if len(hbResp.SupportedModels) > 0 {
			e.SupportedModels = hbResp.SupportedModels
		}
		if len(hbResp.AssignedClusters) > 0 && hbResp.AssignedClusters[0] != "" {
			e.ClusterID = hbResp.AssignedClusters[0]
		}
		e.mu.Unlock()
	}
}

func (e *CoreRunnerEngine) registerNode() {
	e.mu.RLock()
	node := config.Node{
		ID:                e.NodeID,
		Name:              "Dynamic Connected Runner",
		Status:            "online",
		MemoryAllocatedGB: e.MemoryAllocatedGB,
		ComputeGPUCores:   e.ComputeGPUCores,
		SupportedModels:   e.SupportedModels,
		MaxModelSizeGB:    e.MaxModelSizeGB,
		MaxConcurrent:     e.MaxConcurrent,
	}
	e.mu.RUnlock()

	bodyBytes, _ := json.Marshal(node)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", e.RouterURL+"/api/v1/cluster/runners/register", bytes.NewReader(bodyBytes))
	if err != nil {
		log.Printf("[Runner Warning] Failed to create registration request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	e.attachAuthHeaders(ctx, req)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		log.Printf("[Runner Warning] Registration POST failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyErr, _ := io.ReadAll(resp.Body)
		log.Printf("[Runner Warning] Registration returned status %s: %s", resp.Status, string(bodyErr))
	} else {
		log.Println("[Runner Engine] Successfully registered Dynamic Node credentials on startup.")
	}
}

type pollJob struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Payload []byte `json:"payload"`
}

func (e *CoreRunnerEngine) pollAndProcess() {
	e.mu.RLock()
	if e.MaxConcurrent > 0 && e.inFlight >= e.MaxConcurrent {
		e.mu.RUnlock()
		return
	}
	payload := map[string]interface{}{
		"node_id":          e.NodeID,
		"cluster_id":       e.ClusterID,
		"supported_models": e.SupportedModels,
	}
	e.mu.RUnlock()

	bodyBytes, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", e.RouterURL+"/api/v1/cluster/queue/poll", bytes.NewReader(bodyBytes))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	e.attachAuthHeaders(ctx, req)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyErr, _ := io.ReadAll(resp.Body)
		log.Printf("[Runner Warning] Poll returned status %s: %s", resp.Status, string(bodyErr))
		return
	}

	if resp.StatusCode == http.StatusNoContent {
		return // No jobs in queue
	}

	if resp.StatusCode != http.StatusOK {
		return
	}

	var job pollJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return
	}

	log.Printf("[Runner Engine] Pulled job %s targeting model %s. Executing...", job.ID, job.Model)

	e.mu.Lock()
	e.inFlight++
	if e.Active {
		e.Stats.Status = "busy"
	}
	e.mu.Unlock()

	// Forward to local llama.cpp instance with dual schema mapping support
	resPayload, status, err := e.forwardToLlamaCpp(job.Model, job.Payload)

	var errMsg string
	if err != nil {
		errMsg = err.Error()
		e.mu.Lock()
		e.Stats.ErrorCount++
		e.mu.Unlock()
	} else {
		e.mu.Lock()
		e.Stats.ProcessedCount++
		e.mu.Unlock()
	}

	e.resolveJob(job.ID, resPayload, status, errMsg)

	e.mu.Lock()
	if e.inFlight > 0 {
		e.inFlight--
	}
	if e.Active && e.inFlight == 0 {
		e.Stats.Status = "idle"
	}
	e.mu.Unlock()
}

func (e *CoreRunnerEngine) forwardToLlamaCpp(model string, payload []byte) ([]byte, int, error) {
	// 1. Detect if the payload is OpenAI or Gemini format
	isOpenAI := bytes.Contains(payload, []byte(`"messages"`))
	isGemini := bytes.Contains(payload, []byte(`"contents"`))

	if !isOpenAI && !isGemini {
		// Default to treating it as OpenAI if raw text or unrecognized
		isOpenAI = true
	}

	var targetURL string
	var requestBody []byte

	if isOpenAI {
		// Simply forward to llama-server OpenAI endpoint
		targetURL = e.LlamaServerURL + "/v1/chat/completions"
		requestBody = payload
	} else {
		// Gemini request mapping:
		// Parse the Gemini contents + optional systemInstruction payload and translate to OpenAI chat structure for llama.cpp
		type geminiTextPart struct {
			Text string `json:"text"`
		}
		var geminiReq struct {
			SystemInstruction *struct {
				Parts []geminiTextPart `json:"parts"`
			} `json:"systemInstruction"`
			SystemInstructionSnake *struct {
				Parts []geminiTextPart `json:"parts"`
			} `json:"system_instruction"`
			Contents []struct {
				Role  string           `json:"role"`
				Parts []geminiTextPart `json:"parts"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(payload, &geminiReq); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("failed to unmarshal Gemini payload: %w", err)
		}

		type oaiMessage struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		var messages []oaiMessage
		sysInst := geminiReq.SystemInstruction
		if sysInst == nil {
			sysInst = geminiReq.SystemInstructionSnake
		}
		if sysInst != nil && len(sysInst.Parts) > 0 {
			var sysParts []string
			for _, p := range sysInst.Parts {
				if strings.TrimSpace(p.Text) != "" {
					sysParts = append(sysParts, p.Text)
				}
			}
			if len(sysParts) > 0 {
				messages = append(messages, oaiMessage{
					Role:    "system",
					Content: strings.Join(sysParts, "\n"),
				})
			}
		}
		for _, content := range geminiReq.Contents {
			role := content.Role
			if role == "" || role == "user" {
				role = "user"
			} else if role == "model" {
				role = "assistant"
			}
			var textParts []string
			for _, part := range content.Parts {
				textParts = append(textParts, part.Text)
			}
			messages = append(messages, oaiMessage{
				Role:    role,
				Content: strings.Join(textParts, "\n"),
			})
		}

		type oaiReq struct {
			Model    string       `json:"model"`
			Messages []oaiMessage `json:"messages"`
		}
		translatedReq := oaiReq{
			Model:    model,
			Messages: messages,
		}
		requestBody, _ = json.Marshal(translatedReq)
		targetURL = e.LlamaServerURL + "/v1/chat/completions"
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(requestBody))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("local llama.cpp host at %s unavailable: %w", e.LlamaServerURL, err)
	}
	defer resp.Body.Close()

	resBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	if isGemini && resp.StatusCode == http.StatusOK {
		// Translate OpenAI completions output back to Gemini format while preserving token usageMetadata
		var oaiRes struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(resBytes, &oaiRes); err == nil && len(oaiRes.Choices) > 0 {
			finishReason := "STOP"
			if strings.EqualFold(oaiRes.Choices[0].FinishReason, "length") {
				finishReason = "MAX_TOKENS"
			}
			geminiResp := map[string]interface{}{
				"candidates": []interface{}{
					map[string]interface{}{
						"content": map[string]interface{}{
							"parts": []interface{}{
								map[string]interface{}{
									"text": oaiRes.Choices[0].Message.Content,
								},
							},
						},
						"finishReason": finishReason,
					},
				},
			}
			totalTokens := oaiRes.Usage.TotalTokens
			if totalTokens == 0 && (oaiRes.Usage.PromptTokens > 0 || oaiRes.Usage.CompletionTokens > 0) {
				totalTokens = oaiRes.Usage.PromptTokens + oaiRes.Usage.CompletionTokens
			}
			if totalTokens > 0 || oaiRes.Usage.PromptTokens > 0 || oaiRes.Usage.CompletionTokens > 0 {
				geminiResp["usageMetadata"] = map[string]interface{}{
					"promptTokenCount":     oaiRes.Usage.PromptTokens,
					"candidatesTokenCount": oaiRes.Usage.CompletionTokens,
					"totalTokenCount":      totalTokens,
				}
			}
			geminiBytes, _ := json.Marshal(geminiResp)
			return geminiBytes, http.StatusOK, nil
		}
	}

	return resBytes, resp.StatusCode, nil
}

func (e *CoreRunnerEngine) resolveJob(jobID string, payload []byte, statusCode int, errMsg string) {
	resBody := map[string]interface{}{
		"job_id":      jobID,
		"payload":     payload,
		"status_code": statusCode,
	}
	if errMsg != "" {
		resBody["error"] = errMsg
	}

	bodyBytes, _ := json.Marshal(resBody)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", e.RouterURL+"/api/v1/cluster/queue/resolve", bytes.NewReader(bodyBytes))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	e.attachAuthHeaders(ctx, req)

	resp, err := e.httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
