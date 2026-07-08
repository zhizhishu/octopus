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
	hasResponseCreated      bool
	hasMessageItemStarted   bool
	hasReasoningItemStarted bool
	hasContentPartStarted   bool
	hasFinished             bool
	responseCompleted       bool

	// Response metadata
	responseID   string
	model        string
	createdAt    int64
	finishReason string

	// Content tracking
	outputIndex    int
	contentIndex   int
	sequenceNumber int
	currentItemID  string

	// Content accumulation
	accumulatedText      strings.Builder
	accumulatedReasoning strings.Builder

	// Tool call tracking
	toolCalls           map[int]*model.ToolCall
	toolCallItemStarted map[int]bool
	toolCallOutputIndex map[int]int

	// Finalized output items (message / reasoning / function_call / image),
	// accumulated as each response.output_item.done is emitted, so the terminal
	// response.completed event can carry the full output array. The OpenAI Responses
	// API requires response.completed.response.output to hold the final items; SDK
	// clients (and Cherry Studio) read it to get the result. Streaming used to send
	// output:[] there, so those clients saw zero function_calls and stopped after a
	// tool call instead of executing it.
	completedItems []ResponsesItem

	// Usage tracking
	usage *model.Usage

	// Stream chunks storage for aggregation
	streamChunks []*model.InternalLLMResponse
	// storedResponse stores the non-stream response
	storedResponse *model.InternalLLMResponse
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

	// Store the response for later retrieval
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

	// Store the chunk for aggregation
	i.streamChunks = append(i.streamChunks, stream)

	var events [][]byte

	// Initialize tool call tracking maps if needed
	if i.toolCalls == nil {
		i.toolCalls = make(map[int]*model.ToolCall)
		i.toolCallItemStarted = make(map[int]bool)
		i.toolCallOutputIndex = make(map[int]int)
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

		// Handle reasoning content delta.
		// Use GetReasoningContent so upstreams that emit the `reasoning` field
		// (OpenRouter/Ollama ...) instead of `reasoning_content` are not dropped.
		if choice.Delta != nil {
			if reasoning := choice.Delta.GetReasoningContent(); reasoning != "" {
				events = append(events, i.handleReasoningContent(lo.ToPtr(reasoning))...)
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

		// Handle finish reason
		if choice.FinishReason != nil && !i.hasFinished {
			i.hasFinished = true
			i.finishReason = strings.TrimSpace(*choice.FinishReason)

			// Close any open content parts and output items
			events = append(events, i.closeCurrentContentPart()...)
			events = append(events, i.closeCurrentOutputItem()...)
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

func (i *ResponseInbound) completeResponseEvents() [][]byte {
	if i.responseCompleted || !i.hasResponseCreated {
		return nil
	}

	i.responseCompleted = true
	events := i.closeCurrentOutputItem()

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

// finalOutputItems returns the accumulated finalized output items for the terminal
// response.completed event, or an empty (non-nil) slice so it serializes as [] not
// null when there is genuinely no output.
func (i *ResponseInbound) finalOutputItems() []ResponsesItem {
	if len(i.completedItems) == 0 {
		return []ResponsesItem{}
	}
	return i.completedItems
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
	// /image) flows through a response.output_item.done event. Capture it so the
	// terminal response.completed carries the full output array instead of []. Without
	// this, a client that reconstructs the result from response.completed.output sees
	// no function_calls and stops after the first tool call.
	if ev.Type == "response.output_item.done" && ev.Item != nil {
		i.completedItems = append(i.completedItems, *ev.Item)
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return nil
	}

	return formatSSEData(data)
}

func (i *ResponseInbound) handleReasoningContent(content *string) [][]byte {
	var events [][]byte

	// Start reasoning output item if not started
	if !i.hasReasoningItemStarted {
		// Close any previous output item
		events = append(events, i.closeCurrentOutputItem()...)

		i.hasReasoningItemStarted = true
		i.currentItemID = generateItemID()

		item := &ResponsesItem{
			ID:      i.currentItemID,
			Type:    "reasoning",
			Status:  lo.ToPtr("in_progress"),
			Summary: []ResponsesReasoningSummary{},
		}

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(i.outputIndex),
			Item:        item,
		}))

		// Emit reasoning_summary_part.added
		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.reasoning_summary_part.added",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			SummaryIndex: lo.ToPtr(0),
			Part:         &ResponsesContentPart{Type: "summary_text"},
		}))
	}

	// Accumulate reasoning content
	i.accumulatedReasoning.WriteString(*content)

	// Emit reasoning_summary_text.delta
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_text.delta",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		SummaryIndex: lo.ToPtr(0),
		Delta:        *content,
	}))

	return events
}

