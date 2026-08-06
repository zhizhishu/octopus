package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/utils/log"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/xurl"
	"github.com/google/uuid"
)

// maxImageGenerationN caps the bridged /v1/images/generations count so a pathological or
// wrapped n cannot fan out (mirrors new-api's MaxImageN guard). The upstream still enforces
// its own per-model limit (e.g. gpt-image/dall-e-3 only allow 1).
const maxImageGenerationN = 10

type imagesGenerationAPIResponse struct {
	Created int64                   `json:"created"`
	Data    []imagesGenerationDatum `json:"data"`
	Usage   *imagesUsage            `json:"usage,omitempty"`
}

type imagesGenerationDatum struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

func (ra *relayAttempt) shouldBridgeImageGenerationToImages() bool {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return false
	}
	if !ra.internalRequest.IsImageGenerationRequest() {
		return false
	}
	return isImageGenerationRequestCompatibleChannelType(ra.channel.Type)
}

func isOpenAIImagesCompatibleChannelType(channelType outbound.OutboundType) bool {
	switch channelType {
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeCustomOpenAIChat:
		return true
	default:
		return false
	}
}

func isImageGenerationRequestCompatibleChannelType(channelType outbound.OutboundType) bool {
	return isOpenAIImagesCompatibleChannelType(channelType) || channelType == outbound.OutboundTypeGemini
}

func isImagesEndpointCompatibleChannelType(channelType outbound.OutboundType) bool {
	return isOpenAIImagesCompatibleChannelType(channelType) || channelType == outbound.OutboundTypeGemini
}

func (ra *relayAttempt) forwardImageGenerationViaImages(ctx context.Context, spanPath func(string)) (int, error) {
	wantsStream := ra.internalRequest.Stream != nil && *ra.internalRequest.Stream
	payload, outputFormat, err := imagesGenerationPayloadFromInternalRequest(ra.internalRequest)
	if err != nil {
		return 0, err
	}
	actualModel := strings.TrimSpace(ra.internalRequest.Model)
	if actualModel == "" {
		actualModel = ra.requestModel
	}
	if isGrokImagesModel(actualModel) {
		payload = normalizeGrokImagesPayload(payload)
		outputFormat = "png"
	}
	payload["model"] = actualModel
	// Response/Chat clients expect their own protocol on the downstream side.
	// Use a non-stream Images upstream request, then wrap the completed result
	// through the inbound transformer. Direct /v1/images/* streaming remains
	// handled by ImagesHandler.
	if !isGrokImagesModel(actualModel) {
		payload["stream"] = false
	}

	if ra.channel.Type == outbound.OutboundTypeGemini {
		return ra.forwardGeminiImageGenerationViaImages(ctx, payload, outputFormat, actualModel, wantsStream, spanPath)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal image generation payload: %w", err)
	}

	targetURL, err := xurl.JoinOpenAIPath(ra.outboundBaseURL(), "/v1/images/generations")
	if err != nil {
		return 0, fmt.Errorf("failed to build image generation upstream url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("failed to create image generation request: %w", err)
	}
	copyHeadersToUpstream(req, ra.c, ra.channel, ra.usedKey.ChannelKey, "application/json", false)
	req.Header.Set("Accept", "application/json")
	if spanPath != nil {
		if req.URL != nil && req.URL.Path != "" {
			spanPath(req.URL.EscapedPath())
		}
	}

	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		return 0, err
	}
	respUp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to send image generation request: %w", err)
	}
	defer respUp.Body.Close()

	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		upErr := newUpstreamError(respUp.StatusCode, b)
		return respUp.StatusCode, upErr
	}

	internalResponse, err := internalResponseFromImagesGeneration(respUp.Body, ra.requestModel, actualModel, outputFormat)
	if err != nil {
		return respUp.StatusCode, err
	}
	if err := ra.ensureResponsesImageResultsBase64(ctx, internalResponse); err != nil {
		return respUp.StatusCode, err
	}
	if wantsStream {
		if err := ra.writeImageGenerationBridgeStream(ctx, internalResponse); err != nil {
			return respUp.StatusCode, err
		}
		return respUp.StatusCode, nil
	}
	inResponse, err := ra.inAdapter.TransformResponse(ctx, internalResponse)
	if err != nil {
		return respUp.StatusCode, fmt.Errorf("failed to transform image generation response: %w", err)
	}
	ra.c.Header("X-Octopus-Image-Bridge", "images-generations")
	ra.c.Data(http.StatusOK, "application/json", inResponse)
	return respUp.StatusCode, nil
}

