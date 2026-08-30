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
	// A genuine codex client sends a Responses-shaped `input` that already carries its own
	// developer/system instructions and tool declarations inline (SELF-CONTAINED). Detect that
	// here (AFTER any history bridge, which clears ResponsesInputRaw). codexRawInput alone only
	// means the body is a codex-shaped array — NOT enough to skip the default-agent injection,
	// because a bare Responses request (a lone user message, no instructions) is also codex-shaped
	// yet still needs ensureCodexAgentContext to supply the default codex identity + tools. Gate the
	// skip/suppress on codexSelfContained = codexRawInput AND the input actually carries an
	// instruction (developer/system message, via req.Messages) — only THEN would injecting a default
	// agent context or hoisting a Messages-derived copy DUPLICATE what the input already has.
	codexRawInput := len(req.ResponsesInputRaw) > 0 && responsesInputRawLooksCodexShaped(req.ResponsesInputRaw)
	codexSelfContained := codexRawInput && messagesContainInstruction(req.Messages)
	if !codexSelfContained {
		ensureCodexAgentContext(req)
	}
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
	ensureCodexReasoningSummary(req)
	ensureCodexReasoningContext(req)
	ensureCodexParallelToolCalls(req)
	if codexSelfContained {
		// The raw codex input already carries its developer/system instructions and tools; stop
		// the outbound transformer from also hoisting a Messages-derived top-level `instructions`
		// and `tools` copy (see suppressCodexHoistedContext). Otherwise a genuine codex request
		// goes upstream ~+27KB heavier than the real CLI sends — a byte-level fingerprint
		// divergence AND pure extra input tokens that slow the first token and make the
		// capacity-limited upstream likelier to reject with a 500.
		suppressCodexHoistedContext(req)
	} else if !codexRawInput {
		// No usable codex-shaped raw input (empty, or a chat→codex request): synthesize one from
		// the messages, exactly as before. Here the hoisted top-level instructions ARE wanted,
		// since the synthesized input deliberately omits system/developer messages. (A bare-but-
		// codex-shaped raw input is instead kept as-is; the ensureCodexAgentContext defaults
		// injected above are hoisted to top-level for it — matching pre-fix behavior.)
		if raw := synthesizeCodexResponsesInputRaw(req.Messages); len(raw) > 0 {
			req.ResponsesInputRaw = raw
		}
	}
}

