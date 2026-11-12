package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Client represents a router consumer client.
type Client struct {
	ID       string `firestore:"id" json:"id"`
	Name     string `firestore:"name" json:"name"`
	Tier     string `firestore:"tier" json:"tier"` // "free", "standard", "premium"
	RPM      int    `firestore:"rpm" json:"rpm"`
	TPM      int    `firestore:"tpm" json:"tpm"`
	Priority string `firestore:"priority" json:"priority"` // "high", "medium", "low"
}

// ComplexityRouting defines configurations and thresholds for dynamic routing.
type ComplexityRouting struct {
	Enabled                bool              `firestore:"enabled" json:"enabled"`
	AlwaysOverride         bool              `firestore:"always_override" json:"always_override"`
	Engine                 string            `firestore:"engine" json:"engine"` // "gemini-decision", "heuristics", "llm", "jev", "fastino", "clm", "contrastive-lm"
	SimpleModel            string            `firestore:"simple_model" json:"simple_model"`
	MediumModel            string            `firestore:"medium_model" json:"medium_model"`
	ComplexModel           string            `firestore:"complex_model" json:"complex_model"`
	SimpleCharLimit        int               `firestore:"simple_char_limit" json:"simple_char_limit"`
	MediumCharLimit        int               `firestore:"medium_char_limit" json:"medium_char_limit"`
	ForceComplexMultimodal bool              `firestore:"force_complex_multimodal" json:"force_complex_multimodal"`
	ForceComplexTools      bool              `firestore:"force_complex_tools" json:"force_complex_tools"`
	UseLLMClassifier       bool              `firestore:"use_llm_classifier" json:"use_llm_classifier"`
	ClassifierModel        string            `firestore:"classifier_model" json:"classifier_model"`
	AdditionalInstructions string            `firestore:"additional_instructions" json:"additional_instructions"`
	GeminiDecisionEndpoint string            `firestore:"gemini_decision_endpoint,omitempty" json:"gemini_decision_endpoint,omitempty"`
	JevEndpoint            string            `firestore:"jev_endpoint" json:"jev_endpoint"`
	FastinoEndpoint        string            `firestore:"fastino_endpoint" json:"fastino_endpoint"`
	CLMEndpoint            string            `firestore:"clm_endpoint" json:"clm_endpoint"`
	SecretRef              string            `firestore:"secret_ref,omitempty" json:"secret_ref,omitempty"`
	MinConfidence          float64           `firestore:"min_confidence,omitempty" json:"min_confidence,omitempty"`
	SimpleCriteria         string            `firestore:"simple_criteria,omitempty" json:"simple_criteria,omitempty"`
	MediumCriteria         string            `firestore:"medium_criteria,omitempty" json:"medium_criteria,omitempty"`
	ComplexCriteria        string            `firestore:"complex_criteria,omitempty" json:"complex_criteria,omitempty"`
	TaskMapping            map[string]string `firestore:"task_mapping" json:"task_mapping"` // Maps task names to target models
}

// App represents an explicit application belonging to a client.
type App struct {
	ID                   string            `firestore:"id" json:"id"`
	ClientID             string            `firestore:"client_id" json:"client_id"`
	Name                 string            `firestore:"name" json:"name"`
	RPM                  int               `firestore:"rpm" json:"rpm"`
	TPM                  int               `firestore:"tpm" json:"tpm"`
	Priority             string            `firestore:"priority" json:"priority"` // "high", "medium", "low"
	Complexity           ComplexityRouting `firestore:"complexity" json:"complexity"`
	OptOutDynamicRouting bool              `firestore:"opt_out_dynamic_routing" json:"opt_out_dynamic_routing"`
	OptOutTPM            bool              `firestore:"opt_out_tpm" json:"opt_out_tpm"`
}

// APIKey represents an authorized router API key mapping to a client and app.
type APIKey struct {
	KeyHash  string `firestore:"key_hash" json:"key_hash"`
	ClientID string `firestore:"client_id" json:"client_id"` // keep for backwards-compatibility
	AppID    string `firestore:"app_id" json:"app_id"`
	Status   string `firestore:"status" json:"status"` // "active", "revoked"
}