func (ra *relayAttempt) forwardGeminiImageGenerationViaImages(ctx context.Context, payload map[string]any, outputFormat, actualModel string, wantsStream bool, spanPath func(string)) (int, error) {
	prompt := strings.TrimSpace(stringFromPayload(payload, "prompt"))
	if prompt == "" {
		return 0, errors.New("image generation prompt is required")
	}
	if actualModel == "" {
		return 0, errors.New("image generation model is required")
	}

	body, targetURL, err := geminiImageRequestBodyAndURL(ra.outboundBaseURL(), actualModel, ra.usedKey.ChannelKey, payload, prompt)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("failed to create gemini image generation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for _, h := range ra.channel.CustomHeader {
		req.Header.Set(h.HeaderKey, h.HeaderValue)
	}
	// This gemini-image bridge builds its own request (no copyHeadersToUpstream),
	// so apply the unified non-CLI UA here too instead of Go's default client UA.
	setHeaderIfMissing(req.Header, "User-Agent", genericUAForChannel(ra.channel))
	if spanPath != nil && req.URL != nil {
		if path := req.URL.EscapedPath(); path != "" {
			spanPath(path)
		}
	}

	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		return 0, err
	}
	respUp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to send gemini image generation request: %w", err)
	}
	defer respUp.Body.Close()

	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		return respUp.StatusCode, newUpstreamError(respUp.StatusCode, b)
	}

	imageResp, _, err := openAIImagesResponseFromGemini(respUp.Body, actualModel, payload)
	if err != nil {
		return respUp.StatusCode, err
	}
	internalResponse, err := internalResponseFromImagesGenerationAPI(imageResp, ra.requestModel, actualModel, outputFormat)
	if err != nil {
		return respUp.StatusCode, err
	}
	if err := ra.ensureResponsesImageResultsBase64(ctx, internalResponse); err != nil {
		return respUp.StatusCode, err
	}
	if wantsStream {
		if err := ra.writeImageGenerationBridgeStream(ctx, internalResponse); err != nil {
			return respUp.StatusCode, err
		}
		return respUp.StatusCode, nil
	}
	inResponse, err := ra.inAdapter.TransformResponse(ctx, internalResponse)
	if err != nil {
		return respUp.StatusCode, fmt.Errorf("failed to transform gemini image generation response: %w", err)
	}
	ra.c.Header("X-Octopus-Image-Bridge", "gemini-images")
	ra.c.Data(http.StatusOK, "application/json", inResponse)
	return respUp.StatusCode, nil
}

func (ra *relayAttempt) writeImageGenerationBridgeStream(ctx context.Context, response *transformerModel.InternalLLMResponse) error {
	if response == nil {
		return errors.New("image generation response is nil")
	}
	if len(response.Choices) == 0 || response.Choices[0].Message == nil {
		return errors.New("image generation response did not contain a message")
	}
	finishReason := "stop"
	ra.c.Header("Content-Type", "text/event-stream; charset=utf-8")
	ra.c.Header("Cache-Control", "no-cache")
	ra.c.Header("Connection", "keep-alive")
	ra.c.Header("X-Accel-Buffering", "no")
	ra.c.Header("X-Octopus-Image-Bridge", "images-generations")

	chunks := []*transformerModel.InternalLLMResponse{
		{
			ID:      response.ID,
			Object:  "chat.completion.chunk",
			Created: response.Created,
			Model:   response.Model,
			Choices: []transformerModel.Choice{
				{
					Index: 0,
					Delta: &transformerModel.Message{
						Role:    "assistant",
						Content: response.Choices[0].Message.Content,
					},
				},
			},
		},
		{
			ID:      response.ID,
			Object:  "chat.completion.chunk",
			Created: response.Created,
			Model:   response.Model,
			Choices: []transformerModel.Choice{
				{
					Index:        0,
					Delta:        &transformerModel.Message{Role: "assistant"},
					FinishReason: &finishReason,
				},
			},
			Usage: response.Usage,
		},
		{
			Object: "[DONE]",
		},
	}

	for _, chunk := range chunks {
		data, err := ra.inAdapter.TransformStream(ctx, chunk)
		if err != nil {
			return fmt.Errorf("failed to transform image generation stream response: %w", err)
		}
		if len(data) == 0 {
			continue
		}
		if _, err := ra.c.Writer.Write(data); err != nil {
			return fmt.Errorf("failed to write image generation stream response: %w", err)
		}
		ra.c.Writer.Flush()
	}
	return nil
}

