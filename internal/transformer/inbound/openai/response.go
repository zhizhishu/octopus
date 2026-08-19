package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/samber/lo"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

// ResponseInbound implements the Inbound interface for OpenAI Responses API.
type ResponseInbound struct {
	// State tracking
	hasResponseCreated bool
	hasFinished        bool
	responseCompleted  bool

	// Response metadata
	responseID   string
	model        string
	createdAt    int64
	finishReason string

	sequenceNumber int

	// Per-type output-item slots. Each content type (reasoning / message / tool)
	// owns its OWN item id + output_index and finalizes independently: there is no
	// shared "current item" a later type could clobber, and opening one type never
	// force-closes an unrelated type. Mirrors new-api's ChatToResponsesStreamState
	// and sub2api's ChatCompletionsToResponsesStreamState, both immune to the "delta
	// without an active item" family (empty tool id; reasoning interleaved with a
	// still-open tool). reasoning/message re-open per contiguous run (a claude turn
	// interleaves several thinking blocks, each carrying its own signature); parallel
	// tool items stay open together and all close at the finish boundary.
	//
	// nextOutputIndex hands out sequential output_index values in the order items are
	// first opened (see allocOutputIndex).
	nextOutputIndex int

	// reasoning slot — empty id means no reasoning item is currently open.
	reasoningItemID      string
	reasoningOutputIdx   int
	accumulatedReasoning strings.Builder
	// reasoningSignature holds claude's thinking.signature (delivered by the anthropic outbound
	// as a signature_delta -> Delta.ReasoningSignature chunk) so closeReasoningItem can surface
	// it back to the codex client as reasoning.encrypted_content, letting the next turn replay it
	// upstream. Empty when the upstream sent no signature. Reset per reasoning item so each
	// interleaved thinking block round-trips its OWN signature, never a leaked sibling one.
	reasoningSignature string

	// message slot — empty id means no message item is currently open.
	messageItemID    string
	messageOutputIdx int
	contentPartOpen  bool
	accumulatedText  strings.Builder

	// Tool call slots. toolCallItemID is the streaming lifecycle handle: announced
	// once on output_item.added, immutable, referenced by every delta/done. The
	// call_id (the semantic identity the codex client pairs the tool result on next
	// turn) is tracked separately on toolCalls[idx].ID, so a real upstream id arriving
	// in a LATER frame updates the pairing key without invalidating the already
	// announced item id. Mirrors sub2api's split of ToolItemIDs vs ToolCalls[idx].ID.
	toolCalls           map[int]*model.ToolCall
	toolCallItemStarted map[int]bool
	toolCallItemID      map[int]string
	toolCallOutputIndex map[int]int
	// One synthesized id per deprecated function_call stream. The emitted SSE,
	// terminal item, and aggregated transcript must all share the same pairing id.
	legacyFunctionCallIDByChoice map[int]string
	// modernToolCallSeenByChoice records that a choice already received native
	// tool_calls (or a same-chunk modern+legacy pair). A later pure legacy
	// function_call on that choice must only be stripped — re-injecting at
	// index 0 would clobber the modern call id and concatenate arguments into
	// invalid JSON across stream chunks.
	modernToolCallSeenByChoice map[int]bool
	// toolCallArgs buffers each tool call's streamed argument fragments. Growing
	// toolCalls[idx].Function.Arguments with `+=` re-copies the whole accumulated
	// payload on every fragment (O(total²) for a large tool call); the builder
	// appends, and Function.Arguments is refreshed from it so every reader still
	// sees the same complete string.
	toolCallArgs map[int]*strings.Builder

	// Finalized output items keyed by output_index, captured as each
	// response.output_item.done is emitted, so the terminal response.completed event
	// can carry the full output array. The OpenAI Responses API requires
	// response.completed.response.output to hold the final items; SDK clients (and
	// Cherry Studio) read it to get the result. Streaming used to send output:[]
	// there, so those clients saw zero function_calls and stopped after a tool call
	// instead of executing it. Keyed (not appended) because items now finalize out of
	// index order — a message can close at the finish boundary after a later reasoning
	// item already closed — and finalOutputItems must emit them in output_index order.
	completedItems map[int]ResponsesItem

	// Usage tracking
	usage *model.Usage

	// Stream chunks storage for aggregation
	streamChunks []*model.InternalLLMResponse
	// storedResponse stores the non-stream response
	storedResponse *model.InternalLLMResponse
}

// ResetStreamAggregation drops all per-response streaming/aggregation state so this
// adapter can transform a fresh upstream attempt without leaking the previous one's
// chunks. The forced non-stream aggregate path shares one inbound adapter across every
// channel/key retry (relayRequest.inAdapter); a failed attempt that produced partial
// content would otherwise be concatenated into the successful failover attempt's
// aggregated body. The zero value equals a freshly constructed &ResponseInbound{} (the
// setup path builds it that way) and TransformRequest keeps no request-level state on
// the adapter, so resetting to zero is a complete, safe fresh start.
func (i *ResponseInbound) ResetStreamAggregation() {
	*i = ResponseInbound{}
}

// allocOutputIndex returns the next output_index and records that an item claimed
// it, so slots opened in interleaved order still get monotonically increasing,
// non-colliding indexes.
func (i *ResponseInbound) allocOutputIndex() int {
	idx := i.nextOutputIndex
	i.nextOutputIndex++
	return idx
}

func (i *ResponseInbound) TransformRequest(ctx context.Context, body []byte) (*model.InternalLLMRequest, error) {
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("failed to decode responses api request: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}

	return convertToInternalRequest(&req)
}

func (i *ResponseInbound) TransformResponse(ctx context.Context, response *model.InternalLLMResponse) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("response is nil")
	}

	// Guarantee a stable, non-empty response id so the client and octopus session
	// ownership see the same id. Without it, a previous_response_id continuation
	// from the next turn cannot match the recorded owner and history is dropped.
	if strings.TrimSpace(response.ID) == "" {
		if i.responseID == "" {
			i.responseID = generateResponseID()
		}
		response.ID = i.responseID
	}

	for choiceIndex := range response.Choices {
		choice := &response.Choices[choiceIndex]
		i.normalizeLegacyFunctionCallForResponses(choice.Message, choice.Index)
		i.normalizeLegacyFunctionCallForResponses(choice.Delta, choice.Index)
	}
	// Store the normalized response for later retrieval and billing.
	i.storedResponse = response

	// Convert to Responses API format
	resp := convertToResponsesAPIResponse(response)

	body, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal responses api response: %w", err)
	}

	return body, nil
}

func (i *ResponseInbound) TransformStream(ctx context.Context, stream *model.InternalLLMResponse) ([]byte, error) {
	// Handle [DONE] marker
	if stream.Object == "[DONE]" {
		events := i.completeResponseEvents()
		events = append(events, []byte("data: [DONE]\n\n"))
		return bytesJoin(events), nil
	}

	// Upgrade deprecated Chat function_call on every choice before storage so
	// multi-choice aggregation cannot lose legacy tool calls that the SSE
	// path (choice 0 only) never emits as lifecycle events.
	for choiceIndex := range stream.Choices {
		choice := &stream.Choices[choiceIndex]
		i.normalizeLegacyFunctionCallForResponses(choice.Delta, choice.Index)
		i.normalizeLegacyFunctionCallForResponses(choice.Message, choice.Index)
	}

	// Store the normalized chunk for aggregation
	i.streamChunks = append(i.streamChunks, stream)

	var events [][]byte

	// Initialize tool call tracking maps if needed
	if i.toolCalls == nil {
		i.toolCalls = make(map[int]*model.ToolCall)
		i.toolCallItemStarted = make(map[int]bool)
		i.toolCallItemID = make(map[int]string)
		i.toolCallOutputIndex = make(map[int]int)
		i.toolCallArgs = make(map[int]*strings.Builder)
		i.completedItems = make(map[int]ResponsesItem)
	}

	// Update metadata from chunk
	if i.responseID == "" && stream.ID != "" {
		i.responseID = stream.ID
	}
	if i.responseID == "" {
		i.responseID = generateResponseID()
	}
	if i.model == "" && stream.Model != "" {
		i.model = stream.Model
	}
	if i.createdAt == 0 && stream.Created != 0 {
		i.createdAt = stream.Created
	}
	if stream.Usage != nil {
		i.usage = stream.Usage
	}

	// Generate response.created event if first chunk
	if !i.hasResponseCreated {
		i.hasResponseCreated = true

		response := &ResponsesResponse{
			Object:    "response",
			ID:        i.responseID,
			Model:     i.model,
			CreatedAt: i.createdAt,
			Status:    lo.ToPtr("in_progress"),
			Output:    []ResponsesItem{},
		}

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:     "response.created",
			Response: response,
		}))

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:     "response.in_progress",
			Response: response,
		}))
	}

	// Process choices
	if len(stream.Choices) > 0 {
		choice := stream.Choices[0]

		// Ignore any content that arrives AFTER the response was already terminated —
		// either by finish_reason (hasFinished) or by a [DONE] that synthesized the
		// terminal event without ever seeing a finish_reason (responseCompleted). Both
		// paths already finalized every open item via closeAllOpenItems, so a late
		// reasoning/text/tool fragment (some upstreams emit a stray fragment past the
		// end) would emit a delta against an already-done item — a "delta without an
		// active item" — or open a brand-new item after response.completed. The terminal
		// response.completed / usage handling below still runs.
		if !i.hasFinished && !i.responseCompleted {
			// Handle reasoning content delta.
			// Use GetReasoningContent so upstreams that emit the `reasoning` field
			// (OpenRouter/Ollama ...) instead of `reasoning_content` are not dropped.
			if choice.Delta != nil {
				if reasoning := choice.Delta.GetReasoningContent(); reasoning != "" {
					events = append(events, i.handleReasoningContent(lo.ToPtr(reasoning))...)
				}
				// Capture claude's thinking signature (arrives in its own anthropic signature_delta
				// chunk, after the reasoning text) so closeReasoningItem can emit it as
				// reasoning.encrypted_content. Without this the signature is lost and the next codex
				// turn sends an unsigned thinking block Anthropic 400s on. Gate on an OPEN reasoning
				// item: a signature arriving after the run already closed has no item to belong to and
				// would otherwise leak onto the NEXT reasoning item's encrypted_content, breaking the
				// "each thinking block round-trips its OWN signature" guarantee.
				if i.reasoningItemID != "" && choice.Delta.ReasoningSignature != nil && *choice.Delta.ReasoningSignature != "" {
					i.reasoningSignature = *choice.Delta.ReasoningSignature
				}
			}

			// Handle text content delta
			if choice.Delta != nil && choice.Delta.Content.Content != nil && *choice.Delta.Content.Content != "" {
				events = append(events, i.handleTextContent(choice.Delta.Content.Content)...)
			}

			// Handle image content delta produced by canonical Images API bridging.
			if choice.Delta != nil && len(choice.Delta.Content.MultipleContent) > 0 {
				events = append(events, i.handleImageContent(choice.Delta.Content.MultipleContent)...)
			}

			// Handle tool calls
			if choice.Delta != nil && len(choice.Delta.ToolCalls) > 0 {
				events = append(events, i.handleToolCalls(choice.Delta.ToolCalls)...)
			}
		}

		// Handle finish reason
		if choice.FinishReason != nil && !i.hasFinished {
			i.hasFinished = true
			i.finishReason = strings.TrimSpace(*choice.FinishReason)

			// Finalize every still-open item (reasoning / message / parallel tools)
			// at the finish boundary — the unified "close at the end".
			events = append(events, i.closeAllOpenItems()...)
		}
	}

	// Handle final usage chunk and complete response
	if stream.Usage != nil && i.hasFinished && !i.responseCompleted {
		i.responseCompleted = true
		i.usage = stream.Usage

		status, eventType := i.terminalStatusAndEvent()
		response := &ResponsesResponse{
			Object:    "response",
			ID:        i.responseID,
			Model:     i.model,
			CreatedAt: i.createdAt,
			Status:    &status,
			Output:    i.finalOutputItems(),
			Usage:     convertUsageToResponses(i.usage),
		}

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:     eventType,
			Response: response,
		}))
	}

	if len(events) == 0 {
		return nil, nil
	}

	// Join events
	result := make([]byte, 0)
	for _, event := range events {
		if event != nil {
			result = append(result, event...)
		}
	}

	return result, nil
}

