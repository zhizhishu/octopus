package price

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// TestPricedMatchesBillingResolution pins the 未定价 marker semantics (next-1129):
// priced must answer "would billing charge anything" — a non-zero DB row is priced,
// a zero placeholder row falls through to the catalog, a row with only some rates
// set (partial pricing) still counts as priced, and a model with no price anywhere
// is unpriced.
func TestPricedMatchesBillingResolution(t *testing.T) {
	dir := t.TempDir()
	if err := db.InitDB("sqlite", filepath.Join(dir, "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.GetDB().DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	ctx := context.Background()

	llmPriceLock.Lock()
	llmPrice["priced-catalog-only"] = model.LLMPrice{Input: 5, Output: 30}
	llmPriceLock.Unlock()
	defer func() {
		llmPriceLock.Lock()
		delete(llmPrice, "priced-catalog-only")
		llmPriceLock.Unlock()
	}()

	// 有价：non-zero DB row.
	if err := op.LLMCreate(model.LLMInfo{Name: "priced-full", LLMPrice: model.LLMPrice{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.2}}, ctx); err != nil {
		t.Fatalf("create priced-full: %v", err)
	}
	if !Priced("priced-full") {
		t.Errorf("priced-full must be priced")
	}

	// 部分有价：only the input rate set still counts as priced.
	if err := op.LLMCreate(model.LLMInfo{Name: "priced-partial", LLMPrice: model.LLMPrice{Input: 1}}, ctx); err != nil {
		t.Fatalf("create priced-partial: %v", err)
	}
	if !Priced("priced-partial") {
		t.Errorf("priced-partial (input only) must be priced")
	}

	// 无价：zero row, no catalog entry anywhere.
	if err := op.LLMCreate(model.LLMInfo{Name: "priced-none-anywhere"}, ctx); err != nil {
		t.Fatalf("create priced-none-anywhere: %v", err)
	}
	if Priced("priced-none-anywhere") {
		t.Errorf("priced-none-anywhere must be unpriced")
	}

	// 无价行回落价库：zero placeholder row but the catalog prices it -> priced.
	if err := op.LLMCreate(model.LLMInfo{Name: "priced-catalog-only"}, ctx); err != nil {
		t.Fatalf("create priced-catalog-only row: %v", err)
	}
	if !Priced("priced-catalog-only") {
		t.Errorf("zero placeholder with a catalog price must be priced (billing falls through)")
	}
}