func (i *ResponseInbound) handleTextContent(content *string) [][]byte {
	var events [][]byte

	// Close reasoning item if it was started
	if i.hasReasoningItemStarted {
		events = append(events, i.closeReasoningItem()...)
	}

	// Start message output item if not started
	if !i.hasMessageItemStarted {
		i.hasMessageItemStarted = true
		i.currentItemID = generateItemID()

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(i.outputIndex),
			Item: &ResponsesItem{
				ID:      i.currentItemID,
				Type:    "message",
				Status:  lo.ToPtr("in_progress"),
				Role:    "assistant",
				Content: &ResponsesInput{Items: []ResponsesItem{}},
			},
		}))
	}

	// Start content part if not started
	if !i.hasContentPartStarted {
		i.hasContentPartStarted = true

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.content_part.added",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			ContentIndex: &i.contentIndex,
			Part: &ResponsesContentPart{
				Type: "output_text",
				Text: lo.ToPtr(""),
			},
		}))
	}

	// Accumulate text content
	i.accumulatedText.WriteString(*content)

	// Emit output_text.delta
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		ContentIndex: &i.contentIndex,
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

		events = append(events, i.closeCurrentContentPart()...)
		events = append(events, i.closeCurrentOutputItem()...)

		itemID := generateItemID()
		added := &ResponsesItem{
			ID:     itemID,
			Type:   "image_generation_call",
			Status: lo.ToPtr("in_progress"),
		}
		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(i.outputIndex),
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
			OutputIndex: lo.ToPtr(i.outputIndex),
			Item:        done,
		}))
		i.outputIndex++
	}

	return events
}

func (i *ResponseInbound) handleToolCalls(toolCalls []model.ToolCall) [][]byte {
	var events [][]byte

	// Close message item if it was started
	if i.hasMessageItemStarted {
		events = append(events, i.closeMessageItem()...)
	}

	// Close reasoning item if it was started
	if i.hasReasoningItemStarted {
		events = append(events, i.closeReasoningItem()...)
	}

	// Close any still-open message content part once, before opening any tool
	// item. Sibling tool-call items must stay OPEN here: parallel/interleaved
	// tool calls each finalize together at the finish_reason path and on [DONE]
	// via closeCurrentOutputItem(). Finalizing a sibling here would truncate its
	// arguments and emit a wrong output_index.
	events = append(events, i.closeCurrentContentPart()...)

	for _, tc := range toolCalls {
		toolCallIndex := tc.Index

		// Initialize tool call tracking if needed
		if _, ok := i.toolCalls[toolCallIndex]; !ok {
			i.toolCalls[toolCallIndex] = &model.ToolCall{
				Index: toolCallIndex,
				ID:    tc.ID,
				Type:  tc.Type,
				Function: model.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: "",
				},
			}

			itemID := tc.ID
			if itemID == "" {
				itemID = generateItemID()
			}

			item := &ResponsesItem{
				ID:     itemID,
				Type:   "function_call",
				Status: lo.ToPtr("in_progress"),
				CallID: tc.ID,
				Name:   tc.Function.Name,
			}

			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:        "response.output_item.added",
				OutputIndex: lo.ToPtr(i.outputIndex),
				Item:        item,
			}))

			i.toolCallItemStarted[toolCallIndex] = true
			i.toolCallOutputIndex[toolCallIndex] = i.outputIndex
			i.currentItemID = itemID
			i.outputIndex++
		}

		// Accumulate arguments
		i.toolCalls[toolCallIndex].Function.Arguments += tc.Function.Arguments

		// Emit function_call_arguments.delta
		if tc.Function.Arguments != "" {
			itemID := i.toolCalls[toolCallIndex].ID
			if itemID == "" {
				itemID = i.currentItemID
			}

			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:         "response.function_call_arguments.delta",
				ItemID:       &itemID,
				OutputIndex:  lo.ToPtr(i.toolCallOutputIndex[toolCallIndex]),
				ContentIndex: lo.ToPtr(0),
				Delta:        tc.Function.Arguments,
			}))
		}
	}

	return events
}

