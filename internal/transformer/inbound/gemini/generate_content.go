package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

type requestOptionsKey struct{}

type requestOptions struct {
	model  string
	stream bool
}

func WithRequestOptions(ctx context.Context, model string, stream bool) context.Context {
	return context.WithValue(ctx, requestOptionsKey{}, requestOptions{
		model:  strings.TrimSpace(model),
		stream: stream,
	})
}

type GenerateContentInbound struct {
	streamChunks   []*model.InternalLLMResponse
	storedResponse *model.InternalLLMResponse
}

func (i *GenerateContentInbound) TransformRequest(ctx context.Context, body []byte) (*model.InternalLLMRequest, error) {
	var geminiReq model.GeminiGenerateContentRequest
	if err := json.Unmarshal(body, &geminiReq); err != nil {
		return nil, err
	}

	opts, _ := ctx.Value(requestOptionsKey{}).(requestOptions)
	if opts.model == "" {
		return nil, fmt.Errorf("gemini request model is required")
	}

	req := &model.InternalLLMRequest{
		Model:               opts.model,
		Messages:            convertGeminiMessages(&geminiReq),
		Stream:              &opts.stream,
		Temperature:         nil,
		TopP:                nil,
		RawRequest:          body,
		RawAPIFormat:        model.APIFormatGeminiContents,
		TransformerMetadata: map[string]string{},
	}

	applyGeminiGenerationConfig(req, geminiReq.GenerationConfig)
	applyGeminiTools(req, geminiReq.Tools, geminiReq.ToolConfig)
	if len(geminiReq.SafetySettings) > 0 {
		if b, err := json.Marshal(geminiReq.SafetySettings); err == nil {
			req.TransformerMetadata["gemini_safety_settings"] = string(b)
		}
	}

	return req, nil
}

