package behavior

import (
	"reflect"
	"testing"
)

func TestCatalogFamiliesKeepLegacyClassifications(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want []string
	}{
		{"claude-opus-4-6", []string{"anthropic"}},
		{"gpt-5.3-codex", []string{"openai"}},
		{"gemini-3.1-pro-preview", []string{"google"}},
		{"gemini-4-argon", []string{"google"}},
		{"gemini-4-carbon-high-20261110", []string{"google"}},
		{"gemini-4-hydrogen-xhigh-20261112", []string{"google"}},
		{"gpt-image-2.5", []string{"openai"}},
		{"gpt-image-2.6", []string{"openai"}},
		{"doubao-seedream-5-5-261028", []string{"bytedance"}},
		{"imagen-4.0-generate-001", []string{"google"}},
		{"anthropic/gemini-4-argon", []string{"anthropic", "google"}},
		{"I am Claude or Gemini", []string{"anthropic", "google"}},
		{"option1", nil},
		{"taxation", nil},
		{"my-custom-alias", nil},
	} {
		if got := InferFamilies(tc.id); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.id, got, tc.want)
		}
	}
}
