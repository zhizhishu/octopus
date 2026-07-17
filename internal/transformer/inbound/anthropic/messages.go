package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/tokenizer"
	"github.com/bestruirui/octopus/internal/utils/xurl"
	"github.com/samber/lo"
)

type MessagesInbound struct {
	// Stream state tracking
	hasStarted                bool
	hasTextContentStarted     bool
	hasThinkingContentStarted bool
	hasToolContentStarted     bool
	hasFinished               bool
	messageStopped            bool
	messageID                 string
	modelName                 string
	contentIndex              int64
	stopReason                *string
	toolCallIndices           map[int]bool // Track which tool call indices we've seen
	// toolCallBlockIndex maps an upstream tool_call index to the Anthropic content-block index
	// its tool_use occupies, so interleaved/parallel argument deltas are routed to the correct
	// block instead of the latest one.
	toolCallBlockIndex map[int]int64
	inputToken         int64

	// Stream chunks storage for aggregation
	streamChunks []*model.InternalLLMResponse
	// storedResponse stores the non-stream response
	storedResponse *model.InternalLLMResponse
}

func (i *MessagesInbound) TransformRequest(ctx context.Context, body []byte) (*model.InternalLLMRequest, error) {
	var anthropicReq MessageRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, err
	}
	var rawFields map[string]json.RawMessage
	_ = json.Unmarshal(body, &rawFields)
	if anthropicReq.MaxTokens < 1 {
		anthropicReq.MaxTokens = 1
	}
	chatReq := &model.InternalLLMRequest{
		Model:               anthropicReq.Model,
		MaxTokens:           &anthropicReq.MaxTokens,
		Temperature:         anthropicReq.Temperature,
		TopP:                anthropicReq.TopP,
		Stream:              anthropicReq.Stream,
		Metadata:            map[string]string{},
		RawAPIFormat:        model.APIFormatAnthropicMessage,
		TransformerMetadata: map[string]string{},
	}
	if raw, ok := rawFields["thinking"]; ok && rawJSONPresent(raw) {
		chatReq.AnthropicThinking = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := rawFields["output_config"]; ok && rawJSONPresent(raw) {
		chatReq.AnthropicOutputConfig = append(json.RawMessage(nil), raw...)
	}
	if _, ok := rawFields["tools"]; ok {
		chatReq.AnthropicToolsPresent = true
	}
	if anthropicReq.ServiceTier != "" {
		chatReq.ServiceTier = &anthropicReq.ServiceTier
	}
	if len(anthropicReq.ContextManagement) > 0 && string(anthropicReq.ContextManagement) != "null" {
		chatReq.AnthropicContextManagement = append(json.RawMessage(nil), anthropicReq.ContextManagement...)
	}
	for _, beta := range anthropicReq.Betas {
		beta = strings.TrimSpace(beta)
		if beta != "" {
			chatReq.TransformOptions.AnthropicBetas = append(chatReq.TransformOptions.AnthropicBetas, beta)
		}
	}
	if anthropicReq.Metadata != nil {
		chatReq.Metadata["user_id"] = anthropicReq.Metadata.UserID
	}

	// Convert messages
	messages := make([]model.Message, 0, len(anthropicReq.Messages))

	// Add system message if present
	if anthropicReq.System != nil {
		if anthropicReq.System.Prompt != nil {
			systemContent := anthropicReq.System.Prompt
			messages = append(messages, model.Message{
				Role: "system",
				Content: model.MessageContent{
					Content: systemContent,
				},
			})
			i.inputToken += int64(tokenizer.CountTokens(*systemContent, chatReq.Model))
		} else if len(anthropicReq.System.MultiplePrompts) > 0 {
			// Mark that system was originally in array format
			chatReq.TransformerMetadata["anthropic_system_array_format"] = "true"

			for _, prompt := range anthropicReq.System.MultiplePrompts {
				msg := model.Message{
					Role: "system",
					Content: model.MessageContent{
						Content: &prompt.Text,
					},
					CacheControl: convertToLLMCacheControl(prompt.CacheControl),
				}
				i.inputToken += int64(tokenizer.CountTokens(prompt.Text, chatReq.Model))
				messages = append(messages, msg)
			}
		}
	}

	// Convert Anthropic messages to ChatCompletionMessage
	for msgIndex, msg := range anthropicReq.Messages {
		chatMsg := model.Message{
			Role: msg.Role,
		}

		var (
			hasContent    bool
			hasToolResult bool
		)

		// Convert content

		if msg.Content.Content != nil {
			chatMsg.Content = model.MessageContent{
				Content: msg.Content.Content,
			}
			hasContent = true
			i.inputToken += int64(tokenizer.CountTokens(*msg.Content.Content, chatReq.Model))
		} else if len(msg.Content.MultipleContent) > 0 {
			contentParts := make([]model.MessageContentPart, 0, len(msg.Content.MultipleContent))

			var (
				reasoningContent      string
				hasReasoningInContent bool
			)

			var reasoningSignature string

			for _, block := range msg.Content.MultipleContent {
				switch block.Type {
				case "thinking":
					// Keep thinking content in MultipleContent to preserve order
					if block.Thinking != nil && *block.Thinking != "" {
						reasoningContent = *block.Thinking
						hasReasoningInContent = true
						hasContent = true
					}

					if block.Signature != nil && *block.Signature != "" {
						reasoningSignature = *block.Signature
						hasContent = true
					}
				case "redacted_thinking":
					if strings.TrimSpace(block.Data) != "" {
						hasContent = true
					}
				case "text":
					text := block.Text
					if text == nil {
						text = lo.ToPtr("")
					}
					contentParts = append(contentParts, model.MessageContentPart{
						Type:         "text",
						Text:         text,
						CacheControl: convertToLLMCacheControl(block.CacheControl),
					})
					if *text != "" {
						i.inputToken += int64(tokenizer.CountTokens(*text, chatReq.Model))
					}
					hasContent = true
				case "image":
					if block.Source != nil {
						part := model.MessageContentPart{
							Type:         "image_url",
							CacheControl: convertToLLMCacheControl(block.CacheControl),
						}
						if block.Source.Type == "base64" {
							// Convert Anthropic image format to OpenAI format
							imageURL := fmt.Sprintf("data:%s;base64,%s", block.Source.MediaType, block.Source.Data)
							part.ImageURL = &model.ImageURL{
								URL: imageURL,
							}
						} else {
							part.ImageURL = &model.ImageURL{
								URL: block.Source.URL,
							}
						}

						contentParts = append(contentParts, part)
						hasContent = true
					}
				case "document":
					if docPart := convertDocumentBlockToPart(block.Source, block.CacheControl); docPart != nil {
						contentParts = append(contentParts, *docPart)
						hasContent = true
					}
				case "tool_result":
					hasToolResult = true
					toolMsg := model.Message{
						Role:            "tool",
						MessageIndex:    lo.ToPtr(msgIndex),
						ToolCallID:      block.ToolUseID,
						CacheControl:    convertToLLMCacheControl(block.CacheControl),
						ToolCallIsError: block.IsError,
					}
					toolMsg.Content = convertToolResultContent(block.Content, chatReq.Model, &i.inputToken)
					messages = append(messages, toolMsg)
				case "tool_use", "server_tool_use":
					chatMsg.ToolCalls = append(chatMsg.ToolCalls, model.ToolCall{
						ID:   block.ID,
						Type: "function",
						Function: model.FunctionCall{
							Name:      lo.FromPtr(block.Name),
							Arguments: string(block.Input),
						},
						CacheControl: convertToLLMCacheControl(block.CacheControl),
					})
					hasContent = true
				}
			}

			// Check if it's a simple text-only message (single text block)
			if len(contentParts) == 1 && contentParts[0].Type == "text" {
				// Convert single text block to simple content format for compatibility
				chatMsg.Content = model.MessageContent{
					Content: contentParts[0].Text,
				}
				// Preserve cache control at message level when simplifying
				if contentParts[0].CacheControl != nil {
					chatMsg.CacheControl = contentParts[0].CacheControl
				}

				hasContent = true
			} else if len(contentParts) > 0 {
				chatMsg.Content = model.MessageContent{
					MultipleContent: contentParts,
				}
				hasContent = true
			}

			// Assign reasoning content and signature if present
			if reasoningContent != "" && hasReasoningInContent {
				chatMsg.ReasoningContent = &reasoningContent
			}

			if reasoningSignature != "" {
				chatMsg.ReasoningSignature = &reasoningSignature
			}
		}

		if !hasContent {
			continue
		}

		// If this message had tool_result blocks, set MessageIndex so we can match it later
		if hasToolResult {
			chatMsg.MessageIndex = lo.ToPtr(msgIndex)
		}

		messages = append(messages, chatMsg)
	}

	chatReq.Messages = messages

	// Convert tools
	if len(anthropicReq.Tools) > 0 {
		tools := make([]model.Tool, 0, len(anthropicReq.Tools))
		for _, tool := range anthropicReq.Tools {
			if tool.IsBuiltin() {
				// Anthropic built-in tools (computer-use, bash, text_editor, ...)
				// carry no standard input_schema and rely on proprietary fields.
				// Preserve the original tool object verbatim so the Anthropic
				// outbound transformer can re-emit it without loss.
				tools = append(tools, model.Tool{
					Type:         model.ToolTypeAnthropicBuiltin,
					RawTool:      tool.Raw,
					CacheControl: convertToLLMCacheControl(tool.CacheControl),
				})
				i.inputToken += int64(tokenizer.CountTokens(string(tool.Raw), chatReq.Model))
				continue
			}
			llmTool := model.Tool{
				Type: "function",
				Function: model.Function{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.InputSchema,
				},
				CacheControl: convertToLLMCacheControl(tool.CacheControl),
			}
			tools = append(tools, llmTool)
			i.inputToken += int64(tokenizer.CountTokens(tool.Name, chatReq.Model))
			i.inputToken += int64(tokenizer.CountTokens(tool.Description, chatReq.Model))
			i.inputToken += int64(tokenizer.CountTokens(string(tool.InputSchema), chatReq.Model))
		}
		i.inputToken += int64(len(tools) * 3)

		chatReq.Tools = tools
	}

	// Convert tool choice and parallel tool-use preference. Agent/MCP flows often
	// rely on these fields to force a specific tool or disable parallel calls.
	if anthropicReq.ToolChoice != nil {
		if toolChoice, parallelToolCalls := convertAnthropicToolChoice(anthropicReq.ToolChoice); toolChoice != nil || parallelToolCalls != nil {
			chatReq.ToolChoice = toolChoice
			chatReq.ParallelToolCalls = parallelToolCalls
		}
	}

	// Convert stop sequences
	if len(anthropicReq.StopSequences) > 0 {
		if len(anthropicReq.StopSequences) == 1 {
			chatReq.Stop = &model.Stop{
				Stop: &anthropicReq.StopSequences[0],
			}
		} else {
			chatReq.Stop = &model.Stop{
				MultipleStop: anthropicReq.StopSequences,
			}
		}
	}

	// Convert thinking configuration to reasoning effort and preserve budget
	if anthropicReq.Thinking != nil {
		switch anthropicReq.Thinking.Type {
		case ThinkingTypeEnabled:
			if anthropicReq.Thinking.BudgetTokens != nil {
				chatReq.ReasoningEffort = thinkingBudgetToReasoningEffort(*anthropicReq.Thinking.BudgetTokens)
				chatReq.ReasoningBudget = anthropicReq.Thinking.BudgetTokens
			} else {
				log.Warnf("thinking type is 'enabled' but budget_tokens is nil, thinking will be ignored")
			}
		case ThinkingTypeAdaptive:
			effort := EffortHigh
			if anthropicReq.OutputConfig != nil && anthropicReq.OutputConfig.Effort != "" {
				effort = anthropicReq.OutputConfig.Effort
			}
			chatReq.ReasoningEffort = effort
			chatReq.AdaptiveThinking = true
		case ThinkingTypeDisabled:
			// Explicitly disabled, nothing to do
		default:
			log.Warnf("unknown thinking type: %s", anthropicReq.Thinking.Type)
		}
	}
	return chatReq, nil
}

