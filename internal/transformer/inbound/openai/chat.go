package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

type ChatInbound struct {
	// streamChunks stores stream chunks for aggregation
	streamChunks []*model.InternalLLMResponse
	// storedResponse stores the non-stream response
	storedResponse *model.InternalLLMResponse
}

func (i *ChatInbound) TransformRequest(ctx context.Context, body []byte) (*model.InternalLLMRequest, error) {
	var request model.InternalLLMRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	request.RawAPIFormat = model.APIFormatOpenAIChatCompletion
	normalizeOpenAIChatToolMessages(&request)
	return &request, nil
}

func normalizeOpenAIChatToolMessages(request *model.InternalLLMRequest) {
	if request == nil {
		return
	}

	callIDsByName := map[string][]string{}
	for msgIndex := range request.Messages {
		msg := &request.Messages[msgIndex]

		if msg.Role == "assistant" && msg.FunctionCall != nil {
			name := msg.FunctionCall.Name
			callID := fmt.Sprintf("call_%s_%d", name, msgIndex)
			msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{
				ID:    callID,
				Type:  "function",
				Index: len(msg.ToolCalls),
				Function: model.FunctionCall{
					Name:      name,
					Arguments: msg.FunctionCall.Arguments,
				},
			})
			msg.FunctionCall = nil
			if name != "" {
				callIDsByName[name] = append(callIDsByName[name], callID)
			}
			continue
		}

		for _, toolCall := range msg.ToolCalls {
			if toolCall.Function.Name == "" || toolCall.ID == "" {
				continue
			}
			callIDsByName[toolCall.Function.Name] = append(callIDsByName[toolCall.Function.Name], toolCall.ID)
		}

		if msg.Role != "function" && msg.Role != "tool" {
			continue
		}
		if msg.Name == nil || *msg.Name == "" {
			continue
		}

		name := *msg.Name
		if msg.ToolCallID == nil || *msg.ToolCallID == "" {
			if ids := callIDsByName[name]; len(ids) > 0 {
				id := ids[0]
				msg.ToolCallID = &id
				callIDsByName[name] = ids[1:]
			} else {
				msg.ToolCallID = &name
			}
		}
		if msg.ToolCallName == nil {
			msg.ToolCallName = &name
		}
		if msg.Role == "function" {
			msg.Role = "tool"
		}
	}
}

// markdownContentForImageMessage renders an assistant message whose content is an image_url
// multipart array (produced by the image-generation bridge) as a plain string carrying a
// Markdown image, mirroring new-api. A chat client's assistant `content` is typed as a
// string, so an array leaves standard clients showing a blank turn; ![image](...) renders
// everywhere. Returns ("", false) unless the message actually carries an image part, so
// ordinary text/tool responses are left untouched (and never copied).
func markdownContentForImageMessage(msg *model.Message) (string, bool) {
	if msg == nil || len(msg.Content.MultipleContent) == 0 {
		return "", false
	}
	hasImage := false
	var b strings.Builder
	if msg.Content.Content != nil && *msg.Content.Content != "" {
		b.WriteString(*msg.Content.Content)
	}
	for _, part := range msg.Content.MultipleContent {
		switch part.Type {
		case "text", "input_text", "output_text":
			if part.Text != nil && *part.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(*part.Text)
			}
		case "image_url":
			if part.ImageURL != nil && strings.TrimSpace(part.ImageURL.URL) != "" {
				hasImage = true
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString("![image](")
				b.WriteString(part.ImageURL.URL)
				b.WriteString(")")
			}
		}
	}
	if !hasImage {
		return "", false
	}
	return b.String(), true
}

// chatResponseWithMarkdownImages returns a view of the response where any assistant image_url
// content is flattened to a Markdown string for the chat wire format. It does NOT mutate the
// input (or i.storedResponse / streamChunks), so the internal response keeps its image_url
// parts — the token estimator ignores those, whereas a flattened base64 data-URL would be
// counted as text tokens and overcharge. Returns the input unchanged (no copy) when there is
// no image content, so ordinary responses pay nothing.
func chatResponseWithMarkdownImages(resp *model.InternalLLMResponse) *model.InternalLLMResponse {
	if resp == nil {
		return resp
	}
	needsCopy := false
	for i := range resp.Choices {
		if _, ok := markdownContentForImageMessage(resp.Choices[i].Message); ok {
			needsCopy = true
			break
		}
		if _, ok := markdownContentForImageMessage(resp.Choices[i].Delta); ok {
			needsCopy = true
			break
		}
	}
	if !needsCopy {
		return resp
	}
	out := *resp
	out.Choices = make([]model.Choice, len(resp.Choices))
	copy(out.Choices, resp.Choices)
	for i := range out.Choices {
		if md, ok := markdownContentForImageMessage(out.Choices[i].Message); ok {
			m := *out.Choices[i].Message
			m.Content = model.MessageContent{Content: &md}
			out.Choices[i].Message = &m
		}
		if md, ok := markdownContentForImageMessage(out.Choices[i].Delta); ok {
			d := *out.Choices[i].Delta
			d.Content = model.MessageContent{Content: &md}
			out.Choices[i].Delta = &d
		}
	}
	return &out
}

