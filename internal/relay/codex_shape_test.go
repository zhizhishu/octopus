package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// A client that explicitly set store=true on a codex-shaped responses channel is incompatible
// with the reasoning.encrypted_content include the shape adds (store=true + that include makes
// the genuine upstream 500 once real reasoning is produced). prepareCodexRequestShape must
// coerce store back to false, not honor the incoherent store=true.
func TestPrepareCodexRequestShapeForcesStoreFalseOverExplicitTrue(t *testing.T) {
	content := "Say OK only"
	storeTrue := true
	req := &model.InternalLLMRequest{
		Model:        "gpt-5.6-sol",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Store:        &storeTrue,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.Store == nil || *req.Store {
		t.Fatalf("codex shape must coerce an explicit store=true back to false (encrypted_content include requires store=false), got %#v", req.Store)
	}
	if !containsString(req.Include, "reasoning.encrypted_content") {
		t.Fatalf("expected reasoning encrypted content include, got %#v", req.Include)
	}
}

// A genuine codex CLI always sends reasoning:{effort,summary:"auto"}; the summary field is
// what makes a Responses upstream stream reasoning-summary deltas *during* a long reasoning
// turn. oct historically dropped it (the reasoning struct had no summary field), so a
// max-effort turn over a large context streamed nothing to the client until the final
// message and looked frozen. prepareCodexRequestShape must default reasoning.summary="auto"
// when the client left it empty.
func TestPrepareCodexRequestShapeDefaultsReasoningSummaryAuto(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:        "gpt-5.6-sol",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.ReasoningSummary != "auto" {
		t.Fatalf("expected codex shape to default reasoning.summary=auto, got %q", req.ReasoningSummary)
	}
}

// A client that explicitly picked a reasoning.summary level owns it; the codex default must
// never overwrite an explicit choice.
func TestPrepareCodexRequestShapeKeepsExplicitReasoningSummary(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		RawAPIFormat:     model.APIFormatOpenAIResponse,
		ReasoningSummary: "detailed",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.ReasoningSummary != "detailed" {
		t.Fatalf("expected explicit reasoning.summary to be preserved, got %q", req.ReasoningSummary)
	}
}

// Real codex never emits a summary-only reasoning object. For a non-reasoning model on a
// codex-fingerprint channel where no effort is present (normalizeCodexReasoningEffort only
// fills effort for the gpt-5.6 family), the summary default must be skipped so no bare
// reasoning:{summary} materializes on the wire.
func TestPrepareCodexRequestShapeSkipsSummaryWhenNoEffort(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:        "gpt-4o",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.ReasoningEffort != "" {
		t.Fatalf("precondition: non-5.6 model with no effort should keep empty effort, got %q", req.ReasoningEffort)
	}
	if req.ReasoningSummary != "" {
		t.Fatalf("expected no summary default without an effort (avoid summary-only reasoning object), got %q", req.ReasoningSummary)
	}
}

func TestPrepareCodexRequestShapeSynthesizesPlainResponsesInput(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:             "gpt-5.5",
		RawAPIFormat:      model.APIFormatOpenAIResponse,
		ResponsesInputRaw: json.RawMessage(`"Say OK only"`),
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.Store == nil || *req.Store {
		t.Fatalf("expected Codex-shaped responses request to default store=false")
	}
	if !containsString(req.Include, "reasoning.encrypted_content") {
		t.Fatalf("expected reasoning encrypted content include, got %#v", req.Include)
	}
	if len(req.ResponsesTextRaw) != 0 {
		t.Fatalf("expected no text verbosity injection when fast mode is off by default, got %s", string(req.ResponsesTextRaw))
	}
	if req.ReasoningEffort != "" {
		t.Fatalf("expected no forced reasoning effort when fast mode is off by default, got %q", req.ReasoningEffort)
	}
	if strings.Contains(string(req.ResponsesInputRaw), `"Say OK only"`) && strings.HasPrefix(strings.TrimSpace(string(req.ResponsesInputRaw)), `"`) {
		t.Fatalf("expected text shorthand to be replaced, got %s", string(req.ResponsesInputRaw))
	}
	assertCodexInputRaw(t, req.ResponsesInputRaw, "Say OK only")
}

// TestPrepareCodexRequestShapeRebuildsToolOutputContinuation verifies that a tool-output
// continuation on a codex-fingerprint responses channel (forced store=false) is rebuilt from the
// stored transcript into a full, self-contained input: previous_response_id is dropped and the
// synthesized input carries the assistant's function_call PAIRED with the incoming
// function_call_output. Before the fix the bridge bailed on any tool output and forwarded a bare
// function_call_output, which the store=false upstream rejected with
// "No tool call found for function call output with call_id ...". Default Codex tools/tool_choice
// are still never injected onto a tool-output turn.
func TestPrepareCodexRequestShapeRebuildsToolOutputContinuation(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_tool_parent"
	callID := "call_1"
	// The stored transcript retains the assistant turn that ISSUED the matching tool_call, so the
	// rebuild can pair it with the incoming function_call_output (the real sticky flow).
	recordResponsesSessionTranscript(previous, []model.Message{{
		Role: "assistant",
		ToolCalls: []model.ToolCall{{
			ID:   callID,
			Type: "function",
			Function: model.FunctionCall{
				Name:      "shell_command",
				Arguments: `{"command":"ls"}`,
			},
		}},
	}})
	output := "tool result text"
	req := &model.InternalLLMRequest{
		Model:              "gpt-5.5",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &previous,
		ResponsesInputRaw:  json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":"tool result text"}]`),
		Messages: []model.Message{{
			Role:       "tool",
			ToolCallID: &callID,
			Content:    model.MessageContent{Content: &output},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.PreviousResponseID != nil {
		t.Fatalf("tool output continuation must drop previous_response_id after rebuild, got %#v", req.PreviousResponseID)
	}
	var items []map[string]any
	if err := json.Unmarshal(req.ResponsesInputRaw, &items); err != nil {
		t.Fatalf("rebuilt responses input not valid JSON: %v raw=%s", err, string(req.ResponsesInputRaw))
	}
	var pairedCall, pairedOutput bool
	for _, item := range items {
		switch item["type"] {
		case "function_call":
			if item["call_id"] == callID {
				pairedCall = true
			}
		case "function_call_output":
			if item["call_id"] == callID {
				pairedOutput = true
			}
		}
	}
	if !pairedCall || !pairedOutput {
		t.Fatalf("expected paired function_call + function_call_output for %s (no dangling output), got %s", callID, string(req.ResponsesInputRaw))
	}
	if len(req.Tools) != 0 || req.ToolChoice != nil {
		t.Fatalf("tool output continuation must not inject default Codex tools/tool_choice, tools=%#v choice=%#v", req.Tools, req.ToolChoice)
	}
}

// TestBridgeRebuildsReasoningPrefixedToolOutputIncrement covers the reasoning-gate fix: a client
// that echoes the encrypted reasoning item (flushed to a reasoning-only assistant) ahead of its
// function_call_output increment must STILL trigger the transcript rebuild. Before the fix
// responsesMessagesAlreadyCarryAssistantContext saw the reasoning-only assistant, declared context
// already present, skipped the rebuild, and shipped a bare function_call_output → store=false 400.
func TestBridgeRebuildsReasoningPrefixedToolOutputIncrement(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_reasoning_tool_parent"
	callID := "call_reasoning_1"
	recordResponsesSessionTranscript(previous, []model.Message{{
		Role: "assistant",
		ToolCalls: []model.ToolCall{{
			ID:   callID,
			Type: "function",
			Function: model.FunctionCall{
				Name:      "f",
				Arguments: "{}",
			},
		}},
	}})
	reasoning := "prior encrypted reasoning"
	output := "tool result"
	req := &model.InternalLLMRequest{
		Model:              "gpt-5.6-sol",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &previous,
		// The client echoes the reasoning item (a reasoning-only assistant, no text/tool_calls)
		// ahead of the function_call_output increment — this used to trip the assistant-context gate.
		Messages: []model.Message{
			{Role: "assistant", ReasoningContent: &reasoning},
			{Role: "tool", ToolCallID: &callID, Content: model.MessageContent{Content: &output}},
		},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.PreviousResponseID != nil {
		t.Fatalf("reasoning-prefixed tool-output increment must still rebuild (drop previous_response_id), got %#v", req.PreviousResponseID)
	}
	var items []map[string]any
	if err := json.Unmarshal(req.ResponsesInputRaw, &items); err != nil {
		t.Fatalf("rebuilt input not valid JSON: %v raw=%s", err, string(req.ResponsesInputRaw))
	}
	var pairedCall, pairedOutput bool
	for _, item := range items {
		switch item["type"] {
		case "function_call":
			if item["call_id"] == callID {
				pairedCall = true
			}
		case "function_call_output":
			if item["call_id"] == callID {
				pairedOutput = true
			}
		}
	}
	if !pairedCall || !pairedOutput {
		t.Fatalf("expected paired function_call + function_call_output for %s after rebuild, got %s", callID, string(req.ResponsesInputRaw))
	}
}

func TestSynthesizedCodexToolOutputCursorCanRecoverWithTranscript(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_tool_parent"
	callID := "call_1"
	recordResponsesSessionTranscript(previous, []model.Message{{
		Role: "assistant",
		ToolCalls: []model.ToolCall{{
			ID:   callID,
			Type: "function",
			Function: model.FunctionCall{
				Name:      "echo_code",
				Arguments: `{"code":"TOOL"}`,
			},
		}},
	}})
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType: inbound.InboundTypeOpenAIResponse,
			internalRequest: &model.InternalLLMRequest{
				Model:              "gpt-5.5",
				RawAPIFormat:       model.APIFormatOpenAIResponse,
				PreviousResponseID: &previous,
				Messages: []model.Message{{
					Role:       "tool",
					ToolCallID: &callID,
					Content:    model.MessageContent{Content: stringPtrForCodexShapeTest("TOOL")},
				}},
			},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}
	err := newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"invalid codex request"}}`))
	if !ra.shouldRecoverSynthesizedCodexResponsesCursor(http.StatusBadRequest, err) {
		t.Fatalf("plain synthesized tool-output continuation should be recoverable with transcript")
	}
	ra.applyPlainResponsesCodexHistoryForPreviousResponseID(previous)
	if ra.internalRequest.PreviousResponseID != nil {
		t.Fatalf("expected transcript replay to drop previous_response_id")
	}
	if len(ra.internalRequest.Messages) != 2 ||
		len(ra.internalRequest.Messages[0].ToolCalls) != 1 ||
		ra.internalRequest.Messages[1].Role != "tool" {
		t.Fatalf("expected assistant tool call plus tool output replay, got %#v", ra.internalRequest.Messages)
	}
}

// TestDropUnpairedToolItems covers the both-direction pairing guard applied after a codex/Anthropic
// transcript rebuild (mirrors sub2api normalizeAnthropicToolPairing / CLIProxyAPI
// repairResponsesToolCallsArray): an ORPHAN OUTPUT (tool reply whose assistant tool_call was trimmed
// off the front) is dropped, and an UNANSWERED CALL (assistant tool_call with no reply) is pruned —
// an assistant left with neither a kept call nor text is dropped entirely. Paired items, a bare
// id-less tool message, and plain messages survive.
func TestDropUnpairedToolItems(t *testing.T) {
	sp := stringPtrForCodexShapeTest
	callX := "call_X" // answered → kept
	callY := "call_Y" // orphan output → dropped
	callZ := "call_Z" // unanswered call on an assistant that also has callX → callZ pruned, assistant survives
	callW := "call_W" // unanswered call, sole content of its assistant → whole assistant dropped
	msgs := []model.Message{
		{Role: "user", Content: model.MessageContent{Content: sp("hi")}},
		{Role: "tool", ToolCallID: &callY, Content: model.MessageContent{Content: sp("orphan result")}},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: callX, Type: "function", Function: model.FunctionCall{Name: "f", Arguments: "{}"}},
			{ID: callZ, Type: "function", Function: model.FunctionCall{Name: "g", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: &callX, Content: model.MessageContent{Content: sp("real result")}},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: callW, Type: "function", Function: model.FunctionCall{Name: "h", Arguments: "{}"}},
		}},
		{Role: "tool", Content: model.MessageContent{Content: sp("bare tool, no id")}}, // ToolCallID nil
	}

	out := dropUnpairedToolItems(msgs)

	var sawY, sawX, sawUser, sawBare bool
	var assistants int
	var survivorHasX, survivorHasZ, survivorHasW bool
	for _, m := range out {
		switch {
		case m.Role == "tool" && m.ToolCallID != nil && *m.ToolCallID == callY:
			sawY = true
		case m.Role == "tool" && m.ToolCallID != nil && *m.ToolCallID == callX:
			sawX = true
		case m.Role == "tool" && m.ToolCallID == nil:
			sawBare = true
		case m.Role == "user":
			sawUser = true
		case m.Role == "assistant":
			assistants++
			for _, tc := range m.ToolCalls {
				switch tc.ID {
				case callX:
					survivorHasX = true
				case callZ:
					survivorHasZ = true
				case callW:
					survivorHasW = true
				}
			}
		}
	}
	if sawY {
		t.Fatalf("expected orphan tool output %s to be dropped, got %#v", callY, out)
	}
	if !sawX || !sawUser || !sawBare {
		t.Fatalf("expected paired output %s + user + bare id-less tool to survive, got %#v", callX, out)
	}
	if !survivorHasX {
		t.Fatalf("expected the assistant announcing answered call %s to survive with it, got %#v", callX, out)
	}
	if survivorHasZ {
		t.Fatalf("expected unanswered call %s pruned from its assistant, got %#v", callZ, out)
	}
	if survivorHasW || assistants != 1 {
		t.Fatalf("expected the callW-only assistant dropped entirely (exactly 1 assistant survives), got %d: %#v", assistants, out)
	}
}

func stringPtrForCodexShapeTest(value string) *string {
	return &value
}

func TestNormalizeCodexReasoningEffort(t *testing.T) {
	cases := []struct {
		name   string
		effort string
		model  string
		want   string
	}{
		{"max on gpt-5.6 stays max", "max", "gpt-5.6-sol", "max"},
		{"max on bare gpt-5.6 stays max", "max", "gpt-5.6", "max"},
		{"max on gpt-5.5 becomes xhigh", "max", "gpt-5.5", "xhigh"},
		{"max on prefixed dated gpt-5.6 stays max", "max", "openai/gpt-5.6-terra-2026-07-09", "max"},
		{"max on gpt-5.60 (not 5.6 family) becomes xhigh", "max", "gpt-5.60", "xhigh"},
		{"max on gpt-5.6foo (not 5.6 family) becomes xhigh", "max", "gpt-5.6foo", "xhigh"},
		{"high on gpt-5.5 passes through", "high", "gpt-5.5", "high"},
		{"empty effort untouched", "", "gpt-5.5", ""},
		// GPT-5.6: faithful passthrough of any explicit level the client chose; only a
		// completely unspecified (empty) effort defaults to "high" so an omitted field still reasons.
		{"none on gpt-5.6 passes through", "none", "gpt-5.6-sol", "none"},
		{"low on gpt-5.6 passes through", "low", "gpt-5.6-sol", "low"},
		{"empty on gpt-5.6 defaults to high", "", "gpt-5.6-sol", "high"},
		{"minimal on gpt-5.6 passes through", "minimal", "gpt-5.6-terra", "minimal"},
		{"medium on gpt-5.6 preserved", "medium", "gpt-5.6-sol", "medium"},
		{"high on gpt-5.6 preserved", "high", "gpt-5.6-sol", "high"},
		{"xhigh on gpt-5.6 preserved", "xhigh", "gpt-5.6-sol", "xhigh"},
		{"low on gpt-5.5 passes through (only 5.6 defaults)", "low", "gpt-5.5", "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &model.InternalLLMRequest{Model: tc.model, ReasoningEffort: tc.effort}
			normalizeCodexReasoningEffort(req)
			if req.ReasoningEffort != tc.want {
				t.Fatalf("normalizeCodexReasoningEffort(effort=%q, model=%q) = %q, want %q",
					tc.effort, tc.model, req.ReasoningEffort, tc.want)
			}
		})
	}
}

func TestResponsesInputRawLooksCodexShapedRecognizesReasoningToolTurn(t *testing.T) {
	// Incremental tool turn: reasoning (carrying encrypted_content) plus a
	// function_call_output, with no message item. This is exactly what the
	// Codex client sends between tool calls and must be recognized as already
	// Codex-shaped so it is forwarded untouched.
	raw := json.RawMessage(`[{"type":"reasoning","encrypted_content":"BLOB","summary":[]},{"type":"function_call_output","call_id":"call_1","output":"hi"}]`)
	if !responsesInputRawLooksCodexShaped(raw) {
		t.Fatalf("expected native Codex reasoning/tool-output turn to be recognized as Codex-shaped")
	}
}

func TestResponsesInputRawLooksCodexShapedRegression(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"message with content still recognized", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`, true},
		{"standalone function_call recognized", `[{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"}]`, true},
		{"empty array still resynthesized", `[]`, false},
		{"non-array text shorthand still resynthesized", `"Say OK only"`, false},
		{"message without content still resynthesized", `[{"type":"message","role":"user"}]`, false},
		{"unrecognized item only still resynthesized", `[{"type":"something_else"}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesInputRawLooksCodexShaped(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("responsesInputRawLooksCodexShaped(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestPrepareCodexRequestShapePreservesIncrementalReasoningToolInput(t *testing.T) {
	callID := "call_1"
	output := "hi"
	req := &model.InternalLLMRequest{
		Model:             "gpt-5.5",
		RawAPIFormat:      model.APIFormatOpenAIResponse,
		ResponsesInputRaw: json.RawMessage(`[{"type":"reasoning","encrypted_content":"BLOB","summary":[]},{"type":"function_call_output","call_id":"call_1","output":"hi"}]`),
		Messages: []model.Message{{
			Role:       "tool",
			ToolCallID: &callID,
			Content:    model.MessageContent{Content: &output},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	// The native Codex reasoning + tool-output input must be forwarded
	// untouched: re-synthesizing would drop reasoning/encrypted_content and
	// change the byte order, breaking the upstream prompt cache prefix.
	var items []map[string]any
	if err := json.Unmarshal(req.ResponsesInputRaw, &items); err != nil {
		t.Fatalf("expected input array, got %s: %v", string(req.ResponsesInputRaw), err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 input items preserved, got %#v", items)
	}
	body := string(req.ResponsesInputRaw)
	if !strings.Contains(body, `"reasoning"`) ||
		!strings.Contains(body, `"encrypted_content"`) ||
		!strings.Contains(body, "BLOB") {
		t.Fatalf("expected reasoning/encrypted_content to be preserved, got %s", body)
	}
}

// TestPrepareCodexRequestShapeSelfContainedContinuationSuppressesHoist 钉死真 codex 续接轮的
// self-contained 行为：input 自带 developer/system 指令 + 工具输出增量（真 codex store=false 全量
// 回放的形状，实抓包证据见 _references axonhub reasoning.request.json）时，必须走 suppress 分支——
// 不注入默认 agent 上下文、不把 Messages 派生的 instructions/tools 重复 hoist 到顶层（+27KB 病灶）。
// 此前该行为只是 correct-by-construction：改 inbound 的 instructions→system 映射或
// messagesContainInstruction 会静默破坏而无测试报红，故补此回归。
func TestPrepareCodexRequestShapeSelfContainedContinuationSuppressesHoist(t *testing.T) {
	callID := "call_1"
	output := "ok"
	instr := "You are a coding agent operating in this workspace."
	rawInput := `[{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are a coding agent operating in this workspace."}]},{"type":"function_call_output","call_id":"call_1","output":"ok"}]`
	req := &model.InternalLLMRequest{
		Model:             "gpt-5.5",
		RawAPIFormat:      model.APIFormatOpenAIResponse,
		ResponsesInputRaw: json.RawMessage(rawInput),
		Messages: []model.Message{
			{
				// inbound 把 input 里的 developer 指令归一成 system 消息（或顶层 instructions
				// 前插 system）——self-contained 判定依赖的就是这条。
				Role:    "system",
				Content: model.MessageContent{Content: &instr},
			},
			{
				Role:       "tool",
				ToolCallID: &callID,
				Content:    model.MessageContent{Content: &output},
			},
		},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	// self-contained 轮绝不能注入默认 codex 身份（那是 !codexSelfContained 分支的事）。
	for i, msg := range req.Messages {
		if msg.Content.Content != nil && strings.Contains(*msg.Content.Content, "You are Codex, a coding agent based on GPT-5") {
			t.Fatalf("default codex agent context must NOT be injected on a self-contained turn; found at Messages[%d]", i)
		}
	}
	// suppress 必须生效：客户端没发顶层 instructions（ResponsesInstructions 原为 nil）→ 强制 ""
	// 让 omitempty 丢弃，阻止 Messages 派生的顶层 instructions 重复 hoist。
	if req.ResponsesInstructions == nil || *req.ResponsesInstructions != "" {
		got := "<nil>"
		if req.ResponsesInstructions != nil {
			got = *req.ResponsesInstructions
		}
		t.Fatalf("expected suppressed (empty) top-level instructions on self-contained turn, got %q", got)
	}
	// 同理 tools：无客户端顶层 tools（ResponsesToolsRaw 空）时 Messages 派生的 req.Tools 必须清空。
	if req.Tools != nil {
		t.Fatalf("expected Messages-derived tools cleared on self-contained turn, got %#v", req.Tools)
	}
	// input 原字节零改写（前缀缓存/指纹都指着它）。
	if string(req.ResponsesInputRaw) != rawInput {
		t.Fatalf("expected raw input preserved byte-for-byte, got %s", string(req.ResponsesInputRaw))
	}

	// 变体：客户端显式发了顶层 instructions（非 Messages 派生的 hoist）→ 不是重复，绝不能被抹掉。
	explicit := "client sent this top-level"
	req2 := &model.InternalLLMRequest{
		Model:                 "gpt-5.5",
		RawAPIFormat:          model.APIFormatOpenAIResponse,
		ResponsesInputRaw:     json.RawMessage(rawInput),
		ResponsesInstructions: &explicit,
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: &instr}},
			{Role: "tool", ToolCallID: &callID, Content: model.MessageContent{Content: &output}},
		},
	}
	ra2 := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req2,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}
	ra2.prepareCodexRequestShape()
	if req2.ResponsesInstructions == nil || *req2.ResponsesInstructions != explicit {
		t.Fatalf("client-sent top-level instructions must be preserved, got %#v", req2.ResponsesInstructions)
	}
}

func TestOpenAIChatCanRouteThroughCodexResponsesShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	// Codex fast mode (verbosity/effort low) is now opt-in.
	if err := op.SettingSetString(dbmodel.SettingKeyCodexFastMode, "true"); err != nil {
		t.Fatalf("set codex fast mode: %v", err)
	}

	var (
		mu         sync.Mutex
		gotPath    string
		gotHeaders = http.Header{}
		gotBody    map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		gotBody = body
		mu.Unlock()
		writeCodexShapeResponsesSSE(w, "mapped-gpt-5.5")
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "codex-responses-for-chat",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "gpt-5.5", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "mapped-gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"Say OK only"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected chat via Codex Responses channel to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"OK"`) {
		t.Fatalf("expected chat-shaped downstream OK response, got %s", rec.Body.String())
	}
	mu.Lock()
	path, headers, body := gotPath, gotHeaders.Clone(), gotBody
	mu.Unlock()
	assertCodexUpstreamRequest(t, path, headers, body, "mapped-gpt-5.5", "Say OK only")
}

func TestPlainResponsesRoutesThroughCodexShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	// Codex fast mode (verbosity/effort low) is now opt-in.
	if err := op.SettingSetString(dbmodel.SettingKeyCodexFastMode, "true"); err != nil {
		t.Fatalf("set codex fast mode: %v", err)
	}

	var (
		mu         sync.Mutex
		gotPath    string
		gotHeaders = http.Header{}
		gotBody    map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		gotBody = body
		mu.Unlock()
		writeCodexShapeResponsesSSE(w, "mapped-gpt-5.5")
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "codex-responses-for-plain-responses",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "plain-gpt-5.5", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "mapped-gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"plain-gpt-5.5",
		"input":"Say OK only",
		"stream":false
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected plain responses via Codex shape to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"object":"response"`) || !strings.Contains(rec.Body.String(), `"OK"`) {
		t.Fatalf("expected responses-shaped downstream OK response, got %s", rec.Body.String())
	}
	mu.Lock()
	path, headers, body := gotPath, gotHeaders.Clone(), gotBody
	mu.Unlock()
	assertCodexUpstreamRequest(t, path, headers, body, "mapped-gpt-5.5", "Say OK only")
}

func TestPlainResponsesCodexShapeRetriesWithoutCursorOnGeneric400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if _, ok := body["previous_response_id"]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid codex request"}}`))
			return
		}
		writeCodexShapeResponsesSSE(w, "mapped-gpt-5.5")
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "codex-responses-cursor-retry",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if len(channel.Keys) == 0 || channel.Keys[0].ID == 0 {
		t.Fatalf("expected channel key id to be populated, got %#v", channel.Keys)
	}
	recordResponsesSessionWithContext(ctx, "resp_plain_prev", channel.ID, channel.Keys[0].ID)
	group := dbmodel.Group{Name: "plain-gpt-5.5-cursor", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "mapped-gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"plain-gpt-5.5-cursor",
		"previous_response_id":"resp_plain_prev",
		"input":"Say OK only",
		"stream":false
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry without cursor to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected two upstream attempts, got %d bodies=%#v", len(bodies), bodies)
	}
	if _, ok := bodies[0]["previous_response_id"]; !ok {
		t.Fatalf("expected first attempt to include previous_response_id, got %#v", bodies[0])
	}
	if _, ok := bodies[1]["previous_response_id"]; ok {
		t.Fatalf("expected retry to drop previous_response_id, got %#v", bodies[1])
	}
}