// normalizeLegacyFunctionCallForResponses upgrades the deprecated Chat
// function_call field only at the Responses compatibility boundary. Chat
// clients continue to receive the upstream shape unchanged, while Cursor and
// other Responses clients get the function_call lifecycle events they expect.
func (i *ResponseInbound) normalizeLegacyFunctionCallForResponses(message *model.Message, choiceIndex int) {
	if message == nil {
		return
	}
	// Native tool_calls win for this choice for the rest of the stream. Mark
	// before looking at function_call so a same-chunk modern+legacy pair still
	// keeps the modern tool and strips the deprecated field.
	if len(message.ToolCalls) > 0 {
		if i.modernToolCallSeenByChoice == nil {
			i.modernToolCallSeenByChoice = make(map[int]bool)
		}
		i.modernToolCallSeenByChoice[choiceIndex] = true
	}
	if message.FunctionCall == nil {
		return
	}
	if len(message.ToolCalls) > 0 || i.modernToolCallSeenByChoice[choiceIndex] {
		// Modern tools already present (this chunk or an earlier one): drop the
		// deprecated field without synthesizing a competing index-0 tool call.
		message.FunctionCall = nil
		return
	}
	// Pure legacy stream (possibly multi-chunk argument fragments): upgrade in
	// place and reuse one synthesized call_id for the whole choice.
	if i.legacyFunctionCallIDByChoice == nil {
		i.legacyFunctionCallIDByChoice = make(map[int]string)
	}
	callID := i.legacyFunctionCallIDByChoice[choiceIndex]
	if callID == "" {
		callID = generateFunctionCallItemID()
		i.legacyFunctionCallIDByChoice[choiceIndex] = callID
	}
	message.ToolCalls = []model.ToolCall{{
		Index: 0,
		ID:    callID,
		Type:  "function",
		Function: model.FunctionCall{
			Name:      message.FunctionCall.Name,
			Arguments: message.FunctionCall.Arguments,
		},
	}}
	message.FunctionCall = nil
}

func (i *ResponseInbound) completeResponseEvents() [][]byte {
	if i.responseCompleted || !i.hasResponseCreated {
		return nil
	}

	i.responseCompleted = true
	events := i.closeAllOpenItems()

	status, eventType := i.terminalStatusAndEvent()
	response := &ResponsesResponse{
		Object:    "response",
		ID:        i.responseID,
		Model:     i.model,
		CreatedAt: i.createdAt,
		Status:    &status,
		Output:    i.finalOutputItems(),
		Usage:     convertUsageToResponses(i.completionUsage()),
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:     eventType,
		Response: response,
	}))

	return events
}

// finalOutputItems returns the finalized output items for the terminal
// response.completed event in output_index order, or an empty (non-nil) slice so
// it serializes as [] not null when there is genuinely no output. Items are keyed
// by output_index because they no longer finalize in index order (a message can
// close at the finish boundary after a later reasoning item already closed).
func (i *ResponseInbound) finalOutputItems() []ResponsesItem {
	if len(i.completedItems) == 0 {
		return []ResponsesItem{}
	}
	indexes := make([]int, 0, len(i.completedItems))
	for idx := range i.completedItems {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	items := make([]ResponsesItem, 0, len(indexes))
	for _, idx := range indexes {
		items = append(items, i.completedItems[idx])
	}
	return items
}

func (i *ResponseInbound) terminalStatusAndEvent() (string, string) {
	switch strings.TrimSpace(i.finishReason) {
	case "length":
		return "incomplete", "response.incomplete"
	case "error":
		return "failed", "response.failed"
	default:
		return "completed", "response.completed"
	}
}

func (i *ResponseInbound) completionUsage() *model.Usage {
	if i.usage != nil {
		return i.usage
	}
	return &model.Usage{}
}

func bytesJoin(parts [][]byte) []byte {
	result := make([]byte, 0)
	for _, part := range parts {
		if len(part) > 0 {
			result = append(result, part...)
		}
	}
	return result
}

func (i *ResponseInbound) enqueueEvent(ev *ResponsesStreamEvent) []byte {
	ev.SequenceNumber = i.sequenceNumber
	i.sequenceNumber++

	// Single choke point: every finalized output item (message/reasoning/function_call
	// /image) flows through a response.output_item.done event. Capture it keyed by its
	// output_index so the terminal response.completed carries the full output array (in
	// index order) instead of []. Without this, a client that reconstructs the result
	// from response.completed.output sees no function_calls and stops after the first
	// tool call.
	if ev.Type == "response.output_item.done" && ev.Item != nil && ev.OutputIndex != nil {
		if i.completedItems == nil {
			i.completedItems = make(map[int]ResponsesItem)
		}
		i.completedItems[*ev.OutputIndex] = *ev.Item
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return nil
	}

	return formatSSEData(data)
}

func (i *ResponseInbound) handleReasoningContent(content *string) [][]byte {
	var events [][]byte

	// Open a fresh reasoning item for this contiguous thinking run if one is not
	// already open. A new run (after an intervening message/tool closed the previous
	// reasoning item) gets its OWN item id + output_index, so its deltas, its summary,
	// and its own thinking signature never bind to a sibling. Opening reasoning never
	// closes the message item or any tool item.
	if i.reasoningItemID == "" {
		i.reasoningItemID = generateReasoningItemID()
		i.reasoningOutputIdx = i.allocOutputIndex()
		itemID := i.reasoningItemID

		// encrypted_content is emitted as an explicit "" on the in-progress added
		// event (the signature arrives later via signature_delta and is surfaced on
		// output_item.done). A real OpenAI reasoning item and the reference relays
		// (CLIProxyAPI) always carry encrypted_content on the reasoning item; a codex
		// client uses it, together with the summary array, to register the item.
		item := &ResponsesItem{
			ID:               itemID,
			Type:             "reasoning",
			Status:           lo.ToPtr("in_progress"),
			Summary:          []ResponsesReasoningSummary{},
			EncryptedContent: lo.ToPtr(""),
		}

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(i.reasoningOutputIdx),
			Item:        item,
		}))

		// Emit reasoning_summary_part.added (with an explicit empty text, matching the
		// genuine OpenAI shape and CLIProxyAPI, so the client opens summary part 0).
		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.reasoning_summary_part.added",
			ItemID:       &itemID,
			OutputIndex:  lo.ToPtr(i.reasoningOutputIdx),
			SummaryIndex: lo.ToPtr(0),
			Part:         &ResponsesContentPart{Type: "summary_text", Text: lo.ToPtr("")},
		}))
	}

	// Accumulate reasoning content
	i.accumulatedReasoning.WriteString(*content)

	// Emit reasoning_summary_text.delta
	itemID := i.reasoningItemID
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_text.delta",
		ItemID:       &itemID,
		OutputIndex:  lo.ToPtr(i.reasoningOutputIdx),
		SummaryIndex: lo.ToPtr(0),
		Delta:        *content,
	}))

	return events
}

func (i *ResponseInbound) handleTextContent(content *string) [][]byte {
	var events [][]byte

	// Text closes an open reasoning item (the thinking run ended) but leaves any open
	// tool item untouched. It then ensures a message item and its output_text content
	// part are open.
	events = append(events, i.closeReasoningItem()...)

	// Start message output item if not started
	if i.messageItemID == "" {
		i.messageItemID = generateMessageItemID()
		i.messageOutputIdx = i.allocOutputIndex()
		itemID := i.messageItemID

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(i.messageOutputIdx),
			Item: &ResponsesItem{
				ID:      itemID,
				Type:    "message",
				Status:  lo.ToPtr("in_progress"),
				Role:    "assistant",
				Content: &ResponsesInput{Items: []ResponsesItem{}},
			},
		}))
	}

	// Start content part if not started
	if !i.contentPartOpen {
		i.contentPartOpen = true
		itemID := i.messageItemID

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.content_part.added",
			ItemID:       &itemID,
			OutputIndex:  lo.ToPtr(i.messageOutputIdx),
			ContentIndex: lo.ToPtr(0),
			Part: &ResponsesContentPart{
				Type: "output_text",
				Text: lo.ToPtr(""),
			},
		}))
	}

	// Accumulate text content
	i.accumulatedText.WriteString(*content)

	// Emit output_text.delta
	itemID := i.messageItemID
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		ItemID:       &itemID,
		OutputIndex:  lo.ToPtr(i.messageOutputIdx),
		ContentIndex: lo.ToPtr(0),
		Delta:        *content,
	}))

	return events
}