// suppressCodexHoistedContext stops the outbound Responses transformer from emitting a top-level
// `instructions` or `tools` for a genuine codex request whose raw Responses `input` already carries
// its developer/system instructions and tool declarations inline. ConvertToResponsesRequest builds
// Instructions from req.Messages and Tools from req.Tools, and only an explicit req.ResponsesInstructions
// / ResponsesToolsRaw overrides them — so a codex client (which sends neither top-level field, keeping
// both inside `input`) would otherwise get a Messages-derived DUPLICATE hoisted onto the wire. That is a
// byte-level divergence from the real codex CLI and ~+27KB of redundant system-prompt + tool tokens.
// tool_choice is intentionally left untouched (the genuine codex CLI does send it top-level);
// parallel_tool_calls IS forced to false by ensureCodexParallelToolCalls (the upstream hard-pairs
// it to the Lite header oct synthesizes). This function only strips the duplicated
// instructions/tools — it never touches either of those two.
func suppressCodexHoistedContext(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	// Suppress ONLY the Messages-derived top-level hoist (the duplicate), never a field the
	// client explicitly sent at the top level:
	//   - instructions: ConvertToResponsesRequest hoists from req.Messages, and
	//     applyRawResponsesRequestFields overrides it only when req.ResponsesInstructions != nil.
	//     So force "" (→ omitempty drops it) ONLY when the client sent no explicit top-level
	//     instructions; if it did (non-nil), that is not a duplicate — keep it.
	if req.ResponsesInstructions == nil {
		empty := ""
		req.ResponsesInstructions = &empty
	}
	//   - tools: the transformer emits req.Tools as top-level tools only when the client sent no
	//     raw tools (ResponsesToolsRaw). Clear the inbound/Messages-derived req.Tools in that
	//     case (a genuine codex request carries its tools inline in the raw input); leave an
	//     explicit client-sent ResponsesToolsRaw untouched.
	if len(req.ResponsesToolsRaw) == 0 {
		req.Tools = nil
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
//  1. GPT-5.6 family (sol/terra/luna): effort in the allowed set (low/medium/high/xhigh/max)
//     is preserved faithfully. An unspecified (empty) effort defaults to "high", while any effort
//     outside the allowed set (e.g. none/minimal/unknown) is remapped to "low" to avoid upstream 400 errors.
//  2. Non-5.6 codex models reject "max" (400), so only that single value is remapped to
//     "xhigh". Every other effort passes through faithfully.
//
// Mirrors sub2api's normalizeOpenAIReasoningEffortForModel spirit without its aggressive
// "drop unknown efforts" behaviour.
// ensureCodexReasoningSummary guarantees the codex-faithful reasoning.summary="auto".
// A genuine codex CLI always sends reasoning:{effort,summary:"auto"} — the summary field is
// what makes a Responses upstream emit response.reasoning_summary_text.delta events *during*
// a long reasoning turn. When it is missing (a chat->codex request, or a codex client that
// left it empty) the upstream reasons silently, so a max-effort turn over a large context
// streams NOTHING to the client for the whole 60-1200s reasoning window — perceived as a
// hang. Only fill the default when the client left it empty; a client that explicitly chose
// a summary level owns it. Shape-SAFE body change (reasoning.summary only), and it moves the
// codex outbound CLOSER to the real CLI shape (which always carries summary="auto").
func ensureCodexReasoningSummary(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	// Real codex always sends effort+summary together. Only default summary when an
	// effort is present (runs after normalizeCodexReasoningEffort, which fills effort for
	// the gpt-5.6 family) so we never materialize a summary-only reasoning object — a
	// shape the real CLI never produces and that a non-reasoning model's upstream could
	// 400 on.
	if strings.TrimSpace(req.ReasoningEffort) == "" {
		return
	}
	if strings.TrimSpace(req.ReasoningSummary) == "" {
		req.ReasoningSummary = "auto"
	}
}

// codexReasoningContextAllTurns is the only reasoning.context value the upstream accepts
// alongside the X-OpenAI-Internal-Codex-Responses-Lite header oct always sends on codex.
const codexReasoningContextAllTurns = "all_turns"

// ensureCodexReasoningContext guarantees reasoning.context="all_turns" on the codex path.
//
// The upstream added a hard pairing rule: a request carrying
// X-OpenAI-Internal-Codex-Responses-Lite: true MUST also carry reasoning.context="all_turns",
// else it is rejected outright:
//
//	400 X-OpenAI-Internal-Codex-Responses-Lite requires `reasoning.context` to be `all_turns`.
//
// applyCodexHeaderDefaultsWithFingerprint synthesizes that header for every codex outbound
// (oct adds it itself; it is not forwarded from the client), so oct owns the paired body
// field too — filling it here keeps header and body consistent instead of shipping a
// self-contradictory request. That makes this a shape-SAFE change that moves the outbound
// CLOSER to the real codex CLI, exactly like ensureCodexReasoningSummary above.
//
// Deliberately NOT done in ConvertToResponsesRequest: that transformer also serves plain
// (non-codex) Responses channels, which never get the Lite header and whose upstreams may
// reject the unknown field. Living in the codex shaper keeps every other channel's bytes
// untouched.
//
// Unlike the summary default this must NOT be gated on a non-empty effort: the rule is tied
// to the header, not to reasoning being requested, so a request with no reasoning at all
// still needs the field and materializes a context-only reasoning object.
func ensureCodexReasoningContext(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	// Ghost-pairing guard: this field exists ONLY to satisfy the Lite-header pairing
	// rule. A denylisted model (gpt-5.5 family) gets no Lite header on the wire, so
	// forcing its pairing field would ship a headless constraint with no justification.
	// Header and body consult one shared decision (codexResponsesLiteApplies) so they
	// can never disagree about whether the pairing rule is in effect.
	if !codexResponsesLiteApplies(req.Model) {
		return
	}
	// Lite makes this a hard wire-contract value, not a client preference. Preserving an
	// explicit non-all_turns value while still emitting the Lite header produces the exact
	// upstream 400 this helper exists to prevent.
	req.ReasoningContext = codexReasoningContextAllTurns
}

// ensureCodexParallelToolCalls forces parallel_tool_calls=false on the codex path.
//
// The upstream added a hard pairing rule: a request carrying
// X-OpenAI-Internal-Codex-Responses-Lite: true MUST also set parallel_tool_calls=false,
// else it is rejected outright:
//
//	400 X-OpenAI-Internal-Codex-Responses-Lite requires `parallel_tool_calls` to be false.
//
// applyCodexHeaderDefaultsWithFingerprint synthesizes that header for every codex
// outbound (oct adds it itself; it is not forwarded from the client), so oct owns the
// paired body field too — filling it here keeps header and body consistent instead of
// shipping a self-contradictory request. That makes this a shape-SAFE change that moves
// the outbound CLOSER to the real codex CLI, exactly like ensureCodexReasoningContext.
//
// Unlike reasoning.context (which accepts only "all_turns" but still lets a genuine
// codex CLI's explicit value pass through), the upstream here demands a single hard
// value — false — so an explicit client `true` MUST be overridden to false, exactly
// like normalizeCodexReasoningEffort overrides unsupported effort values. A genuine
// codex CLI already sends parallel_tool_calls=false, so this only coerces the
// incoherent true/empty case.
//
// This MUST run AFTER ensureCodexAgentContext so it has the final word on the field:
// ensureCodexAgentContext only sets false when nil + tools present + no raw tools,
// leaving an explicit client `true` OR a no-tools request still 400ing. Placed
// alongside ensureCodexReasoningContext it keeps every Lite-header pairing rule in
// one shaper (Ponytail: one canonical seam, no duplicated wheel in modeltest).
//
// Deliberately NOT done in ConvertToResponsesRequest: that transformer also serves
// plain (non-codex) Responses channels, which never get the Lite header and whose
// upstreams may handle the field differently. Living in the codex shaper keeps every
// other channel's bytes untouched.
func ensureCodexParallelToolCalls(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	// Ghost-pairing guard, mirroring ensureCodexReasoningContext: without the Lite
	// header there is no pairing rule, so an unset field stays unset and a client's
	// explicit true stays true instead of being coerced by a rule that does not apply.
	if !codexResponsesLiteApplies(req.Model) {
		return
	}
	// An explicit false is what we want — keep it (no-op).
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		return
	}
	// nil (omitempty would drop it -> 400) OR explicit true (upstream rejects it) -> force false.
	falseVal := false
	req.ParallelToolCalls = &falseVal
}

var gpt56CodexEffortAllowSet = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
}