// RoutingRule defines dynamic model rewrites and targets.
type RoutingRule struct {
	ID             string  `firestore:"id" json:"id"`
	AppID          string  `firestore:"app_id" json:"app_id"`                               // bound app boundary
	ModelPattern   string  `firestore:"model_pattern" json:"model_pattern"`                 // regex or exact match
	ClientTier     string  `firestore:"client_tier" json:"client_tier"`                     // "all" or specific tier
	HeaderName     string  `firestore:"header_name" json:"header_name"`                     // optional header filter
	HeaderValue    string  `firestore:"header_value" json:"header_value"`                   // optional value pattern to match
	ComplexityTier string  `firestore:"complexity_tier,omitempty" json:"complexity_tier,omitempty"` // optional "simple", "medium", "complex"
	DecisionEngine string  `firestore:"decision_engine,omitempty" json:"decision_engine,omitempty"` // optional "jev", "fastino", "contrastive-lm", "clm", "llm", "heuristics"
	MinConfidence  float64 `firestore:"min_confidence,omitempty" json:"min_confidence,omitempty"`   // optional minimum classifier confidence (0.0 - 1.0)
	TaskKind       string  `firestore:"task_kind,omitempty" json:"task_kind,omitempty"`             // optional X-Task-Kind / task filter
	TargetModel    string  `firestore:"target_model" json:"target_model"`
	TargetLocation string  `firestore:"target_location" json:"target_location"` // E.g. "us-central1" for Vertex AI
	FallbackModel  string  `firestore:"fallback_model" json:"fallback_model"`
	PriorityWeight int     `firestore:"priority_weight" json:"priority_weight"`
}

// CustomHeader defines client-provided custom header verification rules.
type CustomHeader struct {
	ID           string `firestore:"id" json:"id"`
	AppID        string `firestore:"app_id" json:"app_id"`                 // bound app boundary
	Name         string `firestore:"name" json:"name"`                     // e.g. "X-Client-App-ID"
	Description  string `firestore:"description" json:"description"`       // e.g. "Identifies the calling application"
	Required     bool   `firestore:"required" json:"required"`             // Whether the header is mandatory
	Validation   string `firestore:"validation" json:"validation"`         // "non-empty", "regex", "enum"
	ValuePattern string `firestore:"value_pattern" json:"value_pattern"`   // Regex format or comma-separated enum values
}

// ModelConfig defines the registered models and endpoints active in the router.
type ModelConfig struct {
	ID          string `firestore:"id" json:"id"`                     // e.g., "gemini-2.5-flash", "my-custom-model"
	DisplayName string `firestore:"display_name" json:"display_name"`   // e.g., "Gemini 2.5 Flash", "Fine-Tuned Support Model"
	Location    string `firestore:"location" json:"location"`         // e.g., "us-central1", "us", "global"
	Type        string `firestore:"type" json:"type"`                 // "foundation", "custom", "endpoint"
	Active      bool   `firestore:"active" json:"active"`             // true if available for routing
}

// ProviderType defines the primary cloud or local infrastructure type.
type ProviderType string

const (
	ProviderGoogle ProviderType = "google"
	ProviderAWS    ProviderType = "aws"
	ProviderAzure  ProviderType = "azure"
	ProviderLocal  ProviderType = "local"
)

// ProviderConfig configures a top-level cloud or local infrastructure provider.
type ProviderConfig struct {
	ID          ProviderType            `firestore:"id" json:"id"`                     // "google", "aws", "azure", "local"
	DisplayName string                  `firestore:"display_name" json:"display_name"` // "Google Cloud", etc.
	Enabled     bool                    `firestore:"enabled" json:"enabled"`
	Credentials map[string]string       `firestore:"credentials" json:"-"`             // Secured keys/roles (not exposed in JSON)
	Regions     map[string]RegionConfig `firestore:"regions" json:"regions"`           // Map of region-code -> Region configuration
}

// RegionConfig defines enabled features and providers inside a specific region.
type RegionConfig struct {
	Code            string            `firestore:"code" json:"code"`                         // e.g., "us-central1" or "local-cluster"
	Active          bool              `firestore:"active" json:"active"`
	EnabledServices []string          `firestore:"enabled_services" json:"enabled_services"` // e.g., ["gemini", "anthropic", "llama"]
	DiscoveryCron   string            `firestore:"discovery_cron" json:"discovery_cron"`     // e.g., "0 0 * * *" (Daily discovery)
	LocalConfig     *LocalConfig      `firestore:"local_config,omitempty" json:"local_config,omitempty"`
}

