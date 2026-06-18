package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
	"github.com/samber/lo"
)

type MessagesOutbound struct{}

func (o *MessagesOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string) (*http.Request, error) {
	// Convert internal request to Gemini format
	geminiReq := convertLLMToGeminiRequest(request)

	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal gemini request: %w", err)
	}

	// Determine if streaming
	isStream := request.Stream != nil && *request.Stream
	method := "generateContent"
	if isStream {
		method = "streamGenerateContent"
	}

	// Build path: /models/{model}:{method}
	modelName := request.Model
	if !strings.Contains(modelName, "/") {
		modelName = "models/" + modelName
	}
	targetURL, err := xurl.JoinGeminiPath(baseUrl, fmt.Sprintf("/v1beta/%s:%s", modelName, method))
	if err != nil {
		return nil, fmt.Errorf("failed to join gemini url: %w", err)
	}
	parsedUrl, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse gemini url: %w", err)
	}

	q := parsedUrl.Query()
	if request.Query != nil {
		for key, values := range request.Query {
			q.Del(key)
			for _, value := range values {
				q.Add(key, value)
			}
		}
	}
	q.Set("key", key)
	if isStream {
		q.Set("alt", "sse")
	}
	parsedUrl.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsedUrl.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if isStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	return req, nil
}

func (o *MessagesOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("response body is empty")
	}

	var geminiResp model.GeminiGenerateContentResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal gemini response: %w", err)
	}

	// Convert Gemini response to internal format
	return convertGeminiToLLMResponse(&geminiResp, false), nil
}

func (o *MessagesOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	eventData = bytes.TrimSpace(eventData)
	if len(eventData) == 0 {
		return nil, nil
	}

	// Handle [DONE] marker
	if bytes.HasPrefix(eventData, []byte("[DONE]")) {
		return &model.InternalLLMResponse{
			Object: "[DONE]",
		}, nil
	}

	// Parse Gemini streaming response
	var geminiResp model.GeminiGenerateContentResponse
	if err := json.Unmarshal(eventData, &geminiResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal gemini stream chunk: %w", err)
	}

	// Convert to internal format
	return convertGeminiToLLMResponse(&geminiResp, true), nil
}

// Helper functions

// reasoningToThinkingBudget maps reasoning effort levels to thinking budget in tokens
// https://ai.google.dev/gemini-api/docs/thinking
func reasoningToThinkingBudget(effort string) int32 {
	switch strings.ToLower(effort) {
	case "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 24576
	default:
		// 防御性：未知值走动态
		return -1
	}
}

func audioTypeToMimeType(format string) string {
	switch format {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mp3"
	case "aiff":
		return "audio/aiff"
	case "aac":
		return "audio/aac"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	default:
		return "audio/wav"
	}
}

// mimeFromURL infers a MIME type from a remote URL's file extension. Gemini
// fileData parts want an explicit mimeType, and some upstreams reject a bare
// fileUri with an empty mimeType. We only return a value for extensions we are
// confident about; unknown extensions yield "" so callers can leave the field
// empty rather than guessing.
func mimeFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	// Strip query/fragment before inspecting the extension so things like
	// "https://host/a.png?sig=..." still resolve.
	path := rawURL
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	dot := strings.LastIndex(path, ".")
	if dot < 0 {
		return ""
	}
	// Reject a "." that belongs to a host/path segment rather than a filename,
	// e.g. "https://a.b/c" where the last dot precedes a slash.
	if strings.ContainsAny(path[dot:], "/") {
		return ""
	}
	switch strings.ToLower(path[dot+1:]) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "heic":
		return "image/heic"
	case "heif":
		return "image/heif"
	case "bmp":
		return "image/bmp"
	case "pdf":
		return "application/pdf"
	case "mp4":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "webm":
		return "video/webm"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	default:
		return ""
	}
}

