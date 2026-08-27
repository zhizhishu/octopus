package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// assertResponsesStreamItemLifecycle replays decoded Responses stream events and
// fails on the "reasoning delta without an active item" family of defects: a
// reasoning / message / tool sub-event (summary_part.added, *_text.delta,
// *.done, content_part.added, function_call_arguments.delta, ...) that references
// an item which is NOT currently open — either never announced by
// response.output_item.added, or already finalized by response.output_item.done.
// It also fails if an item is opened twice, or is left unfinalized at stream end.
//
// A codex client silently drops (or wedges on) a delta whose item is not active,
// so holding this invariant across interleaved reasoning/tool turns is exactly
// what keeps a long multi-turn tool session from breaking. This is the machine
// check that lets us close the #3 reasoning open item with evidence instead of a
// flaky live capture.
func assertResponsesStreamItemLifecycle(t *testing.T, events []ResponsesStreamEvent) {
	t.Helper()
	open := map[string]string{} // item_id -> type, currently open (added, not yet done)
	done := map[string]bool{}   // item_id -> finalized

	requireActive := func(evType, itemID string) {
		if itemID == "" {
			t.Fatalf("%s carries an empty item_id", evType)
		}
		if done[itemID] {
			t.Fatalf("%s references item %q after it was finalized (delta without an active item)", evType, itemID)
		}
		if _, ok := open[itemID]; !ok {
			t.Fatalf("%s references item %q that was never opened via output_item.added (delta without an active item)", evType, itemID)
		}
	}

	for _, ev := range events {
		switch ev.Type {
		case "response.output_item.added":
			if ev.Item == nil || ev.Item.ID == "" {
				t.Fatalf("output_item.added missing item/id: %#v", ev)
			}
			if _, ok := open[ev.Item.ID]; ok {
				t.Fatalf("output_item.added re-opens still-open item %q", ev.Item.ID)
			}
			if done[ev.Item.ID] {
				t.Fatalf("output_item.added re-opens already-finalized item %q", ev.Item.ID)
			}
			open[ev.Item.ID] = ev.Item.Type
		case "response.output_item.done":
			if ev.Item == nil || ev.Item.ID == "" {
				t.Fatalf("output_item.done missing item/id: %#v", ev)
			}
			if _, ok := open[ev.Item.ID]; !ok {
				t.Fatalf("output_item.done finalizes item %q that is not open", ev.Item.ID)
			}
			delete(open, ev.Item.ID)
			done[ev.Item.ID] = true
		case "response.reasoning_summary_part.added",
			"response.reasoning_summary_text.delta",
			"response.reasoning_summary_text.done",
			"response.reasoning_summary_part.done",
			"response.content_part.added",
			"response.content_part.done",
			"response.output_text.delta",
			"response.output_text.done",
			"response.function_call_arguments.delta",
			"response.function_call_arguments.done",
			"response.custom_tool_call_input.delta",
			"response.custom_tool_call_input.done":
			if ev.ItemID == nil {
				t.Fatalf("%s missing item_id: %#v", ev.Type, ev)
			}
			requireActive(ev.Type, *ev.ItemID)
		}
	}

	for id, typ := range open {
		t.Fatalf("item %q (type %s) was opened but never finalized by output_item.done", id, typ)
	}
}

