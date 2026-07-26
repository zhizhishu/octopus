package price

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// TestGetLLMPriceVendorPrefixFallback verifies billing is not silently zeroed when
// the upstream reports a vendor-prefixed model name: an exact miss retries with the
// last "/" or ":" segment. A truly unknown model still returns nil.
func TestGetLLMPriceVendorPrefixFallback(t *testing.T) {
	llmPriceLock.Lock()
	llmPrice["gpt-x-test"] = model.LLMPrice{Input: 1, Output: 2}
	llmPriceLock.Unlock()
	defer func() {
		llmPriceLock.Lock()
		delete(llmPrice, "gpt-x-test")
		llmPriceLock.Unlock()
	}()

	cases := []struct {
		name    string
		wantHit bool
	}{
		{"gpt-x-test", true},        // exact
		{"GPT-X-Test", true},        // case-insensitive exact
		{"openai/gpt-x-test", true}, // vendor "/" prefix -> last segment
		{"vendor:gpt-x-test", true}, // vendor ":" prefix -> last segment
		{"totally-unknown-model", false},
		{"openai/totally-unknown", false},
	}
	for _, c := range cases {
		got := GetLLMPrice(c.name)
		if c.wantHit && got == nil {
			t.Errorf("GetLLMPrice(%q) = nil, want a price (fallback should hit)", c.name)
		}
		if !c.wantHit && got != nil {
			t.Errorf("GetLLMPrice(%q) = %+v, want nil", c.name, *got)
		}
	}
}