func TestPlainResponsesCodexShapeReplaysTranscriptForPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	clearResponsesSessionCacheForTest()

	var (
		mu          sync.Mutex
		secondBody  map[string]any
		requestSeen int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		requestSeen++
		seen := requestSeen
		if seen == 2 {
			secondBody = body
		}
		mu.Unlock()
		if seen == 1 {
			writeCodexShapeResponsesSSEWithText(w, "resp_plain_bridge_1", "mapped-gpt-5.5", "OK")
			return
		}
		writeCodexShapeResponsesSSEWithText(w, "resp_plain_bridge_2", "mapped-gpt-5.5", "CTX-BRIDGE-KEEP")
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "codex-responses-history-bridge",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := dbmodel.Group{Name: "plain-gpt-5.5-history", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: "mapped-gpt-5.5",
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}

	first := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(first)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"plain-gpt-5.5-history",
		"input":"Remember CTX-BRIDGE-KEEP. Reply OK only.",
		"stream":true
	}`))
	req1.Header.Set("Content-Type", "application/json")
	c1.Request = req1
	c1.Set("api_key_id", 0)
	c1.Set("user_id", 0)
	c1.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIResponse, c1)
	if first.Code != http.StatusOK {
		t.Fatalf("first turn failed: %d %s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(second)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"plain-gpt-5.5-history",
		"previous_response_id":"resp_plain_bridge_1",
		"input":"What codeword did I ask you to remember? Reply only the codeword.",
		"stream":true
	}`))
	req2.Header.Set("Content-Type", "application/json")
	c2.Request = req2
	c2.Set("api_key_id", 0)
	c2.Set("user_id", 0)
	c2.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIResponse, c2)
	if second.Code != http.StatusOK {
		t.Fatalf("second turn failed: %d %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "CTX-BRIDGE-KEEP") {
		t.Fatalf("expected replay-backed downstream answer, got %s", second.Body.String())
	}

	mu.Lock()
	body := secondBody
	mu.Unlock()
	if body == nil {
		t.Fatalf("missing second upstream body")
	}
	if _, ok := body["previous_response_id"]; ok {
		t.Fatalf("plain synthesized Codex retry should replay transcript instead of forwarding cursor, got %#v", body)
	}
	raw, _ := json.Marshal(body["input"])
	if !strings.Contains(string(raw), "Remember CTX-BRIDGE-KEEP") ||
		!strings.Contains(string(raw), "What codeword did I ask you to remember?") {
		t.Fatalf("expected second upstream input to include replayed history and current turn, got %s", string(raw))
	}
}