func normalizeCodexReasoningEffort(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if isGPT56Model(req.Model) {
		if effort == "" {
			req.ReasoningEffort = "high"
			return
		}
		if !gpt56CodexEffortAllowSet[effort] {
			req.ReasoningEffort = "low"
			return
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

// bridgePlainResponsesCodexHistory rebuilds the full input from octopus's stored transcript
// for a plain responses client that continues via previous_response_id on a codex-fingerprint
// responses channel. Such a channel is forced store=false (prepareCodexRequestShape), so the
// upstream keeps NO server-side state and previous_response_id alone cannot recover the prior
// turn — the input must carry the whole conversation.
//
// Tool-output continuations ARE rebuilt (a codex-style agent sends only the function_call_output
// increment after its first tool call). Without the rebuild, forcing store=false left a bare
// function_call_output whose matching function_call lived only in the now-discarded server state,
// so the upstream rejected the turn with "No tool call found for function call output with
// call_id ...". The stored transcript retains the assistant message that issued the matching
// tool_call, so the rebuilt [..., assistant(tool_call), tool(output)] resynthesizes to a paired
// function_call + function_call_output (synthesizeCodexResponsesInputRaw) and previous_response_id
// is dropped — mirroring bridgeResponsesHistoryForChat and CLIProxyAPI's full-input rebuild.
// applyPlainResponsesCodexHistoryForPreviousResponseID leaves the request untouched when the
// current turn already carries assistant context or the transcript is unavailable (no worse than
// forwarding it as-is).
func (ra *relayAttempt) bridgePlainResponsesCodexHistory() {
	if ra == nil || ra.internalRequest == nil || !ra.shouldBridgePlainResponsesCodexHistory() {
		return
	}
	req := ra.internalRequest
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
	req.Messages = dropUnpairedToolItems(req.Messages)
	req.PreviousResponseID = nil
	req.ResponsesInputRaw = nil
}

// dropUnpairedToolItems removes tool items that cannot be paired within the rebuilt message list.
// The stored transcript is trimmed from the FRONT (trimResponsesSessionTranscript:
// maxResponsesSessionTranscriptMessages / char cap), which can split an old
// [assistant(tool_call), tool(output)] pair; and the codex/Anthropic history bridge does not run
// through normalizeChatToolCallPairing. So this applies the BOTH-DIRECTION pruning that sub2api's
// normalizeChatMessages / normalizeAnthropicToolPairing and CLIProxyAPI's repairResponsesToolCallsArray
// apply once previous_response_id is dropped and there is no server-side state to resolve a missing half:
//   - an ORPHAN OUTPUT — a tool reply whose tool_call_id no assistant announced — is dropped; the
//     store=false responses upstream rejects it ("No tool call found for function call output with
//     call_id ..."); and
//   - an UNANSWERED CALL — an assistant tool_call that no tool reply answers — is pruned; a stateless
//     Anthropic upstream rejects a tool_use that has no matching tool_result. An assistant left with
//     neither a kept tool_call nor any text is dropped entirely.
//
// Bare tool messages (no tool_call_id) and non-tool messages pass through, and message ORDER is
// preserved (this filters; it does not re-pair/reorder like normalizeChatToolCallPairing).
func dropUnpairedToolItems(messages []transformerModel.Message) []transformerModel.Message {
	announced := make(map[string]struct{}) // tool_call ids an assistant issued
	answered := make(map[string]struct{})  // tool_call_ids a tool reply answered
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			if id := strings.TrimSpace(tc.ID); id != "" {
				announced[id] = struct{}{}
			}
		}
		if strings.EqualFold(strings.TrimSpace(msg.Role), "tool") && msg.ToolCallID != nil {
			if id := strings.TrimSpace(*msg.ToolCallID); id != "" {
				answered[id] = struct{}{}
			}
		}
	}
	out := make([]transformerModel.Message, 0, len(messages))
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "tool") && msg.ToolCallID != nil {
			if id := strings.TrimSpace(*msg.ToolCallID); id != "" {
				if _, ok := announced[id]; !ok {
					continue // orphan tool output — its function_call was trimmed away; drop it
				}
			}
			out = append(out, msg)
			continue
		}
		if len(msg.ToolCalls) > 0 {
			kept := make([]transformerModel.ToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				id := strings.TrimSpace(tc.ID)
				if id == "" {
					continue // an id-less call can never be answered by tool_call_id; drop it
				}
				if _, ok := answered[id]; ok {
					kept = append(kept, tc)
				}
			}
			if len(kept) == 0 && strings.TrimSpace(messageTextContent(msg.Content)) == "" {
				continue // assistant left with no answered tool_call and no text — drop it
			}
			msg.ToolCalls = kept
			out = append(out, msg)
			continue
		}
		out = append(out, msg)
	}
	return out
}

