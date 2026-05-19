package config

import (
	"context"
	"os"
	"testing"
)

func TestIsValidModelName(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		expected  bool
	}{
		// Valid Gemini 2.5+ Models
		{"Gemini 2.5 Flash", "gemini-2.5-flash", true},
		{"Gemini 2.5 Pro", "gemini-2.5-pro", true},
		{"Gemini 2.5 Flash-Lite", "gemini-2.5-flash-lite", true},
		{"Gemini 3.0 Flash", "gemini-3.0-flash", true},
		{"Gemini 3.1 Pro", "gemini-3.1-pro", true},
		{"Gemini 3.5 Flash-Lite", "gemini-3.5-flash-lite", true},
		
		// Valid Embeddings and Virtual Router
		{"Text Embedding 004", "text-embedding-004", true},
		{"Multimodal Embedding 001", "multimodal-embedding-001", true},
		{"Gemini Dynamic Virtual Router", "gemini-dynamic", true},

		// Valid Custom/Tuned Model and Endpoint paths
		{"Valid Tuned Path", "projects/my-project/locations/us-central1/models/my-custom-model", true},
		{"Valid Endpoint Path", "projects/my-project/locations/us-central1/endpoints/my-endpoint-1", true},

		// Deprecated/Legacy models (pre-2.5 series must be blocked)
		{"Gemini 1.5 Flash (Legacy)", "gemini-1.5-flash", false},
		{"Gemini 1.5 Pro (Legacy)", "gemini-1.5-pro", false},
		{"Gemini 1.0 Pro (Legacy)", "gemini-1.0-pro", false},
		{"Gemini 2.0 Flash (Legacy)", "gemini-2.0-flash", false},

		// Invalid names
		{"Arbitrary string", "some-random-model", false},
		{"Empty name", "", false},
		{"Incomplete path", "projects/my-project/locations/us-central1/models/", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := IsValidModelName(tt.modelName)
			if actual != tt.expected {
				t.Errorf("IsValidModelName(%q) = %t; expected %t", tt.modelName, actual, tt.expected)
			}
		})
	}
}

func TestDefaultModelSeeding(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	defer os.RemoveAll("data/local_db.json")

	ctx := context.Background()
	store, err := NewConfigStore(ctx, "test-project")
	if err != nil {
		t.Fatalf("failed to initialize config store: %v", err)
	}

	models, err := store.GetAllModels(ctx)
	if err != nil {
		t.Fatalf("failed to retrieve seeded models: %v", err)
	}

	// We expect at least the seeded default models (5 standard gemini-2.5, 7 standard gemini-3.x, plus 2 embeddings = 12 models)
	expectedSeededCount := 12
	if len(models) < expectedSeededCount {
		t.Errorf("expected at least %d seeded models, got %d", expectedSeededCount, len(models))
	}

	// Assert that all seeded models are valid and active
	for _, m := range models {
		if !m.Active {
			t.Errorf("expected seeded model %s to be active, but it was inactive", m.ID)
		}
		if !IsValidModelName(m.ID) {
			t.Errorf("seeded model %s has an invalid name code", m.ID)
		}
	}
}