func TestSynthesizedCodexCursorRecoverySkipsRealCodexClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Originator", defaultCodexOriginator)
	previous := "resp_real_codex_prev"
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:           c,
			inboundType: inbound.InboundTypeOpenAIResponse,
			internalRequest: &model.InternalLLMRequest{
				PreviousResponseID: &previous,
			},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	err := newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"invalid codex request"}}`))
	if ra.shouldRecoverSynthesizedCodexResponsesCursor(http.StatusBadRequest, err) {
		t.Fatalf("real Codex inbound headers must not trigger synthesized cursor recovery")
	}
}

func assertCodexUpstreamRequest(t *testing.T, path string, headers http.Header, body map[string]any, wantModel, wantText string) {
	t.Helper()
	if path != "/v1/responses" {
		t.Fatalf("expected upstream /v1/responses, got %q", path)
	}
	if got := headers.Get("Originator"); got != defaultCodexOriginator {
		t.Fatalf("originator = %q, want %q", got, defaultCodexOriginator)
	}
	if got := headers.Get("User-Agent"); got != defaultCodexUserAgent {
		t.Fatalf("user-agent = %q, want %q", got, defaultCodexUserAgent)
	}
	if got := headers.Get("X-Codex-Beta-Features"); got != defaultCodexBetaFeatures {
		t.Fatalf("codex beta features = %q, want %q", got, defaultCodexBetaFeatures)
	}
	// codex 0.144.x always emits this static header on /responses (packet-verified); assert
	// the exact "true" so a dropped injection or a value flip is caught as a shape drift.
	if got := headers.Get("X-Openai-Internal-Codex-Responses-Lite"); got != "true" {
		t.Fatalf("codex responses-lite header = %q, want %q", got, "true")
	}
	if got := headers.Get("Accept"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected upstream SSE Accept header, got %q", got)
	}
	if headers.Get("X-Codex-Window-Id") == "" || headers.Get("X-Codex-Turn-Metadata") == "" {
		t.Fatalf("expected Codex session headers, got %#v", headers)
	}
	if body["model"] != wantModel {
		t.Fatalf("model = %#v, want %q", body["model"], wantModel)
	}
	if body["stream"] != true {
		t.Fatalf("expected upstream stream=true for Codex Responses compatibility, got %#v", body["stream"])
	}
	if body["store"] != false {
		t.Fatalf("expected store=false, got %#v", body["store"])
	}
	if !arrayContainsString(body["include"], "reasoning.encrypted_content") {
		t.Fatalf("expected include reasoning.encrypted_content, got %#v", body["include"])
	}
	text, ok := body["text"].(map[string]any)
	if !ok || text["verbosity"] != "low" {
		t.Fatalf("expected text.verbosity=low, got %#v", body["text"])
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Fatalf("expected reasoning.effort=low, got %#v", body["reasoning"])
	}
	if _, ok := body["client_metadata"].(map[string]any); !ok {
		t.Fatalf("expected client_metadata object, got %#v", body["client_metadata"])
	}
	instructions, ok := body["instructions"].(string)
	if !ok || !strings.Contains(instructions, "You are Codex") {
		t.Fatalf("expected synthesized Codex instructions, got %#v", body["instructions"])
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("expected tool_choice=auto, got %#v", body["tool_choice"])
	}
	assertCodexTools(t, body["tools"])
	raw, err := json.Marshal(body["input"])
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	assertCodexInputRaw(t, raw, wantText)
}

func assertCodexTools(t *testing.T, value any) {
	t.Helper()
	tools, ok := value.([]any)
	if !ok {
		t.Fatalf("expected tools array, got %#v", value)
	}
	want := map[string]bool{
		"shell_command":      false,
		"update_plan":        false,
		"request_user_input": false,
		"view_image":         false,
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || tool["type"] != "function" {
			continue
		}
		if name, _ := tool["name"].(string); name != "" {
			if _, exists := want[name]; exists {
				want[name] = true
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("expected synthesized Codex tool %q in %#v", name, tools)
		}
	}
}

func assertCodexInputRaw(t *testing.T, raw json.RawMessage, wantText string) {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("expected input array, got %s: %v", string(raw), err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one input item, got %#v", items)
	}
	item := items[0]
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("expected user message item, got %#v", item)
	}
	content, ok := item["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("expected one content item, got %#v", item["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "input_text" || part["text"] != wantText {
		t.Fatalf("expected input_text %q, got %#v", wantText, content[0])
	}
}

func writeCodexShapeResponsesSSE(w http.ResponseWriter, modelName string) {
	writeCodexShapeResponsesSSEWithText(w, "resp_codex_shape", modelName, "OK")
}

func writeCodexShapeResponsesSSEWithText(w http.ResponseWriter, responseID, modelName, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"` + responseID + `","object":"response","created_at":123,"model":"` + modelName + `","status":"in_progress","output":[]}}` + "\n\n"))
	_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":` + mustJSONQuote(text) + `}` + "\n\n"))
	_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"` + responseID + `","object":"response","created_at":123,"model":"` + modelName + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + mustJSONQuote(text) + `}]}],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}}}` + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func mustJSONQuote(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// TestCodexAssistantItemsEmitsEncryptedReasoningBeforeMessage pins the