// TestResponseInboundInterleavedReasoningToolNeverOrphansItems reproduces the
// codex→claude multi-turn tool scenario #3 flagged: claude interleaves extended
// thinking with tool calls (think → tool → think → tool) inside one streamed
// response, each thinking block ending with its own signature_delta. Every
// reasoning/tool item must open before its deltas and finalize before the next
// item opens, each reasoning item must round-trip its OWN signature as
// encrypted_content (not a leaked/shared one), and no delta may reference an
// inactive item.
func TestResponseInboundInterleavedReasoningToolNeverOrphansItems(t *testing.T) {
	inbound := &ResponseInbound{}

	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{
			ID:      "chatcmpl_interleave",
			Model:   "claude-opus-4-8",
			Object:  "chat.completion.chunk",
			Created: 123,
		}
	}
	reasoning := func(text string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningContent: ptr(text)}}}
		return c
	}
	signature := func(sig string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningSignature: ptr(sig)}}}
		return c
	}
	toolCall := func(index int, id, name, args string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			Index:    index,
			ID:       id,
			Type:     "function",
			Function: model.FunctionCall{Name: name, Arguments: args},
		}}}}}
		return c
	}

	var raw strings.Builder
	feed := func(stream *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), stream)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	// think1 (+sig1) -> tool A -> think2 (+sig2) -> tool B -> finish(tool_calls)
	feed(reasoning("let me inspect the repo"))
	feed(signature("sig-alpha"))
	feed(toolCall(0, "call_a", "list_dir", `{"path":"."}`))
	feed(reasoning("now read the entry file"))
	feed(signature("sig-beta"))
	feed(toolCall(1, "call_b", "read_file", `{"path":"main.go"}`))

	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream [DONE]: %v", err)
	}
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())

	// Core invariant: no reasoning/tool delta without an active item.
	assertResponsesStreamItemLifecycle(t, events)

	// Non-vacuous guard: the stream must actually carry the interleaved shape, not
	// be silently empty (which would satisfy the lifecycle check for the wrong
	// reason).
	var reasoningDeltas, reasoningDones, toolDones int
	var reasoningSigs []string
	for _, ev := range events {
		switch ev.Type {
		case "response.reasoning_summary_text.delta":
			reasoningDeltas++
		case "response.output_item.done":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "reasoning":
				reasoningDones++
				sig := ""
				if ev.Item.EncryptedContent != nil {
					sig = *ev.Item.EncryptedContent
				}
				reasoningSigs = append(reasoningSigs, sig)
			case "function_call":
				toolDones++
			}
		}
	}
	if reasoningDeltas != 2 {
		t.Fatalf("expected 2 reasoning deltas (one per thinking block), got %d", reasoningDeltas)
	}
	if reasoningDones != 2 {
		t.Fatalf("expected 2 finalized reasoning items, got %d", reasoningDones)
	}
	if toolDones != 2 {
		t.Fatalf("expected 2 finalized function_call items, got %d", toolDones)
	}
	// Each reasoning item round-trips its OWN signature — not a shared/leaked one.
	if len(reasoningSigs) != 2 || reasoningSigs[0] != "sig-alpha" || reasoningSigs[1] != "sig-beta" {
		t.Fatalf("reasoning items must each carry their own encrypted_content, got %#v", reasoningSigs)
	}
}

// TestResponseInboundReasoningAfterTextNeverOrphansItems covers an upstream that
// toggles text and reasoning (some non-claude reasoners emit answer, then more
// thinking, then answer). The message item must close before the new reasoning
// item opens, and neither may emit a delta after being finalized.
func TestResponseInboundReasoningAfterTextNeverOrphansItems(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_toggle", Model: "glm-4.6", Object: "chat.completion.chunk", Created: 1}
	}
	text := func(s string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr(s)}}}}
		return c
	}
	reasoning := func(s string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningContent: ptr(s)}}}
		return c
	}
	var raw strings.Builder
	feed := func(stream *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), stream)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	feed(reasoning("first thought"))
	feed(text("partial answer "))
	feed(reasoning("second thought"))
	feed(text("final answer"))

	finishReason := "stop"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	var textDeltas int
	for _, ev := range events {
		if ev.Type == "response.output_text.delta" {
			textDeltas++
		}
	}
	if textDeltas != 2 {
		t.Fatalf("expected 2 text deltas, got %d", textDeltas)
	}
}

// TestResponseInboundReasoningOnlyFinalizesItem covers a reasoning-only stream
// (thinking with no following text or tool call, e.g. a turn that ends right
// after thinking). The reasoning item opened for the deltas must be finalized at
// the finish boundary, never left dangling.
func TestResponseInboundReasoningOnlyFinalizesItem(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_ro", Model: "claude-opus-4-8", Object: "chat.completion.chunk", Created: 1}
	}
	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	c1 := base()
	c1.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningContent: ptr("only thinking")}}}
	feed(c1)

	finishReason := "stop"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	var reasoningDones int
	for _, ev := range events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.Type == "reasoning" {
			reasoningDones++
		}
	}
	if reasoningDones != 1 {
		t.Fatalf("expected the reasoning-only item to be finalized once, got %d", reasoningDones)
	}
}