func (i *GenerateContentInbound) TransformResponse(ctx context.Context, response *model.InternalLLMResponse) ([]byte, error) {
	i.storedResponse = response

	body, err := json.Marshal(convertInternalResponseToGemini(response, false))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (i *GenerateContentInbound) TransformStream(ctx context.Context, stream *model.InternalLLMResponse) ([]byte, error) {
	if stream == nil || stream.Object == "[DONE]" {
		return nil, nil
	}

	i.streamChunks = append(i.streamChunks, stream)
	body, err := json.Marshal(convertInternalResponseToGemini(stream, true))
	if err != nil {
		return nil, err
	}
	return []byte("data: " + string(body) + "\n\n"), nil
}

func (i *GenerateContentInbound) GetInternalResponse(ctx context.Context) (*model.InternalLLMResponse, error) {
	if i.storedResponse != nil {
		return i.storedResponse, nil
	}
	if len(i.streamChunks) == 0 {
		return nil, nil
	}

	first := i.streamChunks[0]
	result := &model.InternalLLMResponse{
		ID:                first.ID,
		Object:            "chat.completion",
		Created:           first.Created,
		Model:             first.Model,
		SystemFingerprint: first.SystemFingerprint,
		ServiceTier:       first.ServiceTier,
	}

	choices := map[int]*model.Choice{}
	for _, chunk := range i.streamChunks {
		if chunk.ID != "" {
			result.ID = chunk.ID
		}
		if chunk.Model != "" {
			result.Model = chunk.Model
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			existing := choices[choice.Index]
			if existing == nil {
				existing = &model.Choice{
					Index:   choice.Index,
					Message: &model.Message{Role: "assistant"},
				}
				choices[choice.Index] = existing
			}
			if choice.Delta != nil {
				mergeDeltaIntoMessage(existing.Message, choice.Delta)
			}
			if choice.FinishReason != nil {
				existing.FinishReason = choice.FinishReason
			}
		}
	}

	for idx := 0; idx < len(choices); idx++ {
		if choice := choices[idx]; choice != nil {
			result.Choices = append(result.Choices, *choice)
		}
	}
	i.streamChunks = nil
	return result, nil
}

func convertGeminiMessages(req *model.GeminiGenerateContentRequest) []model.Message {
	messages := make([]model.Message, 0, len(req.Contents)+1)
	if req.SystemInstruction != nil {
		systemText := joinGeminiText(req.SystemInstruction.Parts)
		if systemText != "" {
			messages = append(messages, model.Message{
				Role: "system",
				Content: model.MessageContent{
					Content: &systemText,
				},
			})
		}
	}

	callIDsByName := map[string][]string{}
	for contentIndex, content := range req.Contents {
		if content == nil {
			continue
		}
		converted := convertGeminiContent(content, contentIndex, callIDsByName)
		for _, msg := range converted {
			for _, toolCall := range msg.ToolCalls {
				if toolCall.Function.Name == "" || toolCall.ID == "" {
					continue
				}
				callIDsByName[toolCall.Function.Name] = append(callIDsByName[toolCall.Function.Name], toolCall.ID)
			}
		}
		messages = append(messages, converted...)
	}
	return messages
}

func convertGeminiContent(content *model.GeminiContent, contentIndex int, callIDsByName map[string][]string) []model.Message {
	role := "user"
	if content.Role == "model" {
		role = "assistant"
	}

	msg := model.Message{Role: role}
	var parts []model.MessageContentPart
	var toolMessages []model.Message

	for partIndex, part := range content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" {
			text := part.Text
			parts = append(parts, model.MessageContentPart{Type: "text", Text: &text})
		}
		if part.InlineData != nil {
			parts = append(parts, convertGeminiInlineData(part.InlineData))
		}
		if part.FileData != nil && part.FileData.FileURI != "" {
			if strings.HasPrefix(part.FileData.MimeType, "image/") {
				parts = append(parts, model.MessageContentPart{
					Type: "image_url",
					ImageURL: &model.ImageURL{
						URL: part.FileData.FileURI,
					},
				})
			} else {
				// Non-image fileData (e.g. application/pdf) -> internal file part
				// so documents referenced by URI are not dropped.
				parts = append(parts, model.MessageContentPart{
					Type: "file",
					File: &model.File{
						FileURL:   part.FileData.FileURI,
						MediaType: part.FileData.MimeType,
					},
				})
			}
		}
		if part.FunctionCall != nil && role == "assistant" {
			args, _ := json.Marshal(part.FunctionCall.Args)
			callID := geminiToolCallID(part.FunctionCall.Name, contentIndex, partIndex)
			msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{
				ID:    callID,
				Type:  "function",
				Index: len(msg.ToolCalls),
				Function: model.FunctionCall{
					Name:      part.FunctionCall.Name,
					Arguments: string(args),
				},
			})
		}
		if part.FunctionResponse != nil {
			response, _ := json.Marshal(part.FunctionResponse.Response)
			responseText := string(response)
			toolCallID := part.FunctionResponse.Name
			if ids := callIDsByName[part.FunctionResponse.Name]; len(ids) > 0 {
				toolCallID = ids[0]
				callIDsByName[part.FunctionResponse.Name] = ids[1:]
			}
			toolCallName := part.FunctionResponse.Name
			toolMessages = append(toolMessages, model.Message{
				Role:         "tool",
				MessageIndex: &contentIndex,
				ToolCallID:   &toolCallID,
				ToolCallName: &toolCallName,
				Content: model.MessageContent{
					Content: &responseText,
				},
			})
		}
	}

	if len(parts) == 1 && parts[0].Type == "text" {
		msg.Content = model.MessageContent{Content: parts[0].Text}
	} else if len(parts) > 0 {
		msg.Content = model.MessageContent{MultipleContent: parts}
	}

	if msg.Content.Content == nil && len(msg.Content.MultipleContent) == 0 && len(msg.ToolCalls) == 0 {
		return toolMessages
	}
	return append([]model.Message{msg}, toolMessages...)
}

func geminiToolCallID(name string, contentIndex, partIndex int) string {
	return fmt.Sprintf("call_%s_%d_%d", name, contentIndex, partIndex)
}

func convertGeminiInlineData(blob *model.GeminiBlob) model.MessageContentPart {
	dataURL := fmt.Sprintf("data:%s;base64,%s", blob.MimeType, blob.Data)
	if strings.HasPrefix(blob.MimeType, "audio/") {
		return model.MessageContentPart{
			Type: "input_audio",
			Audio: &model.Audio{
				Format: strings.TrimPrefix(blob.MimeType, "audio/"),
				Data:   blob.Data,
			},
		}
	}
	return model.MessageContentPart{
		Type: "image_url",
		ImageURL: &model.ImageURL{
			URL: dataURL,
		},
	}
}