func convertLLMToGeminiRequest(request *model.InternalLLMRequest) *model.GeminiGenerateContentRequest {
	geminiReq := &model.GeminiGenerateContentRequest{
		Contents: []*model.GeminiContent{},
	}

	// Convert messages
	var systemInstruction *model.GeminiContent
	toolCallNamesByID := map[string]string{}

	for _, msg := range request.Messages {
		switch msg.Role {
		case "system", "developer":
			// Collect system messages into system instruction
			if systemInstruction == nil {
				systemInstruction = &model.GeminiContent{
					Parts: []*model.GeminiPart{},
				}
			}
			if msg.Content.Content != nil {
				systemInstruction.Parts = append(systemInstruction.Parts, &model.GeminiPart{
					Text: *msg.Content.Content,
				})
			}

		case "user":
			content := &model.GeminiContent{
				Role:  "user",
				Parts: []*model.GeminiPart{},
			}
			if msg.Content.Content != nil {
				content.Parts = append(content.Parts, &model.GeminiPart{
					Text: *msg.Content.Content,
				})
			}

			if msg.Content.MultipleContent != nil {
				content.Parts = append(content.Parts, geminiPartsFromMultipleContent(msg.Content.MultipleContent)...)
			}

			geminiReq.Contents = append(geminiReq.Contents, content)

		case "assistant":
			content := &model.GeminiContent{
				Role:  "model",
				Parts: []*model.GeminiPart{},
			}
			// Handle text content
			if msg.Content.Content != nil && *msg.Content.Content != "" {
				content.Parts = append(content.Parts, &model.GeminiPart{
					Text: *msg.Content.Content,
				})
			}
			// Handle multimodal content (e.g. images from prior image-generation
			// turns) so multi-turn drawing history is not silently dropped.
			if msg.Content.MultipleContent != nil {
				content.Parts = append(content.Parts, geminiPartsFromMultipleContent(msg.Content.MultipleContent)...)
			}
			// Handle tool calls
			if len(msg.ToolCalls) > 0 {
				for _, toolCall := range msg.ToolCalls {
					if toolCall.ID != "" && toolCall.Function.Name != "" {
						toolCallNamesByID[toolCall.ID] = toolCall.Function.Name
					}
					var args map[string]interface{}
					_ = json.Unmarshal([]byte(toolCall.Function.Arguments), &args)
					content.Parts = append(content.Parts, &model.GeminiPart{
						FunctionCall: &model.GeminiFunctionCall{
							Name: toolCall.Function.Name,
							Args: args,
						},
						ThoughtSignature: "skip_thought_signature_validator",
					})
				}
			}
			geminiReq.Contents = append(geminiReq.Contents, content)

		case "tool":
			// Tool result
			content := convertLLMToolResultToGeminiContent(&msg, toolCallNamesByID)
			geminiReq.Contents = append(geminiReq.Contents, content)
		}
	}

	geminiReq.SystemInstruction = systemInstruction

	// Convert generation config
	config := &model.GeminiGenerationConfig{}
	hasConfig := false

	if request.MaxTokens != nil {
		config.MaxOutputTokens = int(*request.MaxTokens)
		hasConfig = true
	}
	if request.Temperature != nil {
		config.Temperature = request.Temperature
		hasConfig = true
	}
	if request.TopP != nil {
		config.TopP = request.TopP
		hasConfig = true
	}
	// TopK is stored in metadata if present
	if topKStr, ok := request.TransformerMetadata["gemini_top_k"]; ok {
		var topK int
		fmt.Sscanf(topKStr, "%d", &topK)
		config.TopK = &topK
		hasConfig = true
	}
	if request.Stop != nil && request.Stop.MultipleStop != nil {
		config.StopSequences = request.Stop.MultipleStop
		hasConfig = true
	} else if request.Stop != nil && request.Stop.Stop != nil {
		config.StopSequences = []string{*request.Stop.Stop}
		hasConfig = true
	}

	// Prefer an explicit ReasoningBudget (e.g. round-tripped from a Gemini
	// thinkingConfig.thinkingBudget on the inbound side) over the coarse
	// ReasoningEffort mapping so the original budget is preserved.
	if request.ReasoningBudget != nil {
		budget := int32(*request.ReasoningBudget)
		config.ThinkingConfig = &model.GeminiThinkingConfig{
			ThinkingBudget:  &budget,
			IncludeThoughts: true,
		}
		hasConfig = true
	} else if request.ReasoningEffort != "" {
		budget := reasoningToThinkingBudget(request.ReasoningEffort)

		config.ThinkingConfig = &model.GeminiThinkingConfig{
			ThinkingBudget:  &budget,
			IncludeThoughts: true,
		}
		hasConfig = true
	}

	// Convert ResponseFormat to ResponseMimeType and ResponseSchema
	if request.ResponseFormat != nil {
		switch request.ResponseFormat.Type {
		case "json_object":
			config.ResponseMimeType = "application/json"
			hasConfig = true
		case "json_schema":
			config.ResponseMimeType = "application/json"
			if schema := convertResponseFormatSchemaToGemini(request.ResponseFormat.JSONSchema); schema != nil {
				config.ResponseSchema = schema
			}
			hasConfig = true
		case "text":
			config.ResponseMimeType = "text/plain"
			hasConfig = true
		}
	}

	// Convert Modalities to ResponseModalities. Gemini/OpenRouter-compatible
	// bridges use enum-style values ("TEXT", "IMAGE"). If the client only
	// identifies image generation by model/tool name, synthesize the image
	// modalities here so Chat/Responses clients can route to Gemini image
	// channels without knowing Gemini's provider dialect.
	if len(request.Modalities) > 0 {
		convertedModalities := make([]string, len(request.Modalities))
		for i, m := range request.Modalities {
			convertedModalities[i] = geminiResponseModality(m)
		}
		config.ResponseModalities = convertedModalities
		hasConfig = true
	} else if request.IsImageGenerationRequest() {
		config.ResponseModalities = []string{"TEXT", "IMAGE"}
		hasConfig = true
	}

	if imageConfig := geminiImageConfigFromRequest(request); imageConfig != nil {
		config.ImageConfig = imageConfig
		hasConfig = true
	}

	if hasConfig {
		geminiReq.GenerationConfig = config
	}

	// Convert SafetySettings from metadata if present
	if safetyJSON, ok := request.TransformerMetadata["gemini_safety_settings"]; ok {
		var safetySettings []*model.GeminiSafetySetting
		if err := json.Unmarshal([]byte(safetyJSON), &safetySettings); err == nil {
			geminiReq.SafetySettings = safetySettings
		}
	}

	// Convert tools
	if len(request.Tools) > 0 {
		functionDeclarations := make([]*model.GeminiFunctionDeclaration, 0, len(request.Tools))

		for _, tool := range request.Tools {
			if tool.Type != "function" {
				continue
			}

			var params map[string]any
			if len(tool.Function.Parameters) > 0 {
				// Best-effort: if schema can't be parsed, we still send the declaration without parameters.
				_ = json.Unmarshal(tool.Function.Parameters, &params)
			}
			cleanGeminiSchema(params)

			functionDeclarations = append(functionDeclarations, &model.GeminiFunctionDeclaration{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  params,
			})
		}

		if len(functionDeclarations) > 0 {
			geminiReq.Tools = []*model.GeminiTool{{FunctionDeclarations: functionDeclarations}}
		}
	}

	// Convert tool choice to Gemini toolConfig.functionCallingConfig
	if request.ToolChoice != nil {
		mode := "AUTO"
		var allowed []string

		if request.ToolChoice.ToolChoice != nil {
			switch strings.ToLower(*request.ToolChoice.ToolChoice) {
			case "auto":
				mode = "AUTO"
			case "required":
				mode = "ANY"
			case "none":
				mode = "NONE"
			}
		} else if request.ToolChoice.NamedToolChoice != nil && request.ToolChoice.NamedToolChoice.Type == "function" {
			mode = "ANY"
			if request.ToolChoice.NamedToolChoice.Function.Name != "" {
				allowed = []string{request.ToolChoice.NamedToolChoice.Function.Name}
			}
		}

		geminiReq.ToolConfig = &model.GeminiToolConfig{
			FunctionCallingConfig: &model.GeminiFunctionCallingConfig{
				Mode:                 mode,
				AllowedFunctionNames: allowed,
			},
		}
	}

	return geminiReq

}