// DecisionProviderConfig stores global connection and cluster settings for a decision engine (jev, fastino, contrastive-lm).
type DecisionProviderConfig struct {
	Engine         string `firestore:"engine" json:"engine"`
	Endpoint       string `firestore:"endpoint,omitempty" json:"endpoint,omitempty"`
	ConnectionMode string `firestore:"connection_mode,omitempty" json:"connection_mode,omitempty"`
	ClusterID      string `firestore:"cluster_id,omitempty" json:"cluster_id,omitempty"`
	SecretRef      string `firestore:"secret_ref,omitempty" json:"secret_ref,omitempty"`
}

// LocalConfig defines the parameters for running models on local systems/clusters.
type LocalConfig struct {
	DirectEndpoints   []DirectEndpoint                  `firestore:"direct_endpoints" json:"direct_endpoints"` // Static local endpoints
	Clusters          []LocalCluster                    `firestore:"clusters" json:"clusters"`                 // Shared local queues
	DecisionProviders map[string]DecisionProviderConfig `firestore:"decision_providers,omitempty" json:"decision_providers,omitempty"`
}

// DirectEndpoint describes a single static local endpoint bypassing the queue.
type DirectEndpoint struct {
	ID               string `firestore:"id" json:"id"`
	Name             string `firestore:"name,omitempty" json:"name,omitempty"`
	ModelName        string `firestore:"model_name" json:"model_name"` // e.g., "gemma-4-26b", "contrastive-lm"
	URL              string `firestore:"url" json:"url"`               // e.g., "http://10.128.0.5:8080/rank"
	BaseURL          string `firestore:"base_url,omitempty" json:"base_url,omitempty"`
	Active           bool   `firestore:"active" json:"active"`
	Role             string `firestore:"role,omitempty" json:"role,omitempty"`
	Engine           string `firestore:"engine,omitempty" json:"engine,omitempty"`
	SecretRef        string `firestore:"secret_ref,omitempty" json:"secret_ref,omitempty"`
	ConnectionMode   string `firestore:"connection_mode,omitempty" json:"connection_mode,omitempty"`
	ClusterID        string `firestore:"cluster_id,omitempty" json:"cluster_id,omitempty"`
	FallbackModel    string `firestore:"fallback_model,omitempty" json:"fallback_model,omitempty"`
	FallbackLocation string `firestore:"fallback_location,omitempty" json:"fallback_location,omitempty"`
}

// LocalCluster manages dynamic pool queuing for on-prem machines.
type LocalCluster struct {
	ID               string          `firestore:"id" json:"id"`
	Name             string          `firestore:"name" json:"name"`
	Description      string          `firestore:"description" json:"description"`
	QueueName        string          `firestore:"queue_name" json:"queue_name"`
	MaxQueueDepth    int             `firestore:"max_queue_depth,omitempty" json:"max_queue_depth,omitempty"`
	MaxQueueAge      time.Duration   `firestore:"max_queue_age" json:"max_queue_age"`
	FallbackModel    string          `firestore:"fallback_model,omitempty" json:"fallback_model,omitempty"`
	FallbackLocation string          `firestore:"fallback_location,omitempty" json:"fallback_location,omitempty"`
	Nodes            map[string]Node `firestore:"nodes" json:"nodes"` // Heartbeating computing agents
}

// Node represents a computing instance running a Smart Router agent.
type Node struct {
	ID                string    `firestore:"id" json:"id"`
	Name              string    `firestore:"name" json:"name"`
	Status            string    `firestore:"status" json:"status"` // "online", "offline", "busy"
	LastHeartbeat     time.Time `firestore:"last_heartbeat" json:"last_heartbeat"`
	MaxConcurrent     int       `firestore:"max_concurrent" json:"max_concurrent"`
	MemoryAllocatedGB int       `firestore:"memory_allocated_gb" json:"memory_allocated_gb"`
	ComputeGPUCores   int       `firestore:"compute_gpu_cores" json:"compute_gpu_cores"`
	SupportedModels   []string  `firestore:"supported_models" json:"supported_models"`
	MaxModelSizeGB    int       `firestore:"max_model_size_gb" json:"max_model_size_gb"` // Dynamic limit adjustment
}

// LocalDB represents the JSON schema for the local development database.
type LocalDB struct {
	Clients       map[string]Client            `json:"clients"`
	Apps          map[string]App               `json:"apps"`
	APIKeys       map[string]APIKey            `json:"api_keys"`
	RoutingRules  []RoutingRule                `json:"routing_rules"`
	CustomHeaders []CustomHeader               `json:"custom_headers"`
	Models        []ModelConfig                `json:"models"`
	Providers     map[string]ProviderConfig    `json:"providers"`
	Runners       []RunnerResponse             `json:"runners,omitempty"`
}