// maxDownloadedImageBytes bounds an image fetched to satisfy a Responses base64 result.
const maxDownloadedImageBytes = 32 << 20

// ensureResponsesImageResultsBase64 makes a Responses image_generation_call.result valid:
// that field is base64 image bytes, so when the upstream returned a plain URL (some image
// providers only return URLs) and the downstream client speaks the Responses protocol, the
// URL is fetched and re-encoded as a data URL (ExtractBase64FromDataURL then yields base64).
// b64 upstreams already carry a data URL and are skipped, so gpt-image-2 pays nothing. Chat
// downstreams keep the URL (rendered as a Markdown link), so this only runs for Responses.
func (ra *relayAttempt) ensureResponsesImageResultsBase64(ctx context.Context, response *transformerModel.InternalLLMResponse) error {
	if response == nil || ra.inboundType != inbound.InboundTypeOpenAIResponse {
		return nil
	}
	for ci := range response.Choices {
		msg := response.Choices[ci].Message
		if msg == nil {
			continue
		}
		for pi := range msg.Content.MultipleContent {
			part := &msg.Content.MultipleContent[pi]
			if part.Type != "image_url" || part.ImageURL == nil {
				continue
			}
			u := strings.TrimSpace(part.ImageURL.URL)
			if u == "" || strings.HasPrefix(u, "data:") {
				continue
			}
			dataURL, err := ra.downloadImageToBase64DataURL(ctx, u)
			if err != nil {
				// Best-effort: a fetch failure must not fail an upstream generation that
				// already succeeded. Leave the URL (no worse than before) — the Responses
				// result is then a URL rather than base64, which the client can still fetch.
				log.Warnf("image bridge: could not fetch %s for base64 responses result, leaving URL: %v", u, err)
				continue
			}
			part.ImageURL.URL = dataURL
		}
	}
	return nil
}