// geminiPartsFromMultipleContent converts an OpenAI-style multimodal content
// array into Gemini parts. It is shared by the user and assistant branches so
// images/files round-trip in multi-turn (e.g. image-generation) histories.
//
//   - text         -> text part
//   - image_url    -> inlineData for base64 data URLs, otherwise fileData with
//     the remote http(s) URI (symmetric with the inbound fileData->image_url path)
//   - input_audio  -> inlineData
//   - file         -> inlineData for base64 data URLs; remote File.FileURL or
//     provider File.FileID fall back to fileData, using File.MediaType as the
//     mime hint when the data URL does not carry one
func geminiPartsFromMultipleContent(parts []model.MessageContentPart) []*model.GeminiPart {
	out := make([]*model.GeminiPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text != nil {
				out = append(out, &model.GeminiPart{
					Text: *part.Text,
				})
			}
		case "image_url":
			if part.ImageURL == nil || part.ImageURL.URL == "" {
				continue
			}
			// get mime type from url extension
			dataurl := xurl.ParseDataURL(part.ImageURL.URL)
			if dataurl != nil && dataurl.IsBase64 {
				out = append(out, &model.GeminiPart{
					InlineData: &model.GeminiBlob{
						MimeType: dataurl.MediaType,
						Data:     dataurl.Data,
					},
				})
			} else {
				// Remote http(s) image URL: reference it via fileData instead of
				// dropping it (symmetric with inbound fileData -> image_url).
				// Infer mimeType from the URL extension; Gemini wants it set and
				// some upstreams reject fileData with an empty mimeType.
				out = append(out, &model.GeminiPart{
					FileData: &model.GeminiFileData{
						MimeType: mimeFromURL(part.ImageURL.URL),
						FileURI:  part.ImageURL.URL,
					},
				})
			}
		case "input_audio":
			if part.Audio != nil {
				out = append(out, &model.GeminiPart{
					InlineData: &model.GeminiBlob{
						MimeType: audioTypeToMimeType(part.Audio.Format),
						Data:     part.Audio.Data,
					},
				})
			}
		case "file":
			if part.File == nil {
				continue
			}
			dataurl := xurl.ParseDataURL(part.File.FileData)
			if dataurl != nil && dataurl.IsBase64 {
				mimeType := dataurl.MediaType
				// Prefer the explicit File.MediaType when the data URL carries no
				// real media type. ParseDataURL defaults a missing media type to
				// "text/plain" (RFC 2397), which is rarely correct for documents.
				if (mimeType == "" || mimeType == "text/plain") && part.File.MediaType != "" {
					mimeType = part.File.MediaType
				}
				out = append(out, &model.GeminiPart{
					InlineData: &model.GeminiBlob{
						MimeType: mimeType,
						Data:     dataurl.Data,
					},
				})
			} else if uri := part.File.FileURL; uri != "" {
				// Remote document URL -> fileData reference. Prefer an explicit
				// File.MediaType; otherwise infer it from the URL extension so the
				// fileData mimeType is not left empty.
				mimeType := part.File.MediaType
				if mimeType == "" {
					mimeType = mimeFromURL(uri)
				}
				out = append(out, &model.GeminiPart{
					FileData: &model.GeminiFileData{
						MimeType: mimeType,
						FileURI:  uri,
					},
				})
			} else if id := part.File.FileID; id != "" {
				// Provider-side file id -> fileData reference.
				out = append(out, &model.GeminiPart{
					FileData: &model.GeminiFileData{
						MimeType: part.File.MediaType,
						FileURI:  id,
					},
				})
			}
		}
	}
	return out
}