// TestResponseInboundEmptyIDParallelToolCallsNeverOrphan covers upstreams that
// stream a tool call whose FIRST fragment carries an empty id (some
// OpenAI-compatible upstreams send arguments before, or without, an id). Two
// such parallel calls must each finalize as their OWN distinct item. Before the
// fix the generated id lived only in the shared currentItemID, so the second
// tool overwrote the first's fallback id: the first tool's delta/done mis-bound
// to the second's (already-finalized) id — a "delta without an active item" —
// and the first item was left orphaned.
func TestResponseInboundEmptyIDParallelToolCallsNeverOrphan(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_emptyid", Model: "glm-4.6", Object: "chat.completion.chunk", Created: 1}
	}
	toolChunk := func(tcs ...model.ToolCall) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: tcs}}}
		return c
	}
	tc := func(index int, id, name, args string) model.ToolCall {
		return model.ToolCall{Index: index, ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
	}
	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	// Both tools' first appearance carries an empty id; argument fragments interleave.
	feed(toolChunk(tc(0, "", "list_dir", `{"path":`)))
	feed(toolChunk(tc(1, "", "read_file", `{"path":`)))
	feed(toolChunk(tc(0, "", "", `"."}`)))
	feed(toolChunk(tc(1, "", "", `"main.go"}`)))

	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	// Two distinct tool items must finalize, each with its full arguments intact.
	itemOutputIndex := map[string]int{}
	finalArgs := map[string]string{}
	toolDones := 0
	for _, ev := range events {
		switch ev.Type {
		case "response.output_item.added", "response.output_item.done":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				continue
			}
			if ev.OutputIndex != nil {
				assertOutputIndexAgrees(t, itemOutputIndex, ev.Item.ID, *ev.OutputIndex)
			}
			if ev.Type == "response.output_item.done" {
				toolDones++
				finalArgs[ev.Item.ID] = derefString(ev.Item.Arguments)
			}
		}
	}
	if toolDones != 2 {
		t.Fatalf("expected 2 finalized function_call items, got %d", toolDones)
	}
	if len(finalArgs) != 2 {
		t.Fatalf("expected 2 distinct finalized tool item ids, got %d: %#v", len(finalArgs), finalArgs)
	}
	for id, args := range finalArgs {
		if args != `{"path":"."}` && args != `{"path":"main.go"}` {
			t.Fatalf("tool item %q finalized with corrupted arguments %q", id, args)
		}
	}
}