// resynthesis path: an assistant message carrying ReasoningSignature must
// produce a leading {type:"reasoning", encrypted_content, summary:[]} item so a
// bridged/rebuilt turn keeps encrypted reasoning continuity.
func TestCodexAssistantItemsEmitsEncryptedReasoningBeforeMessage(t *testing.T) {
	encrypted := "gAAAAABbridged-sig"
	// Only an OpenAI-encrypted-tagged signature is re-emitted (as the raw blob); a
	// bare/foreign one is dropped (see TestCodexAssistantItemsDropsForeignReasoning).
	tagged := model.TagOpenAIEncryptedContent(encrypted)
	text := "continue"
	summary := "prior thought"
	items := codexAssistantItems(model.Message{
		Role:               "assistant",
		ReasoningSignature: &tagged,
		ReasoningContent:   &summary,
		Content:            model.MessageContent{Content: &text},
		ToolCalls: []model.ToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: model.FunctionCall{
				Name:      "shell",
				Arguments: `{"cmd":"ls"}`,
			},
		}},
	})
	if len(items) < 3 {
		t.Fatalf("expected reasoning + message + function_call items, got %#v", items)
	}
	if typ, _ := items[0]["type"].(string); typ != "reasoning" {
		t.Fatalf("expected first item type=reasoning, got %#v", items[0])
	}
	if sig, _ := items[0]["encrypted_content"].(string); sig != encrypted {
		t.Fatalf("expected encrypted_content %q, got %#v", encrypted, items[0]["encrypted_content"])
	}
	summaryVal, ok := items[0]["summary"].([]map[string]any)
	if !ok || len(summaryVal) != 1 {
		t.Fatalf("expected non-empty summary from reasoning content, got %#v", items[0]["summary"])
	}
	if typ, _ := items[1]["type"].(string); typ != "message" {
		t.Fatalf("expected second item type=message, got %#v", items[1])
	}
	if typ, _ := items[2]["type"].(string); typ != "function_call" {
		t.Fatalf("expected third item type=function_call, got %#v", items[2])
	}
}