func geminiResponseModality(modality string) string {
	switch strings.ToLower(strings.TrimSpace(modality)) {
	case "text":
		return "TEXT"
	case "image":
		return "IMAGE"
	default:
		return strings.ToUpper(strings.TrimSpace(modality))
	}
}

func geminiImageConfigFromRequest(request *model.InternalLLMRequest) *model.GeminiImageConfig {
	if request == nil || !request.IsImageGenerationRequest() {
		return nil
	}
	tool := firstGeminiImageGenerationTool(request.Tools)
	if tool == nil {
		return nil
	}
	cfg := &model.GeminiImageConfig{}
	if aspectRatio := geminiAspectRatioFromSize(tool.Size); aspectRatio != "" {
		cfg.AspectRatio = aspectRatio
	}
	if imageSize := geminiImageSizeFromQuality(tool.Quality); imageSize != "" {
		cfg.ImageSize = imageSize
	}
	if cfg.AspectRatio == "" && cfg.ImageSize == "" {
		return nil
	}
	return cfg
}

func firstGeminiImageGenerationTool(tools []model.Tool) *model.ImageGeneration {
	for _, tool := range tools {
		if tool.ImageGeneration != nil {
			return tool.ImageGeneration
		}
		if strings.EqualFold(strings.TrimSpace(tool.Type), "image_generation") {
			return &model.ImageGeneration{}
		}
	}
	return nil
}

