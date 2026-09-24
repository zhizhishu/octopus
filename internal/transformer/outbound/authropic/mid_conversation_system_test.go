package authropic

import (
	"encoding/json"
	"strings"
	"testing"

	anthropicModel "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestMidConversationSystemMessagePlacement pins the golden claude-CLI shape:
// with the mid-conversation-system beta (claude CLI 2.1.28x sends
// "mid-conversation-system-2026-04-07" and places a role:system message INSIDE
// messages, after the user turn), a system message that appears after the first
// non-system message must stay in messages at its original position, and only the
// LEADING system run may be hoisted into the top-level system array. Without the
// beta the historical hoist-everything behavior is kept (safe for chat clients
// whose interleaved system messages Anthropic would otherwise reject).
func TestMidConversationSystemMessagePlacement(t *testing.T) {
	leading := sysMsg("You are Claude Code.")
	user := model.Message{Role: "user", Content: model.MessageContent{Content: stringPtr("hello")}}
	envBlock := sysMsg("# Environment\nshell info")
	envBlock.CacheControl = &model.CacheControl{Type: "ephemeral"}
	envBlock.AnthropicMessageOutputConfig = json.RawMessage(`{"effort":"medium"}`)

	build := func(betas ...string) *model.InternalLLMRequest {
		return &model.InternalLLMRequest{
			Model: "claude-opus-4-8",
			TransformOptions: model.TransformOptions{
				AnthropicBetas: betas,
			},
			Messages: []model.Message{leading, user, envBlock},
		}
	}

	t.Run("with_beta_keeps_mid_system_in_messages", func(t *testing.T) {
		got := convertToAnthropicRequest(build("claude-code-20250219", "mid-conversation-system-2026-04-07"))
		if got.System == nil {
			t.Fatalf("system is nil")
		}
		sysTexts := systemTexts(got)
		// billing + agent identity are injected ahead of the leading run; the
		// mid-conversation env block must NOT be here.
		if len(sysTexts) != 3 || sysTexts[2] != "You are Claude Code." {
			t.Fatalf("system array must hold only the leading run (after injected billing+identity), got %#v", sysTexts)
		}
		if len(got.Messages) != 2 {
			t.Fatalf("messages must be [user, system], got %d: %#v", len(got.Messages), got.Messages)
		}
		if got.Messages[0].Role != "user" || got.Messages[1].Role != "system" {
			t.Fatalf("message roles = [%s, %s], want [user, system]", got.Messages[0].Role, got.Messages[1].Role)
		}
		blocks := got.Messages[1].Content.MultipleContent
		if len(blocks) != 1 || !strings.HasPrefix(*blocks[0].Text, "# Environment") {
			t.Fatalf("mid system message content mismatch: %#v", blocks)
		}
		if blocks[0].CacheControl == nil {
			t.Fatalf("mid system message must keep its cache_control")
		}
		if string(got.Messages[1].OutputConfig) != `{"effort":"medium"}` {
			t.Fatalf("mid system message must keep its per-message output_config, got %s", got.Messages[1].OutputConfig)
		}
	})

	t.Run("without_beta_hoists_as_before", func(t *testing.T) {
		got := convertToAnthropicRequest(build("claude-code-20250219"))
		if got.System == nil {
			t.Fatalf("system is nil")
		}
		sysTexts := systemTexts(got)
		// billing + agent identity are injected ahead of the hoisted run.
		if len(sysTexts) != 4 || !strings.HasPrefix(sysTexts[3], "# Environment") {
			t.Fatalf("without the beta both system messages must hoist (after the injected billing+identity), got %#v", sysTexts)
		}
		if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
			t.Fatalf("messages must be [user] only, got %#v", got.Messages)
		}
	})
}

func systemTexts(req *anthropicModel.MessageRequest) []string {
	var out []string
	for _, part := range req.System.MultiplePrompts {
		out = append(out, part.Text)
	}
	return out
}