func (i *ResponseInbound) handleImageContent(parts []model.MessageContentPart) [][]byte {
	var events [][]byte

	for _, part := range parts {
		if part.Type != "image_url" || part.ImageURL == nil {
			continue
		}
		result := xurl.ExtractBase64FromDataURL(part.ImageURL.URL)
		if strings.TrimSpace(result) == "" {
			continue
		}

		// An image item is emitted whole (added + done together). Close the
		// sequential reasoning/message lane first so it lands in output order; open
		// tool items stay open and finalize at the stream boundary.
		events = append(events, i.closeReasoningItem()...)
		events = append(events, i.closeMessageItem()...)

		itemID := generateImageGenerationItemID()
		outputIdx := i.allocOutputIndex()
		added := &ResponsesItem{
			ID:     itemID,
			Type:   "image_generation_call",
			Status: lo.ToPtr("in_progress"),
		}
		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(outputIdx),
			Item:        added,
		}))

		done := &ResponsesItem{
			ID:     itemID,
			Type:   "image_generation_call",
			Status: lo.ToPtr("completed"),
			Result: &result,
		}
		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.done",
			OutputIndex: lo.ToPtr(outputIdx),
			Item:        done,
		}))
	}

	return events
}

func (i *ResponseInbound) handleToolCalls(toolCalls []model.ToolCall) [][]byte {
	var events [][]byte

	// A tool call closes an open reasoning item AND an open message item before the
	// function_call item is announced. The Responses stream is sequential: the message's
	// output_item.done must be emitted before the next output_item.added, or a codex
	// client's item state machine errors ("... without active item"). This mirrors the
	// ordering every reference Responses emitter uses — CLIProxyAPI finalizes the message
	// "to match Codex expected ordering" / "Responses streaming requires message done
	// events before the next output_item.added". Text arriving after a tool simply opens
	// a fresh message item. It still does NOT force-close a sibling tool item: parallel
	// tool calls stay open and finalize together at the finish boundary — force-closing a
	// still-incomplete tool when reasoning interleaves (R1) would orphan its later
	// argument deltas, the very bug this refactor kills.
	events = append(events, i.closeReasoningItem()...)
	events = append(events, i.closeMessageItem()...)

	for _, tc := range toolCalls {
		toolCallIndex := tc.Index

		// Initialize tool call tracking if needed
		if _, ok := i.toolCalls[toolCallIndex]; !ok {
			// call_id (semantic identity) = the upstream tool id, or a synthesized one
			// when the first fragment carries none (some OpenAI-compatible upstreams
			// stream arguments before, or without, an id). It is stored back so a later
			// argument-delta and the finalizing *.done reuse the same non-empty value
			// the codex client pairs the tool result on next turn — an empty call_id
			// can never be matched.
			callID := strings.TrimSpace(tc.ID)
			if callID == "" {
				callID = generateFunctionCallItemID()
			}

			i.toolCalls[toolCallIndex] = &model.ToolCall{
				Index: toolCallIndex,
				ID:    callID,
				Type:  tc.Type,
				Function: model.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: "",
				},
			}

			// The streaming item id (lifecycle handle) is announced once here and is
			// immutable: every delta/done references it. It starts equal to the initial
			// call_id — reusing the real upstream id when present (oct's item-id-equals-
			// call-id contract, which real upstreams and the tool_use tests rely on),
			// or the synthesized id when the first fragment carried none. Tracking it
			// separately from toolCalls[idx].ID (the call_id / pairing key) is what lets
			// a real id arriving in a LATER frame update only call_id, without
			// invalidating this already-announced, already-referenced item id.
			itemID := callID
			i.toolCallItemID[toolCallIndex] = itemID
			i.toolCallOutputIndex[toolCallIndex] = i.allocOutputIndex()
			// Defer the output_item.added announcement when the upstream sent the tool
			// id first but the function name is still empty (some OpenAI-compatible
			// upstreams split the id and name across two stream chunks). Announcing an
			// item with an empty name now would force a cursor / codex SDK that routes
			// tool calls by name (AskQuestion / Cherry Studio) to drop the call: the name
			// field is omitempty so the event loses it entirely, and a later *.done can
			// not retroactively re-add it. Mirrors sub2api's announceChatToolItem: keep
			// the slot half-open, let the name-backfill below announce once it arrives,
			// and let closeToolItem force-announce if a name never arrives.
			i.toolCallItemStarted[toolCallIndex] = tc.Function.Name != ""

			if i.toolCallItemStarted[toolCallIndex] {
				events = append(events, i.announceToolItem(toolCallIndex)...)
			}
		}

		// Backfill the call_id (pairing key) from a later frame that carries the real
		// upstream id when the first fragment's was empty (a synthesized id was
		// announced, but the next turn's tool result must reference the real one). The
		// already-announced item id (lifecycle handle) never changes. Mirrors sub2api.
		if realID := strings.TrimSpace(tc.ID); realID != "" {
			i.toolCalls[toolCallIndex].ID = realID
		}

		// Backfill the tool name from a later frame when the first fragment carried
		// none (some upstreams stream the id first and the name in a following chunk).
		// Take the first non-empty value and never concatenate — a name is atomic, not
		// a streamed fragment. Mirrors chat.go mergeToolCall.
		nameBackfilled := false
		if tc.Function.Name != "" && i.toolCalls[toolCallIndex].Function.Name == "" {
			i.toolCalls[toolCallIndex].Function.Name = tc.Function.Name
			nameBackfilled = true
		}

		// Now that the name has arrived (this frame or earlier), announce the deferred
		// output_item.added the first fragment held back, and replay the argument
		// deltas that accumulated while the announcement was pending so the codex
		// client receives them in the correct lifecycle order (added → delta → done)
		// instead of as orphan deltas against an unannounced item. Mirrors sub2api's
		// announceChatToolItem, which emits a held-then-released delta on late announce.
		if nameBackfilled && !i.toolCallItemStarted[toolCallIndex] {
			events = append(events, i.announceToolItem(toolCallIndex)...)
			i.toolCallItemStarted[toolCallIndex] = true
			if accumulated := i.toolCalls[toolCallIndex].Function.Arguments; accumulated != "" {
				itemID := i.toolCallItemID[toolCallIndex]
				outputIdx := i.toolCallOutputIndex[toolCallIndex]
				if i.toolCalls[toolCallIndex].Type == model.ToolCallTypeCustom {
					events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
						Type:        "response.custom_tool_call_input.delta",
						ItemID:      &itemID,
						OutputIndex: lo.ToPtr(outputIdx),
						Delta:       accumulated,
					}))
				} else {
					events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
						Type:         "response.function_call_arguments.delta",
						ItemID:       &itemID,
						OutputIndex:  lo.ToPtr(outputIdx),
						ContentIndex: lo.ToPtr(0),
						Delta:        accumulated,
					}))
				}
			}
		}

		// Accumulate arguments (custom tool calls carry their freeform input here too).
		// Buffered instead of `+=` so a long argument stream is not re-copied per
		// fragment; Function.Arguments is refreshed from the buffer on every write, so
		// closeToolItem and the aggregation read the same value as before.
		if tc.Function.Arguments != "" {
			buffer := streamTextBuffer(i.toolCallArgs, toolCallIndex)
			buffer.WriteString(tc.Function.Arguments)
			i.toolCalls[toolCallIndex].Function.Arguments = buffer.String()
		}

		// Emit the incremental input/arguments delta against this tool's own item id.
		// Skip when the item is still pending announcement (name not yet arrived): the
		// delta would arrive before output_item.added, an "orphan delta without an
		// active item" the codex SDK rejects. The deferred announcement path above
		// replays this accumulated delta in lifecycle order once the name arrives.
		if tc.Function.Arguments != "" && i.toolCallItemStarted[toolCallIndex] {
			itemID := i.toolCallItemID[toolCallIndex]
			outputIdx := i.toolCallOutputIndex[toolCallIndex]

			if i.toolCalls[toolCallIndex].Type == model.ToolCallTypeCustom {
				events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
					Type:        "response.custom_tool_call_input.delta",
					ItemID:      &itemID,
					OutputIndex: lo.ToPtr(outputIdx),
					Delta:       tc.Function.Arguments,
				}))
			} else {
				events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
					Type:         "response.function_call_arguments.delta",
					ItemID:       &itemID,
					OutputIndex:  lo.ToPtr(outputIdx),
					ContentIndex: lo.ToPtr(0),
					Delta:        tc.Function.Arguments,
				}))
			}
		}
	}

	return events
}

func (i *ResponseInbound) closeReasoningItem() [][]byte {
	if i.reasoningItemID == "" {
		return nil
	}

	var events [][]byte
	itemID := i.reasoningItemID
	outputIdx := i.reasoningOutputIdx
	fullReasoning := i.accumulatedReasoning.String()

	// Emit reasoning_summary_text.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_text.done",
		ItemID:       &itemID,
		OutputIndex:  lo.ToPtr(outputIdx),
		SummaryIndex: lo.ToPtr(0),
		Text:         fullReasoning,
	}))

	// Emit reasoning_summary_part.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_part.done",
		ItemID:       &itemID,
		OutputIndex:  lo.ToPtr(outputIdx),
		SummaryIndex: lo.ToPtr(0),
		Part:         &ResponsesContentPart{Type: "summary_text", Text: &fullReasoning},
	}))

	// Emit output_item.done
	item := ResponsesItem{
		ID:   itemID,
		Type: "reasoning",
		Summary: []ResponsesReasoningSummary{{
			Type: "summary_text",
			Text: fullReasoning,
		}},
	}
	// Round-trip claude's captured signature as reasoning.encrypted_content so the codex client
	// can replay it next turn. enqueueEvent also copies this item into response.completed.output.
	if i.reasoningSignature != "" {
		item.EncryptedContent = lo.ToPtr(i.reasoningSignature)
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: lo.ToPtr(outputIdx),
		Item:        &item,
	}))

	// Clear the slot so the next contiguous thinking run opens a fresh reasoning
	// item with its own id + signature.
	i.reasoningItemID = ""
	i.accumulatedReasoning.Reset()
	i.reasoningSignature = ""

	return events
}

func (i *ResponseInbound) closeMessageItem() [][]byte {
	if i.messageItemID == "" {
		return nil
	}

	var events [][]byte
	itemID := i.messageItemID
	outputIdx := i.messageOutputIdx

	// Close content part first (references the still-open message item id).
	events = append(events, i.closeContentPart()...)

	fullText := i.accumulatedText.String()

	// Emit output_item.done
	item := ResponsesItem{
		ID:     itemID,
		Type:   "message",
		Status: lo.ToPtr("completed"),
		Role:   "assistant",
		Content: &ResponsesInput{
			Items: []ResponsesItem{{
				Type: "output_text",
				Text: &fullText,
			}},
		},
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: lo.ToPtr(outputIdx),
		Item:        &item,
	}))

	// Clear the slot so a later contiguous text run opens a fresh message item.
	i.messageItemID = ""
	i.accumulatedText.Reset()

	return events
}