func geminiAspectRatioFromSize(size string) string {
	size = strings.TrimSpace(size)
	if size == "" {
		return ""
	}
	if strings.Contains(size, ":") {
		return size
	}
	switch size {
	case "256x256", "512x512", "1024x1024":
		return "1:1"
	case "1536x1024":
		return "3:2"
	case "1024x1536":
		return "2:3"
	case "1024x1792":
		return "9:16"
	case "1792x1024":
		return "16:9"
	default:
		return ""
	}
}

func geminiImageSizeFromQuality(quality string) string {
	switch strings.TrimSpace(quality) {
	case "hd", "high", "2K":
		return "2K"
	case "standard", "medium", "low", "auto", "1K":
		return "1K"
	default:
		return ""
	}
}

func convertLLMToolResultToGeminiContent(msg *model.Message, toolCallNamesByID map[string]string) *model.GeminiContent {
	content := &model.GeminiContent{
		Role: "user", // Function responses come from user role in Gemini
	}

	var responseData map[string]any
	if msg.Content.Content != nil {
		_ = json.Unmarshal([]byte(*msg.Content.Content), &responseData)
	}

	if responseData == nil {
		responseData = map[string]any{"result": lo.FromPtrOr(msg.Content.Content, "")}
	}

	name := lo.FromPtrOr(msg.ToolCallName, "")
	if name == "" && msg.ToolCallID != nil {
		name = toolCallNamesByID[*msg.ToolCallID]
	}
	if name == "" {
		name = lo.FromPtrOr(msg.ToolCallID, "")
	}

	fp := &model.GeminiFunctionResponse{
		Name:     name,
		Response: responseData,
	}

	content.Parts = []*model.GeminiPart{
		{FunctionResponse: fp},
	}

	return content
}