func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func convertAnthropicToolChoice(choice *ToolChoice) (*model.ToolChoice, *bool) {
	if choice == nil {
		return nil, nil
	}

	var result *model.ToolChoice
	switch strings.ToLower(choice.Type) {
	case "auto", "none":
		mode := strings.ToLower(choice.Type)
		result = &model.ToolChoice{ToolChoice: &mode}
	case "any":
		mode := "required"
		result = &model.ToolChoice{ToolChoice: &mode}
	case "tool":
		if choice.Name != nil && strings.TrimSpace(*choice.Name) != "" {
			result = &model.ToolChoice{
				NamedToolChoice: &model.NamedToolChoice{
					Type: "function",
					Function: model.ToolFunction{
						Name: *choice.Name,
					},
				},
			}
		}
	}

	var parallelToolCalls *bool
	if choice.DisableParallelToolUse != nil {
		parallel := !*choice.DisableParallelToolUse
		parallelToolCalls = &parallel
	}

	return result, parallelToolCalls
}

func convertToolResultContent(content *MessageContent, modelName string, inputToken *int64) model.MessageContent {
	if content == nil {
		return model.MessageContent{Content: lo.ToPtr("")}
	}

	if content.Content != nil {
		if inputToken != nil && *content.Content != "" {
			*inputToken += int64(tokenizer.CountTokens(*content.Content, modelName))
		}
		return model.MessageContent{Content: content.Content}
	}

	if len(content.MultipleContent) == 0 {
		return model.MessageContent{Content: lo.ToPtr("")}
	}

	parts := make([]model.MessageContentPart, 0, len(content.MultipleContent))
	for _, contentBlock := range content.MultipleContent {
		switch contentBlock.Type {
		case "text":
			text := contentBlock.Text
			if text == nil {
				text = lo.ToPtr("")
			}
			parts = append(parts, model.MessageContentPart{
				Type: "text",
				Text: text,
			})
			if inputToken != nil && *text != "" {
				*inputToken += int64(tokenizer.CountTokens(*text, modelName))
			}
		case "image":
			if contentBlock.Source == nil {
				continue
			}
			imageURL := contentBlock.Source.URL
			if contentBlock.Source.Type == "base64" {
				imageURL = fmt.Sprintf("data:%s;base64,%s", contentBlock.Source.MediaType, contentBlock.Source.Data)
			}
			if imageURL == "" {
				continue
			}
			parts = append(parts, model.MessageContentPart{
				Type: "image_url",
				ImageURL: &model.ImageURL{
					URL: imageURL,
				},
			})
		case "document":
			if docPart := convertDocumentBlockToPart(contentBlock.Source, contentBlock.CacheControl); docPart != nil {
				parts = append(parts, *docPart)
			}
		}
	}

	if len(parts) == 0 {
		return model.MessageContent{Content: lo.ToPtr("")}
	}
	return model.MessageContent{MultipleContent: parts}
}