// TestResponseInboundEmptyIDToolThenTextNeverOrphans covers an empty-id tool call
// followed by a text delta. handleTextContent closes an open reasoning item but
// NOT an open tool item, so the tool's only correct id lived in currentItemID —
// which the message item then overwrote. At finish the tool's *.done bound to the
// already-finalized message id (without an active item), orphaning the tool.
func TestResponseInboundEmptyIDToolThenTextNeverOrphans(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_emptyid_text", Model: "glm-4.6", Object: "chat.completion.chunk", Created: 1}
	}
	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	toolChunk := base()
	toolChunk.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
		Index: 0, ID: "", Type: "function", Function: model.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`},
	}}}}}
	feed(toolChunk)

	textChunk := base()
	textChunk.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr("done looking")}}}}
	feed(textChunk)

	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	toolDones, messageDones := 0, 0
	for _, ev := range events {
		if ev.Type != "response.output_item.done" || ev.Item == nil {
			continue
		}
		switch ev.Item.Type {
		case "function_call":
			toolDones++
		case "message":
			messageDones++
		}
	}
	if toolDones != 1 {
		t.Fatalf("expected the tool item to finalize exactly once as its own item, got %d", toolDones)
	}
	if messageDones != 1 {
		t.Fatalf("expected the message item to finalize once, got %d", messageDones)
	}
}

// TestResponseInboundNonStreamEmptyIDToolCallSynthesizesID guards the non-stream
// sibling of the empty-id fix: convertToResponsesAPIResponse must synthesize both
// the Responses item id and the tool-result pairing call_id for a tool call whose
// upstream id is empty. The item id follows the Responses function_call item shape
// (fc_*), while call_id follows the tool-result pairing shape (call_*).
func TestResponseInboundNonStreamEmptyIDToolCallSynthesizesID(t *testing.T) {
	resp := &model.InternalLLMResponse{
		ID:     "resp_emptyid",
		Object: "chat.completion",
		Model:  "glm-4.6",
		Choices: []model.Choice{{
			Index: 0,
			Message: &model.Message{
				Role: "assistant",
				ToolCalls: []model.ToolCall{{
					Index:    0,
					ID:       "",
					Type:     "function",
					Function: model.FunctionCall{Name: "list_dir", Arguments: `{"path":"."}`},
				}},
			},
			FinishReason: ptr("tool_calls"),
		}},
	}

	body, err := (&ResponseInbound{}).TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}

	var out ResponsesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode responses response %q: %v", string(body), err)
	}

	var fc *ResponsesItem
	for i := range out.Output {
		if out.Output[i].Type == "function_call" {
			fc = &out.Output[i]
			break
		}
	}
	if fc == nil {
		t.Fatalf("expected a function_call output item, got %s", string(body))
	}
	if strings.TrimSpace(fc.ID) == "" {
		t.Fatalf("function_call item id must be synthesized non-empty, got %q (body: %s)", fc.ID, string(body))
	}
	if !strings.HasPrefix(fc.ID, "fc_") {
		t.Fatalf("function_call item id must use the Responses fc_ prefix, got %q", fc.ID)
	}
	if !strings.HasPrefix(fc.CallID, "call_") {
		t.Fatalf("function_call call_id must use the tool pairing call_ prefix, got %q", fc.CallID)
	}
	if fc.CallID == fc.ID {
		t.Fatalf("call_id (%q) must be distinct from item id (%q)", fc.CallID, fc.ID)
	}
}

// TestResponseInboundToolCallNameBackfilledFromLaterFrame guards R3's impactful
// part: an upstream that streams a tool call's real name in a LATER frame (the
// first fragment's name is empty) must still finalize the function_call with that
// name, so the codex client can dispatch it. Mirrors chat.go mergeToolCall — first
// non-empty name, never concatenated. The streamed item id is announced once and
// cannot change, so only the name (carried on the finalizing output_item.done) is
// backfilled.
func TestResponseInboundToolCallNameBackfilledFromLaterFrame(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_latename", Model: "glm-4.6", Object: "chat.completion.chunk", Created: 1}
	}
	toolChunk := func(tcs ...model.ToolCall) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: tcs}}}
		return c
	}
	tc := func(index int, id, name, args string) model.ToolCall {
		return model.ToolCall{Index: index, ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
	}
	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	// Frame 1 carries the id but an empty name; the real name arrives in frame 2.
	feed(toolChunk(tc(0, "call_x", "", `{"city":`)))
	feed(toolChunk(tc(0, "", "get_weather", `"paris"}`)))

	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	var name, args string
	for _, ev := range events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.Type == "function_call" {
			name = ev.Item.Name
			args = derefString(ev.Item.Arguments)
		}
	}
	if name != "get_weather" {
		t.Fatalf("tool name must be backfilled from the later frame, got %q", name)
	}
	if args != `{"city":"paris"}` {
		t.Fatalf("tool arguments corrupted, got %q", args)
	}
}

// TestResponseInboundLateSignatureDoesNotLeakToNextReasoning guards against a thinking
// signature that arrives AFTER its reasoning run already closed (an abnormal upstream
// ordering — the signature lands after the text/tool that ended the block) being
// captured and leaked onto the NEXT reasoning item's encrypted_content. A reasoning
// item may only round-trip a signature that arrived while it was the open item; the
// per-type-slot refactor otherwise reopened a fresh reasoning item carrying a stale
// signature that belonged to the previous run.
func TestResponseInboundLateSignatureDoesNotLeakToNextReasoning(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_latesig", Model: "claude-opus-4-8", Object: "chat.completion.chunk", Created: 1}
	}
	reasoning := func(s string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningContent: ptr(s)}}}
		return c
	}
	text := func(s string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr(s)}}}}
		return c
	}
	signature := func(sig string) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningSignature: ptr(sig)}}}
		return c
	}
	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	// R1 -> text (closes R1, signature still empty) -> stale signature (no open
	// reasoning) -> R2 -> finish. The stale signature must not attach to R2.
	feed(reasoning("block one"))
	feed(text("answer one"))
	feed(signature("sig-for-block-one"))
	feed(reasoning("block two"))

	finishReason := "stop"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	var reasoningDones int
	for _, ev := range events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.Type == "reasoning" {
			reasoningDones++
			if ev.Item.EncryptedContent != nil && *ev.Item.EncryptedContent == "sig-for-block-one" {
				t.Fatalf("reasoning item leaked a signature that arrived after its run closed: %q", *ev.Item.EncryptedContent)
			}
		}
	}
	if reasoningDones != 2 {
		t.Fatalf("expected 2 finalized reasoning items, got %d", reasoningDones)
	}
}

// TestResponseInboundToolFragmentAfterFinishDoesNotOrphan guards against a tool-call
// argument fragment arriving AFTER finish_reason already finalized the tool item: it
// must not emit a function_call_arguments.delta against the now-closed item (a "delta
// without an active item"). Content past the finish boundary is ignored.
func TestResponseInboundToolFragmentAfterFinishDoesNotOrphan(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_postfinish", Model: "glm-4.6", Object: "chat.completion.chunk", Created: 1}
	}
	toolChunk := func(tcs ...model.ToolCall) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: tcs}}}
		return c
	}
	tc := func(index int, id, name, args string) model.ToolCall {
		return model.ToolCall{Index: index, ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
	}
	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	feed(toolChunk(tc(0, "call_a", "exec", `{"cmd":`)))

	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	// Stray fragment after the finish boundary — must be ignored, not orphaned.
	feed(toolChunk(tc(0, "", "", `"ls"}`)))

	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	var toolDones int
	for _, ev := range events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.Type == "function_call" {
			toolDones++
		}
	}
	if toolDones != 1 {
		t.Fatalf("expected exactly 1 finalized function_call item, got %d", toolDones)
	}
}

// TestResponseInboundContentAfterDoneWithoutFinishDoesNotOrphan guards the [DONE]-
// without-finish_reason path: when the stream ends with [DONE] but never sent a
// finish_reason, completeResponseEvents finalizes every open item and emits
// response.completed but does not set hasFinished. A stray content fragment arriving
// after that must still be ignored (gated on responseCompleted) rather than emitting a
// delta against the now-finalized item or opening a new item past response.completed.
func TestResponseInboundContentAfterDoneWithoutFinishDoesNotOrphan(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_postdone", Model: "glm-4.6", Object: "chat.completion.chunk", Created: 1}
	}
	toolChunk := func(tcs ...model.ToolCall) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: tcs}}}
		return c
	}
	tc := func(index int, id, name, args string) model.ToolCall {
		return model.ToolCall{Index: index, ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
	}
	var raw strings.Builder
	feed := func(s *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), s)
		if err != nil {
			t.Fatalf("TransformStream: %v", err)
		}
		raw.WriteString(string(out))
	}

	// Tool opens, then [DONE] with NO finish_reason, then a stray tool fragment.
	feed(toolChunk(tc(0, "call_a", "exec", `{"x":`)))
	done, _ := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	raw.WriteString(string(done))
	feed(toolChunk(tc(0, "", "", `1}`)))

	events := parseResponsesStreamEvents(t, raw.String())
	assertResponsesStreamItemLifecycle(t, events)

	var toolDones int
	for _, ev := range events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.Type == "function_call" {
			toolDones++
		}
	}
	if toolDones != 1 {
		t.Fatalf("expected exactly 1 finalized function_call item, got %d", toolDones)
	}
}