func responsesMessagesContainToolOutput(messages []transformerModel.Message) bool {
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "tool") {
			return true
		}
	}
	return false
}

// responsesMessagesAlreadyCarryAssistantContext reports whether the current turn already replays
// real assistant conversation context, in which case the transcript rebuild is skipped (the client
// carried the history itself). A REASONING-ONLY assistant (no text, no tool_calls) does NOT count:
// it is the encrypted_content reasoning item octopus surfaces to the client, flushed back into a
// standalone assistant by the inbound converter. A client that echoes it ahead of its
// function_call_output increment ([reasoning, function_call_output]) would otherwise trip this gate,
// skip the rebuild, and ship a bare function_call_output that the store=false upstream rejects with
// "No tool call found for function call output ...". Only assistant TEXT or TOOL_CALLS short-circuit
// the rebuild.
func responsesMessagesAlreadyCarryAssistantContext(messages []transformerModel.Message) bool {
	for _, msg := range messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			continue
		}
		if strings.TrimSpace(messageTextContent(msg.Content)) != "" || len(msg.ToolCalls) > 0 {
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
	var singleReplyID string
	replyCount := 0
	for _, m := range messages {
		if id := toolReplyID(m); id != "" {
			replies[id] = m
			singleReplyID = id
			replyCount++
		}
	}

	totalToolCalls := 0
	for _, m := range messages {
		if strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			totalToolCalls += len(m.ToolCalls)
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
				if id == "" && replyCount == 1 && totalToolCalls == 1 {
					id = singleReplyID
				}
				if id == "" {
					continue
				}
				if _, ok := replies[id]; ok {
					tc.ID = id // canonicalize to the trimmed id so the emitted call matches its reply exactly
					kept = append(kept, tc)
				} else if replyCount == 1 && totalToolCalls == 1 {
					matchedReply := replies[singleReplyID]
					delete(replies, singleReplyID)
					tc.ID = id
					replies[id] = matchedReply
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
		case "function_call_output", "function_call", "reasoning",
			// Native codex tool-call / tool-output increments — kept in lockstep with the inbound
			// parser's isResponsesToolCallItemType / isResponsesToolOutputItemType
			// (transformer/inbound/openai/response.go). A genuine codex custom/mcp/tool-search/
			// local-shell continuation whose raw input is only one of these increments must be
			// recognized as codex-shaped and passed through untouched: resynthesis rewrites it into
			// a generic function_call/function_call_output and loses the byte fidelity the upstream
			// requires for custom/mcp tool history.
			"tool_call", "local_shell_call", "tool_search_call", "custom_tool_call", "mcp_tool_call",
			"tool_search_output", "custom_tool_call_output", "mcp_tool_call_output":
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

// EnsureCodexReasoningContext is the exported wrapper around ensureCodexReasoningContext
// for callers outside the relay package (notably internal/modeltest). The modeltest path
// also synthesizes X-OpenAI-Internal-Codex-Responses-Lite via applyCodexHeaderDefaults,
// so it MUST run the same context normalization the relay forward path runs, or the
// upstream rejects its probe with the same
// `requires reasoning.context to be all_turns` 400 the relay path used to 400 on.
// See prepareCodexModelTestShape in internal/modeltest/runner.go.
func EnsureCodexReasoningContext(req *transformerModel.InternalLLMRequest) {
	ensureCodexReasoningContext(req)
}

// NormalizeCodexReasoningEffort is the exported wrapper around
// normalizeCodexReasoningEffort for callers outside the relay package (notably
// internal/modeltest). Same reasoning as EnsureCodexReasoningContext: the modeltest
// path also injects the Lite header, so the upstream applies the same effort rule
// and would 400 with `level "minimal" not supported` without this remap.
func NormalizeCodexReasoningEffort(req *transformerModel.InternalLLMRequest) {
	normalizeCodexReasoningEffort(req)
}

// EnsureCodexParallelToolCalls is the exported wrapper around
// ensureCodexParallelToolCalls for callers outside the relay package (notably
// internal/modeltest). Same reasoning as EnsureCodexReasoningContext: the modeltest
// path also injects the Lite header, so the upstream applies the same hard pairing
// rule and would 400 the probe with the same
// `requires parallel_tool_calls to be false` 400 the relay path used to 400 on.
// See prepareCodexModelTestShape in internal/modeltest/runner.go.
func EnsureCodexParallelToolCalls(req *transformerModel.InternalLLMRequest) {
	ensureCodexParallelToolCalls(req)
}