// convertDocumentBlockToPart maps an Anthropic `document` block source to the
// internal "file" content part. It preserves base64 data + media type, remote
// URL, or a provider file id so the document can be faithfully rebuilt by the
// outbound transformers. Returns nil when the source carries nothing usable.
func convertDocumentBlockToPart(source *ImageSource, cacheControl *CacheControl) *model.MessageContentPart {
	if source == nil {
		return nil
	}

	file := &model.File{}
	switch source.Type {
	case "base64":
		if source.Data == "" {
			return nil
		}
		mediaType := source.MediaType
		if mediaType == "" {
			// Anthropic base64 document sources require a media type (PDF is the
			// only base64 document type), so default a missing one instead of
			// round-tripping an invalid source.media_type="" upstream.
			mediaType = "application/pdf"
		}
		file.MediaType = mediaType
		file.FileData = fmt.Sprintf("data:%s;base64,%s", mediaType, source.Data)
	case "url":
		if source.URL == "" {
			return nil
		}
		file.FileURL = source.URL
		file.MediaType = source.MediaType
	case "file":
		if source.FileID == "" {
			return nil
		}
		file.FileID = source.FileID
		file.MediaType = source.MediaType
	default:
		// Tolerate sources that omit an explicit type but carry data/url/id.
		switch {
		case source.Data != "":
			file.MediaType = source.MediaType
			if source.MediaType != "" {
				file.FileData = fmt.Sprintf("data:%s;base64,%s", source.MediaType, source.Data)
			} else {
				file.FileData = source.Data
			}
		case source.URL != "":
			file.FileURL = source.URL
			file.MediaType = source.MediaType
		case source.FileID != "":
			file.FileID = source.FileID
			file.MediaType = source.MediaType
		default:
			return nil
		}
	}

	return &model.MessageContentPart{
		Type:         "file",
		File:         file,
		CacheControl: convertToLLMCacheControl(cacheControl),
	}
}