func applyGeminiGenerationConfig(req *model.InternalLLMRequest, cfg *model.GeminiGenerationConfig) {
	if cfg == nil {
		return
	}
	req.Temperature = cfg.Temperature
	req.TopP = cfg.TopP
	if cfg.MaxOutputTokens > 0 {
		v := int64(cfg.MaxOutputTokens)
		req.MaxTokens = &v
	}
	if len(cfg.StopSequences) > 0 {
		req.Stop = &model.Stop{MultipleStop: append([]string(nil), cfg.StopSequences...)}
	}
	switch cfg.ResponseMimeType {
	case "application/json":
		req.ResponseFormat = &model.ResponseFormat{Type: "json_object"}
	case "text/plain":
		req.ResponseFormat = &model.ResponseFormat{Type: "text"}
	}
	if len(cfg.ResponseModalities) > 0 {
		req.Modalities = make([]string, 0, len(cfg.ResponseModalities))
		for _, modality := range cfg.ResponseModalities {
			req.Modalities = append(req.Modalities, strings.ToLower(modality))
		}
	}
	if cfg.ThinkingConfig != nil && cfg.ThinkingConfig.ThinkingBudget != nil {
		budget := int64(*cfg.ThinkingConfig.ThinkingBudget)
		req.ReasoningBudget = &budget
	}
}

func applyGeminiTools(req *model.InternalLLMRequest, tools []*model.GeminiTool, toolConfig *model.GeminiToolConfig) {
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		for _, decl := range tool.FunctionDeclarations {
			if decl == nil || decl.Name == "" {
				continue
			}
			params := json.RawMessage(`{}`)
			if decl.Parameters != nil {
				if b, err := json.Marshal(decl.Parameters); err == nil {
					params = b
				}
			}
			req.Tools = append(req.Tools, model.Tool{
				Type: "function",
				Function: model.Function{
					Name:        decl.Name,
					Description: decl.Description,
					Parameters:  params,
				},
			})
		}
	}

	if toolConfig == nil || toolConfig.FunctionCallingConfig == nil {
		return
	}
	fc := toolConfig.FunctionCallingConfig
	switch strings.ToUpper(fc.Mode) {
	case "NONE":
		none := "none"
		req.ToolChoice = &model.ToolChoice{ToolChoice: &none}
	case "ANY":
		if len(fc.AllowedFunctionNames) > 0 {
			req.ToolChoice = &model.ToolChoice{
				NamedToolChoice: &model.NamedToolChoice{
					Type: "function",
					Function: model.ToolFunction{
						Name: fc.AllowedFunctionNames[0],
					},
				},
			}
		} else {
			required := "required"
			req.ToolChoice = &model.ToolChoice{ToolChoice: &required}
		}
	case "AUTO":
		auto := "auto"
		req.ToolChoice = &model.ToolChoice{ToolChoice: &auto}
	}
}

func convertInternalResponseToGemini(response *model.InternalLLMResponse, stream bool) *model.GeminiGenerateContentResponse {
	geminiResp := &model.GeminiGenerateContentResponse{}
	if response == nil {
		return geminiResp
	}
	geminiResp.ModelVersion = response.Model
	for _, choice := range response.Choices {
		msg := choice.Message
		if stream {
			msg = choice.Delta
		}
		candidate := &model.GeminiCandidate{
			Index:        choice.Index,
			Content:      convertMessageToGeminiContent(msg),
			FinishReason: convertFinishReasonToGemini(choice.FinishReason),
		}
		geminiResp.Candidates = append(geminiResp.Candidates, candidate)
	}
	if response.Usage != nil {
		geminiResp.UsageMetadata = &model.GeminiUsageMetadata{
			PromptTokenCount:     int(response.Usage.PromptTokens),
			CandidatesTokenCount: int(response.Usage.CompletionTokens),
			TotalTokenCount:      int(response.Usage.TotalTokens),
		}
		if response.Usage.PromptTokensDetails != nil {
			geminiResp.UsageMetadata.CachedContentTokenCount = int(response.Usage.PromptTokensDetails.CachedTokens)
		}
		if response.Usage.CompletionTokensDetails != nil {
			geminiResp.UsageMetadata.ThoughtsTokenCount = int(response.Usage.CompletionTokensDetails.ReasoningTokens)
		}
	}
	return geminiResp
}

