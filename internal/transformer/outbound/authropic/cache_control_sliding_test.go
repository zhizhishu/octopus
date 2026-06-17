package authropic

import (
	"strings"
	"testing"

	anthropicModel "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

// longText returns a string guaranteed to exceed the (non-haiku) min-chars gate of 4096.
func longText(tag string) string {
	return strings.Repeat(tag+" ", 2200)
}

// userMsg / assistantMsg build single-string-content messages for the outbound model.
func userMsg(text string) anthropicModel.MessageParam {
	return anthropicModel.MessageParam{Role: "user", Content: anthropicModel.MessageContent{Content: &text}}
}

func assistantMsg(text string) anthropicModel.MessageParam {
	return anthropicModel.MessageParam{Role: "assistant", Content: anthropicModel.MessageContent{Content: &text}}
}

// msgHasCacheControl reports whether the message at index i carries a cache_control marker
// on any of its content blocks.
func msgHasCacheControl(req *anthropicModel.MessageRequest, i int) bool {
	if i < 0 || i >= len(req.Messages) {
		return false
	}
	for _, block := range messageContentBlocks(req.Messages[i].Content) {
		if block.CacheControl != nil {
			return true
		}
	}
	return false
}

// Scenario ①: a multi-turn request (>=4 messages) with a long stable prefix and no
// client-supplied cache_control should end up with three breakpoints — the prefix
// (system here), the last user message, and the second-to-last user message — and the
// total must stay within Anthropic's 4-breakpoint ceiling.
func TestSlidingCacheControlMarksPrefixAndLastTwoUsers(t *testing.T) {
	req := &anthropicModel.MessageRequest{
		Model: "claude-sonnet-4-5",
		System: &anthropicModel.SystemPrompt{
			MultiplePrompts: []anthropicModel.SystemPromptPart{
				{Type: "text", Text: longText("stable system prompt")},
			},
		},
		Messages: []anthropicModel.MessageParam{
			userMsg("first turn question"),       // 0
			assistantMsg("first turn answer"),    // 1
			userMsg("second turn follow-up"),     // 2
			assistantMsg("second turn answer"),   // 3
			userMsg("third turn final question"), // 4 — last user
		},
	}

	applyAutomaticCacheControl(req)

	// Prefix breakpoint should be on the system prompt.
	if req.System.MultiplePrompts[len(req.System.MultiplePrompts)-1].CacheControl == nil {
		t.Fatalf("expected system prefix breakpoint")
	}
	// Last user (index 4) and second-to-last user (index 2) should be marked.
	if !msgHasCacheControl(req, 4) {
		t.Fatalf("expected last user message (idx 4) to be marked")
	}
	if !msgHasCacheControl(req, 2) {
		t.Fatalf("expected second-to-last user message (idx 2) to be marked")
	}
	// First user (idx 0) should NOT be marked — system claimed the prefix breakpoint.
	if msgHasCacheControl(req, 0) {
		t.Fatalf("did not expect first user message (idx 0) to be marked")
	}

	total := countCacheControls(req)
	if total != 3 {
		t.Fatalf("expected exactly 3 breakpoints (prefix + 2 sliding), got %d", total)
	}
	if total > 4 {
		t.Fatalf("exceeded Anthropic's 4-breakpoint limit: %d", total)
	}
}

// When there is no stable prefix (no system, no tools) the first long user message takes
// the prefix breakpoint. The sliding breakpoints must not re-mark that same message, and
// the second-to-last user breakpoint must still land on a different user message.
func TestSlidingCacheControlAvoidsDoubleMarkingPrefixUser(t *testing.T) {
	req := &anthropicModel.MessageRequest{
		Model: "claude-sonnet-4-5",
		Messages: []anthropicModel.MessageParam{
			userMsg(longText("reference document")), // 0 — long, becomes prefix breakpoint
			assistantMsg("noted"),                   // 1
			userMsg("second user turn"),             // 2
			assistantMsg("ok"),                      // 3
			userMsg("third user turn"),              // 4 — last user
		},
	}

	applyAutomaticCacheControl(req)

	// idx 0 is the prefix breakpoint, idx 4 is the last-user sliding breakpoint,
	// idx 2 is the second-to-last-user sliding breakpoint. Exactly 3 distinct blocks.
	if !msgHasCacheControl(req, 0) {
		t.Fatalf("expected first long user (idx 0) to be the prefix breakpoint")
	}
	if !msgHasCacheControl(req, 4) {
		t.Fatalf("expected last user (idx 4) sliding breakpoint")
	}
	if !msgHasCacheControl(req, 2) {
		t.Fatalf("expected second-to-last user (idx 2) sliding breakpoint")
	}

	// Each marked user message should carry exactly one cache_control (no duplicate
	// marking within a single block).
	if got := countCacheControls(req); got != 3 {
		t.Fatalf("expected 3 breakpoints with no double-marking, got %d", got)
	}
}

// When the last user message IS the prefix breakpoint (single short conversation with no
// stable prefix), the sliding logic must skip it rather than mark it twice.
func TestSlidingCacheControlSkipsWhenLastUserIsPrefix(t *testing.T) {
	req := &anthropicModel.MessageRequest{
		Model: "claude-sonnet-4-5",
		Messages: []anthropicModel.MessageParam{
			userMsg(longText("single long user prompt")), // 0 — also the last user
		},
	}

	applyAutomaticCacheControl(req)

	if got := countCacheControls(req); got != 1 {
		t.Fatalf("expected a single breakpoint for a one-message request, got %d", got)
	}
	if !msgHasCacheControl(req, 0) {
		t.Fatalf("expected the only user message to be marked once")
	}
}

// Scenario ③ (a): a short single-turn request below 4 messages should get the prefix
// breakpoint plus only one sliding breakpoint (the last user), never the second.
func TestSlidingCacheControlShortConversationOnlyOneSliding(t *testing.T) {
	req := &anthropicModel.MessageRequest{
		Model: "claude-sonnet-4-5",
		System: &anthropicModel.SystemPrompt{
			MultiplePrompts: []anthropicModel.SystemPromptPart{
				{Type: "text", Text: longText("stable system prompt")},
			},
		},
		Messages: []anthropicModel.MessageParam{
			userMsg("hello"),         // 0 — last user
			assistantMsg("hi there"), // 1
		},
	}

	applyAutomaticCacheControl(req)

	// system prefix + last user (idx 0). Only 2 messages -> no second sliding breakpoint.
	if req.System.MultiplePrompts[len(req.System.MultiplePrompts)-1].CacheControl == nil {
		t.Fatalf("expected system prefix breakpoint")
	}
	if !msgHasCacheControl(req, 0) {
		t.Fatalf("expected last user (idx 0) sliding breakpoint")
	}
	if got := countCacheControls(req); got != 2 {
		t.Fatalf("expected exactly 2 breakpoints for short conversation, got %d", got)
	}
}

// Scenario ③ (b): a short single-turn request with no system/tools — the lone long user
// message is both the prefix and the last user; result is a single breakpoint.
func TestSlidingCacheControlShortConversationNoPrefix(t *testing.T) {
	req := &anthropicModel.MessageRequest{
		Model: "claude-sonnet-4-5",
		Messages: []anthropicModel.MessageParam{
			userMsg(longText("a single self-contained question")), // 0
		},
	}

	applyAutomaticCacheControl(req)

	if got := countCacheControls(req); got != 1 {
		t.Fatalf("expected exactly 1 breakpoint, got %d", got)
	}
}

// Scenario ②: a request that already carries a client-supplied cache_control must be left
// completely untouched — no sliding breakpoints added. Driven through the full conversion
// path to match real usage.
func TestSlidingCacheControlLeavesClientSuppliedRequestUntouched(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: stringPtr(strings.Repeat("stable system prompt ", 260))}},
			{Role: "user", Content: model.MessageContent{Content: stringPtr("first turn")}},
			{Role: "assistant", Content: model.MessageContent{Content: stringPtr("answer one")}},
			{
				Role:         "user",
				Content:      model.MessageContent{Content: stringPtr(strings.Repeat("long user context ", 260))},
				CacheControl: &model.CacheControl{Type: "ephemeral", TTL: "1h"},
			},
			{Role: "assistant", Content: model.MessageContent{Content: stringPtr("answer two")}},
			{Role: "user", Content: model.MessageContent{Content: stringPtr("final turn")}},
		},
		TransformOptions: model.TransformOptions{AnthropicAutoCacheControl: true},
	}

	converted := convertToAnthropicRequest(req)

	// Client already supplied one breakpoint; auto-injection (prefix + sliding) must be
	// skipped entirely, so the count stays at exactly 1.
	if got := countCacheControls(converted); got != 1 {
		t.Fatalf("expected client-supplied request to keep its single breakpoint untouched, got %d", got)
	}
}

