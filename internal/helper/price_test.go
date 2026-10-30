package helper

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// A row priced only per call (video generation) is a real price, not an empty
// placeholder; the sync cleanup must not delete it.
func TestLLMPriceDeleteFromDBWithNoPriceKeepsPerCallOnlyRow(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	ctx := context.Background()
	if err := op.LLMBatchCreate([]model.LLMInfo{
		{Name: "video-only", LLMPrice: model.LLMPrice{PerCall: 0.3}},
		{Name: "empty-row"},
	}, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := LLMPriceDeleteFromDBWithNoPrice([]string{"video-only", "empty-row"}, ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if p, err := op.LLMGet("video-only"); err != nil || p.PerCall != 0.3 {
		t.Fatalf("per-call-only row must survive: price=%+v err=%v", p, err)
	}
	var n int64
	db.GetDB().Model(&model.LLMInfo{}).Where("name = ?", "video-only").Count(&n)
	if n != 1 {
		t.Fatalf("per-call-only row missing from db (count=%d)", n)
	}
	if _, err := op.LLMGet("empty-row"); err == nil {
		t.Fatalf("empty placeholder row should still be deleted")
	}
}