func (i *ResponseInbound) closeContentPart() [][]byte {
	if !i.contentPartOpen {
		return nil
	}

	var events [][]byte
	i.contentPartOpen = false
	itemID := i.messageItemID
	outputIdx := i.messageOutputIdx
	fullText := i.accumulatedText.String()

	// Emit output_text.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		ItemID:       &itemID,
		OutputIndex:  lo.ToPtr(outputIdx),
		ContentIndex: lo.ToPtr(0),
		Text:         fullText,
	}))

	// Emit content_part.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.content_part.done",
		ItemID:       &itemID,
		OutputIndex:  lo.ToPtr(outputIdx),
		ContentIndex: lo.ToPtr(0),
		Part: &ResponsesContentPart{
			Type: "output_text",
			Text: lo.ToPtr(fullText),
		},
	}))

	return events
}

// closeAllOpenItems finalizes every still-open slot at the stream boundary
// (finish_reason and [DONE]): the sequential reasoning/message lane first, then
// every open tool item in deterministic ascending index order — map iteration is
// random and would scramble the parallel tool_call ordering in the terminal
// response.completed.output. Idempotent: each close guards on its slot, so calling
// it again after the finish path already ran is a no-op.
func (i *ResponseInbound) closeAllOpenItems() [][]byte {
	var events [][]byte

	events = append(events, i.closeReasoningItem()...)
	events = append(events, i.closeMessageItem()...)

	toolCallIndexes := make([]int, 0, len(i.toolCalls))
	for idx := range i.toolCalls {
		toolCallIndexes = append(toolCallIndexes, idx)
	}
	sort.Ints(toolCallIndexes)
	for _, idx := range toolCallIndexes {
		events = append(events, i.closeToolItem(idx)...)
	}

	return events
}

// closeToolItem finalizes one open tool call item. The output item id is the
// streaming lifecycle handle (toolCallItemID); call_id is the semantic pairing key
// (toolCalls[idx].ID) — the two are emitted as distinct fields so a late upstream id
// reaches the client as call_id without ever changing the announced item id.
func (i *ResponseInbound) closeToolItem(idx int) [][]byte {
	if _, ok := i.toolCalls[idx]; !ok {
		return nil
	}

	// Force-announce a tool call whose output_item.added was deferred because the
	// upstream streamed the id first and the name never arrived: emit it now (with
	// the empty name the upstream left us) so closeToolItem has an item to finalize
	// and the codex client pairs the call_id on next turn. Mirrors sub2api's
	// announceChatToolItem force=true fallback in closeChatToolItems.
	if !i.toolCallItemStarted[idx] {
		defer func() { i.toolCallItemStarted[idx] = true }()
		events := i.announceToolItem(idx)
		remaining := i.finalizeToolItem(idx)
		return append(events, remaining...)
	}

	return i.finalizeToolItem(idx)
}

// finalizeToolItem emits the terminal *.done events for a tool call whose
// output_item.added has already been announced. It is the original tail of
// closeToolItem, factored out so the deferred-announcement fallback can reuse it.
func (i *ResponseInbound) finalizeToolItem(idx int) [][]byte {
	if !i.toolCallItemStarted[idx] {
		return nil
	}

	var events [][]byte
	tc := i.toolCalls[idx]
	itemID := i.toolCallItemID[idx]
	outputIdx := i.toolCallOutputIndex[idx]

	if tc.Type == model.ToolCallTypeCustom {
		// Custom (freeform) tool call: terminate with custom_tool_call_input.done +
		// a custom_tool_call output item whose `input` holds the full freeform
		// payload, so the codex client sees exactly the custom-tool shape it
		// registered.
		input := tc.Function.Arguments
		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.custom_tool_call_input.done",
			ItemID:      &itemID,
			OutputIndex: lo.ToPtr(outputIdx),
			CallID:      tc.ID,
			Name:        tc.Function.Name,
			Input:       input,
		}))

		item := ResponsesItem{
			ID:     itemID,
			Type:   "custom_tool_call",
			Status: lo.ToPtr("completed"),
			CallID: tc.ID,
			Name:   tc.Function.Name,
			Input:  &input,
		}

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.done",
			OutputIndex: lo.ToPtr(outputIdx),
			Item:        &item,
		}))

		i.toolCallItemStarted[idx] = false
		// Drop the slot entirely so a second closeAllOpenItems pass (finish_reason +
		// re-enter closeToolItem, mistake the still-present
		// slot for a deferred-announce fallback, and re-emit output_item.added against
		// an already-finalized item. Same fix as the function_call tail above.
		delete(i.toolCalls, idx)
		return events
	}

	// Emit function_call_arguments.done. Empty upstream arguments are normalized to
	// "{}" so the codex client's json.Unmarshal never fails (mirrors sub2api).
	// Carry Name + CallID so SDK clients (cursor / Cherry Studio) that read the
	// function name from the done event - not just from output_item.added - can
	// route the call without a second lookup. Mirrors sub2api closeChatToolItems.
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		ItemID:      &itemID,
		OutputIndex: lo.ToPtr(outputIdx),
		CallID:      tc.ID,
		Name:        tc.Function.Name,
		Arguments:   normalizeToolArguments(tc.Function.Arguments),
	}))

	// Emit output_item.done
	item := ResponsesItem{
		ID:        itemID,
		Type:      "function_call",
		Status:    lo.ToPtr("completed"),
		CallID:    tc.ID,
		Name:      tc.Function.Name,
		Arguments: lo.ToPtr(normalizeToolArguments(tc.Function.Arguments)),
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: lo.ToPtr(outputIdx),
		Item:        &item,
	}))

	i.toolCallItemStarted[idx] = false
	// Drop the slot entirely so a second closeAllOpenItems pass (finish_reason +
	// [DONE] both run it) cannot re-enter closeToolItem, mistake the still-present
	// slot for a deferred-announce fallback, and re-emit output_item.added against an
	// already-finalized item ("re-opens already-finalized item"). Mirrors sub2api's
	// finalize-once contract.
	delete(i.toolCalls, idx)
	return events
}

// announceToolItem emits response.output_item.added for a tool call slot, using the
// current toolCalls[idx].Function.Name (which may have been backfilled from a later
// frame). It is the single place that constructs an in-progress Responses item for a
// tool call, used both by the eager path (name arrived in the first chunk) and by
// the deferred path (name arrived in a later chunk, replayed after announcement).
func (i *ResponseInbound) announceToolItem(toolCallIndex int) [][]byte {
	if _, ok := i.toolCalls[toolCallIndex]; !ok {
		return nil
	}
	tc := i.toolCalls[toolCallIndex]
	itemID := i.toolCallItemID[toolCallIndex]
	outputIdx := i.toolCallOutputIndex[toolCallIndex]

	var item *ResponsesItem
	if tc.Type == model.ToolCallTypeCustom {
		item = &ResponsesItem{
			ID:     itemID,
			Type:   "custom_tool_call",
			Status: lo.ToPtr("in_progress"),
			CallID: tc.ID,
			Name:   tc.Function.Name,
			Input:  lo.ToPtr(""),
		}
	} else {
		// arguments is an explicit empty string on the in-progress added event — the
		// genuine OpenAI Responses shape (sub2api emits it too). The real value is
		// streamed via *.delta and finalized on *.done.
		item = &ResponsesItem{
			ID:        itemID,
			Type:      "function_call",
			Status:    lo.ToPtr("in_progress"),
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: lo.ToPtr(""),
		}
	}
	return [][]byte{i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: lo.ToPtr(outputIdx),
		Item:        item,
	})}
}

// normalizeToolArguments returns a valid-JSON arguments string for a function_call
// output item, so a codex client's json.Unmarshal on it never fails. An empty
// upstream arguments string is normalized to "{}" (mirroring sub2api's
// chatcompletions_to_responses, args == "" → "{}"); a non-empty but syntactically
// invalid value (a truncated/garbled upstream chunk) also collapses to "{}", the
// same defensive fallback the reference relays use (CLIProxyAPI FixJSON+gjson.Valid,
// axonhub SafeJSONRawMessage, new-api). Any valid JSON passes through byte-for-byte —
// real tool arguments are never rewritten. Only function_call items use this; a
// custom_tool_call carries its freeform (non-JSON) payload in Input, where it is valid.
func normalizeToolArguments(args string) string {
	if args == "" {
		return "{}"
	}
	if !json.Valid([]byte(args)) {
		return "{}"
	}
	return args
}