// HashKey returns the hex-encoded SHA-256 hash of an API key.
func HashKey(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

// GetMultiRegionParent returns the multi-region group identifier ("us", "eu", "asia", etc.) for a given region code.
func GetMultiRegionParent(loc string) string {
	if loc == "us" || strings.HasPrefix(loc, "us-") {
		return "us"
	}
	if loc == "eu" || strings.HasPrefix(loc, "europe-") || strings.HasPrefix(loc, "eu-") {
		return "eu"
	}
	if loc == "asia" || strings.HasPrefix(loc, "asia-") {
		return "asia"
	}
	if loc == "me" || strings.HasPrefix(loc, "me-") {
		return "me"
	}
	if loc == "africa" || strings.HasPrefix(loc, "africa-") {
		return "africa"
	}
	if loc == "northamerica" || strings.HasPrefix(loc, "northamerica-") {
		return "northamerica"
	}
	if loc == "southamerica" || strings.HasPrefix(loc, "southamerica-") {
		return "southamerica"
	}
	if loc == "australia" || strings.HasPrefix(loc, "australia-") {
		return "australia"
	}
	return ""
}

// IsLocationCompatible returns true if the modelLoc is compatible with the router's serving boundary.
func IsLocationCompatible(routerLoc, modelLoc string) bool {
	if modelLoc == "global" || modelLoc == "local" || modelLoc == "" {
		return true
	}
	if routerLoc == modelLoc {
		return true
	}
	routerParent := GetMultiRegionParent(routerLoc)
	modelParent := GetMultiRegionParent(modelLoc)
	return routerParent != "" && routerParent == modelParent
}

// GetLocationLevel returns the specificity level (1 = specific region, 2 = multi-region, 3 = global).
func GetLocationLevel(loc string) int {
	if loc == "global" || loc == "" {
		return 3
	}
	if strings.Contains(loc, "-") {
		return 1 // Specific region (e.g., us-central1)
	}
	return 2 // Multi-region (e.g., us)
}

// GetSmallestCompatibleLocation returns the most specific (smallest) compatible location between locA and locB.
func GetSmallestCompatibleLocation(locA, locB string) string {
	if locA == "global" || locA == "" {
		return locB
	}
	if locB == "global" || locB == "" {
		return locA
	}
	return locB
}

// ExtractLocationFromResourceName parses a GCP resource name to extract the location segment.
func ExtractLocationFromResourceName(name string) string {
	if !strings.HasPrefix(name, "projects/") {
		return ""
	}
	parts := strings.Split(name, "/")
	for i, part := range parts {
		if part == "locations" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// IsValidModelName validates standard and custom Vertex AI model names.
func IsValidModelName(name string) bool {
	if strings.HasPrefix(name, "gemini-") {
		// Reject non-existent 3.8 model variants (only 3.8-flash exists for text/multimodal)
		if name == "gemini-3.8-pro" || name == "gemini-3.8-flash-lite" {
			return false
		}
		// Reject legacy Gemini versions below 2.5 (e.g., gemini-1.5, gemini-2.0 up to gemini-2.4)
		reLegacy := regexp.MustCompile(`^gemini-(1\.[0-9]|2\.[0-4])(?:-|$)`)
		if reLegacy.MatchString(name) {
			return false
		}
		return true
	}

	if strings.HasPrefix(name, "gemma-") {
		// Only Gemma 4 models are permitted (reject Gemma 1, 2, 3)
		reGemmaLegacy := regexp.MustCompile(`^gemma-([1-3])(?:-|$)`)
		if reGemmaLegacy.MatchString(name) {
			return false
		}
		return true
	}

	// Matches standard Embeddings, text-embeddings, or Virtual Dynamic Router
	if strings.Contains(name, "embedding") || name == "gemini-dynamic" {
		return true
	}

	// Matches Custom tuned model resource path
	if strings.HasPrefix(name, "projects/") && strings.Contains(name, "/models/") {
		parts := strings.Split(name, "/models/")
		return len(parts) == 2 && len(strings.TrimSpace(parts[1])) > 0
	}

	// Matches Serving Endpoint resource path
	if strings.HasPrefix(name, "projects/") && strings.Contains(name, "/endpoints/") {
		parts := strings.Split(name, "/endpoints/")
		return len(parts) == 2 && len(strings.TrimSpace(parts[1])) > 0
	}

	return false
}

// StripLocationSuffix returns the base model ID by removing any "@location" suffix.
func StripLocationSuffix(id string) string {
	if idx := strings.Index(id, "@"); idx != -1 {
		return id[:idx]
	}
	return id
}

// ExtractLocationSuffix returns the location segment after "@" in a model ID, or "" if none is present.
func ExtractLocationSuffix(id string) string {
	if idx := strings.Index(id, "@"); idx != -1 && idx+1 < len(id) {
		return id[idx+1:]
	}
	return ""
}

// SplitModelAndLocation splits a model identifier with an optional "@location" suffix into (baseModel, location).
func SplitModelAndLocation(id string) (string, string) {
	return StripLocationSuffix(id), ExtractLocationSuffix(id)
}

// IsMultiRegionOrGlobal returns true if the location segment represents a multi-region (e.g., "us", "eu") or the global region.
func IsMultiRegionOrGlobal(loc string) bool {
	if loc == "" {
		return true
	}
	return loc == "global" || !strings.Contains(loc, "-")
}

// GetVertexEndpointHost returns the official hostname for Google Cloud Vertex AI based on the location ID.
func GetVertexEndpointHost(loc string) string {
	if loc == "global" || loc == "" {
		return "aiplatform.googleapis.com"
	}
	if loc == "us" || loc == "eu" || loc == "asia" {
		return fmt.Sprintf("aiplatform.%s.rep.googleapis.com", loc)
	}
	if strings.Contains(loc, "-") {
		return fmt.Sprintf("%s-aiplatform.googleapis.com", loc)
	}
	// Fallback for any other multiregion
	return fmt.Sprintf("aiplatform.%s.rep.googleapis.com", loc)
}

// GetDefaultLocationForModel returns the appropriate Vertex AI location for a model.
// Gemini 3.x and 4.x models are served exclusively via global/multi-region endpoints (e.g. "global", "us").
func GetDefaultLocationForModel(modelName string, currentLoc string) string {
	if resLoc := ExtractLocationFromResourceName(modelName); resLoc != "" {
		return resLoc
	}
	if sufLoc := ExtractLocationSuffix(modelName); sufLoc != "" {
		return sufLoc
	}
	baseID := StripLocationSuffix(modelName)
	if baseID == "gemini-decision-1" {
		return "global"
	}
	if strings.HasPrefix(baseID, "gemini-3.") || strings.HasPrefix(baseID, "gemini-4") {
		if currentLoc == "us" || currentLoc == "eu" || currentLoc == "global" {
			return currentLoc
		}
		return "global"
	}
	if currentLoc != "" {
		return currentLoc
	}
	return "us-central1"
}

// QueueSnapshotItem represents a request in the scheduling queue.
type QueueSnapshotItem struct {
	AppID          string    `json:"app_id"`
	Model          string    `json:"model"`
	Priority       string    `json:"priority"`
	Tier           string    `json:"tier"`
	Status         string    `json:"status"`
	ArrivalTime    time.Time `json:"arrival_time"`
	DurationMs     int64     `json:"duration_ms"`
	ClusterID      string    `json:"cluster_id,omitempty"`
	AssignedNodeID string    `json:"assigned_node_id,omitempty"`
	QueueSource    string    `json:"queue_source,omitempty"`
}

// QueueJob represents an active held request inside the priority queue.
type QueueJob struct {
	ID             string           `json:"id"`
	ClusterID      string           `json:"cluster_id"`
	AssignedNodeID string           `json:"assigned_node_id,omitempty"`
	AppID          string           `json:"app_id"`
	Model          string           `json:"model"`
	Priority       string           `json:"priority"` // "high", "medium", "low"
	Tier           string           `json:"tier,omitempty"`
	MaxAge         time.Duration    `json:"max_age,omitempty"`
	Payload        []byte           `json:"payload"`
	ResponseChan   chan QueueResult `json:"-"`
	CreatedAt      time.Time        `json:"created_at"`
	StartedAt      time.Time        `json:"started_at,omitempty"`
}

// QueueResult carries the execution output back to the proxy loop.
type QueueResult struct {
	Payload    []byte
	StatusCode int
	Error      error
}


