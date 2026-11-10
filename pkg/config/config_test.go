package config

import (
	"testing"
)

func TestLocationHelpers(t *testing.T) {
	t.Run("GetMultiRegionParent", func(t *testing.T) {
		tests := []struct {
			input string
			want  string
		}{
			{"us-central1", "us"},
			{"us", "us"},
			{"europe-west3", "eu"},
			{"eu", "eu"},
			{"asia-northeast1", "asia"},
			{"asia", "asia"},
			{"other-region", ""},
			{"", ""},
		}
		for _, tc := range tests {
			if got := GetMultiRegionParent(tc.input); got != tc.want {
				t.Errorf("GetMultiRegionParent(%q) = %q; want %q", tc.input, got, tc.want)
			}
		}
	})

	t.Run("IsLocationCompatible", func(t *testing.T) {
		tests := []struct {
			routerLoc string
			modelLoc  string
			want      bool
		}{
			{"us-central1", "us-central1", true},
			{"us-central1", "us-east4", true},
			{"us-central1", "europe-west3", false},
			{"us-central1", "global", true},
			{"us-central1", "", true},
			{"europe-west1", "eu", true},
			{"europe-west1", "europe-west2", true},
			{"europe-west1", "us-central1", false},
		}
		for _, tc := range tests {
			if got := IsLocationCompatible(tc.routerLoc, tc.modelLoc); got != tc.want {
				t.Errorf("IsLocationCompatible(%q, %q) = %t; want %t", tc.routerLoc, tc.modelLoc, got, tc.want)
			}
		}
	})

	t.Run("GetLocationLevel", func(t *testing.T) {
		tests := []struct {
			input string
			want  int
		}{
			{"us-central1", 1},
			{"us", 2},
			{"global", 3},
			{"", 3},
		}
		for _, tc := range tests {
			if got := GetLocationLevel(tc.input); got != tc.want {
				t.Errorf("GetLocationLevel(%q) = %d; want %d", tc.input, got, tc.want)
			}
		}
	})

	t.Run("GetSmallestCompatibleLocation", func(t *testing.T) {
		tests := []struct {
			locA string
			locB string
			want string
		}{
			{"global", "us-central1", "us-central1"},
			{"us-central1", "global", "us-central1"},
			{"us-central1", "us-east4", "us-east4"},
			{"", "us-central1", "us-central1"},
		}
		for _, tc := range tests {
			if got := GetSmallestCompatibleLocation(tc.locA, tc.locB); got != tc.want {
				t.Errorf("GetSmallestCompatibleLocation(%q, %q) = %q; want %q", tc.locA, tc.locB, got, tc.want)
			}
		}
	})

	t.Run("ExtractLocationFromResourceName", func(t *testing.T) {
		tests := []struct {
			input string
			want  string
		}{
			{"projects/my-proj/locations/us-central1/models/gemini-2.5-pro", "us-central1"},
			{"projects/my-proj/locations/europe-west3/endpoints/ep-1", "europe-west3"},
			{"invalid-name", ""},
			{"projects/my-proj/models/gemini-2.5-pro", ""},
		}
		for _, tc := range tests {
			if got := ExtractLocationFromResourceName(tc.input); got != tc.want {
				t.Errorf("ExtractLocationFromResourceName(%q) = %q; want %q", tc.input, got, tc.want)
			}
		}
	})

	t.Run("IsMultiRegionOrGlobal", func(t *testing.T) {
		tests := []struct {
			input string
			want  bool
		}{
			{"us", true},
			{"global", true},
			{"", true},
			{"us-central1", false},
			{"europe-west3", false},
		}
		for _, tc := range tests {
			if got := IsMultiRegionOrGlobal(tc.input); got != tc.want {
				t.Errorf("IsMultiRegionOrGlobal(%q) = %t; want %t", tc.input, got, tc.want)
			}
		}
	})

	t.Run("GetVertexEndpointHost", func(t *testing.T) {
		tests := []struct {
			input string
			want  string
		}{
			{"global", "aiplatform.googleapis.com"},
			{"", "aiplatform.googleapis.com"},
			{"us", "aiplatform.us.rep.googleapis.com"},
			{"eu", "aiplatform.eu.rep.googleapis.com"},
			{"asia", "aiplatform.asia.rep.googleapis.com"},
			{"us-central1", "us-central1-aiplatform.googleapis.com"},
			{"europe-west1", "europe-west1-aiplatform.googleapis.com"},
			{"europe-west9", "europe-west9-aiplatform.googleapis.com"},
			{"europe-west3", "europe-west3-aiplatform.googleapis.com"},
			{"asia-northeast1", "asia-northeast1-aiplatform.googleapis.com"},
		}
		for _, tc := range tests {
			if got := GetVertexEndpointHost(tc.input); got != tc.want {
				t.Errorf("GetVertexEndpointHost(%q) = %q; want %q", tc.input, got, tc.want)
			}
		}
	})
}

