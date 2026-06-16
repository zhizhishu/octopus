package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestChannelUpdateResetsRuntimeStatsForEndpointIdentityChanges(t *testing.T) {
	ctx := setupChannelImportTest(t)
	if err := statsRefreshCache(ctx); err != nil {
		t.Fatalf("refresh stats cache: %v", err)
	}

	channel := model.Channel{
		Name:     "identity-reset",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: "https://old.example", Delay: 0}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "sk-old", Remark: "old"}},
		Model:    "gpt-4o",
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := channelRefreshCacheByID(channel.ID, ctx); err != nil {
		t.Fatalf("refresh channel cache: %v", err)
	}
	current, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if len(current.Keys) != 1 || current.Keys[0].ID == 0 {
		t.Fatalf("expected persisted key, got %+v", current.Keys)
	}
	key := current.Keys[0]

	if err := StatsChannelUpdate(channel.ID, model.StatsMetrics{
		RequestSuccess: 7,
		RequestFailed:  2,
		InputToken:     100,
		OutputToken:    50,
		InputCost:      0.1,
		OutputCost:     0.2,
	}); err != nil {
		t.Fatalf("seed channel stats: %v", err)
	}
	if err := StatsSaveDB(ctx); err != nil {
		t.Fatalf("save channel stats: %v", err)
	}
	if err := ChannelKeyRecordUse(key, 429, 1718000000, 1.25); err != nil {
		t.Fatalf("record key use: %v", err)
	}
	if err := ChannelKeySaveDB(ctx); err != nil {
		t.Fatalf("save key use: %v", err)
	}

	nextBaseUrls := []model.BaseUrl{{URL: "https://new.example", Delay: 0}}
	nextKey := "sk-new"
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:       channel.ID,
		BaseUrls: &nextBaseUrls,
		KeysToUpdate: []model.ChannelKeyUpdateRequest{{
			ID:         key.ID,
			ChannelKey: &nextKey,
		}},
	}, ctx)
	if err != nil {
		t.Fatalf("update identity: %v", err)
	}

	if got := StatsChannelGet(channel.ID); got.RequestSuccess != 0 || got.RequestFailed != 0 || got.InputToken != 0 || got.OutputCost != 0 {
		t.Fatalf("expected channel stats reset after endpoint identity change, got %+v", got)
	}
	var statsRows int64
	if err := db.GetDB().WithContext(ctx).Model(&model.StatsChannel{}).Where("channel_id = ?", channel.ID).Count(&statsRows).Error; err != nil {
		t.Fatalf("count channel stats: %v", err)
	}
	if statsRows != 0 {
		t.Fatalf("expected channel stats row deleted, got %d", statsRows)
	}
	if len(updated.Keys) != 1 {
		t.Fatalf("expected one key after update, got %+v", updated.Keys)
	}
	updatedKey := updated.Keys[0]
	if updatedKey.ChannelKey != nextKey || updatedKey.StatusCode != 0 || updatedKey.LastUseTimeStamp != 0 || updatedKey.TotalCost != 0 {
		t.Fatalf("expected key runtime fields reset, got %+v", updatedKey)
	}

	if err := StatsChannelUpdate(channel.ID, model.StatsMetrics{RequestSuccess: 3}); err != nil {
		t.Fatalf("seed stats after reset: %v", err)
	}
	nextModel := "gpt-4.1"
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:    channel.ID,
		Model: &nextModel,
	}, ctx); err != nil {
		t.Fatalf("update model only: %v", err)
	}
	if got := StatsChannelGet(channel.ID); got.RequestSuccess != 3 {
		t.Fatalf("model-only update should not reset runtime stats, got %+v", got)
	}
}
