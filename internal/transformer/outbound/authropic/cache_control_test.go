package authropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestAutoCacheControlAddsSystemBreakpoint(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: stringPtr(strings.Repeat("stable system prompt ", 260))}},
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hello")}},
		},
		TransformOptions: model.TransformOptions{AnthropicAutoCacheControl: true},
	}

	converted := convertToAnthropicRequest(req)

	// Claude CLI billing-header + agent-identity blocks are injected as the first two
	// system parts, so the real system prompt is the last part and carries the cache
	// control.
	if converted.System == nil || len(converted.System.MultiplePrompts) != 3 {
		t.Fatalf("expected billing header + agent identity + one system prompt part, got %#v", converted.System)
	}
	cc := converted.System.MultiplePrompts[len(converted.System.MultiplePrompts)-1].CacheControl
	if cc == nil || cc.Type != "ephemeral" || cc.TTL != "" {
		t.Fatalf("expected default ephemeral cache control, got %#v", cc)
	}
	// Automatic caching now also places a sliding breakpoint on the last user message
	// (leapfrog caching), so a system prefix breakpoint plus the last-user breakpoint = 2.
	if countCacheControls(converted) != 2 {
		t.Fatalf("expected system prefix + last-user sliding breakpoint (2), got %d", countCacheControls(converted))
	}
}

func TestAutoCacheControlPreservesExplicitBreakpoints(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{
				Role: "system",
				Content: model.MessageContent{
					Content: stringPtr(strings.Repeat("stable system prompt ", 260)),
				},
				CacheControl: &model.CacheControl{Type: "ephemeral", TTL: "1h"},
			},
			{Role: "user", Content: model.MessageContent{Content: stringPtr(strings.Repeat("long user context ", 260))}},
		},
		TransformOptions: model.TransformOptions{AnthropicAutoCacheControl: true},
	}

	converted := convertToAnthropicRequest(req)

	if countCacheControls(converted) != 1 {
		t.Fatalf("expected auto injection to skip when explicit cache control exists, got %d blocks", countCacheControls(converted))
	}
	cc := converted.System.MultiplePrompts[len(converted.System.MultiplePrompts)-1].CacheControl
	if cc == nil || cc.TTL != "1h" {
		t.Fatalf("expected explicit ttl to be preserved, got %#v", cc)
	}
}

func TestAutoCacheControlSkipsShortPrompts(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: stringPtr("short")}},
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hello")}},
		},
		TransformOptions: model.TransformOptions{AnthropicAutoCacheControl: true},
	}

	converted := convertToAnthropicRequest(req)

	if countCacheControls(converted) != 0 {
		t.Fatalf("expected no cache control for short prompt, got %d blocks", countCacheControls(converted))
	}
}

func TestAutoCacheControlMarksFirstLongUserWhenNoStablePrefix(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: stringPtr(strings.Repeat("reference document ", 260))}},
			{Role: "assistant", Content: model.MessageContent{Content: stringPtr("noted")}},
			{Role: "user", Content: model.MessageContent{Content: stringPtr("answer the next question")}},
		},
		TransformOptions: model.TransformOptions{AnthropicAutoCacheControl: true},
	}

	converted := convertToAnthropicRequest(req)

	// The first long user message takes the prefix breakpoint; the last user message gets
	// a sliding (leapfrog) breakpoint. With only 3 messages there is no second sliding
	// breakpoint, so the total is 2.
	if countCacheControls(converted) != 2 {
		t.Fatalf("expected first-user prefix + last-user sliding breakpoint (2), got %d", countCacheControls(converted))
	}
	if len(converted.Messages[0].Content.MultipleContent) != 1 {
		t.Fatalf("expected first user string content to be converted to a text block")
	}
	cc := converted.Messages[0].Content.MultipleContent[0].CacheControl
	if cc == nil || cc.Type != "ephemeral" {
		t.Fatalf("expected first long user content to be cacheable, got %#v", cc)
	}
}

func TestAutoCacheControlDisabledByDefault(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: stringPtr(strings.Repeat("stable system prompt ", 260))}},
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hello")}},
		},
	}

	converted := convertToAnthropicRequest(req)

	if countCacheControls(converted) != 0 {
		body, _ := json.Marshal(converted)
		t.Fatalf("expected no cache control without transform option, got %s", body)
	}
}

func stringPtr(value string) *string {
	return &value
}