func TestIsValidModelName(t *testing.T) {
	tests := []struct {
		name  string
		model string
		valid bool
	}{
		// Verified 3.8 models
		{"gemini-3.8-flash is valid", "gemini-3.8-flash", true},
		{"gemini-3.8-flash-cyber is valid", "gemini-3.8-flash-cyber", true},
		{"gemini-3.8-pro is invalid (does not exist)", "gemini-3.8-pro", false},
		{"gemini-3.8-flash-lite is invalid (does not exist)", "gemini-3.8-flash-lite", false},

		// Verified 3.5 & 3.1 models
		{"gemini-3.5-flash is valid", "gemini-3.5-flash", true},
		{"gemini-3.5-flash-lite is valid", "gemini-3.5-flash-lite", true},
		{"gemini-3.1-pro is valid", "gemini-3.1-pro", true},
		{"gemini-3.1-pro-preview is valid", "gemini-3.1-pro-preview", true},
		{"gemini-3.1-flash-lite is valid", "gemini-3.1-flash-lite", true},

		// 2.5 models
		{"gemini-2.5-flash is valid", "gemini-2.5-flash", true},
		{"gemini-2.5-pro is valid", "gemini-2.5-pro", true},
		{"gemini-2.5-flash-lite is valid", "gemini-2.5-flash-lite", true},

		// Legacy Gemini models (must be rejected)
		{"gemini-1.5-flash is rejected", "gemini-1.5-flash", false},
		{"gemini-1.5-pro is rejected", "gemini-1.5-pro", false},
		{"gemini-2.0-flash is rejected", "gemini-2.0-flash", false},
		{"gemini-2.0-flash-lite is rejected", "gemini-2.0-flash-lite", false},

		// Gemma models
		{"gemma-4-26b is valid", "gemma-4-26b", true},
		{"gemma-4-31b is valid", "gemma-4-31b", true},
		{"gemma-4-4b is valid", "gemma-4-4b", true},
		{"gemma-2-9b is rejected", "gemma-2-9b", false},
		{"gemma-3-12b is rejected", "gemma-3-12b", false},

		// Dynamic & Embeddings
		{"gemini-dynamic is valid", "gemini-dynamic", true},
		{"text-embedding-005 is valid", "text-embedding-005", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsValidModelName(tc.model)
			if got != tc.valid {
				t.Errorf("IsValidModelName(%q) = %v; want %v", tc.model, got, tc.valid)
			}
		})
	}
}

func TestStripLocationSuffix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gemini-2.5-flash@us-central1", "gemini-2.5-flash"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"gemini-3.8-flash@europe-west1", "gemini-3.8-flash"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := StripLocationSuffix(tc.input); got != tc.want {
			t.Errorf("StripLocationSuffix(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestHashKey(t *testing.T) {
	key := "my-secret-api-key"
	hash1 := HashKey(key)
	hash2 := HashKey(key)
	if hash1 != hash2 {
		t.Errorf("HashKey must be deterministic")
	}
	if len(hash1) != 64 {
		t.Errorf("HashKey should produce 64-char hex string, got length %d", len(hash1))
	}
}

func TestGetDefaultLocationForModel(t *testing.T) {
	tests := []struct {
		model    string
		current  string
		expected string
	}{
		{"gemini-3.8-flash", "us-central1", "global"},
		{"gemini-3.8-flash", "us", "us"},
		{"gemini-3.8-flash", "europe-west1", "global"},
		{"gemini-3.5-flash-lite", "us-central1", "global"},
		{"gemini-3.1-pro-preview", "", "global"},
		{"gemini-2.5-flash", "us-central1", "us-central1"},
		{"gemini-2.5-pro", "europe-west1", "europe-west1"},
		{"gemini-2.5-flash", "", "us-central1"},
	}
	for _, tc := range tests {
		got := GetDefaultLocationForModel(tc.model, tc.current)
		if got != tc.expected {
			t.Errorf("GetDefaultLocationForModel(%q, %q) = %q, expected %q", tc.model, tc.current, got, tc.expected)
		}
	}
}

