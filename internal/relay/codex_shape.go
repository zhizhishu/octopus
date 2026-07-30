package relay

import (
	"encoding/json"
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

const codexReasoningEncryptedContentInclude = "reasoning.encrypted_content"
const codexDefaultInstructions = "You are Codex, a coding agent based on GPT-5. You and the user share one workspace. Answer directly and do not call tools unless the user asks for workspace inspection or file changes."

func (ra *relayAttempt) prepareCodexRequestShape() {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil || !ra.shouldUseCodexFingerprint() {
		return
	}
	if ra.channel.Type != outbound.OutboundTypeOpenAIResponse {
		return
	}
	req := ra.internalRequest
	addCodexResponsesInclude(req)
	ra.bridgePlainResponsesCodexHistory()
	ensureCodexAgentContext(req)
	// Codex shape requires store=false: the reasoning.encrypted_content include added above is
	// the store=false stateless-reasoning channel, and combining it with store=true makes the
	// genuine upstream 500 once real reasoning is produced (empirically confirmed; a trivial
	// no-reasoning turn slips through, a substantive one does not). Force store=false even when
	// the client explicitly set store=true — mirrors sub2api's applyCodexOAuthTransform, which
	// overwrites an incoherent store=true rather than forwarding it. Real codex clients already
	// run store=false, so this only coerces the incoherent store=true case.
	store := false
	req.Store = &store
	applyCodexFastMode(req)
	normalizeCodexReasoningEffort(req)
	if len(req.ResponsesInputRaw) == 0 || !responsesInputRawLooksCodexShaped(req.ResponsesInputRaw) {
		if raw := synthesizeCodexResponsesInputRaw(req.Messages); len(raw) > 0 {
			req.ResponsesInputRaw = raw
		}
	}
}

func applyCodexFastMode(req *transformerModel.InternalLLMRequest) {
	if req == nil || !settingBool(dbmodel.SettingKeyCodexFastMode, false) {
		return
	}
	if len(req.ResponsesTextRaw) == 0 {
		req.ResponsesTextRaw = json.RawMessage(`{"verbosity":"low"}`)
	}
	if strings.TrimSpace(req.ReasoningEffort) == "" {
		req.ReasoningEffort = "low"
	}
}

// normalizeCodexReasoningEffort adjusts the request body's reasoning.effort for two
// codex-specific mismatches. It is a shape-SAFE body change (only reasoning.effort,
// never any TLS/header fingerprint); the GPT-5.6 check keys on req.Model, which at this
// point is the effective upstream model (applyModelMapping has already run).
//
//  1. GPT-5.6 family (sol/terra/luna): faithful passthrough of whatever effort the client
//     EXPLICITLY chose (none/minimal/low/medium/high/xhigh/max — including "max", which these
//     models accept unlike 5.5). Only a COMPLETELY UNSPECIFIED (empty) effort defaults to
//     "high", so a codex client that omits reasoning.effort for the (to it) unknown 5.6 model
//     still reasons instead of barely thinking — while a client that deliberately picked a
//     level owns it. (Previously none/minimal/low were also force-lifted to high; that
//     overrode a deliberate low, so it was narrowed to the empty case only.)
//  2. Non-5.6 codex models reject "max" (400), so only that single value is remapped to
//     "xhigh". Every other effort passes through faithfully.
//
// Mirrors sub2api's normalizeOpenAIReasoningEffortForModel spirit without its aggressive
// "drop unknown efforts" behaviour.
func normalizeCodexReasoningEffort(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if isGPT56Model(req.Model) {
		if effort == "" {
			req.ReasoningEffort = "high"
		}
		return
	}
	if effort == "max" {
		req.ReasoningEffort = "xhigh"
	}
}

// isGPT56Model reports whether model refers to the GPT-5.6 family (gpt-5.6,
// gpt-5.6-sol/terra/luna, …). It mirrors sub2api's isOpenAIGPT56Model semantics:
// case-insensitive, tolerant of a leading provider/path prefix (e.g.
// "openai/gpt-5.6-terra") and of date/effort suffixes (e.g.
// "gpt-5.6-terra-2026-07-09"). A leading path/vendor segment is stripped the same
// way transformer/model.IsImagenModel does so "openai/gpt-5.6" and "models/gpt-5.6"
// match like a bare "gpt-5.6".
func isGPT56Model(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	// Require an exact "gpt-5.6" or a "gpt-5.6-<suffix>" (variant/date/effort) so adjacent
	// names like "gpt-5.60" or "gpt-5.6foo" are NOT mistaken for the 5.6 family.
	return name == "gpt-5.6" || strings.HasPrefix(name, "gpt-5.6-")
}

func (ra *relayAttempt) bridgePlainResponsesCodexHistory() {
	if ra == nil || ra.internalRequest == nil || !ra.shouldBridgePlainResponsesCodexHistory() {
		return
	}
	req := ra.internalRequest
	if responsesMessagesContainToolOutput(req.Messages) {
		return
	}
	if req.PreviousResponseID == nil || strings.TrimSpace(*req.PreviousResponseID) == "" {
		return
	}
	ra.applyPlainResponsesCodexHistoryForPreviousResponseID(*req.PreviousResponseID)
}

func (ra *relayAttempt) applyPlainResponsesCodexHistoryForPreviousResponseID(previousResponseID string) {
	if ra == nil || ra.internalRequest == nil {
		return
	}
	req := ra.internalRequest
	previousResponseID = strings.TrimSpace(previousResponseID)
	if previousResponseID == "" {
		return
	}
	if responsesMessagesAlreadyCarryAssistantContext(req.Messages) {
		return
	}
	history, ok := responsesSessionTranscript(previousResponseID, ra.apiKeyID, ra.userID)
	if !ok || len(history) == 0 {
		return
	}
	req.Messages = appendPlainResponsesHistory(history, req.Messages)
	req.PreviousResponseID = nil
	req.ResponsesInputRaw = nil
}

func responsesMessagesContainToolOutput(messages []transformerModel.Message) bool {
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "tool") {
			return true
		}
	}
	return false
}

