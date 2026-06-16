package op

import (
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestModelHourlyHealthAggregatesLogExperience(t *testing.T) {
	ctx := setupRelayLogTest(t)

	channelCache.Set(101, model.Channel{ID: 101, Name: "anthropic-main", Type: outbound.OutboundTypeAnthropic})
	channelCache.Set(102, model.Channel{ID: 102, Name: "gemini-main", Type: outbound.OutboundTypeGemini})

	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	logs := []model.RelayLog{
		{
			ID:               3001,
			Time:             start.Add(8 * time.Hour).Unix(),
			RequestModelName: "claude-sonnet",
			ChannelId:        101,
			ActualModelName:  "claude-sonnet",
			InputTokens:      100,
			OutputTokens:     40,
			CacheHitTokens:   90,
			CacheInputTokens: 100,
			Ftut:             500,
			UseTime:          1500,
		},
		{
			ID:               3002,
			Time:             start.Add(8*time.Hour + 10*time.Minute).Unix(),
			RequestModelName: "claude-sonnet",
			ChannelId:        101,
			ActualModelName:  "claude-sonnet",
			InputTokens:      100,
			OutputTokens:     60,
			CacheHitTokens:   10,
			CacheInputTokens: 100,
			Ftut:             900,
			UseTime:          1900,
		},
		{
			ID:               3003,
			Time:             start.Add(8*time.Hour + 20*time.Minute).Unix(),
			RequestModelName: "claude-sonnet",
			ChannelId:        101,
			ActualModelName:  "claude-sonnet",
			Error:            "upstream failed",
			UseTime:          200,
		},
		{
			ID:               3005,
			Time:             start.Add(8*time.Hour + 30*time.Minute).Unix(),
			RequestModelName: "claude-sonnet",
			ChannelId:        0,
			ActualModelName:  "claude-sonnet",
			Error:            "empty request: messages or input is required",
			ErrorCode:        model.RelayLogErrorCodeClientEmptyRequest,
			ErrorStatus:      400,
			ErrorStrategy:    model.RelayLogErrorStrategyLocalValidation,
		},
		{
			ID:               3006,
			Time:             start.Add(8*time.Hour + 40*time.Minute).Unix(),
			RequestModelName: "claude-sonnet",
			ChannelId:        101,
			ActualModelName:  "claude-sonnet",
			Error:            "channel Claude-CPA failed: context canceled",
			ErrorCode:        "octopus_client_canceled",
			ErrorStatus:      499,
			ErrorStrategy:    "client_canceled;upstream_forwarded=true;breaker_counted=false",
		},
		{
			ID:               3007,
			Time:             start.Add(8*time.Hour + 50*time.Minute).Unix(),
			RequestModelName: "claude-sonnet",
			ChannelId:        101,
			ActualModelName:  "claude-sonnet",
			Error:            "no available channel: Claude-CPA (circuit breaker tripped)",
			ErrorCode:        "octopus_channel_circuit_open",
			ErrorStatus:      503,
			ErrorStrategy:    "local_route_selection;reason=circuit_break;upstream_forwarded=false",
		},
		{
			ID:               3004,
			Time:             start.Add(9 * time.Hour).Unix(),
			RequestModelName: "gemini-2.5-pro",
			ChannelId:        102,
			ActualModelName:  "gemini-2.5-pro",
			InputTokens:      50,
			OutputTokens:     20,
			CacheHitTokens:   25,
			CacheInputTokens: 50,
			Ftut:             400,
			UseTime:          1400,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	health, err := ModelHourlyHealth(ctx)
	if err != nil {
		t.Fatalf("model health: %v", err)
	}

	anthropic := findHealthProvider(t, health, "Anthropic")
	if len(anthropic.Models) != 1 {
		t.Fatalf("expected one anthropic model, got %d", len(anthropic.Models))
	}
	claude := anthropic.Models[0]
	if claude.Model != "claude-sonnet" {
		t.Fatalf("expected claude model, got %s", claude.Model)
	}
	if claude.Summary.RequestSuccess != 2 || claude.Summary.RequestFailed != 1 {
		t.Fatalf("unexpected summary counts: %#v", claude.Summary)
	}
	if claude.Summary.FirstTokenP90Ms != 900 {
		t.Fatalf("expected p90 900ms, got %d", claude.Summary.FirstTokenP90Ms)
	}
	if claude.Summary.AvgThroughput != 50 {
		t.Fatalf("expected throughput 50 tok/s, got %f", claude.Summary.AvgThroughput)
	}
	if claude.Summary.CacheHitRate != 0.5 {
		t.Fatalf("expected cache rate 0.5, got %f", claude.Summary.CacheHitRate)
	}
	if len(claude.Hours) != 24 {
		t.Fatalf("expected 24 hourly buckets, got %d", len(claude.Hours))
	}
	if claude.Hours[8].RequestSuccess != 2 || claude.Hours[8].RequestFailed != 1 {
		t.Fatalf("unexpected hour 8 counts: %#v", claude.Hours[8])
	}

	gemini := findHealthProvider(t, health, "Gemini")
	if len(gemini.Models) != 1 || gemini.Models[0].Model != "gemini-2.5-pro" {
		t.Fatalf("expected gemini model, got %#v", gemini.Models)
	}
}

func findHealthProvider(t *testing.T, health model.ModelHealthResponse, provider string) model.ModelHealthProvider {
	t.Helper()
	for _, item := range health.Providers {
		if item.Provider == provider {
			return item
		}
	}
	t.Fatalf("provider %s not found in %#v", provider, health.Providers)
	return model.ModelHealthProvider{}
}
