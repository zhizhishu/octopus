package price

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// TestGetLLMPriceZeroDBEntryFallsThroughToCatalog guards the shadow fix: a zero-price
// placeholder LLMInfo row (auto-created when a model is added to a channel) must NOT hide
// the models.dev catalog price, or the model bills at 0 forever. A non-zero DB row (a real
// manual override) still wins.
func TestGetLLMPriceZeroDBEntryFallsThroughToCatalog(t *testing.T) {
	dir := t.TempDir()
	if err := db.InitDB("sqlite", filepath.Join(dir, "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	// Close the DB before t.TempDir's cleanup runs (LIFO), so Windows can unlink the file.
	t.Cleanup(func() {
		if sqlDB, err := db.GetDB().DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	ctx := context.Background()

	llmPriceLock.Lock()
	llmPrice["gpt-shadow-test"] = model.LLMPrice{Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 6.25}
	llmPrice["gpt-override-test"] = model.LLMPrice{Input: 5, Output: 30}
	llmPriceLock.Unlock()
	defer func() {
		llmPriceLock.Lock()
		delete(llmPrice, "gpt-shadow-test")
		delete(llmPrice, "gpt-override-test")
		llmPriceLock.Unlock()
	}()

	// Zero placeholder DB row must fall through to the catalog price.
	if err := op.LLMCreate(model.LLMInfo{Name: "gpt-shadow-test"}, ctx); err != nil {
		t.Fatalf("create zero placeholder: %v", err)
	}
	if got := GetLLMPrice("gpt-shadow-test"); got == nil || got.Output != 30 {
		t.Fatalf("expected catalog price (output 30) to win over the zero placeholder, got %+v", got)
	}

	// A real non-zero DB override still wins over the catalog.
	if err := op.LLMCreate(model.LLMInfo{Name: "gpt-override-test", LLMPrice: model.LLMPrice{Input: 2, Output: 12}}, ctx); err != nil {
		t.Fatalf("create override: %v", err)
	}
	if got := GetLLMPrice("gpt-override-test"); got == nil || got.Output != 12 {
		t.Fatalf("expected DB override (output 12) to win over the catalog, got %+v", got)
	}
}

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

// TestGetLLMPriceCatalogCoversGrokDeepSeekGeminiFamilies locks the 2026 catalog
// refresh: current frontier ids (and common client rewrites) must remain billable,
// and soft-merged legacy names that models.dev dropped must not fall to zero.
func TestGetLLMPriceCatalogCoversGrokDeepSeekGeminiFamilies(t *testing.T) {
	cases := map[string]struct {
		wantInput  float64
		wantOutput float64
	}{
		"grok-4.6":                    {wantInput: 2, wantOutput: 6},
		"grok-4-6":                    {wantInput: 2, wantOutput: 6},
		"x-ai/grok-4.6":               {wantInput: 2, wantOutput: 6},
		"grok-4.5":                    {wantInput: 2, wantOutput: 6},
		"deepseek-v4-flash":           {wantInput: 0.14, wantOutput: 0.28},
		"deepseek-v4-pro":             {wantInput: 0.435, wantOutput: 0.87},
		"deepseek-ai/deepseek-v4-pro": {wantInput: 0.435, wantOutput: 0.87},
		"gemini-3.1-pro-preview":      {wantInput: 2, wantOutput: 12},
		"gemini-3-flash-preview":      {wantInput: 0.5, wantOutput: 3},
		"gemini-3.5-flash":            {wantInput: 1.5, wantOutput: 9},
		// Soft-merge retains still-live legacy names after models.dev drops them.
		"grok-4": {wantInput: 3, wantOutput: 15},
		"grok-3": {wantInput: 3, wantOutput: 15},
	}
	for modelName, want := range cases {
		got := GetLLMPrice(modelName)
		if got == nil {
			t.Fatalf("expected catalog price for %q, got nil", modelName)
		}
		if got.Input != want.wantInput || got.Output != want.wantOutput {
			t.Fatalf("%s price = input %v output %v, want input %v output %v",
				modelName, got.Input, got.Output, want.wantInput, want.wantOutput)
		}
	}
}
