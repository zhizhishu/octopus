package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func TestApplyPromptOverridesAppendsSourcesInOrder(t *testing.T) {
	user := "ping"
	req := &transformerModel.InternalLLMRequest{
		Model: "request-model",
		Messages: []transformerModel.Message{
			{Role: "user", Content: transformerModel.MessageContent{Content: &user}},
		},
	}
	plan := &dbmodel.AccessPlan{Slug: "vip", SystemPromptOverride: "plan prompt"}
	rule := &dbmodel.AccessRouteRule{RequestModel: "request-model", SystemPromptOverride: "route prompt"}
	channel := &dbmodel.Channel{Name: "channel-a", SystemPromptOverride: "channel prompt"}

	snapshot := applyPromptOverrides(req, plan, rule, channel)

	if snapshot.Mode != dbmodel.PromptOverrideModeAppendSystem {
		t.Fatalf("expected append mode, got %q", snapshot.Mode)
	}
	wantSources := []string{"access_plan:vip", "route_rule:request-model", "channel:channel-a"}
	if len(snapshot.Sources) != len(wantSources) {
		t.Fatalf("unexpected sources: %#v", snapshot.Sources)
	}
	for i := range wantSources {
		if snapshot.Sources[i] != wantSources[i] {
			t.Fatalf("source %d: got %q want %q", i, snapshot.Sources[i], wantSources[i])
		}
	}
	if len(req.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(req.Messages))
	}
	if got := *req.Messages[0].Content.Content; got != "channel prompt" {
		t.Fatalf("expected latest override at front, got %q", got)
	}
	if got := *req.Messages[1].Content.Content; got != "route prompt" {
		t.Fatalf("expected route override second, got %q", got)
	}
	if got := *req.Messages[2].Content.Content; got != "plan prompt" {
		t.Fatalf("expected plan override third, got %q", got)
	}
}

func TestApplyPromptOverridesCanReplaceExistingSystem(t *testing.T) {
	system := "original"
	user := "ping"
	req := &transformerModel.InternalLLMRequest{
		Model: "request-model",
		Messages: []transformerModel.Message{
			{Role: "system", Content: transformerModel.MessageContent{Content: &system}},
			{Role: "user", Content: transformerModel.MessageContent{Content: &user}},
		},
	}
	rule := &dbmodel.AccessRouteRule{
		RequestModel:         "request-model",
		SystemPromptOverride: "replacement",
		PromptOverrideMode:   dbmodel.PromptOverrideModeReplaceSystem,
	}

	snapshot := applyPromptOverrides(req, nil, rule, nil)

	if snapshot.Mode != dbmodel.PromptOverrideModeReplaceSystem {
		t.Fatalf("expected replace mode, got %q", snapshot.Mode)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected replacement system plus user, got %d messages", len(req.Messages))
	}
	if got := *req.Messages[0].Content.Content; got != "replacement" {
		t.Fatalf("expected replacement system prompt, got %q", got)
	}
	if req.Messages[1].Role != "user" {
		t.Fatalf("expected user message after replacement, got %#v", req.Messages[1])
	}
}