func (i *MessagesInbound) TransformResponse(ctx context.Context, response *model.InternalLLMResponse) ([]byte, error) {
	// Store the response for later retrieval
	i.storedResponse = response

	resp := &Message{
		ID:    response.ID,
		Type:  "message",
		Role:  "assistant",
		Model: response.Model,
	}

	// Convert choices to content blocks
	if len(response.Choices) > 0 {
		choice := response.Choices[0]

		var message *model.Message

		if choice.Message != nil {
			message = choice.Message
		} else if choice.Delta != nil {
			message = choice.Delta
		}

		if message != nil {
			var contentBlocks []MessageContentBlock

			// Handle reasoning content (thinking) first if present
			if message.ReasoningContent != nil && *message.ReasoningContent != "" {
				thinkingBlock := MessageContentBlock{
					Type:     "thinking",
					Thinking: message.ReasoningContent,
				}
				if message.ReasoningSignature != nil && *message.ReasoningSignature != "" {
					thinkingBlock.Signature = message.ReasoningSignature
				} else {
					thinkingBlock.Signature = lo.ToPtr("ANTHROPIC_MAGIC_STRING_TRIGGER_REDACTED_THINKING_46C9A13E193C177646C7398A98432ECCCE4C1253D5E2D82641AC0E52CC2876CB")
				}

				contentBlocks = append(contentBlocks, thinkingBlock)
			}

			// Handle regular content
			if message.Content.Content != nil && *message.Content.Content != "" {
				contentBlocks = append(contentBlocks, MessageContentBlock{
					Type: "text",
					Text: message.Content.Content,
				})
			} else if len(message.Content.MultipleContent) > 0 {
				for _, part := range message.Content.MultipleContent {
					switch part.Type {
					case "text":
						if part.Text != nil {
							contentBlocks = append(contentBlocks, MessageContentBlock{
								Type: "text",
								Text: part.Text,
							})
						}
					case "image_url":
						if part.ImageURL != nil && part.ImageURL.URL != "" {
							// Convert OpenAI image format to Anthropic format
							url := part.ImageURL.URL
							if parsed := xurl.ParseDataURL(url); parsed != nil {
								contentBlocks = append(contentBlocks, MessageContentBlock{
									Type: "image",
									Source: &ImageSource{
										Type:      "base64",
										MediaType: parsed.MediaType,
										Data:      parsed.Data,
									},
								})
							} else {
								contentBlocks = append(contentBlocks, MessageContentBlock{
									Type: "image",
									Source: &ImageSource{
										Type: "url",
										URL:  part.ImageURL.URL,
									},
								})
							}
						}
					}
				}
			}

			// Handle tool calls
			if len(message.ToolCalls) > 0 {
				for _, toolCall := range message.ToolCalls {
					var input json.RawMessage
					if toolCall.Function.Arguments != "" {
						// Attempt to use the provided arguments; repair if invalid, fallback to {}
						if json.Valid([]byte(toolCall.Function.Arguments)) {
							input = json.RawMessage(toolCall.Function.Arguments)
						} else {
							input = json.RawMessage("{}")
						}
					} else {
						input = json.RawMessage("{}")
					}

					contentBlocks = append(contentBlocks, MessageContentBlock{
						Type:  "tool_use",
						ID:    toolCall.ID,
						Name:  &toolCall.Function.Name,
						Input: input,
					})
				}
			}

			resp.Content = contentBlocks
		}

		// Convert finish reason
		if choice.FinishReason != nil {
			switch *choice.FinishReason {
			case "stop":
				stopReason := "end_turn"
				resp.StopReason = &stopReason
			case "length":
				stopReason := "max_tokens"
				resp.StopReason = &stopReason
			case "tool_calls":
				stopReason := "tool_use"
				resp.StopReason = &stopReason
			default:
				resp.StopReason = choice.FinishReason
			}
		}
	}

	// Convert usage
	if response.Usage != nil {
		resp.Usage = convertAnthropicClientUsage(response.Usage)
	}

	return json.Marshal(resp)
}