func responsesMessagesAlreadyCarryAssistantContext(messages []transformerModel.Message) bool {
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			return true
		}
	}
	return false
}

func appendPlainResponsesHistory(history, current []transformerModel.Message) []transformerModel.Message {
	out := make([]transformerModel.Message, 0, len(history)+len(current))
	for _, msg := range current {
		role := strings.TrimSpace(msg.Role)
		if strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
			out = append(out, cloneResponseSessionMessage(msg))
		}
	}
	out = append(out, cloneResponsesSessionMessages(history)...)
	for _, msg := range current {
		role := strings.TrimSpace(msg.Role)
		if strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
			continue
		}
		out = append(out, cloneResponseSessionMessage(msg))
	}
	return out
}

// normalizeChatToolCallPairing enforces the tool-call invariant the OpenAI Chat
// Completions schema requires (and strict upstreams like DeepSeek / Grok reject when
// violated): an assistant message with tool_calls must be immediately followed by one
// tool message per tool_call_id, in order, with nothing in between. Ported from sub2api's
// normalizeChatMessages (backend/internal/pkg/apicompat) so a rebuilt responses→chat
// history can never present an unanswered tool_call or an orphan tool reply upstream.
//
// It re-emits each assistant's ANSWERED tool_calls directly followed by their replies (in
// call order); unanswered tool_calls are dropped (an assistant then left with no tool_calls
// and no text content is dropped); orphan tool replies (a tool_call_id no assistant
// announced) are dropped; a bare tool message with no tool_call_id is a direct chat
// passthrough kept in place; any message that landed between an assistant's tool_calls and
// its replies is re-emitted in its natural position but never between them.
func normalizeChatToolCallPairing(messages []transformerModel.Message) []transformerModel.Message {
	toolReplyID := func(m transformerModel.Message) string {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "tool") || m.ToolCallID == nil {
			return ""
		}
		return strings.TrimSpace(*m.ToolCallID)
	}

	// Index every tool reply by its tool_call_id (last wins on duplicates).
	replies := make(map[string]transformerModel.Message)
	for _, m := range messages {
		if id := toolReplyID(m); id != "" {
			replies[id] = m
		}
	}

	out := make([]transformerModel.Message, 0, len(messages))
	for _, m := range messages {
		switch {
		case strings.EqualFold(strings.TrimSpace(m.Role), "tool"):
			// Bare tool message (no id): direct passthrough. A reply whose id an
			// assistant announces is emitted right after that assistant (skip the
			// standalone occurrence); any other tool reply is an orphan and is dropped.
			if toolReplyID(m) == "" {
				out = append(out, m)
			}
			continue
		case len(m.ToolCalls) > 0:
			kept := make([]transformerModel.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				id := strings.TrimSpace(tc.ID)
				if id == "" {
					continue
				}
				if _, ok := replies[id]; ok {
					tc.ID = id // canonicalize to the trimmed id so the emitted call matches its reply exactly
					kept = append(kept, tc)
				}
			}
			if len(kept) == 0 {
				// No answered tool_calls left: keep as a plain message if it still has
				// text content, otherwise drop it entirely.
				if strings.TrimSpace(messageTextContent(m.Content)) == "" {
					continue
				}
				m.ToolCalls = nil
				out = append(out, m)
				continue
			}
			m.ToolCalls = kept
			out = append(out, m)
			for _, tc := range kept {
				// kept ids are already trimmed; canonicalize the reply's tool_call_id to the
				// same value so a whitespace-padded input can't leave the wire pair mismatched.
				reply := replies[tc.ID]
				canonicalID := tc.ID
				reply.ToolCallID = &canonicalID
				out = append(out, reply)
			}
		default:
			out = append(out, m)
		}
	}
	return out
}