// GetInternalResponse returns the complete internal response for logging, statistics, etc.
// For streaming: aggregates all stored stream chunks into a complete response
// For non-streaming: returns the stored response
func (i *ResponseInbound) GetInternalResponse(ctx context.Context) (*model.InternalLLMResponse, error) {
	// Return stored response for non-stream scenario
	if i.storedResponse != nil {
		return i.storedResponse, nil
	}

	// Aggregate stream chunks for stream scenario
	if len(i.streamChunks) == 0 {
		return nil, nil
	}

	// Use the first chunk as the base
	firstChunk := i.streamChunks[0]
	result := &model.InternalLLMResponse{
		ID:                firstChunk.ID,
		Object:            "chat.completion",
		Created:           firstChunk.Created,
		Model:             firstChunk.Model,
		SystemFingerprint: firstChunk.SystemFingerprint,
		ServiceTier:       firstChunk.ServiceTier,
	}

	// Aggregate choices by index
	choicesMap := make(map[int]*model.Choice)

	// Streamed text is buffered per choice index instead of grown with `*p += frag`:
	// that re-allocates and copies the whole accumulated prefix on every chunk, so a
	// long response costs O(total²). Materialized once after the loop below.
	contentBuf := make(map[int]*strings.Builder)
	reasoningBuf := make(map[int]*strings.Builder)
	// Tool-call argument fragments, buffered as choice index -> tool call index.
	toolArgsBuf := make(map[int]map[int]*strings.Builder)

	for _, chunk := range i.streamChunks {
		// Update ID and Model if they appear in later chunks
		if chunk.ID != "" {
			result.ID = chunk.ID
		}
		if chunk.Model != "" {
			result.Model = chunk.Model
		}

		// Capture usage from the last chunk that has it
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			existingChoice, exists := choicesMap[choice.Index]
			if !exists {
				existingChoice = &model.Choice{
					Index:   choice.Index,
					Message: &model.Message{},
				}
				choicesMap[choice.Index] = existingChoice
			}

			// Aggregate delta content into message
			if choice.Delta != nil {
				delta := choice.Delta

				// Set role if present
				if delta.Role != "" {
					existingChoice.Message.Role = delta.Role
				}

				// Append content
				if delta.Content.Content != nil {
					streamTextBuffer(contentBuf, choice.Index).WriteString(*delta.Content.Content)
				}
				if len(delta.Content.MultipleContent) > 0 {
					existingChoice.Message.Content.MultipleContent = append(
						existingChoice.Message.Content.MultipleContent,
						delta.Content.MultipleContent...,
					)
				}

				// Append reasoning content
				if delta.ReasoningContent != nil {
					streamTextBuffer(reasoningBuf, choice.Index).WriteString(*delta.ReasoningContent)
				}

				// Aggregate tool calls. Argument fragments are buffered per tool index
				// and written back after the loop for the same reason as the text
				// above: mergeToolCall's `Arguments += frag` re-copies the whole
				// accumulated payload on every fragment.
				for _, toolCall := range delta.ToolCalls {
					if toolCall.Function.Arguments != "" {
						byToolIndex, ok := toolArgsBuf[choice.Index]
						if !ok {
							byToolIndex = make(map[int]*strings.Builder)
							toolArgsBuf[choice.Index] = byToolIndex
						}
						streamTextBuffer(byToolIndex, toolCall.Index).WriteString(toolCall.Function.Arguments)
						toolCall.Function.Arguments = ""
					}
					existingChoice.Message.ToolCalls = mergeToolCall(existingChoice.Message.ToolCalls, toolCall)
				}

				// Set refusal if present
				if delta.Refusal != "" {
					existingChoice.Message.Refusal = delta.Refusal
				}
			}

			// Capture finish reason
			if choice.FinishReason != nil {
				existingChoice.FinishReason = choice.FinishReason
			}

			// Capture logprobs
			if choice.Logprobs != nil {
				if existingChoice.Logprobs == nil {
					existingChoice.Logprobs = &model.LogprobsContent{}
				}
				existingChoice.Logprobs.Content = append(existingChoice.Logprobs.Content, choice.Logprobs.Content...)
			}
		}
	}
	if result.Usage == nil && i.usage != nil {
		result.Usage = i.usage
	}

	// The streamed response.created event already committed an id to the client
	// (i.responseID, which falls back to a generated id when the upstream omits
	// one). Session ownership and transcript bridging must key on that same
	// client-visible id; otherwise the next turn's previous_response_id can never
	// resolve and the conversation silently drops history. A late upstream chunk
	// id (result.ID = chunk.ID during aggregation) can diverge from the id already
	// sent to the client when the first chunk carried no id, so realign to the
	// client-visible i.responseID whenever it is set, not only when result.ID is
	// empty.
	if strings.TrimSpace(i.responseID) != "" {
		result.ID = i.responseID
	}

	materializeStreamText(choicesMap, contentBuf, reasoningBuf, toolArgsBuf)

	// Convert map to slice, sorted by index
	result.Choices = make([]model.Choice, 0, len(choicesMap))
	for idx := 0; idx < len(choicesMap); idx++ {
		if choice, exists := choicesMap[idx]; exists {
			result.Choices = append(result.Choices, *choice)
		}
	}

	// Clear stored chunks after aggregation
	i.streamChunks = nil

	return result, nil
}

// formatSSEData formats data as SSE data line
func formatSSEData(data []byte) []byte {
	// Avoid fmt.Sprintf + intermediate string on every stream delta; wire bytes stay identical.
	out := make([]byte, 0, 6+len(data)+2)
	out = append(out, "data: "...)
	out = append(out, data...)
	out = append(out, '\n', '\n')
	return out
}

// Request types

type ResponsesRequest struct {
	Model                string                `json:"model"`
	Instructions         string                `json:"instructions,omitempty"`
	Input                ResponsesInput        `json:"input"`
	Tools                []ResponsesTool       `json:"tools,omitempty"`
	ToolChoice           *ResponsesToolChoice  `json:"tool_choice,omitempty"`
	ParallelToolCalls    *bool                 `json:"parallel_tool_calls,omitempty"`
	Stream               *bool                 `json:"stream,omitempty"`
	ToolStream           *bool                 `json:"tool_stream,omitempty"`
	Text                 *ResponsesTextOptions `json:"text,omitempty"`
	Store                *bool                 `json:"store,omitempty"`
	ServiceTier          *string               `json:"service_tier,omitempty"`
	User                 *string               `json:"user,omitempty"`
	PromptCacheKey       *string               `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention *string               `json:"prompt_cache_retention,omitempty"`
	PreviousResponseID   *string               `json:"previous_response_id,omitempty"`
	Metadata             map[string]string     `json:"metadata,omitempty"`
	ClientMetadata       json.RawMessage       `json:"client_metadata,omitempty"`
	MaxOutputTokens      *int64                `json:"max_output_tokens,omitempty"`
	Temperature          *float64              `json:"temperature,omitempty"`
	TopP                 *float64              `json:"top_p,omitempty"`
	Reasoning            *ResponsesReasoning   `json:"reasoning,omitempty"`
	Include              []string              `json:"include,omitempty"`
	TopLogprobs          *int64                `json:"top_logprobs,omitempty"`
}

type ResponsesInput struct {
	Text  *string
	Items []ResponsesItem
	Raw   json.RawMessage
}

func (i ResponsesInput) MarshalJSON() ([]byte, error) {
	if len(i.Raw) > 0 {
		return i.Raw, nil
	}
	if i.Text != nil {
		return json.Marshal(i.Text)
	}
	return json.Marshal(i.Items)
}

func (i *ResponsesInput) UnmarshalJSON(data []byte) error {
	i.Raw = append(i.Raw[:0], data...)
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		i.Text = &text
		return nil
	}
	var items []ResponsesItem
	if err := json.Unmarshal(data, &items); err == nil {
		i.Items = items
		return nil
	}
	return fmt.Errorf("invalid input format")
}

type ResponsesItem struct {
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Role     string          `json:"role,omitempty"`
	Content  *ResponsesInput `json:"content,omitempty"`
	Status   *string         `json:"status,omitempty"`
	Text     *string         `json:"text,omitempty"`
	ImageURL *string         `json:"image_url,omitempty"`
	Detail   *string         `json:"detail,omitempty"`

	// input_file fields (Codex / OpenAI Responses document input)
	FileID   *string `json:"file_id,omitempty"`
	Filename *string `json:"filename,omitempty"`
	FileData *string `json:"file_data,omitempty"`
	FileURL  *string `json:"file_url,omitempty"`

	// Annotations for output_text content
	Annotations *[]ResponsesAnnotation `json:"annotations,omitempty"`

	// Function call fields
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	// Arguments is a pointer for the same reason Input is (below): a function_call's
	// in-progress output_item.added event must carry an explicit empty string
	// ("arguments":""), the genuine OpenAI Responses shape — sub2api's passthrough
	// relay confirms real upstream emits it — and omitempty on a bare string would
	// drop it. A nil pointer still omits the field, so message/reasoning items that
	// never set it stay byte-for-byte unchanged. Finalized function_call values are
	// normalized to valid JSON ("{}" when empty) via normalizeToolArguments so a
	// codex client's json.Unmarshal never fails.
	Arguments *string `json:"arguments,omitempty"`
	// Input carries a custom_tool_call's freeform payload. It is a pointer so an
	// explicit empty string ("input":"") can be emitted on the in-progress
	// output_item.added event, matching the genuine upstream shape, while
	// function_call items (nil) omit the field entirely.
	Input *string `json:"input,omitempty"`

	// Function call output
	Output *ResponsesInput `json:"output,omitempty"`

	// Image generation fields
	Result       *string `json:"result,omitempty"`
	Background   *string `json:"background,omitempty"`
	OutputFormat *string `json:"output_format,omitempty"`
	Quality      *string `json:"quality,omitempty"`
	Size         *string `json:"size,omitempty"`

	// Reasoning fields
	Summary          []ResponsesReasoningSummary `json:"summary,omitempty"`
	EncryptedContent *string                     `json:"encrypted_content,omitempty"`
}

// MarshalJSON guarantees a reasoning item always carries a "summary" array, even
// when empty. A codex client allocates the summary container from the
// output_item.added(reasoning) event; if omitempty drops an empty summary, the
// following response.reasoning_summary_part.added / reasoning_summary_text.delta
// reference a summary slot the client never created and it errors ("...
// without active item"). Real OpenAI and the reference relays (CLIProxyAPI) always
// emit "summary":[] on the reasoning item. Other item types are unaffected.
func (item ResponsesItem) MarshalJSON() ([]byte, error) {
	type alias ResponsesItem
	data, err := json.Marshal(alias(item))
	if err != nil {
		return nil, err
	}
	if item.Type != "reasoning" || len(item.Summary) != 0 {
		return data, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if _, exists := m["summary"]; !exists {
		m["summary"] = json.RawMessage("[]")
	}
	return json.Marshal(m)
}

func (item *ResponsesItem) UnmarshalJSON(data []byte) error {
	type alias ResponsesItem
	var aux struct {
		alias
		Output json.RawMessage `json:"output,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*item = ResponsesItem(aux.alias)
	if len(aux.Output) == 0 || string(aux.Output) == "null" {
		return nil
	}
	var output ResponsesInput
	if err := json.Unmarshal(aux.Output, &output); err != nil {
		var text string
		if textErr := json.Unmarshal(aux.Output, &text); textErr == nil {
			output.Text = &text
			output.Raw = append(output.Raw[:0], aux.Output...)
		} else {
			return err
		}
	}
	item.Output = &output
	return nil
}

func (item ResponsesItem) isOutputMessageContent() bool {
	if item.Content == nil || len(item.Content.Items) == 0 {
		return false
	}
	for _, ci := range item.Content.Items {
		if ci.Type == "output_text" {
			return true
		}
	}
	return false
}

func isResponsesToolCallItemType(typ string) bool {
	switch strings.TrimSpace(typ) {
	case "tool_call",
		"function_call",
		"local_shell_call",
		"tool_search_call",
		"custom_tool_call",
		"mcp_tool_call":
		return true
	default:
		return false
	}
}

func isResponsesToolOutputItemType(typ string) bool {
	switch strings.TrimSpace(typ) {
	case "function_call_output",
		"tool_search_output",
		"custom_tool_call_output",
		"mcp_tool_call_output":
		return true
	default:
		return false
	}
}