func convertMessageToGeminiContent(msg *model.Message) *model.GeminiContent {
	content := &model.GeminiContent{
		Role:  "model",
		Parts: []*model.GeminiPart{},
	}
	if msg == nil {
		return content
	}
	if msg.Role == "user" || msg.Role == "tool" {
		content.Role = "user"
	}
	if msg.GetReasoningContent() != "" {
		content.Parts = append(content.Parts, &model.GeminiPart{
			Text:    msg.GetReasoningContent(),
			Thought: true,
		})
	}
	if msg.Content.Content != nil && *msg.Content.Content != "" {
		content.Parts = append(content.Parts, &model.GeminiPart{Text: *msg.Content.Content})
	}
	for _, part := range msg.Content.MultipleContent {
		switch part.Type {
		case "text":
			if part.Text != nil && *part.Text != "" {
				content.Parts = append(content.Parts, &model.GeminiPart{Text: *part.Text})
			}
		case "image_url":
			if part.ImageURL != nil && part.ImageURL.URL != "" {
				if dataURL := xurl.ParseDataURL(part.ImageURL.URL); dataURL != nil && dataURL.IsBase64 {
					content.Parts = append(content.Parts, &model.GeminiPart{
						InlineData: &model.GeminiBlob{
							MimeType: dataURL.MediaType,
							Data:     dataURL.Data,
						},
					})
				} else {
					content.Parts = append(content.Parts, &model.GeminiPart{
						FileData: &model.GeminiFileData{
							FileURI: part.ImageURL.URL,
						},
					})
				}
			}
		case "input_audio":
			if part.Audio != nil {
				content.Parts = append(content.Parts, &model.GeminiPart{
					InlineData: &model.GeminiBlob{
						MimeType: "audio/" + part.Audio.Format,
						Data:     part.Audio.Data,
					},
				})
			}
		}
	}
	for _, toolCall := range msg.ToolCalls {
		var args map[string]interface{}
		_ = json.Unmarshal([]byte(toolCall.Function.Arguments), &args)
		content.Parts = append(content.Parts, &model.GeminiPart{
			FunctionCall: &model.GeminiFunctionCall{
				Name: toolCall.Function.Name,
				Args: args,
			},
		})
	}
	return content
}

func convertFinishReasonToGemini(reason *string) *string {
	if reason == nil {
		return nil
	}
	value := "STOP"
	switch *reason {
	case "length":
		value = "MAX_TOKENS"
	case "content_filter":
		value = "SAFETY"
	case "tool_calls", "function_call":
		value = "STOP"
	}
	return &value
}

func joinGeminiText(parts []*model.GeminiPart) string {
	var texts []string
	for _, part := range parts {
		if part != nil && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func mergeDeltaIntoMessage(dst *model.Message, delta *model.Message) {
	if delta.Role != "" {
		dst.Role = delta.Role
	}
	if delta.Content.Content != nil {
		if dst.Content.Content == nil {
			dst.Content.Content = new(string)
		}
		*dst.Content.Content += *delta.Content.Content
	}
	if len(delta.Content.MultipleContent) > 0 {
		dst.Content.MultipleContent = append(dst.Content.MultipleContent, delta.Content.MultipleContent...)
	}
	if delta.GetReasoningContent() != "" {
		if dst.ReasoningContent == nil {
			dst.ReasoningContent = new(string)
		}
		*dst.ReasoningContent += delta.GetReasoningContent()
	}
	for _, toolCall := range delta.ToolCalls {
		dst.ToolCalls = mergeToolCall(dst.ToolCalls, toolCall)
	}
}

func mergeToolCall(toolCalls []model.ToolCall, delta model.ToolCall) []model.ToolCall {
	for i, tc := range toolCalls {
		if tc.Index == delta.Index {
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
	return append(toolCalls, delta)
}
