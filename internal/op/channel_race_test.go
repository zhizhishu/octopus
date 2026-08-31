package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestChannelUpdateAndCreateRaceModeFields(t *testing.T) {
	ctx := setupChannelImportTest(t)

	channel := model.Channel{
		Name:               "race-channel",
		Type:               outbound.OutboundTypeOpenAIChat,
		Enabled:            true,
		RaceMode:           true,
		RaceKeyConcurrency: 3,
		RaceDelayMs:        200,
		BaseUrls: []model.BaseUrl{
			{URL: "https://api.openai.com"},
		},
		Keys: []model.ChannelKey{
			{ChannelKey: "sk-key-1", Enabled: true},
			{ChannelKey: "sk-key-2", Enabled: true},
		},
	}

	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	created, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet failed: %v", err)
	}
	if !created.RaceMode || created.RaceKeyConcurrency != 3 || created.RaceDelayMs != 200 {
		t.Fatalf("unexpected created race fields: mode=%v conc=%d delay=%d", created.RaceMode, created.RaceKeyConcurrency, created.RaceDelayMs)
	}

	// Update race fields
	raceModeFalse := false
	newConc := 4
	newDelay := 500
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:                 channel.ID,
		RaceMode:           &raceModeFalse,
		RaceKeyConcurrency: &newConc,
		RaceDelayMs:        &newDelay,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate failed: %v", err)
	}
	if updated.RaceMode != false || updated.RaceKeyConcurrency != 4 || updated.RaceDelayMs != 500 {
		t.Fatalf("unexpected updated race fields: mode=%v conc=%d delay=%d", updated.RaceMode, updated.RaceKeyConcurrency, updated.RaceDelayMs)
	}
}
