package op

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func setupChannelImportTest(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	ctx := context.Background()
	if err := channelRefreshCache(ctx); err != nil {
		t.Fatalf("refresh channel cache: %v", err)
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		t.Fatalf("refresh access plan cache: %v", err)
	}
	return ctx
}

func TestChannelCSVImportCreatesAndNormalizesRows(t *testing.T) {
	ctx := setupChannelImportTest(t)
	csv := `type,name,baseURL,apiKey,supportedModels,defaultTestModel
openai,OpenAI GPT,https://api.openai.com/v1/chat/completions,sk-openai,gpt-4|gpt-3.5-turbo,gpt-3.5-turbo
anthropic,Anthropic Claude,https://api.anthropic.com/v1/messages,claude-key,claude-3-opus|claude-3-sonnet,claude-3-opus
deepseek,DeepSeek AI,https://api.deepseek.com/v1,sk-deepseek,deepseek-chat|deepseek-coder,deepseek-chat
deepseek_anthropic,DeepSeek Anthropic,https://api.deepseek.com/anthropic/v1/messages,sk-da,deepseek-chat|deepseek-coder,deepseek-coder
`
	result, changed, err := ChannelImportCSV(ctx, []byte(csv), model.ChannelCSVImportOptions{})
	if err != nil {
		t.Fatalf("import csv: %v", err)
	}
	if result.Total != 4 || result.Created != 4 || result.Updated != 0 || result.Failed != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(changed) != 4 {
		t.Fatalf("changed count = %d", len(changed))
	}

	channels, err := ChannelList(ctx)
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	byName := map[string]model.Channel{}
	for _, ch := range channels {
		byName[ch.Name] = ch
	}
	if got := byName["OpenAI GPT"]; got.Type != outbound.OutboundTypeOpenAIChat || got.BaseUrls[0].URL != "https://api.openai.com" || got.Model != "gpt-3.5-turbo,gpt-4" || got.CustomModel != "" {
		t.Fatalf("unexpected openai channel: %+v", got)
	}
	assertChannelModels(t, byName["OpenAI GPT"].SelectedModels, []string{"gpt-3.5-turbo", "gpt-4"})
	assertChannelModels(t, byName["OpenAI GPT"].DiscoveredModels, nil)
	if got := byName["OpenAI GPT"]; got.AutoSync {
		t.Fatalf("imported channels should not auto-sync upstream models the operator did not pick, got auto_sync=%v", got.AutoSync)
	}
	if got := byName["Anthropic Claude"]; got.Type != outbound.OutboundTypeAnthropic || got.BaseUrls[0].URL != "https://api.anthropic.com" {
		t.Fatalf("unexpected anthropic channel: %+v", got)
	}
	if got := byName["DeepSeek Anthropic"]; got.Type != outbound.OutboundTypeAnthropic || got.BaseUrls[0].URL != "https://api.deepseek.com/anthropic" || got.Model != "deepseek-coder,deepseek-chat" {
		t.Fatalf("unexpected deepseek anthropic channel: %+v", got)
	}
	if strings.Contains(strings.ToLower(result.Rows[0].Error), "sk-") {
		t.Fatalf("result must not expose api keys: %+v", result.Rows[0])
	}
}

func TestChannelCSVImportUpsertsAndMergesKeys(t *testing.T) {
	ctx := setupChannelImportTest(t)
	channel := model.Channel{
		Name:        "OpenAI GPT",
		Type:        outbound.OutboundTypeOpenAIChat,
		Enabled:     true,
		BaseUrls:    []model.BaseUrl{{URL: "https://old.example", Delay: 0}},
		Keys:        []model.ChannelKey{{Enabled: true, ChannelKey: "old-key", Remark: "old"}},
		Model:       "old-model",
		CustomModel: "",
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	csv := `type,name,baseURL,apiKey,supportedModels,defaultTestModel
openai,OpenAI GPT,https://api.openai.com/v1,sk-new,gpt-4|gpt-4o,gpt-4o
`
	result, _, err := ChannelImportCSV(ctx, []byte(csv), model.ChannelCSVImportOptions{})
	if err != nil {
		t.Fatalf("import csv: %v", err)
	}
	if result.Updated != 1 || result.Created != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	updated, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("get updated channel: %v", err)
	}
	if updated.Model != "gpt-4o,gpt-4" || updated.CustomModel != "" || len(updated.Keys) != 2 {
		t.Fatalf("unexpected updated channel: %+v", updated)
	}
	assertChannelModels(t, updated.SelectedModels, []string{"gpt-4o", "gpt-4"})
	assertChannelModels(t, updated.DiscoveredModels, nil)
}

func TestChannelCreateDoesNotEnableOneMillionFromDiscoveredOnly(t *testing.T) {
	ctx := setupChannelImportTest(t)

	discoveredOnly := model.Channel{
		Name:             "Discovered 1M",
		Type:             outbound.OutboundTypeAnthropic,
		Enabled:          true,
		DiscoveredModels: []string{"claude-fable-5[1m]"},
	}
	if err := ChannelCreate(&discoveredOnly, ctx); err != nil {
		t.Fatalf("create discovered-only channel: %v", err)
	}
	got, err := ChannelGet(discoveredOnly.ID, ctx)
	if err != nil {
		t.Fatalf("get discovered-only channel: %v", err)
	}
	if got.AnthropicContext1M {
		t.Fatalf("discovered-only model aliases must not auto-enable 1M capability")
	}
	assertChannelModels(t, got.DiscoveredModels, []string{"claude-fable-5"})
	assertChannelModels(t, got.SelectedModels, nil)

	selected := model.Channel{
		Name:    "Selected 1M",
		Type:    outbound.OutboundTypeAnthropic,
		Enabled: true,
		Model:   "claude-fable-5[1m]",
	}
	if err := ChannelCreate(&selected, ctx); err != nil {
		t.Fatalf("create selected channel: %v", err)
	}
	selectedGot, err := ChannelGet(selected.ID, ctx)
	if err != nil {
		t.Fatalf("get selected channel: %v", err)
	}
	if !selectedGot.AnthropicContext1M {
		t.Fatalf("selected legacy 1M alias should enable 1M capability")
	}
	assertChannelModels(t, selectedGot.SelectedModels, []string{"claude-fable-5"})
}

func TestChannelCSVImportValidatesBeforeWriting(t *testing.T) {
	ctx := setupChannelImportTest(t)
	csv := `type,name,baseURL,apiKey,supportedModels,defaultTestModel
openai,Good,https://api.openai.com/v1,sk-good,gpt-4,gpt-4
openai,Bad,https://api.openai.com/v1,sk-bad,gpt-4,gpt-missing
`
	if _, _, err := ChannelImportCSV(ctx, []byte(csv), model.ChannelCSVImportOptions{}); err == nil {
		t.Fatalf("expected validation error")
	}
	channels, err := ChannelList(ctx)
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	if len(channels) != 0 {
		t.Fatalf("validation failure should not write channels, got %+v", channels)
	}
}

func assertChannelModels(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got models %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got models %v, want %v", got, want)
		}
	}
}
