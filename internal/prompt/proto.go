// 协议拼接核心：从 internal/relay/prompt_override.go（a7fd2e93）原样搬入，
// 供 relay 生产注入与提示词页预览共用。测试 golden 在 relay 包里是独立复制的
// oracle，不引用本包；改这里的拼接逻辑不会自动改变测试基线。
package prompt

import (
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)
type Slot struct {
	// index of the message holding the system prompt, -1 when the request has none.
	index int
	// asNewBlock: the protocol keeps the system prompt as an array of blocks; a layer is
	// added as its own block (Anthropic array-form system) instead of editing text.
	asNewBlock bool
	// beforeAt / afterAt: where a new block goes for "before" / "after".
	beforeAt, afterAt int
	// trimExisting: new-api TrimSpace's the existing text and drops the "\n" when it is blank
	// (Anthropic string system, Responses instructions).
	trimExisting bool
}

func SlotFor(req *transformerModel.InternalLLMRequest) Slot {
	switch req.RawAPIFormat {
	case transformerModel.APIFormatAnthropicMessage:
		return anthropicSlot(req)
	case transformerModel.APIFormatOpenAIResponse:
		// Inbound puts a non-empty `instructions` as Messages[0] and keeps the raw value in
		// ResponsesInstructions. developer/system items inside `input` are NOT the
		// instructions slot (new-api's codex adaptor only looks at `instructions`).
		if req.ResponsesInstructions != nil && len(req.Messages) > 0 && IsSystemRole(req.Messages[0].Role) {
			return Slot{index: 0, trimExisting: true}
		}
		return Slot{index: -1}
	case transformerModel.APIFormatGeminiContents:
		// Inbound joins systemInstruction.parts' non-empty texts with "\n" into Messages[0],
		// so editing its start/end equals editing the first/last non-empty text part.
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			return Slot{index: 0}
		}
		return Slot{index: -1}
	default:
		// OpenAI chat: first system (or developer) message anywhere, like new-api's
		// containSystemPrompt loop.
		for i, msg := range req.Messages {
			if IsSystemRole(msg.Role) {
				return Slot{index: i}
			}
		}
		return Slot{index: -1}
	}
}

// Claude CLI system header blocks. A genuine Claude Code request starts its system array
// with the billing header block followed by the agent identity block; the Anthropic outbound
// cloak keys on exactly that (billing header must stay parts[0], otherwise it synthesizes
// one). A "before" block is therefore inserted AFTER these blocks so the CLI's own blocks
// keep their positions and bytes and no second billing header is ever synthesized.
const (
	promptClaudeBillingHeaderPrefix = "x-anthropic-billing-header:"
	promptClaudeAgentSDKMarker      = "built on Anthropic's Claude Agent SDK"
	promptClaudeCodeIdentityMarker  = "You are Claude Code, Anthropic's official CLI for Claude"
)

func anthropicSlot(req *transformerModel.InternalLLMRequest) Slot {
	// Only the leading system run is the top-level `system`; later role=system messages are
	// mid-conversation system messages (claude CLI beta) and stay where they are.
	leading := 0
	for leading < len(req.Messages) && req.Messages[leading].Role == "system" {
		leading++
	}
	if leading == 0 {
		return Slot{index: -1}
	}
	if req.TransformerMetadata["anthropic_system_array_format"] != "true" {
		return Slot{index: 0, trimExisting: true}
	}
	beforeAt := 0
	if beforeAt < leading && strings.HasPrefix(strings.TrimSpace(MessageText(req.Messages[beforeAt])), promptClaudeBillingHeaderPrefix) {
		beforeAt++
	}
	if beforeAt < leading {
		text := MessageText(req.Messages[beforeAt])
		if strings.Contains(text, promptClaudeAgentSDKMarker) || strings.HasPrefix(strings.TrimSpace(text), promptClaudeCodeIdentityMarker) {
			beforeAt++
		}
	}
	return Slot{index: 0, asNewBlock: true, beforeAt: beforeAt, afterAt: leading}
}

func ApplyText(req *transformerModel.InternalLLMRequest, text string, mode dbmodel.PromptOverrideMode) {
	after := mode == dbmodel.PromptOverrideModeAfter
	slot := SlotFor(req)
	if slot.index < 0 {
		InsertSystemMessage(req, 0, text)
		if req.RawAPIFormat == transformerModel.APIFormatOpenAIResponse {
			instructions := text
			req.ResponsesInstructions = &instructions
		}
		return
	}
	if slot.asNewBlock {
		if after {
			InsertSystemMessage(req, slot.afterAt, text)
		} else {
			InsertSystemMessage(req, slot.beforeAt, text)
		}
		return
	}
	msg := &req.Messages[slot.index]
	if len(msg.Content.MultipleContent) > 0 {
		part := transformerModel.MessageContentPart{Type: "text", Text: &text}
		parts := make([]transformerModel.MessageContentPart, 0, len(msg.Content.MultipleContent)+1)
		if after {
			parts = append(append(parts, msg.Content.MultipleContent...), part)
		} else {
			parts = append(append(parts, part), msg.Content.MultipleContent...)
		}
		msg.Content.MultipleContent = parts
	} else {
		existing := ""
		if msg.Content.Content != nil {
			existing = *msg.Content.Content
		}
		if slot.trimExisting {
			existing = strings.TrimSpace(existing)
		}
		merged := text
		switch {
		case slot.trimExisting && existing == "":
		case after:
			merged = existing + "\n" + text
		default:
			merged = text + "\n" + existing
		}
		msg.Content.Content = &merged
	}
	if req.RawAPIFormat == transformerModel.APIFormatOpenAIResponse && slot.index == 0 && msg.Content.Content != nil {
		instructions := *msg.Content.Content
		req.ResponsesInstructions = &instructions
	}
}

func InsertSystemMessage(req *transformerModel.InternalLLMRequest, at int, text string) {
	content := text
	msg := transformerModel.Message{Role: "system", Content: transformerModel.MessageContent{Content: &content}}
	out := make([]transformerModel.Message, 0, len(req.Messages)+1)
	out = append(out, req.Messages[:at]...)
	out = append(out, msg)
	out = append(out, req.Messages[at:]...)
	req.Messages = out
}

func IsSystemRole(role string) bool {
	return role == "system" || role == "developer"
}

func MessageText(msg transformerModel.Message) string {
	if msg.Content.Content != nil {
		return *msg.Content.Content
	}
	for _, part := range msg.Content.MultipleContent {
		if part.Type == "text" && part.Text != nil {
			return *part.Text
		}
	}
	return ""
}

func ClearResponsesRawPromptShape(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	req.ResponsesInstructions = nil
	req.ResponsesInputRaw = nil
}