// Sliding breakpoints should target user messages even when interleaved assistant turns
// are the actual last/second-to-last messages.
func TestSlidingCacheControlPicksUserMessagesNotAssistant(t *testing.T) {
	req := &anthropicModel.MessageRequest{
		Model: "claude-sonnet-4-5",
		System: &anthropicModel.SystemPrompt{
			MultiplePrompts: []anthropicModel.SystemPromptPart{
				{Type: "text", Text: longText("stable system prompt")},
			},
		},
		Messages: []anthropicModel.MessageParam{
			userMsg("turn one"),          // 0
			assistantMsg("answer one"),   // 1
			userMsg("turn two"),          // 2 — second-to-last user
			userMsg("turn three"),        // 3 — last user
			assistantMsg("answer three"), // 4 — trailing assistant
		},
	}

	applyAutomaticCacheControl(req)

	if !msgHasCacheControl(req, 3) {
		t.Fatalf("expected last user (idx 3) to be marked, not the trailing assistant")
	}
	if !msgHasCacheControl(req, 2) {
		t.Fatalf("expected second-to-last user (idx 2) to be marked")
	}
	if msgHasCacheControl(req, 4) {
		t.Fatalf("did not expect trailing assistant (idx 4) to be marked")
	}
	if got := countCacheControls(req); got != 3 {
		t.Fatalf("expected 3 breakpoints, got %d", got)
	}
}