func (i *ChatInbound) TransformResponse(ctx context.Context, response *model.InternalLLMResponse) ([]byte, error) {
	// Store the (unflattened) response for later retrieval / billing.
	i.storedResponse = response

	// Marshal a view with any bridged image_url content rendered as a Markdown image string.
	body, err := json.Marshal(chatResponseWithMarkdownImages(response))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (i *ChatInbound) TransformStream(ctx context.Context, stream *model.InternalLLMResponse) ([]byte, error) {
	if stream.Object == "[DONE]" {
		return []byte("data: [DONE]\n\n"), nil
	}

	// Store the (unflattened) chunk for aggregation
	i.streamChunks = append(i.streamChunks, stream)

	// Non-mutating Markdown view of any bridged image_url delta for chat clients.
	wire := chatResponseWithMarkdownImages(stream)

	var body []byte
	var err error

	// Handle the case where choices are empty but we need them to be present as an empty array
	// This is to satisfy some clients (like Cherry Studio) that require choices field to be present
	if len(wire.Choices) == 0 && wire.Object == "chat.completion.chunk" {
		type Alias model.InternalLLMResponse
		aux := &struct {
			*Alias
			Choices []model.Choice `json:"choices"`
		}{
			Alias:   (*Alias)(wire),
			Choices: []model.Choice{},
		}
		body, err = json.Marshal(aux)
	} else {
		body, err = json.Marshal(wire)
	}

	if err != nil {
		return nil, err
	}
	return []byte("data: " + string(body) + "\n\n"), nil
}

// GetInternalResponse returns the complete internal response for logging, statistics, etc.
// For streaming: aggregates all stored stream chunks into a complete response
// For non-streaming: returns the stored response
func (i *ChatInbound) GetInternalResponse(ctx context.Context) (*model.InternalLLMResponse, error) {
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
		// Update ID and Model if they appear in later chunks (some providers send these later)
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

				// Append content (handle both string content and multipart content)
				if delta.Content.Content != nil {
					streamTextBuffer(contentBuf, choice.Index).WriteString(*delta.Content.Content)
				}

				// Append multipart content (for images, audio, etc.)
				if len(delta.Content.MultipleContent) > 0 {
					existingChoice.Message.Content.MultipleContent = append(
						existingChoice.Message.Content.MultipleContent,
						delta.Content.MultipleContent...,
					)
				}

				// Append images (used by Gemini via OpenAI compat endpoint for image generation)
				if len(delta.Images) > 0 {
					existingChoice.Message.Content.MultipleContent = append(
						existingChoice.Message.Content.MultipleContent,
						delta.Images...,
					)
				}

				// Append reasoning content (supports both reasoning_content and reasoning fields)
				if reasoning := delta.GetReasoningContent(); reasoning != "" {
					streamTextBuffer(reasoningBuf, choice.Index).WriteString(reasoning)
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

// streamTextBuffer returns the accumulation buffer for one index, creating it on
// first use. Presence in the map is what marks "this index received a fragment",
// which is how the aggregation keeps the nil-vs-empty distinction the previous
// pointer-append form had.
func streamTextBuffer(buffers map[int]*strings.Builder, index int) *strings.Builder {
	if buffer, ok := buffers[index]; ok {
		return buffer
	}
	buffer := &strings.Builder{}
	buffers[index] = buffer
	return buffer
}

// materializeStreamText writes the buffered fragments back onto the aggregated
// choices. Only indexes that actually received a fragment are written, so a
// choice that never carried content/reasoning keeps the nil pointer it had
// before, and one that carried an empty fragment still gets a non-nil empty
// value — exactly what the old `new(string)` + `+=` form produced.
func materializeStreamText(
	choicesMap map[int]*model.Choice,
	contentBuf map[int]*strings.Builder,
	reasoningBuf map[int]*strings.Builder,
	toolArgsBuf map[int]map[int]*strings.Builder,
) {
	for idx, choice := range choicesMap {
		if buffer, ok := contentBuf[idx]; ok {
			content := buffer.String()
			choice.Message.Content.Content = &content
		}
		if buffer, ok := reasoningBuf[idx]; ok {
			reasoning := buffer.String()
			choice.Message.ReasoningContent = &reasoning
		}
		for k := range choice.Message.ToolCalls {
			if buffer, ok := toolArgsBuf[idx][choice.Message.ToolCalls[k].Index]; ok {
				choice.Message.ToolCalls[k].Function.Arguments = buffer.String()
			}
		}
	}
}

// mergeToolCall merges a tool call delta into the existing tool calls slice
func mergeToolCall(toolCalls []model.ToolCall, delta model.ToolCall) []model.ToolCall {
	// Find existing tool call by index
	for i, tc := range toolCalls {
		if tc.Index == delta.Index {
			// Merge the delta into existing tool call
			if delta.ID != "" {
				toolCalls[i].ID = delta.ID
			}
			if delta.Type != "" {
				toolCalls[i].Type = delta.Type
			}
			if delta.Function.Name != "" && toolCalls[i].Function.Name == "" {
				// Tool call name is atomic, never a streamed fragment. Some
				// upstream shapes repeat the full name on every chunk; take the
				// first non-empty value and never concatenate (which would yield
				// "get_weatherget_weather...").
				toolCalls[i].Function.Name = delta.Function.Name
			}
			if delta.Function.Arguments != "" {
				toolCalls[i].Function.Arguments += delta.Function.Arguments
			}
			return toolCalls
		}
	}

	// New tool call, add it
	return append(toolCalls, delta)
}