func (i *ResponseInbound) closeReasoningItem() [][]byte {
	if !i.hasReasoningItemStarted {
		return nil
	}

	var events [][]byte
	i.hasReasoningItemStarted = false
	fullReasoning := i.accumulatedReasoning.String()

	// Emit reasoning_summary_text.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_text.done",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		SummaryIndex: lo.ToPtr(0),
		Text:         fullReasoning,
	}))

	// Emit reasoning_summary_part.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_part.done",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		SummaryIndex: lo.ToPtr(0),
		Part:         &ResponsesContentPart{Type: "summary_text", Text: &fullReasoning},
	}))

	// Emit output_item.done
	item := ResponsesItem{
		ID:   i.currentItemID,
		Type: "reasoning",
		Summary: []ResponsesReasoningSummary{{
			Type: "summary_text",
			Text: fullReasoning,
		}},
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: lo.ToPtr(i.outputIndex),
		Item:        &item,
	}))

	i.outputIndex++
	i.accumulatedReasoning.Reset()

	return events
}

func (i *ResponseInbound) closeMessageItem() [][]byte {
	if !i.hasMessageItemStarted {
		return nil
	}

	var events [][]byte
	i.hasMessageItemStarted = false
	fullText := i.accumulatedText.String()

	// Close content part first
	events = append(events, i.closeCurrentContentPart()...)

	// Emit output_item.done
	item := ResponsesItem{
		ID:     i.currentItemID,
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
		OutputIndex: lo.ToPtr(i.outputIndex),
		Item:        &item,
	}))

	i.outputIndex++
	i.contentIndex = 0
	i.accumulatedText.Reset()

	return events
}

func (i *ResponseInbound) closeCurrentContentPart() [][]byte {
	if !i.hasContentPartStarted {
		return nil
	}

	var events [][]byte
	i.hasContentPartStarted = false
	fullText := i.accumulatedText.String()

	// Emit output_text.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		ContentIndex: &i.contentIndex,
		Text:         fullText,
	}))

	// Emit content_part.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.content_part.done",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		ContentIndex: &i.contentIndex,
		Part: &ResponsesContentPart{
			Type: "output_text",
			Text: lo.ToPtr(fullText),
		},
	}))

	return events
}