func (i *MessagesInbound) TransformStream(ctx context.Context, stream *model.InternalLLMResponse) ([]byte, error) {
	// Handle [DONE] marker
	if stream.Object == "[DONE]" {
		if i.hasFinished && !i.messageStopped {
			events, err := i.finalizeMessageEvents(nil)
			if err != nil {
				return nil, err
			}
			return joinSSEEvents(events), nil
		}
		// Abnormal end: the message envelope was opened and content streamed, but the
		// upstream never delivered a finish_reason (a weak model running away to
		// max_tokens, or a truncated/cut upstream stream). Without a terminal here the
		// anthropic stream is left open (dangling content_block, no message_delta /
		// message_stop) and strict clients (Claude Code) reject the turn as truncated.
		// Mirror the finish path: close the open content block, synthesize a stop_reason,
		// then emit message_delta + message_stop. Only this abnormal case is affected;
		// the clean paths above/return-nil below are byte-for-byte unchanged.
		if i.hasStarted && !i.messageStopped {
			var events [][]byte
			if i.hasTextContentStarted || i.hasThinkingContentStarted || i.hasToolContentStarted {
				i.hasTextContentStarted = false
				i.hasThinkingContentStarted = false
				i.hasToolContentStarted = false
				stopEvent := StreamEvent{
					Type:  "content_block_stop",
					Index: &i.contentIndex,
				}
				data, err := json.Marshal(stopEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_stop", data))
			}
			if i.stopReason == nil {
				stopReason := "max_tokens"
				i.stopReason = &stopReason
			}
			finalEvents, err := i.finalizeMessageEvents(nil)
			if err != nil {
				return nil, err
			}
			events = append(events, finalEvents...)
			return joinSSEEvents(events), nil
		}
		return nil, nil
	}

	// Store the chunk for aggregation
	i.streamChunks = append(i.streamChunks, stream)

	var events [][]byte

	// Initialize message ID and model from first chunk
	if i.messageID == "" && stream.ID != "" {
		i.messageID = stream.ID
	}
	if i.modelName == "" && stream.Model != "" {
		i.modelName = stream.Model
	}

	// Generate message_start event if this is the first chunk
	if !i.hasStarted {
		i.hasStarted = true

		usage := &Usage{
			InputTokens:  i.inputToken,
			OutputTokens: 1,
		}
		if stream.Usage != nil {
			usage = i.convertUsage(stream.Usage)
		}

		startEvent := StreamEvent{
			Type: "message_start",
			Message: &StreamMessage{
				ID:      i.messageID,
				Type:    "message",
				Role:    "assistant",
				Model:   i.modelName,
				Content: []MessageContentBlock{},
				Usage:   usage,
			},
		}

		data, err := json.Marshal(startEvent)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal message_start event: %w", err)
		}
		events = append(events, formatSSEEvent("message_start", data))
	}

	// Process the current chunk
	if len(stream.Choices) > 0 {
		choice := stream.Choices[0]

		// Handle reasoning content (thinking) delta.
		// Use GetReasoningContent so upstreams that emit the `reasoning` field
		// (OpenRouter/Ollama ...) instead of `reasoning_content` are not dropped.
		var reasoningDelta string
		if choice.Delta != nil {
			reasoningDelta = choice.Delta.GetReasoningContent()
		}
		if reasoningDelta != "" {
			// If the tool content has started before the thinking content, we need to stop it
			if i.hasToolContentStarted {
				i.hasToolContentStarted = false

				stopEvent := StreamEvent{
					Type:  "content_block_stop",
					Index: &i.contentIndex,
				}
				data, err := json.Marshal(stopEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_stop", data))

				i.contentIndex++
			}

			// Generate content_block_start if this is the first thinking content
			if !i.hasThinkingContentStarted {
				i.hasThinkingContentStarted = true

				startEvent := StreamEvent{
					Type:  "content_block_start",
					Index: &i.contentIndex,
					ContentBlock: &MessageContentBlock{
						Type:      "thinking",
						Thinking:  lo.ToPtr(""),
						Signature: lo.ToPtr(""),
					},
				}
				data, err := json.Marshal(startEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_start event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_start", data))
			}

			// Generate content_block_delta for thinking
			deltaEvent := StreamEvent{
				Type:  "content_block_delta",
				Index: &i.contentIndex,
				Delta: &StreamDelta{
					Type:     lo.ToPtr("thinking_delta"),
					Thinking: lo.ToPtr(reasoningDelta),
				},
			}
			data, err := json.Marshal(deltaEvent)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal content_block_delta event: %w", err)
			}
			events = append(events, formatSSEEvent("content_block_delta", data))
		}

		// Add signature delta if signature is available
		if choice.Delta != nil && choice.Delta.ReasoningSignature != nil && *choice.Delta.ReasoningSignature != "" {
			sigEvent := StreamEvent{
				Type:  "content_block_delta",
				Index: &i.contentIndex,
				Delta: &StreamDelta{
					Type:      lo.ToPtr("signature_delta"),
					Signature: choice.Delta.ReasoningSignature,
				},
			}
			data, err := json.Marshal(sigEvent)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal signature_delta event: %w", err)
			}
			events = append(events, formatSSEEvent("content_block_delta", data))
		}

		// Handle content delta
		if choice.Delta != nil && choice.Delta.Content.Content != nil && *choice.Delta.Content.Content != "" {
			// If the thinking content has started before the text content, we need to stop it
			if i.hasThinkingContentStarted {
				i.hasThinkingContentStarted = false

				stopEvent := StreamEvent{
					Type:  "content_block_stop",
					Index: &i.contentIndex,
				}
				data, err := json.Marshal(stopEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_stop", data))

				i.contentIndex++
			}

			// If the tool content has started before the content block, we need to stop it
			if i.hasToolContentStarted {
				i.hasToolContentStarted = false

				stopEvent := StreamEvent{
					Type:  "content_block_stop",
					Index: &i.contentIndex,
				}
				data, err := json.Marshal(stopEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_stop", data))

				i.contentIndex++
			}

			// Generate content_block_start if this is the first content
			if !i.hasTextContentStarted {
				i.hasTextContentStarted = true

				startEvent := StreamEvent{
					Type:  "content_block_start",
					Index: &i.contentIndex,
					ContentBlock: &MessageContentBlock{
						Type: "text",
						Text: lo.ToPtr(""),
					},
				}
				data, err := json.Marshal(startEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_start event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_start", data))
			}

			// Generate content_block_delta
			deltaEvent := StreamEvent{
				Type:  "content_block_delta",
				Index: &i.contentIndex,
				Delta: &StreamDelta{
					Type: lo.ToPtr("text_delta"),
					Text: choice.Delta.Content.Content,
				},
			}
			data, err := json.Marshal(deltaEvent)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal content_block_delta event: %w", err)
			}
			events = append(events, formatSSEEvent("content_block_delta", data))
		}

		// Handle tool calls
		if choice.Delta != nil && len(choice.Delta.ToolCalls) > 0 {
			// If the thinking content has started before the tool content, we need to stop it
			if i.hasThinkingContentStarted {
				i.hasThinkingContentStarted = false

				stopEvent := StreamEvent{
					Type:  "content_block_stop",
					Index: &i.contentIndex,
				}
				data, err := json.Marshal(stopEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_stop", data))

				i.contentIndex++
			}

			// If the text content has started before the tool content, we need to stop it
			if i.hasTextContentStarted {
				i.hasTextContentStarted = false

				stopEvent := StreamEvent{
					Type:  "content_block_stop",
					Index: &i.contentIndex,
				}
				data, err := json.Marshal(stopEvent)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
				}
				events = append(events, formatSSEEvent("content_block_stop", data))

				i.contentIndex++
			}

			// Initialize tool call index tracking if needed
			if i.toolCallIndices == nil {
				i.toolCallIndices = make(map[int]bool)
			}
			if i.toolCallBlockIndex == nil {
				i.toolCallBlockIndex = make(map[int]int64)
			}

			for _, deltaToolCall := range choice.Delta.ToolCalls {
				toolCallIndex := deltaToolCall.Index

				// Initialize tool call if it doesn't exist
				if !i.toolCallIndices[toolCallIndex] {
					// Start a new tool use block; close the previous tool block only if one is
					// actually open (guard avoids a spurious content_block_stop when the first
					// tool fragment carries a non-zero index).
					if toolCallIndex > 0 && i.hasToolContentStarted {
						stopEvent := StreamEvent{
							Type:  "content_block_stop",
							Index: &i.contentIndex,
						}
						data, err := json.Marshal(stopEvent)
						if err != nil {
							return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
						}
						events = append(events, formatSSEEvent("content_block_stop", data))

						i.contentIndex++
					}

					i.toolCallIndices[toolCallIndex] = true
					i.hasToolContentStarted = true
					// Remember which Anthropic block index this tool_call index owns, so its
					// later argument deltas (which may interleave with other indices) go here.
					i.toolCallBlockIndex[toolCallIndex] = i.contentIndex

					startEvent := StreamEvent{
						Type:  "content_block_start",
						Index: &i.contentIndex,
						ContentBlock: &MessageContentBlock{
							Type:  "tool_use",
							ID:    deltaToolCall.ID,
							Name:  &deltaToolCall.Function.Name,
							Input: json.RawMessage("{}"),
						},
					}
					data, err := json.Marshal(startEvent)
					if err != nil {
						return nil, fmt.Errorf("failed to marshal content_block_start event: %w", err)
					}
					events = append(events, formatSSEEvent("content_block_start", data))

					// If the tool call has arguments, we need to generate a content_block_delta
					if deltaToolCall.Function.Arguments != "" {
						deltaEvent := StreamEvent{
							Type:  "content_block_delta",
							Index: &i.contentIndex,
							Delta: &StreamDelta{
								Type:        lo.ToPtr("input_json_delta"),
								PartialJSON: &deltaToolCall.Function.Arguments,
							},
						}
						data, err := json.Marshal(deltaEvent)
						if err != nil {
							return nil, fmt.Errorf("failed to marshal content_block_delta event: %w", err)
						}
						events = append(events, formatSSEEvent("content_block_delta", data))
					}
				} else {
					// Generate content_block_delta for input_json_delta, routed to the block this
					// tool_call index owns (not the latest) so interleaved args don't cross-contaminate.
					blockIdx := i.toolCallBlockIndex[toolCallIndex]
					deltaEvent := StreamEvent{
						Type:  "content_block_delta",
						Index: &blockIdx,
						Delta: &StreamDelta{
							Type:        lo.ToPtr("input_json_delta"),
							PartialJSON: &deltaToolCall.Function.Arguments,
						},
					}
					data, err := json.Marshal(deltaEvent)
					if err != nil {
						return nil, fmt.Errorf("failed to marshal content_block_delta event: %w", err)
					}
					events = append(events, formatSSEEvent("content_block_delta", data))
				}
			}
		}

		// Handle finish reason
		if choice.FinishReason != nil && !i.hasFinished {
			i.hasFinished = true

			stopEvent := StreamEvent{
				Type:  "content_block_stop",
				Index: &i.contentIndex,
			}
			data, err := json.Marshal(stopEvent)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal content_block_stop event: %w", err)
			}
			events = append(events, formatSSEEvent("content_block_stop", data))

			// Convert finish reason to Anthropic format
			var stopReason string
			switch *choice.FinishReason {
			case "stop":
				stopReason = "end_turn"
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			default:
				stopReason = "end_turn"
			}

			// Store the stop reason, but don't generate message_delta yet
			// We'll wait for the usage chunk to combine them
			i.stopReason = &stopReason
		}
	}

	// Handle usage chunk after finish_reason
	if stream.Usage != nil && i.hasFinished && !i.messageStopped {
		finalEvents, err := i.finalizeMessageEvents(stream.Usage)
		if err != nil {
			return nil, err
		}
		events = append(events, finalEvents...)
	}

	if len(events) == 0 {
		return nil, nil
	}

	return joinSSEEvents(events), nil
}

func (i *MessagesInbound) finalizeMessageEvents(usage *model.Usage) ([][]byte, error) {
	msgDeltaEvent := StreamEvent{
		Type: "message_delta",
	}

	if i.stopReason != nil {
		msgDeltaEvent.Delta = &StreamDelta{
			StopReason: i.stopReason,
		}
	}
	if usage != nil {
		msgDeltaEvent.Usage = i.convertUsage(usage)
	}

	data, err := json.Marshal(msgDeltaEvent)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message_delta event: %w", err)
	}
	events := [][]byte{formatSSEEvent("message_delta", data)}

	msgStopEvent := StreamEvent{
		Type: "message_stop",
	}
	data, err = json.Marshal(msgStopEvent)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message_stop event: %w", err)
	}
	events = append(events, formatSSEEvent("message_stop", data))

	i.messageStopped = true
	return events, nil
}

