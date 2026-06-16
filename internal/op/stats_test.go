package op

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupStatsTest(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})

	return context.Background()
}

func TestStatsRefreshCacheLoadsModelSnapshots(t *testing.T) {
	ctx := setupStatsTest(t)

	existing := model.StatsModel{
		ID:        42,
		Name:      "gpt-4.1",
		ChannelID: 7,
		StatsMetrics: model.StatsMetrics{
			InputToken:     10,
			OutputToken:    20,
			RequestSuccess: 2,
			CacheHitToken:  5,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&existing).Error; err != nil {
		t.Fatalf("create existing model stats: %v", err)
	}
	if err := statsRefreshCache(ctx); err != nil {
		t.Fatalf("refresh stats cache: %v", err)
	}

	if err := StatsModelUpdate(model.StatsModel{
		ID: 42,
		StatsMetrics: model.StatsMetrics{
			InputToken:     3,
			OutputToken:    4,
			RequestSuccess: 1,
			CacheHitToken:  2,
		},
	}); err != nil {
		t.Fatalf("update model stats: %v", err)
	}
	if err := StatsSaveDB(ctx); err != nil {
		t.Fatalf("save stats: %v", err)
	}

	var got model.StatsModel
	if err := db.GetDB().WithContext(ctx).First(&got, 42).Error; err != nil {
		t.Fatalf("load saved model stats: %v", err)
	}
	if got.Name != "gpt-4.1" || got.ChannelID != 7 {
		t.Fatalf("expected model identity preserved, got name=%q channel=%d", got.Name, got.ChannelID)
	}
	if got.InputToken != 13 || got.OutputToken != 24 || got.RequestSuccess != 3 || got.CacheHitToken != 7 {
		t.Fatalf("expected metrics to accumulate on refreshed snapshot, got %#v", got.StatsMetrics)
	}
}

func TestStatsSkipZeroDimensionsAndReadGettersDoNotDirty(t *testing.T) {
	ctx := setupStatsTest(t)
	if err := statsRefreshCache(ctx); err != nil {
		t.Fatalf("refresh stats cache: %v", err)
	}

	if err := StatsAPIKeyUpdate(0, model.StatsMetrics{RequestSuccess: 1}); err != nil {
		t.Fatalf("update zero api key stats: %v", err)
	}
	if err := StatsChannelUpdate(0, model.StatsMetrics{RequestSuccess: 1}); err != nil {
		t.Fatalf("update zero channel stats: %v", err)
	}
	if got := StatsAPIKeyGet(123); got.APIKeyID != 123 || got.RequestSuccess != 0 {
		t.Fatalf("expected read-only empty api key stats, got %#v", got)
	}
	if got := StatsChannelGet(456); got.ChannelID != 456 || got.RequestSuccess != 0 {
		t.Fatalf("expected read-only empty channel stats, got %#v", got)
	}
	if len(statsAPIKeyCacheNeedUpdate) != 0 || len(statsChannelCacheNeedUpdate) != 0 {
		t.Fatalf("read/update zero should not dirty stats: api=%v channel=%v", statsAPIKeyCacheNeedUpdate, statsChannelCacheNeedUpdate)
	}
	if err := StatsSaveDB(ctx); err != nil {
		t.Fatalf("save stats: %v", err)
	}
	var apiKeyRows int64
	if err := db.GetDB().WithContext(ctx).Model(&model.StatsAPIKey{}).Count(&apiKeyRows).Error; err != nil {
		t.Fatalf("count api key stats: %v", err)
	}
	var channelRows int64
	if err := db.GetDB().WithContext(ctx).Model(&model.StatsChannel{}).Count(&channelRows).Error; err != nil {
		t.Fatalf("count channel stats: %v", err)
	}
	if apiKeyRows != 0 || channelRows != 0 {
		t.Fatalf("expected no zero/read-only dimension rows, api=%d channel=%d", apiKeyRows, channelRows)
	}
}