func (item ResponsesItem) GetContentItems() []ResponsesContentItem {
	if item.Content == nil || len(item.Content.Items) == 0 {
		return nil
	}
	result := make([]ResponsesContentItem, 0, len(item.Content.Items))
	for _, ci := range item.Content.Items {
		text := ""
		if ci.Text != nil {
			text = *ci.Text
		}
		result = append(result, ResponsesContentItem{
			Type: ci.Type,
			Text: text,
		})
	}
	return result
}

type ResponsesContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ResponsesReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponsesAnnotation struct {
	Type       string  `json:"type"`
	StartIndex *int    `json:"start_index,omitempty"`
	EndIndex   *int    `json:"end_index,omitempty"`
	URL        *string `json:"url,omitempty"`
	Title      *string `json:"title,omitempty"`
	FileID     *string `json:"file_id,omitempty"`
	Filename   *string `json:"filename,omitempty"`
}

type ResponsesTool struct {
	Raw               json.RawMessage `json:"-"`
	Type              string          `json:"type,omitempty"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Parameters        map[string]any  `json:"parameters,omitempty"`
	Strict            *bool           `json:"strict,omitempty"`
	Background        string          `json:"background,omitempty"`
	OutputFormat      string          `json:"output_format,omitempty"`
	Quality           string          `json:"quality,omitempty"`
	Size              string          `json:"size,omitempty"`
	OutputCompression *int64          `json:"output_compression,omitempty"`
	InputFidelity     string          `json:"input_fidelity,omitempty"`
	InputImageMask    map[string]any  `json:"input_image_mask,omitempty"`
	Moderation        string          `json:"moderation,omitempty"`
	PartialImages     *int64          `json:"partial_images,omitempty"`
	Watermark         bool            `json:"watermark,omitempty"`
}

func (t *ResponsesTool) UnmarshalJSON(data []byte) error {
	type Alias ResponsesTool
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*t = ResponsesTool(alias)
	t.Raw = append(t.Raw[:0], data...)
	return nil
}

type ResponsesToolChoice struct {
	Raw      json.RawMessage `json:"-"`
	Mode     *string         `json:"mode,omitempty"`
	Type     *string         `json:"type,omitempty"`
	Name     *string         `json:"name,omitempty"`
	Function *struct {
		Name string `json:"name,omitempty"`
	} `json:"function,omitempty"`
}

func (t *ResponsesToolChoice) UnmarshalJSON(data []byte) error {
	raw := append(json.RawMessage(nil), data...)
	var mode string
	if err := json.Unmarshal(data, &mode); err == nil {
		if mode == "" {
			// JSON null (json.Unmarshal leaves the string zero-valued and returns
			// no error) or an explicit empty string is NOT a usable tool_choice
			// mode. Leaving Mode nil lets the outbound omit tool_choice entirely so
			// strict upstreams (e.g. deepseek's serde) don't reject `"tool_choice": ""`.
			t.Raw = raw
			return nil
		}
		t.Mode = &mode
		t.Raw = raw
		return nil
	}

	type Alias ResponsesToolChoice
	var alias Alias
	if err := json.Unmarshal(data, &alias); err == nil {
		*t = ResponsesToolChoice(alias)
		t.Raw = raw
		return nil
	}

	return fmt.Errorf("invalid tool choice format")
}

type ResponsesTextOptions struct {
	Raw       json.RawMessage      `json:"-"`
	Format    *ResponsesTextFormat `json:"format,omitempty"`
	Verbosity *string              `json:"verbosity,omitempty"`
}

func (t *ResponsesTextOptions) UnmarshalJSON(data []byte) error {
	type Alias ResponsesTextOptions
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*t = ResponsesTextOptions(alias)
	t.Raw = append(t.Raw[:0], data...)
	return nil
}

type ResponsesTextFormat struct {
	Type   string          `json:"type,omitempty"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type ResponsesReasoning struct {
	Effort    string `json:"effort,omitempty"`
	Summary   string `json:"summary,omitempty"`
	MaxTokens *int64 `json:"max_tokens,omitempty"`
}

// Response types

type ResponsesResponse struct {
	Object    string          `json:"object"`
	ID        string          `json:"id"`
	Model     string          `json:"model"`
	CreatedAt int64           `json:"created_at"`
	Output    []ResponsesItem `json:"output"`
	Status    *string         `json:"status,omitempty"`
	Usage     *ResponsesUsage `json:"usage,omitempty"`
	Error     *ResponsesError `json:"error,omitempty"`
}

type ResponsesUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	InputTokenDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
		AudioTokens  int64 `json:"audio_tokens,omitempty"`
	} `json:"input_tokens_details"`
	OutputTokens       int64 `json:"output_tokens"`
	OutputTokenDetails struct {
		ReasoningTokens          int64 `json:"reasoning_tokens"`
		AudioTokens              int64 `json:"audio_tokens,omitempty"`
		AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens,omitempty"`
		RejectedPredictionTokens int64 `json:"rejected_prediction_tokens,omitempty"`
	} `json:"output_tokens_details"`
	TotalTokens int64 `json:"total_tokens"`
}

type ResponsesError struct {
	Code    ResponsesErrorCode `json:"code"`
	Message string             `json:"message"`
}

// ResponsesErrorCode accepts both OpenAI string error codes and numeric
// status-like codes from compatible gateways.
type ResponsesErrorCode string

func (c *ResponsesErrorCode) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*c = ""
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = ResponsesErrorCode(s)
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var n json.Number
	if err := decoder.Decode(&n); err == nil {
		*c = ResponsesErrorCode(n.String())
		return nil
	}

	*c = ResponsesErrorCode(string(data))
	return nil
}

type ResponsesStreamEvent struct {
	Type           string             `json:"type"`
	SequenceNumber int                `json:"sequence_number"`
	Response       *ResponsesResponse `json:"response,omitempty"`
	OutputIndex    *int               `json:"output_index,omitempty"`
	Item           *ResponsesItem     `json:"item,omitempty"`
	ItemID         *string            `json:"item_id,omitempty"`
	ContentIndex   *int               `json:"content_index,omitempty"`
	Delta          string             `json:"delta,omitempty"`
	Text           string             `json:"text,omitempty"`
	Name           string             `json:"name,omitempty"`
	CallID         string             `json:"call_id,omitempty"`
	Arguments      string             `json:"arguments,omitempty"`
	// Input carries the full freeform tool input on a
	// response.custom_tool_call_input.done event.
	Input        string                `json:"input,omitempty"`
	SummaryIndex *int                  `json:"summary_index,omitempty"`
	Part         *ResponsesContentPart `json:"part,omitempty"`
}

type ResponsesContentPart struct {
	Type        string                `json:"type"`
	Text        *string               `json:"text,omitempty"`
	Annotations []ResponsesAnnotation `json:"annotations,omitempty"`
}

// Conversion functions

func convertToInternalRequest(req *ResponsesRequest) (*model.InternalLLMRequest, error) {
	chatReq := &model.InternalLLMRequest{
		Model:                req.Model,
		Temperature:          req.Temperature,
		TopP:                 req.TopP,
		Stream:               req.Stream,
		Store:                req.Store,
		ServiceTier:          req.ServiceTier,
		User:                 req.User,
		PromptCacheKey:       req.PromptCacheKey,
		PromptCacheRetention: req.PromptCacheRetention,
		PreviousResponseID:   req.PreviousResponseID,
		Metadata:             req.Metadata,
		ClientMetadata:       cloneRawMessage(req.ClientMetadata),
		MaxCompletionTokens:  req.MaxOutputTokens,
		TopLogprobs:          req.TopLogprobs,
		ParallelToolCalls:    req.ParallelToolCalls,
		ToolStream:           req.ToolStream,
		RawAPIFormat:         model.APIFormatOpenAIResponse,
		TransformerMetadata:  map[string]string{},
		Include:              append([]string(nil), req.Include...),
		ResponsesInputRaw:    cloneRawMessage(req.Input.Raw),
	}
	if req.Instructions != "" {
		chatReq.ResponsesInstructions = lo.ToPtr(req.Instructions)
	}
	if req.ToolChoice != nil {
		chatReq.ResponsesToolChoiceRaw = cloneRawMessage(req.ToolChoice.Raw)
	}
	if req.Text != nil {
		chatReq.ResponsesTextRaw = cloneRawMessage(req.Text.Raw)
	}
	if len(req.Tools) > 0 {
		chatReq.ResponsesToolsRaw = cloneResponsesToolsRaw(req.Tools)
	}

	if req.Input.Text == nil && len(req.Input.Items) > 0 {
		chatReq.TransformOptions.ArrayInputs = lo.ToPtr(true)
	}

	// Convert reasoning
	if req.Reasoning != nil {
		if req.Reasoning.Effort != "" {
			chatReq.ReasoningEffort = req.Reasoning.Effort
		}
		// Preserve the client's reasoning.summary (a genuine codex CLI sends "auto") so the
		// Responses outbound can forward it verbatim; dropping it makes the upstream reason
		// silently and long turns stream nothing until the end (looks like a hang).
		if req.Reasoning.Summary != "" {
			chatReq.ReasoningSummary = req.Reasoning.Summary
		}
		if req.Reasoning.MaxTokens != nil {
			chatReq.ReasoningBudget = req.Reasoning.MaxTokens
		}
	}

	// Convert tool choice
	if req.ToolChoice != nil {
		chatReq.ToolChoice = convertToolChoiceToInternal(req.ToolChoice)
	}

	// Convert instructions to system message
	messages := make([]model.Message, 0)
	if req.Instructions != "" {
		messages = append(messages, model.Message{
			Role: "system",
			Content: model.MessageContent{
				Content: lo.ToPtr(req.Instructions),
			},
		})
	}

	// Convert input to messages
	inputMessages, err := convertInputToMessages(&req.Input)
	if err != nil {
		return nil, err
	}
	messages = append(messages, inputMessages...)
	chatReq.Messages = messages

	// Convert tools
	if len(req.Tools) > 0 {
		tools, err := convertToolsToInternal(req.Tools)
		if err != nil {
			return nil, err
		}
		chatReq.Tools = tools
	}

	// Convert text format
	if req.Text != nil && req.Text.Format != nil && req.Text.Format.Type != "" {
		chatReq.ResponseFormat = &model.ResponseFormat{
			Type:       req.Text.Format.Type,
			JSONSchema: req.Text.Format.Schema,
		}
	}

	return chatReq, nil
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

func cloneResponsesToolsRaw(tools []ResponsesTool) []json.RawMessage {
	result := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		if len(tool.Raw) == 0 {
			continue
		}
		result = append(result, cloneRawMessage(tool.Raw))
	}
	return result
}