func convertGeminiToLLMResponse(geminiResp *model.GeminiGenerateContentResponse, isStream bool) *model.InternalLLMResponse {
	resp := &model.InternalLLMResponse{
		Choices: []model.Choice{},
	}

	if isStream {
		resp.Object = "chat.completion.chunk"
	} else {
		resp.Object = "chat.completion"
	}

	// Convert candidates to choices
	for _, candidate := range geminiResp.Candidates {
		choice := model.Choice{
			Index: candidate.Index,
		}

		// Convert finish reason
		if candidate.FinishReason != nil {
			reason := convertGeminiFinishReason(*candidate.FinishReason)
			choice.FinishReason = &reason
		}

		// Convert content
		if candidate.Content != nil {
			msg := &model.Message{
				Role: "assistant",
			}

			// Extract text, images and function calls from parts
			var textParts []string
			var contentParts []model.MessageContentPart
			var toolCalls []model.ToolCall
			var reasoningContent *string
			var hasInlineData bool

			for _, part := range candidate.Content.Parts {
				if part.Thought {
					// Handle thinking/reasoning content
					if part.Text != "" && reasoningContent == nil {
						reasoningContent = &part.Text
					}
				} else if part.Text != "" {
					textParts = append(textParts, part.Text)
					// Also add to content parts for multimodal response
					text := part.Text
					contentParts = append(contentParts, model.MessageContentPart{
						Type: "text",
						Text: &text,
					})
				}
				// Handle inline data (images, audio, etc.)
				if part.InlineData != nil {
					hasInlineData = true
					// Convert to data URL format: data:{mimeType};base64,{data}
					dataURL := fmt.Sprintf("data:%s;base64,%s", part.InlineData.MimeType, part.InlineData.Data)
					contentParts = append(contentParts, model.MessageContentPart{
						Type: "image_url",
						ImageURL: &model.ImageURL{
							URL: dataURL,
						},
					})
				}
				if part.FunctionCall != nil {
					argsJSON, _ := json.Marshal(part.FunctionCall.Args)
					// Index must count tool calls only; using the parts-loop idx
					// (which also covers text/thought parts) makes the streaming
					// tool_call index jump or collide when multiple part kinds
					// interleave. Use the current tool-call count instead.
					toolCallIndex := len(toolCalls)
					toolCall := model.ToolCall{
						Index: toolCallIndex,
						ID:    fmt.Sprintf("call_%s_%d", part.FunctionCall.Name, toolCallIndex),
						Type:  "function",
						Function: model.FunctionCall{
							Name:      part.FunctionCall.Name,
							Arguments: string(argsJSON),
						},
					}
					toolCalls = append(toolCalls, toolCall)
				}
			}

			// Set content - use MultipleContent if we have inline data (images)
			if hasInlineData {
				msg.Content = model.MessageContent{
					MultipleContent: contentParts,
				}
			} else if len(textParts) > 0 {
				text := strings.Join(textParts, "")
				msg.Content = model.MessageContent{
					Content: &text,
				}
			}

			// Set reasoning content
			if reasoningContent != nil {
				msg.ReasoningContent = reasoningContent
			}

			// Set tool calls
			if len(toolCalls) > 0 {
				msg.ToolCalls = toolCalls
				if choice.FinishReason == nil {
					reason := "tool_calls"
					choice.FinishReason = &reason
				}
			}

			if isStream {
				choice.Delta = msg
			} else {
				choice.Message = msg
			}
		}

		resp.Choices = append(resp.Choices, choice)
	}

	// Convert usage metadata
	if geminiResp.UsageMetadata != nil {
		usage := &model.Usage{
			PromptTokens:     int64(geminiResp.UsageMetadata.PromptTokenCount),
			CompletionTokens: int64(geminiResp.UsageMetadata.CandidatesTokenCount),
			TotalTokens:      int64(geminiResp.UsageMetadata.TotalTokenCount),
		}

		// Add cached tokens to prompt tokens details if present
		if geminiResp.UsageMetadata.CachedContentTokenCount > 0 {
			if usage.PromptTokensDetails == nil {
				usage.PromptTokensDetails = &model.PromptTokensDetails{}
			}
			usage.PromptTokensDetails.CachedTokens = int64(geminiResp.UsageMetadata.CachedContentTokenCount)
		}

		// Add thoughts tokens to completion tokens details if present
		if geminiResp.UsageMetadata.ThoughtsTokenCount > 0 {
			if usage.CompletionTokensDetails == nil {
				usage.CompletionTokensDetails = &model.CompletionTokensDetails{}
			}
			usage.CompletionTokensDetails.ReasoningTokens = int64(geminiResp.UsageMetadata.ThoughtsTokenCount)
		}

		resp.Usage = usage
	}

	return resp
}

func convertGeminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "RECITATION":
		return "content_filter"
	default:
		return "stop"
	}
}

func cleanGeminiSchema(schema map[string]any) {
	if schema == nil {
		return
	}
	t := &geminiSchemaTransformer{
		root:    schema,
		visited: map[uintptr]struct{}{},
	}
	t.transform(schema)
}

func convertResponseFormatSchemaToGemini(raw json.RawMessage) *model.GeminiSchema {
	if len(raw) == 0 {
		return nil
	}

	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil
	}

	if nested, ok := schema["schema"].(map[string]any); ok {
		schema = nested
	} else if wrapped, ok := schema["json_schema"].(map[string]any); ok {
		if nested, ok := wrapped["schema"].(map[string]any); ok {
			schema = nested
		} else {
			schema = wrapped
		}
	}

	cleanGeminiSchema(schema)

	var converted model.GeminiSchema
	if b, err := json.Marshal(schema); err == nil {
		if err := json.Unmarshal(b, &converted); err == nil && converted.Type != "" {
			return &converted
		}
	}
	return nil
}