func ensureCodexAgentContext(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	if req.ResponsesInstructions == nil && !messagesContainInstruction(req.Messages) {
		content := codexDefaultInstructions
		req.Messages = append([]transformerModel.Message{{
			Role:    "system",
			Content: transformerModel.MessageContent{Content: &content},
		}}, req.Messages...)
	}
	if responsesMessagesContainToolOutput(req.Messages) {
		return
	}
	if len(req.Tools) == 0 && len(req.ResponsesToolsRaw) == 0 {
		req.Tools = defaultCodexTools()
	}
	if req.ToolChoice == nil && len(req.ResponsesToolChoiceRaw) == 0 && len(req.Tools) > 0 {
		choice := "auto"
		req.ToolChoice = &transformerModel.ToolChoice{ToolChoice: &choice}
	}
	if req.ParallelToolCalls == nil && len(req.Tools) > 0 && len(req.ResponsesToolsRaw) == 0 {
		parallel := false
		req.ParallelToolCalls = &parallel
	}
}

func messagesContainInstruction(messages []transformerModel.Message) bool {
	for _, msg := range messages {
		switch strings.TrimSpace(msg.Role) {
		case "system", "developer":
			return true
		}
	}
	return false
}

func defaultCodexTools() []transformerModel.Tool {
	return []transformerModel.Tool{
		codexFunctionTool(
			"shell_command",
			"Runs a Powershell command (Windows) and returns its output.",
			`{"additionalProperties":false,"properties":{"command":{"description":"The shell script to execute in the user's default shell","type":"string"},"justification":{"description":"Only set if sandbox_permissions is \"require_escalated\". Request approval from the user to run this command outside the sandbox.","type":"string"},"login":{"description":"Whether to run the shell with login semantics. Defaults to true.","type":"boolean"},"prefix_rule":{"description":"Only specify when sandbox_permissions is require_escalated. Suggest a prefix command pattern for similar future requests.","items":{"type":"string"},"type":"array"},"sandbox_permissions":{"description":"Sandbox permissions for the command. Set to require_escalated to request running without sandbox restrictions; defaults to use_default.","type":"string"},"timeout_ms":{"description":"The timeout for the command in milliseconds","type":"number"},"workdir":{"description":"The working directory to execute the command in","type":"string"}},"required":["command"],"type":"object"}`,
		),
		codexFunctionTool(
			"update_plan",
			"Updates the task plan. At most one step can be in_progress at a time.",
			`{"additionalProperties":false,"properties":{"explanation":{"type":"string"},"plan":{"description":"The list of steps","items":{"additionalProperties":false,"properties":{"status":{"description":"One of: pending, in_progress, completed","type":"string"},"step":{"type":"string"}},"required":["step","status"],"type":"object"},"type":"array"}},"required":["plan"],"type":"object"}`,
		),
		codexFunctionTool(
			"request_user_input",
			"Request user input for one to three short questions and wait for the response.",
			`{"additionalProperties":false,"properties":{"questions":{"description":"Questions to show the user. Prefer 1 and do not exceed 3","items":{"additionalProperties":false,"properties":{"header":{"description":"Short header label shown in the UI (12 or fewer chars).","type":"string"},"id":{"description":"Stable identifier for mapping answers (snake_case).","type":"string"},"options":{"description":"Provide 2-3 mutually exclusive choices. The client may add a free-form Other option.","items":{"additionalProperties":false,"properties":{"description":{"description":"One short sentence explaining impact/tradeoff if selected.","type":"string"},"label":{"description":"User-facing label (1-5 words).","type":"string"}},"required":["label","description"],"type":"object"},"type":"array"},"question":{"description":"Single-sentence prompt shown to the user.","type":"string"}},"required":["id","header","question","options"],"type":"object"},"type":"array"}},"required":["questions"],"type":"object"}`,
		),
		codexFunctionTool(
			"view_image",
			"View a local image from the filesystem.",
			`{"additionalProperties":false,"properties":{"detail":{"description":"Optional detail override. Supported values are high and original.","enum":["high","original"],"type":"string"},"path":{"description":"Local filesystem path to an image file","type":"string"}},"required":["path"],"type":"object"}`,
		),
	}
}