func (i *ResponseInbound) closeCurrentOutputItem() [][]byte {
	var events [][]byte

	// Close message item if open
	if i.hasMessageItemStarted {
		events = append(events, i.closeMessageItem()...)
	}

	// Close reasoning item if open
	if i.hasReasoningItemStarted {
		events = append(events, i.closeReasoningItem()...)
	}

	// Close any open tool call items in deterministic ascending index order — map
	// iteration order is random and would scramble the parallel tool_call output ordering
	// in the terminal response.completed.output array.
	toolCallIndexes := make([]int, 0, len(i.toolCalls))
	for idx := range i.toolCalls {
		toolCallIndexes = append(toolCallIndexes, idx)
	}
	sort.Ints(toolCallIndexes)
	for _, idx := range toolCallIndexes {
		tc := i.toolCalls[idx]
		if i.toolCallItemStarted[idx] {
			itemID := tc.ID
			if itemID == "" {
				itemID = i.currentItemID
			}

			// Emit function_call_arguments.done
			toolCallOutputIdx := i.toolCallOutputIndex[idx]
			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:        "response.function_call_arguments.done",
				ItemID:      &itemID,
				OutputIndex: &toolCallOutputIdx,
				Arguments:   tc.Function.Arguments,
			}))

			// Emit output_item.done
			item := ResponsesItem{
				ID:        itemID,
				Type:      "function_call",
				Status:    lo.ToPtr("completed"),
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}

			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:        "response.output_item.done",
				OutputIndex: &toolCallOutputIdx,
				Item:        &item,
			}))

			i.toolCallItemStarted[idx] = false
		}
	}

	return events
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
					if existingChoice.Message.Content.Content == nil {
						existingChoice.Message.Content.Content = new(string)
					}
					*existingChoice.Message.Content.Content += *delta.Content.Content
				}
				if len(delta.Content.MultipleContent) > 0 {
					existingChoice.Message.Content.MultipleContent = append(
						existingChoice.Message.Content.MultipleContent,
						delta.Content.MultipleContent...,
					)
				}

				// Append reasoning content
				if delta.ReasoningContent != nil {
					if existingChoice.Message.ReasoningContent == nil {
						existingChoice.Message.ReasoningContent = new(string)
					}
					*existingChoice.Message.ReasoningContent += *delta.ReasoningContent
				}

				// Aggregate tool calls
				for _, toolCall := range delta.ToolCalls {
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
	return []byte(fmt.Sprintf("data: %s\n\n", string(data)))
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
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

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
	Type           string                `json:"type"`
	SequenceNumber int                   `json:"sequence_number"`
	Response       *ResponsesResponse    `json:"response,omitempty"`
	OutputIndex    *int                  `json:"output_index,omitempty"`
	Item           *ResponsesItem        `json:"item,omitempty"`
	ItemID         *string               `json:"item_id,omitempty"`
	ContentIndex   *int                  `json:"content_index,omitempty"`
	Delta          string                `json:"delta,omitempty"`
	Text           string                `json:"text,omitempty"`
	Name           string                `json:"name,omitempty"`
	CallID         string                `json:"call_id,omitempty"`
	Arguments      string                `json:"arguments,omitempty"`
	SummaryIndex   *int                  `json:"summary_index,omitempty"`
	Part           *ResponsesContentPart `json:"part,omitempty"`
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
		return &model.Message{
			Role: "assistant",
			ToolCalls: []model.ToolCall{
				{
					ID:   callID,
					Type: "function",
					Function: model.FunctionCall{
						Name:      name,
						Arguments: item.Arguments,
					},
				},
			},
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
			result.Output = append(result.Output, ResponsesItem{
				ID:     generateItemID(),
				Type:   "reasoning",
				Status: lo.ToPtr("completed"),
				Summary: []ResponsesReasoningSummary{
					{
						Type: "summary_text",
						Text: *message.ReasoningContent,
					},
				},
			})
		}

		// Handle tool calls
		if len(message.ToolCalls) > 0 {
			for _, toolCall := range message.ToolCalls {
				result.Output = append(result.Output, ResponsesItem{
					ID:        toolCall.ID,
					Type:      "function_call",
					CallID:    toolCall.ID,
					Name:      toolCall.Function.Name,
					Arguments: toolCall.Function.Arguments,
					Status:    lo.ToPtr("completed"),
				})
			}
		}

		// Handle text content
		if message.Content.Content != nil && *message.Content.Content != "" {
			text := *message.Content.Content
			result.Output = append(result.Output, ResponsesItem{
				ID:   generateItemID(),
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
							ID:     generateItemID(),
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
					ID:      generateItemID(),
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
				ID:   generateItemID(),
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

func generateItemID() string {
	return fmt.Sprintf("item_%s", lo.RandomString(16, lo.AlphanumericCharset))
}

func generateResponseID() string {
	return fmt.Sprintf("resp_%s", lo.RandomString(24, lo.AlphanumericCharset))
}