func joinSSEEvents(events [][]byte) []byte {
	result := make([]byte, 0)
	for idx, event := range events {
		if idx > 0 {
			result = append(result, '\n')
		}
		result = append(result, event...)
	}
	return result
}

func (i *MessagesInbound) convertUsage(usage *model.Usage) *Usage {
	return convertAnthropicClientUsage(usage)
}

func convertAnthropicClientUsage(usage *model.Usage) *Usage {
	if usage == nil {
		return nil
	}
	anthropicUsage := &Usage{
		InputTokens:              usage.PromptTokens,
		OutputTokens:             usage.CompletionTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
	}
	if usage.PromptTokensDetails != nil {
		anthropicUsage.CacheReadInputTokens = usage.PromptTokensDetails.CachedTokens
	}
	if !usage.AnthropicUsage && !usage.SeparateCacheInputTokens {
		anthropicUsage.InputTokens -= anthropicUsage.CacheReadInputTokens
		if anthropicUsage.InputTokens < 0 {
			anthropicUsage.InputTokens = 0
		}
	}
	return anthropicUsage
}

// GetInternalResponse returns the complete internal response for logging, statistics, etc.
// For streaming: aggregates all stored stream chunks into a complete response
// For non-streaming: returns the stored response
func (i *MessagesInbound) GetInternalResponse(ctx context.Context) (*model.InternalLLMResponse, error) {
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

				// Append reasoning content
				if delta.ReasoningContent != nil {
					if existingChoice.Message.ReasoningContent == nil {
						existingChoice.Message.ReasoningContent = new(string)
					}
					*existingChoice.Message.ReasoningContent += *delta.ReasoningContent
				}

				// Preserve reasoning signature. Unlike reasoning content, the
				// signature arrives as a complete value (Anthropic emits it via a
				// single signature_delta, mapped to ReasoningSignature by the
				// outbound transformer) rather than as incremental fragments, so we
				// keep the last non-empty value instead of appending. Without this
				// the aggregated thinking block loses its signature and gets
				// rejected when sent back to Anthropic on the next turn.
				if delta.ReasoningSignature != nil && *delta.ReasoningSignature != "" {
					sig := *delta.ReasoningSignature
					existingChoice.Message.ReasoningSignature = &sig
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
				// Tool call name is atomic, never a streamed fragment. Take the
				// first non-empty value; never concatenate (some upstream shapes
				// repeat the full name on every chunk, which would duplicate it).
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

// formatSSEEvent 格式化为完整的 SSE 事件格式
func formatSSEEvent(eventType string, data []byte) []byte {
	return []byte(fmt.Sprintf("event:%s\ndata:%s\n\n", eventType, string(data)))
}