// TestCodexAssistantItemsEncryptedOnlyBackfillsEmptySummary covers the case
// where only the encrypted blob is available (no summary text): still emit the
// reasoning item with summary:[] so the wire shape matches store=false continuity.
func TestCodexAssistantItemsEncryptedOnlyBackfillsEmptySummary(t *testing.T) {
	encrypted := "gAAAAABsig-only"
	tagged := model.TagOpenAIEncryptedContent(encrypted)
	items := codexAssistantItems(model.Message{
		Role:               "assistant",
		ReasoningSignature: &tagged,
	})
	if len(items) != 1 {
		t.Fatalf("expected a single reasoning item, got %#v", items)
	}
	if typ, _ := items[0]["type"].(string); typ != "reasoning" {
		t.Fatalf("expected type=reasoning, got %#v", items[0])
	}
	if sig, _ := items[0]["encrypted_content"].(string); sig != encrypted {
		t.Fatalf("expected encrypted_content %q, got %#v", encrypted, items[0]["encrypted_content"])
	}
	summaryVal, ok := items[0]["summary"].([]map[string]any)
	if !ok || len(summaryVal) != 0 {
		t.Fatalf("expected empty summary:[] backfill, got %#v", items[0]["summary"])
	}
}

// TestCodexAssistantItemsDropsForeignReasoning pins FIX A6: a foreign
// provider-tagged signature (Anthropic redacted, Gemini thoughtSignature) must NOT
// become an OpenAI encrypted_content reasoning item, or the codex upstream rejects the
// cross-protocol blob. Only an OpenAI-encrypted-tagged signature emits a reasoning item.
func TestCodexAssistantItemsDropsForeignReasoning(t *testing.T) {
	text := "continue"
	cases := []struct {
		name string
		sig  string
	}{
		{"redacted_tagged", model.EncodeRedactedThinkingSignature("REDACTED_BLOB")},
		{"gemini_tagged", model.TagGeminiThoughtSignature("gemini-sig")},
		{"bare_untagged", "bare-thinking-sig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := tc.sig
			items := codexAssistantItems(model.Message{
				Role:               "assistant",
				ReasoningSignature: &sig,
				Content:            model.MessageContent{Content: &text},
			})
			for _, item := range items {
				if typ, _ := item["type"].(string); typ == "reasoning" {
					t.Fatalf("foreign signature %q must not emit a reasoning item, got %#v", tc.name, items)
				}
			}
		})
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func arrayContainsString(value any, want string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if s, ok := item.(string); ok && s == want {
			return true
		}
	}
	return false
}