func convertToolChoiceToInternal(src *ResponsesToolChoice) *model.ToolChoice {
	if src == nil {
		return nil
	}

	result := &model.ToolChoice{}
	if src.Mode != nil {
		result.ToolChoice = src.Mode
		return result
	}

	if src.Type != nil {
		name := ""
		if src.Name != nil {
			name = *src.Name
		}
		if name == "" && src.Function != nil {
			name = src.Function.Name
		}
		if name == "" {
			return nil
		}
		result.NamedToolChoice = &model.NamedToolChoice{
			Type: "function",
			Function: model.ToolFunction{
				Name: name,
			},
		}
		return result
	}
	return nil
}

func convertInputToMessages(input *ResponsesInput) ([]model.Message, error) {
	if input == nil {
		return nil, nil
	}

	// Simple text input
	if input.Text != nil {
		return []model.Message{
			{
				Role: "user",
				Content: model.MessageContent{
					Content: input.Text,
				},
			},
		}, nil
	}

	// Array of items.
	//
	// pendingReasoningText and pendingReasoningSignature hold the content from a
	// reasoning item until we know what follows it.  DeepSeek V4 requires that the
	// reasoning_content which produced a tool call lives on the SAME assistant
	// message as that tool call; sending them as two separate consecutive assistant
	// messages causes a 400 on the second multi-turn request.
	//
	// Strategy:
	//   reasoning item      → save text/signature, do NOT emit yet.
	//   function_call item  → create the tool-call assistant message and inject the
	//                         saved reasoning text into its ReasoningContent (if it
	//                         is not already set); clear pending state.
	//   any other item      → flush any saved reasoning as a standalone assistant
	//                         message first (preserves existing behaviour for turns
	//                         where reasoning precedes a plain text response).
	messages := make([]model.Message, 0, len(input.Items))
	var pendingReasoningText string
	var pendingReasoningSignature string
	// lastToolCallMsgIdx points at the assistant message currently accumulating a
	// run of parallel tool calls, or -1 when the previous item was not a tool call.
	// Consecutive function_call items in the Responses input belong to the SAME
	// assistant turn (parallel calls) and MUST be merged into one assistant
	// message's tool_calls array. Emitting one assistant message per call produces
	// an assistant→assistant→tool→tool sequence that strict OpenAI-compatible
	// upstreams (DeepSeek) reject with a misleading deserialize error.
	lastToolCallMsgIdx := -1

	flushPendingReasoning := func() {
		if pendingReasoningText == "" && pendingReasoningSignature == "" {
			return
		}
		msg := model.Message{Role: "assistant"}
		if pendingReasoningText != "" {
			msg.ReasoningContent = lo.ToPtr(pendingReasoningText)
		}
		if pendingReasoningSignature != "" {
			msg.ReasoningSignature = lo.ToPtr(pendingReasoningSignature)
		}
		messages = append(messages, msg)
		pendingReasoningText = ""
		pendingReasoningSignature = ""
	}

	for _, item := range input.Items {
		if item.Type == "reasoning" {
			// Accumulate summary text.  Overwriting any prior value is intentional:
			// a new reasoning item in the same turn replaces the old one (this
			// should not happen in practice but guards against malformed histories).
			var sb strings.Builder
			for _, summary := range item.Summary {
				sb.WriteString(summary.Text)
			}
			if sb.Len() > 0 {
				pendingReasoningText = sb.String()
			}
			if item.EncryptedContent != nil && *item.EncryptedContent != "" {
				pendingReasoningSignature = *item.EncryptedContent
			}
			// A reasoning item marks the start of a new assistant turn (it precedes
			// the tool calls it produced), so it closes any open parallel-call run.
			lastToolCallMsgIdx = -1
			// Do not emit a message yet; wait to see what follows.
			continue
		}

		if isResponsesToolCallItemType(item.Type) {
			msg, err := convertItemToMessage(&item)
			if err != nil {
				return nil, err
			}
			if msg != nil {
				if lastToolCallMsgIdx >= 0 && len(msg.ToolCalls) > 0 {
					// Parallel tool call in the SAME assistant turn: append it to the
					// open assistant message's tool_calls array instead of emitting a
					// second assistant message.
					messages[lastToolCallMsgIdx].ToolCalls = append(messages[lastToolCallMsgIdx].ToolCalls, msg.ToolCalls...)
				} else {
					// Inject the pending reasoning onto this tool-call assistant message.
					// Only inject when the message carries no reasoning already so the
					// function stays idempotent if the caller sets it elsewhere.
					if pendingReasoningText != "" && msg.GetReasoningContent() == "" {
						msg.ReasoningContent = lo.ToPtr(pendingReasoningText)
					}
					// Carry the reasoning signature too — the standalone-flush path and
					// convertItemToMessage both set it, so dropping it only here loses the
					// encrypted-reasoning signature on a reasoning->tool_call turn.
					if pendingReasoningSignature != "" && msg.ReasoningSignature == nil {
						msg.ReasoningSignature = lo.ToPtr(pendingReasoningSignature)
					}
					messages = append(messages, *msg)
					lastToolCallMsgIdx = len(messages) - 1
				}
			}
			pendingReasoningText = ""
			pendingReasoningSignature = ""
			continue
		}

		// Any other item (message, tool output, ...) ends the parallel tool-call
		// run and flushes pending reasoning as a standalone assistant message,
		// preserving prior behaviour for turns where reasoning precedes plain text.
		lastToolCallMsgIdx = -1
		flushPendingReasoning()

		msg, err := convertItemToMessage(&item)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			messages = append(messages, *msg)
		}
	}

	// Flush any reasoning item that was the last in the array (e.g., a
	// reasoning-only response with no following text or tool call).
	flushPendingReasoning()

	return messages, nil
}

func convertItemToMessage(item *ResponsesItem) (*model.Message, error) {
	if item == nil {
		return nil, nil
	}

	switch {
	case item.Type == "message" || item.Type == "input_text" || item.Type == "":
		msg := &model.Message{
			Role: normalizeResponsesInputRole(item.Role),
		}

		if item.Content != nil && len(item.Content.Items) > 0 && item.isOutputMessageContent() {
			msg.Content = convertContentItemsToMessageContent(item.GetContentItems())
		} else if item.Content != nil {
			msg.Content = convertInputToMessageContent(*item.Content)
		} else if item.Text != nil {
			msg.Content = model.MessageContent{Content: item.Text}
		}

		return msg, nil

	case item.Type == "input_image":
		if item.ImageURL != nil {
			return &model.Message{
				Role: normalizeResponsesInputRole(item.Role),
				Content: model.MessageContent{
					MultipleContent: []model.MessageContentPart{
						{
							Type: "image_url",
							ImageURL: &model.ImageURL{
								URL:    *item.ImageURL,
								Detail: item.Detail,
							},
						},
					},
				},
			}, nil
		}
		return nil, nil

	case item.Type == "input_file":
		if filePart := convertInputFileToPart(*item); filePart != nil {
			return &model.Message{
				Role: normalizeResponsesInputRole(item.Role),
				Content: model.MessageContent{
					MultipleContent: []model.MessageContentPart{*filePart},
				},
			}, nil
		}
		return nil, nil

	case isResponsesToolCallItemType(item.Type):
		callID := strings.TrimSpace(item.CallID)
		if callID == "" {
			callID = strings.TrimSpace(item.ID)
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = strings.TrimSuffix(strings.TrimSpace(item.Type), "_call")
		}
		toolCall := model.ToolCall{
			ID:   callID,
			Type: "function",
			Function: model.FunctionCall{
				Name:      name,
				Arguments: lo.FromPtr(item.Arguments),
			},
		}
		// A custom (freeform) tool call carries its payload in `input`, not
		// `arguments`, and must keep its custom nature so the outbound side
		// re-emits a custom_tool_call rather than a function_call — a codex
		// client that registered a custom tool rejects a function_call in its
		// place, and on multi-turn the freeform payload would otherwise be
		// dropped. Mirror the client-facing emit path (the custom branches in
		// handleToolCalls / closeToolItem / convertToResponsesAPIResponse).
		if item.Type == "custom_tool_call" {
			toolCall.Type = model.ToolCallTypeCustom
			if item.Input != nil {
				toolCall.Function.Arguments = lo.FromPtr(item.Input)
			}
		}
		return &model.Message{
			Role:      "assistant",
			ToolCalls: []model.ToolCall{toolCall},
		}, nil

	case isResponsesToolOutputItemType(item.Type):
		callID := strings.TrimSpace(item.CallID)
		if callID == "" {
			callID = strings.TrimSpace(item.ID)
		}
		content := model.MessageContent{Content: lo.ToPtr("")}
		if item.Output != nil {
			content = convertInputToMessageContent(*item.Output)
			if content.Content == nil && len(content.MultipleContent) == 0 {
				content.Content = lo.ToPtr("")
			}
		}
		msg := &model.Message{
			Role:       "tool",
			ToolCallID: lo.ToPtr(callID),
			Content:    content,
		}
		if name := strings.TrimSpace(item.Name); name != "" {
			msg.ToolCallName = lo.ToPtr(name)
		}
		return msg, nil

	case item.Type == "reasoning":
		msg := &model.Message{
			Role: "assistant",
		}

		var reasoningText strings.Builder
		for _, summary := range item.Summary {
			reasoningText.WriteString(summary.Text)
		}

		if reasoningText.Len() > 0 {
			msg.ReasoningContent = lo.ToPtr(reasoningText.String())
		}

		if item.EncryptedContent != nil && *item.EncryptedContent != "" {
			msg.ReasoningSignature = item.EncryptedContent
		}

		return msg, nil

	default:
		return nil, nil
	}
}

func normalizeResponsesInputRole(role string) string {
	switch strings.TrimSpace(role) {
	case "":
		return "user"
	case "developer":
		return "system"
	default:
		return role
	}
}

// convertInputFileToPart maps a Codex / OpenAI Responses `input_file` item to
// the internal "file" content part, preserving base64 data, remote url, file id
// and filename so the document can be faithfully rebuilt outbound. Returns nil
// when the item carries no usable file reference.
func convertInputFileToPart(item ResponsesItem) *model.MessageContentPart {
	file := &model.File{}
	has := false
	if item.Filename != nil && *item.Filename != "" {
		file.Filename = *item.Filename
		has = true
	}
	if item.FileData != nil && *item.FileData != "" {
		file.FileData = *item.FileData
		has = true
	}
	if item.FileURL != nil && *item.FileURL != "" {
		file.FileURL = *item.FileURL
		has = true
	}
	if item.FileID != nil && *item.FileID != "" {
		file.FileID = *item.FileID
		has = true
	}
	if !has {
		return nil
	}
	return &model.MessageContentPart{
		Type: "file",
		File: file,
	}
}