func codexFunctionTool(name, description, parameters string) transformerModel.Tool {
	strict := false
	return transformerModel.Tool{
		Type: "function",
		Function: transformerModel.Function{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(parameters),
			Strict:      &strict,
		},
	}
}

func addCodexResponsesInclude(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	for _, item := range req.Include {
		if strings.EqualFold(strings.TrimSpace(item), codexReasoningEncryptedContentInclude) {
			return
		}
	}
	req.Include = append(req.Include, codexReasoningEncryptedContentInclude)
}

func responsesInputRawLooksCodexShaped(raw json.RawMessage) bool {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		// Incremental tool/reasoning turns arrive as native Codex Responses
		// items (function_call_output / function_call / reasoning) with no
		// message item. The client already sent them in Codex shape, so pass
		// them through untouched to keep reasoning/encrypted_content continuity
		// and a stable prompt cache prefix. Re-synthesizing would drop the
		// reasoning items and change the byte order, forcing the upstream to
		// think from scratch and miss the prompt cache.
		switch strings.TrimSpace(codexShapeStringValue(item["type"])) {
		case "message":
			if _, ok := item["content"]; ok {
				return true
			}
		case "function_call_output", "function_call", "reasoning":
			return true
		}
	}
	return false
}

func synthesizeCodexResponsesInputRaw(messages []transformerModel.Message) json.RawMessage {
	items := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			continue
		case "assistant":
			items = append(items, codexAssistantItems(msg)...)
		case "tool":
			if item := codexToolOutputItem(msg); item != nil {
				items = append(items, item)
			}
		default:
			if item := codexUserMessageItem(msg); item != nil {
				items = append(items, item)
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return raw
}

func codexUserMessageItem(msg transformerModel.Message) map[string]any {
	content := codexInputContentItems(msg.Content)
	if len(content) == 0 {
		return nil
	}
	role := strings.TrimSpace(msg.Role)
	if role == "" || role == "function" || role == "tool" {
		role = "user"
	}
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": content,
	}
}

