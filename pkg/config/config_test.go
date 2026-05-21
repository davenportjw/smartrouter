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
			{"us-central1", "us-central1-aiplatform.googleapis.com"},
			{"europe-west3", "europe-west3-aiplatform.googleapis.com"},
		}
		for _, tc := range tests {
			if got := GetVertexEndpointHost(tc.input); got != tc.want {
				t.Errorf("GetVertexEndpointHost(%q) = %q; want %q", tc.input, got, tc.want)
			}
		}
	})
}

func TestStripLocationSuffix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gemini-2.5-flash@us-central1", "gemini-2.5-flash"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
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
