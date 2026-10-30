package task

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/helper"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func setupPriceKeepDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	return context.Background()
}

func priceRowExists(t *testing.T, name string) bool {
	t.Helper()
	var n int64
	if err := db.GetDB().Model(&model.LLMInfo{}).Where("name = ?", name).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", name, err)
	}
	_, cacheErr := op.LLMGet(name)
	if (n == 1) != (cacheErr == nil) {
		t.Fatalf("%s: db count=%d but cache err=%v", name, n, cacheErr)
	}
	return n == 1
}

// Regression: the model sync used to delete, on every start, the no-price rows of
// models that only lived on channels without auto-sync. Like new-api and sub2api,
// the sync now never deletes a price row; only an explicit admin delete does.
func TestSyncModelsKeepsPriceRowsOfNonAutoSyncChannels(t *testing.T) {
	ctx := setupPriceKeepDB(t)
	if err := op.ChannelCreate(&model.Channel{
		Name:           "manual-channel",
		Enabled:        true,
		AutoSync:       false,
		SelectedModels: []string{"manual-only-model"},
	}, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	// The placeholder a channel save creates: no price.
	if err := helper.LLMPriceAddToDB([]string{"manual-only-model"}, ctx); err != nil {
		t.Fatalf("add price row: %v", err)
	}
	for i := 0; i < 3; i++ { // startup run, a scheduled run, the admin's 同步 button
		SyncModelsTask()
	}
	if !priceRowExists(t, "manual-only-model") {
		t.Fatalf("sync deleted the price row of a non-auto-sync channel model")
	}
}

// No row is ever removed by the sync, however orphaned or empty it is.
func TestSyncModelsNeverDeletesPriceRows(t *testing.T) {
	ctx := setupPriceKeepDB(t)
	if err := op.ChannelCreate(&model.Channel{Name: "disabled-channel", Enabled: false, Model: "disabled-channel-model"}, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := helper.LLMPriceAddToDB([]string{"orphan-placeholder", "disabled-channel-model"}, ctx); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := op.LLMBatchCreate([]model.LLMInfo{
		{Name: "admin-empty", Source: model.LLMSourceLibrary},
		{Name: "legacy-empty"},
		{Name: "video-only", LLMPrice: model.LLMPrice{PerCall: 0.3}},
		{Name: "manual-priced", LLMPrice: model.LLMPrice{Input: 1, Output: 2}, Source: model.LLMSourceManual},
	}, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 3; i++ {
		SyncModelsTask()
	}
	for _, name := range []string{"orphan-placeholder", "disabled-channel-model", "admin-empty", "legacy-empty", "video-only", "manual-priced"} {
		if !priceRowExists(t, name) {
			t.Fatalf("sync deleted price row %s", name)
		}
	}
	if p, _ := op.LLMGet("video-only"); p.PerCall != 0.3 {
		t.Fatalf("per-call price changed: %+v", p)
	}
	// Explicit admin delete still works.
	if err := op.LLMDelete("orphan-placeholder", ctx); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
	if priceRowExists(t, "orphan-placeholder") {
		t.Fatalf("admin delete did not remove the row")
	}
}

// Placeholder behaviour is kept: every channel's models (auto-sync or not) get a
// no-price row when missing.
func TestSyncModelsAddsPlaceholdersForAllChannels(t *testing.T) {
	ctx := setupPriceKeepDB(t)
	if err := op.ChannelCreate(&model.Channel{Name: "manual", Enabled: true, AutoSync: false, SelectedModels: []string{"New-Model"}}, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if priceRowExists(t, "new-model") {
		t.Fatalf("precondition: row should not exist yet")
	}
	SyncModelsTask()
	if !priceRowExists(t, "new-model") {
		t.Fatalf("sync did not add a placeholder row for a non-auto-sync channel model")
	}
}