func codexAssistantItems(msg transformerModel.Message) []map[string]any {
	items := make([]map[string]any, 0, len(msg.ToolCalls)+2)
	// When re-synthesizing a bridged/rebuilt assistant turn that carries encrypted
	// reasoning, emit the reasoning item first so the next Responses request keeps
	// encrypted_content continuity. Mirror the outbound Responses converter: keep
	// the encrypted blob, strip any id, and backfill summary:[] (empty when no
	// summary text is available). Without this, resynthesis drops the signature and
	// the upstream has to re-think from scratch / rejects unsigned continuity.
	// Emit the reasoning item ONLY for an OpenAI-encrypted-tagged signature, using the
	// raw (untagged) blob as encrypted_content. A foreign/untagged signature (Gemini
	// thoughtSignature, Anthropic thinking/redacted, DeepSeek bare) must NOT become an
	// OpenAI encrypted_content, or the upstream rejects the cross-protocol blob.
	if raw, ok := transformerModel.OpenAIEncryptedContent(msg.ReasoningSignature); ok {
		reasoningItem := map[string]any{
			"type":              "reasoning",
			"encrypted_content": raw,
			"summary":           []map[string]any{},
		}
		if text := strings.TrimSpace(msg.GetReasoningContent()); text != "" {
			reasoningItem["summary"] = []map[string]any{{
				"type": "summary_text",
				"text": text,
			}}
		}
		items = append(items, reasoningItem)
	}
	if text := strings.TrimSpace(messageTextContent(msg.Content)); text != "" {
		items = append(items, map[string]any{
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]any{{
				"type": "output_text",
				"text": text,
			}},
		})
	}
	for _, toolCall := range msg.ToolCalls {
		callID := strings.TrimSpace(toolCall.ID)
		if callID == "" {
			callID = strings.TrimSpace(toolCall.Function.Name)
		}
		if callID == "" {
			continue
		}
		items = append(items, map[string]any{
			"type":      "function_call",
			"call_id":   callID,
			"name":      strings.TrimSpace(toolCall.Function.Name),
			"arguments": toolCall.Function.Arguments,
		})
	}
	return items
}

func codexToolOutputItem(msg transformerModel.Message) map[string]any {
	callID := ""
	if msg.ToolCallID != nil {
		callID = strings.TrimSpace(*msg.ToolCallID)
	}
	if callID == "" {
		return nil
	}
	output := messageTextContent(msg.Content)
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  output,
	}
}

func codexInputContentItems(content transformerModel.MessageContent) []map[string]any {
	if content.Content != nil {
		text := *content.Content
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []map[string]any{{
			"type": "input_text",
			"text": text,
		}}
	}
	items := make([]map[string]any, 0, len(content.MultipleContent))
	for _, part := range content.MultipleContent {
		switch part.Type {
		case "text":
			if part.Text != nil && strings.TrimSpace(*part.Text) != "" {
				items = append(items, map[string]any{
					"type": "input_text",
					"text": *part.Text,
				})
			}
		case "image_url":
			if part.ImageURL != nil && strings.TrimSpace(part.ImageURL.URL) != "" {
				item := map[string]any{
					"type":      "input_image",
					"image_url": part.ImageURL.URL,
				}
				if part.ImageURL.Detail != nil && strings.TrimSpace(*part.ImageURL.Detail) != "" {
					item["detail"] = *part.ImageURL.Detail
				}
				items = append(items, item)
			}
		}
	}
	return items
}

func messageTextContent(content transformerModel.MessageContent) string {
	if content.Content != nil {
		return *content.Content
	}
	var sb strings.Builder
	for _, part := range content.MultipleContent {
		if part.Type != "text" || part.Text == nil {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(*part.Text)
	}
	return sb.String()
}

func codexShapeStringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return ""
	}
}