type geminiSchemaTransformer struct {
	root    map[string]any
	visited map[uintptr]struct{}
}

func (t *geminiSchemaTransformer) transform(schemaNode any) {
	if schemaNode == nil {
		return
	}

	// Cycle guard: schema graphs can contain shared sub-objects (or be cyclic after merges).
	// We only track reference-like kinds to avoid false positives.
	rv := reflect.ValueOf(schemaNode)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer:
		if rv.IsNil() {
			return
		}
		id := rv.Pointer()
		if _, seen := t.visited[id]; seen {
			return
		}
		t.visited[id] = struct{}{}
	}

	switch node := schemaNode.(type) {
	case []any:
		for _, item := range node {
			t.transform(item)
		}
		return

	case map[string]any:
		// 1) Resolve $ref (local-only: #/...)
		if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			path := strings.Split(ref[2:], "/")
			var cur any = t.root
			for _, seg := range path {
				seg = strings.ReplaceAll(seg, "~1", "/")
				seg = strings.ReplaceAll(seg, "~0", "~")
				m, ok := cur.(map[string]any)
				if !ok {
					cur = nil
					break
				}
				cur = m[seg]
				if cur == nil {
					break
				}
			}

			if resolved, ok := cur.(map[string]any); ok && resolved != nil {
				// Merge resolved schema into node, but keep local overrides in node.
				overlay := make(map[string]any, len(node))
				for k, v := range node {
					if k != "$ref" {
						overlay[k] = v
					}
				}

				var copied map[string]any
				if b, err := json.Marshal(resolved); err == nil {
					_ = json.Unmarshal(b, &copied)
				}
				if copied == nil {
					copied = make(map[string]any, len(resolved))
					for k, v := range resolved {
						copied[k] = v
					}
				}

				for k := range node {
					delete(node, k)
				}
				for k, v := range copied {
					node[k] = v
				}
				for k, v := range overlay {
					node[k] = v
				}
				delete(node, "$ref")
			}
		}

		// 2) Merge allOf into current node
		if allOf, ok := node["allOf"].([]any); ok {
			for _, item := range allOf {
				t.transform(item)
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}

				// Merge properties (existing props win)
				if itemProps, ok := itemMap["properties"].(map[string]any); ok {
					props, _ := node["properties"].(map[string]any)
					if props == nil {
						props = map[string]any{}
					}
					for k, v := range itemProps {
						if _, exists := props[k]; !exists {
							props[k] = v
						}
					}
					node["properties"] = props
				}

				// Merge required
				itemReq := t.asStringSlice(itemMap["required"])
				if len(itemReq) > 0 {
					curReq := t.asStringSlice(node["required"])
					curReq = append(curReq, itemReq...)
					node["required"] = t.dedupeStrings(curReq)
				}
			}
			delete(node, "allOf")
		}

		// 3) Type mapping (and nullable union handling)
		if typ, ok := node["type"]; ok {
			primary := ""
			switch v := typ.(type) {
			case string:
				primary = v
			case []any:
				for _, it := range v {
					if s, ok := it.(string); ok && strings.ToLower(s) != "null" {
						primary = s
						break
					}
				}
			case []string:
				for _, s := range v {
					if strings.ToLower(s) != "null" {
						primary = s
						break
					}
				}
			}

			switch strings.ToLower(primary) {
			case "string":
				node["type"] = "STRING"
			case "number":
				node["type"] = "NUMBER"
			case "integer":
				node["type"] = "INTEGER"
			case "boolean":
				node["type"] = "BOOLEAN"
			case "array":
				node["type"] = "ARRAY"
			case "object":
				node["type"] = "OBJECT"
			}
		}

		// 4) ARRAY items fixes + tuple handling
		if node["type"] == "ARRAY" {
			if node["items"] == nil {
				node["items"] = map[string]any{}
			} else if tuple, ok := node["items"].([]any); ok {
				for _, it := range tuple {
					t.transform(it)
				}

				// Add tuple hint to description
				tupleTypes := make([]string, 0, len(tuple))
				for _, it := range tuple {
					if itMap, ok := it.(map[string]any); ok {
						if tt, ok := itMap["type"].(string); ok && tt != "" {
							tupleTypes = append(tupleTypes, tt)
						} else {
							tupleTypes = append(tupleTypes, "any")
						}
					} else {
						tupleTypes = append(tupleTypes, "any")
					}
				}
				hint := fmt.Sprintf("(Tuple: [%s])", strings.Join(tupleTypes, ", "))
				if origDesc, _ := node["description"].(string); origDesc == "" {
					node["description"] = hint
				} else {
					node["description"] = strings.TrimSpace(origDesc + " " + hint)
				}

				// Homogeneous tuple => collapse to list schema; otherwise loosen.
				firstType := ""
				if len(tuple) > 0 {
					if itMap, ok := tuple[0].(map[string]any); ok {
						firstType, _ = itMap["type"].(string)
					}
				}
				isHomogeneous := firstType != ""
				for _, it := range tuple {
					itMap, ok := it.(map[string]any)
					if !ok {
						isHomogeneous = false
						break
					}
					tt, _ := itMap["type"].(string)
					if tt != firstType {
						isHomogeneous = false
						break
					}
				}

				if isHomogeneous {
					node["items"] = tuple[0]
				} else {
					node["items"] = map[string]any{}
				}
			}
		}

		// 5) anyOf: try const->enum; otherwise take first usable schema if no type set
		if anyOf, ok := node["anyOf"].([]any); ok {
			for _, item := range anyOf {
				t.transform(item)
			}

			allConst := true
			enumVals := make([]string, 0, len(anyOf))
			for _, item := range anyOf {
				itemMap, ok := item.(map[string]any)
				if !ok {
					allConst = false
					break
				}
				c, ok := itemMap["const"]
				if !ok {
					allConst = false
					break
				}
				if c == nil || c == "" {
					continue
				}
				enumVals = append(enumVals, fmt.Sprint(c))
			}

			if allConst && len(enumVals) > 0 {
				node["type"] = "STRING"
				node["enum"] = enumVals
			} else if _, hasType := node["type"]; !hasType {
				for _, item := range anyOf {
					if itemMap, ok := item.(map[string]any); ok {
						if itemMap["type"] != nil || itemMap["enum"] != nil {
							for k, v := range itemMap {
								node[k] = v
							}
							break
						}
					}
				}
			}
			delete(node, "anyOf")
		}

		// 6) Default value -> description hint (then delete default)
		if def, ok := node["default"]; ok {
			if desc, ok := node["description"].(string); ok && desc != "" {
				if b, err := json.Marshal(def); err == nil {
					node["description"] = desc + " (Default: " + string(b) + ")"
				}
			}
		}

		// 7) Remove unsupported fields
		for _, k := range []string{
			"title", "$schema", "$ref", "strict",
			"exclusiveMaximum", "exclusiveMinimum",
			"additionalProperties", "oneOf", "default",
			"$defs", "propertyNames", "pattern", "minLength",
			"maxLength", "minimum", "maximum", "maxItems", "minItems",
			"uniqueItems", "multipleOf",
		} {
			delete(node, k)
		}

		// 8) Recurse into properties/items
		if props, ok := node["properties"].(map[string]any); ok {
			for _, prop := range props {
				t.transform(prop)
			}
		}
		if items := node["items"]; items != nil {
			t.transform(items)
		}

		// 9) Ensure required is de-duped (allOf merge can introduce duplicates)
		if req := t.asStringSlice(node["required"]); len(req) > 0 {
			node["required"] = t.dedupeStrings(req)
		}
	}
}

func (t *geminiSchemaTransformer) asStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return append([]string(nil), s...)
	case []any:
		out := make([]string, 0, len(s))
		for _, it := range s {
			if str, ok := it.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

func (t *geminiSchemaTransformer) dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