func (ra *relayAttempt) downloadImageToBase64DataURL(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("image download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadedImageBytes))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", errors.New("image download returned an empty body")
	}
	mime := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if idx := strings.IndexByte(mime, ';'); idx >= 0 {
		mime = strings.TrimSpace(mime[:idx])
	}
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func imagesGenerationPayloadFromInternalRequest(req *transformerModel.InternalLLMRequest) (map[string]any, string, error) {
	if req == nil {
		return nil, "", errors.New("request is nil")
	}
	prompt := imagePromptFromMessages(req.Messages)
	if strings.TrimSpace(prompt) == "" {
		return nil, "", errors.New("image generation prompt is required")
	}

	payload := map[string]any{
		"prompt": prompt,
	}
	if req.User != nil && strings.TrimSpace(*req.User) != "" {
		payload["user"] = *req.User
	}
	// Thread the requested image count so a client asking for n images gets n instead of
	// always 1. Billing is usage-token based, so cost scales with the upstream usage the
	// n images produce — no separate n multiplier. Clamp to a sane ceiling so a pathological
	// n cannot fan out; the upstream still enforces its own per-model limit.
	if req.N != nil && *req.N > 0 {
		payload["n"] = min(*req.N, maxImageGenerationN)
	}

	tool := firstImageGenerationTool(req.Tools)
	outputFormat := ""
	if tool != nil {
		if value := strings.TrimSpace(tool.Background); value != "" {
			payload["background"] = value
		}
		if value := strings.TrimSpace(tool.InputFidelity); value != "" {
			payload["input_fidelity"] = value
		}
		if len(tool.InputImageMask) > 0 {
			payload["input_image_mask"] = tool.InputImageMask
		}
		if value := strings.TrimSpace(tool.Moderation); value != "" {
			payload["moderation"] = value
		}
		if tool.OutputCompression != nil {
			payload["output_compression"] = *tool.OutputCompression
		}
		if value := strings.TrimSpace(tool.OutputFormat); value != "" {
			payload["output_format"] = value
			outputFormat = value
		}
		if tool.PartialImages != nil {
			payload["partial_images"] = *tool.PartialImages
		}
		if value := strings.TrimSpace(tool.Quality); value != "" {
			payload["quality"] = value
		}
		if value := strings.TrimSpace(tool.Size); value != "" {
			payload["size"] = value
		}
		if value := strings.TrimSpace(tool.ResponseFormat); value != "" {
			payload["response_format"] = value
		}
		if value := strings.TrimSpace(tool.Style); value != "" {
			payload["style"] = value
		}
		if tool.Watermark {
			payload["watermark"] = tool.Watermark
		}
	}
	if outputFormat == "" {
		outputFormat = "png"
	}
	if isAgnesImagesModel(req.Model) {
		// Emit Agnes-shaped bodies for the chat/responses→images bridge too:
		// nest response_format/image under extra_body. No-op for non-Agnes and
		// when neither field is present, so non-Agnes output stays byte-identical.
		normalizeAgnesImagesPayload(payload)
	}
	return payload, outputFormat, nil
}

func firstImageGenerationTool(tools []transformerModel.Tool) *transformerModel.ImageGeneration {
	for _, tool := range tools {
		if tool.ImageGeneration != nil {
			return tool.ImageGeneration
		}
		if strings.EqualFold(strings.TrimSpace(tool.Type), "image_generation") {
			return &transformerModel.ImageGeneration{}
		}
	}
	return nil
}

func imagePromptFromMessages(messages []transformerModel.Message) string {
	var parts []string
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "assistant" || role == "tool" || role == "function" || role == "system" || role == "developer" {
			continue
		}
		if msg.Content.Content != nil {
			if text := strings.TrimSpace(*msg.Content.Content); text != "" {
				parts = append(parts, text)
			}
		}
		for _, part := range msg.Content.MultipleContent {
			partType := strings.ToLower(strings.TrimSpace(part.Type))
			if (partType == "text" || partType == "input_text" || partType == "") && part.Text != nil {
				if text := strings.TrimSpace(*part.Text); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func internalResponseFromImagesGeneration(body io.Reader, requestModel, actualModel, outputFormat string) (*transformerModel.InternalLLMResponse, error) {
	var imageResp imagesGenerationAPIResponse
	if err := json.NewDecoder(body).Decode(&imageResp); err != nil {
		return nil, fmt.Errorf("failed to decode image generation response: %w", err)
	}
	return internalResponseFromImagesGenerationAPI(&imageResp, requestModel, actualModel, outputFormat)
}

func internalResponseFromImagesGenerationAPI(imageResp *imagesGenerationAPIResponse, requestModel, actualModel, outputFormat string) (*transformerModel.InternalLLMResponse, error) {
	if imageResp == nil {
		return nil, errors.New("image generation response is nil")
	}
	parts := make([]transformerModel.MessageContentPart, 0, len(imageResp.Data)+1)
	for _, item := range imageResp.Data {
		url := strings.TrimSpace(item.URL)
		if b64 := strings.TrimSpace(item.B64JSON); b64 != "" {
			url = "data:" + imageMediaType(outputFormat) + ";base64," + b64
		}
		if url == "" {
			continue
		}
		parts = append(parts, transformerModel.MessageContentPart{
			Type: "image_url",
			ImageURL: &transformerModel.ImageURL{
				URL: url,
			},
		})
	}
	if len(parts) == 0 {
		return nil, errors.New("image generation response did not contain image data")
	}

	finishReason := "stop"
	modelName := strings.TrimSpace(actualModel)
	if modelName == "" {
		modelName = requestModel
	}
	created := imageResp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	return &transformerModel.InternalLLMResponse{
		ID:      "img_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Object:  "chat.completion",
		Created: created,
		Model:   modelName,
		Choices: []transformerModel.Choice{
			{
				Index: 0,
				Message: &transformerModel.Message{
					Role: "assistant",
					Content: transformerModel.MessageContent{
						MultipleContent: parts,
					},
				},
				FinishReason: &finishReason,
			},
		},
		Usage: internalUsageFromImagesUsage(imageResp.Usage),
	}, nil
}

func imageMediaType(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func internalUsageFromImagesUsage(u *imagesUsage) *transformerModel.Usage {
	if u == nil {
		return nil
	}
	promptTokens := int64(u.InputTokenCount())
	completionTokens := int64(u.OutputTokenCount())
	totalTokens := int64(u.TotalTokens)
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}
	usage := &transformerModel.Usage{
		PromptTokens:             promptTokens,
		CompletionTokens:         completionTokens,
		TotalTokens:              totalTokens,
		CacheCreationInputTokens: int64(u.CacheWriteTokenCount()),
		SeparateCacheInputTokens: u.HasSeparateCacheInputTokens(),
	}
	if cached := int64(u.CacheReadTokenCount()); cached > 0 {
		usage.PromptTokensDetails = &transformerModel.PromptTokensDetails{CachedTokens: cached}
	}
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens > 0 {
		usage.CompletionTokensDetails = &transformerModel.CompletionTokensDetails{
			ReasoningTokens: int64(u.OutputTokensDetails.ReasoningTokens),
		}
	}
	return usage
}
