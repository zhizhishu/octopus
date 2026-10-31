package relay

import (
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

type promptOverrideSnapshot struct {
	// Mode is "before", "after" or "before+after" (which positions were applied); "" when no
	// layer applied.
	Mode    dbmodel.PromptOverrideMode
	Sources []string
}

type promptOverrideLayer struct {
	source string
	mode   dbmodel.PromptOverrideMode
	text   string
}

// applyPromptOverrides applies the system prompt layers global → plan → route rule → channel.
//
// A layer is skipped entirely unless its 启用 switch is on and its text is non-blank
// (dbmodel.PromptOverrideActive). An applied layer puts its text before or after the
// request's own system prompt, using new-api's per-protocol joining rules
// (new-api relay/chat_completions_via_responses.go applySystemPromptIfNeeded,
// relay/claude_handler.go, relay/gemini_handler.go, relay/channel/codex/adaptor.go) and their
// mirror image for "after". When the request has no system prompt the text becomes it.
// The client's own system content is never removed or reordered.
//
// Each layer is applied on top of the previous result, so the final order is
//
//	channel-before, route-before, plan-before, global-before, CLIENT, global-after, plan-after, route-after, channel-after
//
// i.e. the more specific a layer is, the further out it sits.
func applyPromptOverrides(req *transformerModel.InternalLLMRequest, plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel) promptOverrideSnapshot {
	snapshot := promptOverrideSnapshot{}
	if req == nil || !req.IsChatRequest() {
		return snapshot
	}
	before, after := false, false
	for _, layer := range promptOverrideLayers(plan, rule, channel) {
		mode := dbmodel.NormalizePromptOverrideMode(layer.mode)
		applyPromptOverrideText(req, strings.TrimSpace(layer.text), mode)
		snapshot.Sources = append(snapshot.Sources, layer.source)
		if mode == dbmodel.PromptOverrideModeAfter {
			after = true
		} else {
			before = true
		}
	}
	switch {
	case before && after:
		snapshot.Mode = "before+after"
	case after:
		snapshot.Mode = dbmodel.PromptOverrideModeAfter
	case before:
		snapshot.Mode = dbmodel.PromptOverrideModeBefore
	}
	return snapshot
}

// promptSlot locates the request's own system prompt inside the internal message list.
type promptSlot struct {
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

func promptSlotFor(req *transformerModel.InternalLLMRequest) promptSlot {
	switch req.RawAPIFormat {
	case transformerModel.APIFormatAnthropicMessage:
		return anthropicPromptSlot(req)
	case transformerModel.APIFormatOpenAIResponse:
		// Inbound puts a non-empty `instructions` as Messages[0] and keeps the raw value in
		// ResponsesInstructions. developer/system items inside `input` are NOT the
		// instructions slot (new-api's codex adaptor only looks at `instructions`).
		if req.ResponsesInstructions != nil && len(req.Messages) > 0 && isSystemRole(req.Messages[0].Role) {
			return promptSlot{index: 0, trimExisting: true}
		}
		return promptSlot{index: -1}
	case transformerModel.APIFormatGeminiContents:
		// Inbound joins systemInstruction.parts' non-empty texts with "\n" into Messages[0],
		// so editing its start/end equals editing the first/last non-empty text part.
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			return promptSlot{index: 0}
		}
		return promptSlot{index: -1}
	default:
		// OpenAI chat: first system (or developer) message anywhere, like new-api's
		// containSystemPrompt loop.
		for i, msg := range req.Messages {
			if isSystemRole(msg.Role) {
				return promptSlot{index: i}
			}
		}
		return promptSlot{index: -1}
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

func anthropicPromptSlot(req *transformerModel.InternalLLMRequest) promptSlot {
	// Only the leading system run is the top-level `system`; later role=system messages are
	// mid-conversation system messages (claude CLI beta) and stay where they are.
	leading := 0
	for leading < len(req.Messages) && req.Messages[leading].Role == "system" {
		leading++
	}
	if leading == 0 {
		return promptSlot{index: -1}
	}
	if req.TransformerMetadata["anthropic_system_array_format"] != "true" {
		return promptSlot{index: 0, trimExisting: true}
	}
	beforeAt := 0
	if beforeAt < leading && strings.HasPrefix(strings.TrimSpace(promptMessageText(req.Messages[beforeAt])), promptClaudeBillingHeaderPrefix) {
		beforeAt++
	}
	if beforeAt < leading {
		text := promptMessageText(req.Messages[beforeAt])
		if strings.Contains(text, promptClaudeAgentSDKMarker) || strings.HasPrefix(strings.TrimSpace(text), promptClaudeCodeIdentityMarker) {
			beforeAt++
		}
	}
	return promptSlot{index: 0, asNewBlock: true, beforeAt: beforeAt, afterAt: leading}
}

func applyPromptOverrideText(req *transformerModel.InternalLLMRequest, text string, mode dbmodel.PromptOverrideMode) {
	after := mode == dbmodel.PromptOverrideModeAfter
	slot := promptSlotFor(req)
	if slot.index < 0 {
		insertSystemMessage(req, 0, text)
		if req.RawAPIFormat == transformerModel.APIFormatOpenAIResponse {
			instructions := text
			req.ResponsesInstructions = &instructions
		}
		return
	}
	if slot.asNewBlock {
		if after {
			insertSystemMessage(req, slot.afterAt, text)
		} else {
			insertSystemMessage(req, slot.beforeAt, text)
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

func insertSystemMessage(req *transformerModel.InternalLLMRequest, at int, text string) {
	content := text
	msg := transformerModel.Message{Role: "system", Content: transformerModel.MessageContent{Content: &content}}
	out := make([]transformerModel.Message, 0, len(req.Messages)+1)
	out = append(out, req.Messages[:at]...)
	out = append(out, msg)
	out = append(out, req.Messages[at:]...)
	req.Messages = out
}

func isSystemRole(role string) bool {
	return role == "system" || role == "developer"
}

func promptMessageText(msg transformerModel.Message) string {
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

func clearResponsesRawPromptShape(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	req.ResponsesInstructions = nil
	req.ResponsesInputRaw = nil
}

// promptOverrideLayers returns only the layers that will add something (switch on, text set).
func promptOverrideLayers(plan *dbmodel.AccessPlan, rule *dbmodel.AccessRouteRule, channel *dbmodel.Channel) []promptOverrideLayer {
	layers := make([]promptOverrideLayer, 0, 4)
	if text, mode, enabled := globalPromptOverride(); dbmodel.PromptOverrideActive(enabled, mode, text) {
		layers = append(layers, promptOverrideLayer{source: "global", mode: mode, text: text})
	}
	if plan != nil && dbmodel.PromptOverrideActive(plan.PromptOverrideEnabled, plan.PromptOverrideMode, plan.SystemPromptOverride) {
		layers = append(layers, promptOverrideLayer{source: "access_plan:" + plan.Slug, mode: plan.PromptOverrideMode, text: plan.SystemPromptOverride})
	}
	if rule != nil && dbmodel.PromptOverrideActive(rule.PromptOverrideEnabled, rule.PromptOverrideMode, rule.SystemPromptOverride) {
		layers = append(layers, promptOverrideLayer{source: "route_rule:" + rule.RequestModel, mode: rule.PromptOverrideMode, text: rule.SystemPromptOverride})
	}
	if channel != nil && dbmodel.PromptOverrideActive(channel.PromptOverrideEnabled, channel.PromptOverrideMode, channel.SystemPromptOverride) {
		layers = append(layers, promptOverrideLayer{source: "channel:" + channel.Name, mode: channel.PromptOverrideMode, text: channel.SystemPromptOverride})
	}
	return layers
}

func globalPromptOverride() (string, dbmodel.PromptOverrideMode, bool) {
	text, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideSystem)
	if err != nil {
		return "", dbmodel.PromptOverrideModeBefore, false
	}
	mode := dbmodel.PromptOverrideModeBefore
	if value, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideMode); err == nil {
		mode = dbmodel.PromptOverrideMode(value)
	}
	enabled := false
	if value, err := op.SettingGetString(dbmodel.SettingKeyPromptOverrideEnabled); err == nil {
		enabled = value == "true"
	}
	return strings.TrimSpace(text), mode, enabled
}