func convertInputToMessageContent(input ResponsesInput) model.MessageContent {
	if input.Text != nil {
		return model.MessageContent{Content: input.Text}
	}

	parts := make([]model.MessageContentPart, 0, len(input.Items))
	for _, item := range input.Items {
		switch item.Type {
		case "input_text", "text", "output_text":
			if item.Text != nil {
				parts = append(parts, model.MessageContentPart{
					Type: "text",
					Text: item.Text,
				})
			}
		case "input_image":
			if item.ImageURL != nil {
				parts = append(parts, model.MessageContentPart{
					Type: "image_url",
					ImageURL: &model.ImageURL{
						URL:    *item.ImageURL,
						Detail: item.Detail,
					},
				})
			}
		case "input_file":
			if filePart := convertInputFileToPart(item); filePart != nil {
				parts = append(parts, *filePart)
			}
		}
	}

	if len(parts) == 1 && parts[0].Type == "text" && parts[0].Text != nil {
		return model.MessageContent{Content: parts[0].Text}
	}

	return model.MessageContent{MultipleContent: parts}
}

func convertContentItemsToMessageContent(items []ResponsesContentItem) model.MessageContent {
	if len(items) == 1 && (items[0].Type == "output_text" || items[0].Type == "input_text" || items[0].Type == "text") {
		return model.MessageContent{Content: lo.ToPtr(items[0].Text)}
	}

	parts := make([]model.MessageContentPart, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "output_text", "input_text", "text":
			parts = append(parts, model.MessageContentPart{
				Type: "text",
				Text: lo.ToPtr(item.Text),
			})
		}
	}

	return model.MessageContent{MultipleContent: parts}
}

func convertToolsToInternal(tools []ResponsesTool) ([]model.Tool, error) {
	result := make([]model.Tool, 0, len(tools))

	for _, tool := range tools {
		switch tool.Type {
		case "function":
			params, err := json.Marshal(tool.Parameters)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function parameters: %w", err)
			}

			result = append(result, model.Tool{
				Type: "function",
				Function: model.Function{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  params,
					Strict:      tool.Strict,
				},
			})

		case "image_generation":
			result = append(result, model.Tool{
				Type: "image_generation",
				ImageGeneration: &model.ImageGeneration{
					Background:        tool.Background,
					InputFidelity:     tool.InputFidelity,
					InputImageMask:    tool.InputImageMask,
					Moderation:        tool.Moderation,
					OutputFormat:      tool.OutputFormat,
					Quality:           tool.Quality,
					Size:              tool.Size,
					OutputCompression: tool.OutputCompression,
					PartialImages:     tool.PartialImages,
					Watermark:         tool.Watermark,
				},
			})
		}
	}

	return result, nil
}

func convertToResponsesAPIResponse(resp *model.InternalLLMResponse) *ResponsesResponse {
	result := &ResponsesResponse{
		Object:    "response",
		ID:        resp.ID,
		Model:     resp.Model,
		CreatedAt: resp.Created,
		Output:    make([]ResponsesItem, 0),
		Status:    lo.ToPtr("completed"),
	}

	// Convert usage
	result.Usage = convertUsageToResponses(resp.Usage)

	// Convert choices to output items
	for _, choice := range resp.Choices {
		var message *model.Message
		if choice.Message != nil {
			message = choice.Message
		} else if choice.Delta != nil {
			message = choice.Delta
		}

		if message == nil {
			continue
		}

		// Handle reasoning content
		if message.ReasoningContent != nil && *message.ReasoningContent != "" {
			reasoningItem := ResponsesItem{
				ID:     generateReasoningItemID(),
				Type:   "reasoning",
				Status: lo.ToPtr("completed"),
				Summary: []ResponsesReasoningSummary{
					{
						Type: "summary_text",
						Text: *message.ReasoningContent,
					},
				},
			}
			// Round-trip claude's thinking.signature as reasoning.encrypted_content so codex can
			// replay it next turn (prevents the unsigned-thinking-block 400 on the following turn).
			if message.ReasoningSignature != nil && *message.ReasoningSignature != "" {
				reasoningItem.EncryptedContent = message.ReasoningSignature
			}
			result.Output = append(result.Output, reasoningItem)
		}

		// Handle tool calls
		if len(message.ToolCalls) > 0 {
			for _, toolCall := range message.ToolCalls {
				// Synthesize a stable id when the upstream tool call carries none and
				// use it for both the item id and call_id — mirrors the streaming path
				// (handleToolCalls). An empty call_id can never be paired with its
				// function_call_output on the next turn.
				callID := toolCall.ID
				if strings.TrimSpace(callID) == "" {
					callID = generateFunctionCallItemID()
				}
				if toolCall.Type == model.ToolCallTypeCustom {
					// Custom (freeform) tool call: re-emit as a custom_tool_call
					// carrying `input`, so a codex client that registered a custom
					// tool receives the shape it expects (a function_call with the
					// freeform text as JSON arguments would be rejected).
					input := toolCall.Function.Arguments
					result.Output = append(result.Output, ResponsesItem{
						ID:     callID,
						Type:   "custom_tool_call",
						CallID: callID,
						Name:   toolCall.Function.Name,
						Input:  &input,
						Status: lo.ToPtr("completed"),
					})
					continue
				}
				result.Output = append(result.Output, ResponsesItem{
					ID:        callID,
					Type:      "function_call",
					CallID:    callID,
					Name:      toolCall.Function.Name,
					Arguments: lo.ToPtr(normalizeToolArguments(toolCall.Function.Arguments)),
					Status:    lo.ToPtr("completed"),
				})
			}
		}

		// Handle text content
		if message.Content.Content != nil && *message.Content.Content != "" {
			text := *message.Content.Content
			result.Output = append(result.Output, ResponsesItem{
				ID:   generateMessageItemID(),
				Type: "message",
				Role: "assistant",
				Content: &ResponsesInput{
					Items: []ResponsesItem{
						{
							Type:        "output_text",
							Text:        &text,
							Annotations: &[]ResponsesAnnotation{},
						},
					},
				},
				Status: lo.ToPtr("completed"),
			})
		} else if len(message.Content.MultipleContent) > 0 {
			contentItems := make([]ResponsesItem, 0)

			for _, part := range message.Content.MultipleContent {
				switch part.Type {
				case "text":
					if part.Text != nil {
						text := *part.Text
						contentItems = append(contentItems, ResponsesItem{
							Type:        "output_text",
							Text:        &text,
							Annotations: &[]ResponsesAnnotation{},
						})
					}
				case "image_url":
					if part.ImageURL != nil {
						result.Output = append(result.Output, ResponsesItem{
							ID:     generateImageGenerationItemID(),
							Type:   "image_generation_call",
							Role:   "assistant",
							Result: lo.ToPtr(xurl.ExtractBase64FromDataURL(part.ImageURL.URL)),
							Status: lo.ToPtr("completed"),
						})
					}
				}
			}

			if len(contentItems) > 0 {
				result.Output = append(result.Output, ResponsesItem{
					ID:      generateMessageItemID(),
					Type:    "message",
					Role:    "assistant",
					Content: &ResponsesInput{Items: contentItems},
					Status:  lo.ToPtr("completed"),
				})
			}
		}

		// Set status based on finish reason
		if choice.FinishReason != nil {
			switch *choice.FinishReason {
			case "stop":
				result.Status = lo.ToPtr("completed")
			case "length":
				result.Status = lo.ToPtr("incomplete")
			case "tool_calls":
				result.Status = lo.ToPtr("completed")
			case "error":
				result.Status = lo.ToPtr("failed")
			}
		}
	}

	// If no output items, create empty message
	if len(result.Output) == 0 {
		emptyText := ""
		result.Output = []ResponsesItem{
			{
				ID:   generateMessageItemID(),
				Type: "message",
				Role: "assistant",
				Content: &ResponsesInput{
					Items: []ResponsesItem{
						{
							Type: "output_text",
							Text: &emptyText,
						},
					},
				},
				Status: lo.ToPtr("completed"),
			},
		}
	}

	return result
}

func convertUsageToResponses(usage *model.Usage) *ResponsesUsage {
	if usage == nil {
		return nil
	}

	result := &ResponsesUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}

	// Anthropic input_tokens excludes cache tokens; OpenAI Responses input_tokens includes them.
	if usage.AnthropicUsage || usage.SeparateCacheInputTokens {
		var cachedTokens int64
		if usage.PromptTokensDetails != nil {
			cachedTokens = usage.PromptTokensDetails.CachedTokens
		}
		result.InputTokens = usage.PromptTokens + cachedTokens + usage.CacheCreationInputTokens
	}

	if usage.PromptTokensDetails != nil {
		result.InputTokenDetails.CachedTokens = usage.PromptTokensDetails.CachedTokens
		result.InputTokenDetails.AudioTokens = usage.PromptTokensDetails.AudioTokens
	}

	if usage.CompletionTokensDetails != nil {
		result.OutputTokenDetails.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
		result.OutputTokenDetails.AudioTokens = usage.CompletionTokensDetails.AudioTokens
		result.OutputTokenDetails.AcceptedPredictionTokens = usage.CompletionTokensDetails.AcceptedPredictionTokens
		result.OutputTokenDetails.RejectedPredictionTokens = usage.CompletionTokensDetails.RejectedPredictionTokens
	}

	return result
}

// Official Responses API id prefixes per item type. A codex/Responses client echoes
// these ids back in the next request's input history, and strict upstreams validate
// the prefix per type (400 "Invalid 'input[N].id' ... Expected an ID that begins with
// 'msg'"). So mint what the real backend mints: msg_ / rs_ / fc_ / ig_.
func generateMessageItemID() string {
	return fmt.Sprintf("msg_%s", lo.RandomString(16, lo.AlphanumericCharset))
}

func generateReasoningItemID() string {
	return fmt.Sprintf("rs_%s", lo.RandomString(16, lo.AlphanumericCharset))
}

func generateFunctionCallItemID() string {
	return fmt.Sprintf("fc_%s", lo.RandomString(16, lo.AlphanumericCharset))
}

func generateImageGenerationItemID() string {
	return fmt.Sprintf("ig_%s", lo.RandomString(16, lo.AlphanumericCharset))
}

func generateResponseID() string {
	return fmt.Sprintf("resp_%s", lo.RandomString(24, lo.AlphanumericCharset))
}
