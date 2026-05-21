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
	Enabled                bool   `firestore:"enabled" json:"enabled"`
	AlwaysOverride         bool   `firestore:"always_override" json:"always_override"`
	SimpleModel            string `firestore:"simple_model" json:"simple_model"`
	MediumModel            string `firestore:"medium_model" json:"medium_model"`
	ComplexModel           string `firestore:"complex_model" json:"complex_model"`
	SimpleCharLimit        int    `firestore:"simple_char_limit" json:"simple_char_limit"`
	MediumCharLimit        int    `firestore:"medium_char_limit" json:"medium_char_limit"`
	ForceComplexMultimodal bool   `firestore:"force_complex_multimodal" json:"force_complex_multimodal"`
	ForceComplexTools      bool   `firestore:"force_complex_tools" json:"force_complex_tools"`
	UseLLMClassifier       bool   `firestore:"use_llm_classifier" json:"use_llm_classifier"`
	ClassifierModel        string `firestore:"classifier_model" json:"classifier_model"`
	AdditionalInstructions string `firestore:"additional_instructions" json:"additional_instructions"`
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
	ID             string `firestore:"id" json:"id"`
	AppID          string `firestore:"app_id" json:"app_id"`               // bound app boundary
	ModelPattern   string `firestore:"model_pattern" json:"model_pattern"` // regex or exact match
	ClientTier     string `firestore:"client_tier" json:"client_tier"`     // "all" or specific tier
	HeaderName     string `firestore:"header_name" json:"header_name"`     // optional header filter
	HeaderValue    string `firestore:"header_value" json:"header_value"`   // optional value pattern to match
	TargetModel    string `firestore:"target_model" json:"target_model"`
	TargetLocation string `firestore:"target_location" json:"target_location"` // E.g. "us-central1" for Vertex AI
	FallbackModel  string `firestore:"fallback_model" json:"fallback_model"`
	PriorityWeight int    `firestore:"priority_weight" json:"priority_weight"`
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

// LocalDB represents the JSON schema for the local development database.
type LocalDB struct {
	Clients       map[string]Client      `json:"clients"`
	Apps          map[string]App         `json:"apps"`
	APIKeys       map[string]APIKey      `json:"api_keys"`
	RoutingRules  []RoutingRule          `json:"routing_rules"`
	CustomHeaders []CustomHeader         `json:"custom_headers"`
	Models        []ModelConfig          `json:"models"`
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
	return ""
}

// IsLocationCompatible returns true if the modelLoc is compatible with the router's serving boundary.
func IsLocationCompatible(routerLoc, modelLoc string) bool {
	if modelLoc == "global" || modelLoc == "" {
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
	// Matches standard and preview Gemini model formats (supporting optional minor versions and preview suffixes)
	reGemini := regexp.MustCompile(`^gemini-([2-9])(?:\.([0-9]))?-(flash|pro|flash-lite)(?:-preview(?:-\d{2}-\d{4})?)?$`)
	matches := reGemini.FindStringSubmatch(name)
	if matches != nil {
		majorStr := matches[1]
		minorStr := matches[2]
		if majorStr == "2" {
			// For Gemini 2.x, it must be 2.5 or higher
			if minorStr == "" || minorStr < "5" {
				return false
			}
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
	if loc == "us" || loc == "eu" {
		return fmt.Sprintf("aiplatform.%s.rep.googleapis.com", loc)
	}
	if strings.Contains(loc, "-") {
		return fmt.Sprintf("%s-aiplatform.googleapis.com", loc)
	}
	// Fallback for any other multiregion
	return fmt.Sprintf("aiplatform.%s.rep.googleapis.com", loc)
}

// QueueSnapshotItem represents a request in the scheduling queue.
type QueueSnapshotItem struct {
	AppID       string    `json:"app_id"`
	Model       string    `json:"model"`
	Priority    string    `json:"priority"`
	Tier        string    `json:"tier"`
	Status      string    `json:"status"`
	ArrivalTime time.Time `json:"arrival_time"`
	DurationMs  int64     `json:"duration_ms"`
}

