package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
	"github.com/gin-gonic/gin"
)

type geminiImagePredictRequest struct {
	Instances  []geminiImageInstance `json:"instances"`
	Parameters geminiImageParameters `json:"parameters,omitempty"`
}

type geminiImageInstance struct {
	Prompt string `json:"prompt"`
}

type geminiImageParameters struct {
	SampleCount      int    `json:"sampleCount,omitempty"`
	AspectRatio      string `json:"aspectRatio,omitempty"`
	PersonGeneration string `json:"personGeneration,omitempty"`
	ImageSize        string `json:"imageSize,omitempty"`
}

type geminiImagePredictResponse struct {
	Predictions []geminiImagePrediction `json:"predictions"`
}

type geminiImagePrediction struct {
	MimeType           string `json:"mimeType"`
	BytesBase64Encoded string `json:"bytesBase64Encoded"`
	RaiFilteredReason  string `json:"raiFilteredReason,omitempty"`
}

func geminiImagesAttempt(
	ctx context.Context,
	endpoint string,
	c *gin.Context,
	isMultipart bool,
	jsonPayload map[string]any,
	stream bool,
	channel *dbmodel.Channel,
	channelKey string,
	metrics *imagesRelayMetrics,
	actualModel string,
) (statusCode int, written bool, usage *imagesUsage, upstreamCT string, err error) {
	if endpoint != "/images/generations" {
		return 0, false, nil, "", fmt.Errorf("gemini image bridge only supports /v1/images/generations, got %s", endpoint)
	}
	if isMultipart {
		return 0, false, nil, "", errors.New("gemini image bridge currently supports JSON image requests only")
	}
	if jsonPayload == nil {
		return 0, false, nil, "", errors.New("nil json payload")
	}
	if stream {
		return 0, false, nil, "", errors.New("gemini image bridge does not support streaming Images responses")
	}

	prompt := strings.TrimSpace(stringFromPayload(jsonPayload, "prompt"))
	if prompt == "" {
		return 0, false, nil, "", errors.New("image generation prompt is required")
	}

	modelName := strings.TrimSpace(actualModel)
	if modelName == "" {
		modelName = strings.TrimSpace(stringFromPayload(jsonPayload, "model"))
	}
	if modelName == "" {
		return 0, false, nil, "", errors.New("image generation model is required")
	}

	body, targetURL, err := geminiImageRequestBodyAndURL(channel.GetBaseUrl(), modelName, channelKey, jsonPayload, prompt)
	if err != nil {
		return 0, false, nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, false, nil, "", fmt.Errorf("failed to create gemini image request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for _, h := range channel.CustomHeader {
		req.Header.Set(h.HeaderKey, h.HeaderValue)
	}

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, false, nil, "", err
	}
	respUp, err := httpClient.Do(req)
	if err != nil {
		return 0, false, nil, "", fmt.Errorf("failed to send gemini image request: %w", err)
	}
	defer respUp.Body.Close()

	upstreamCT = respUp.Header.Get("Content-Type")
	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		return respUp.StatusCode, false, nil, upstreamCT, newUpstreamError(respUp.StatusCode, b)
	}

	imageResp, usage, err := openAIImagesResponseFromGemini(respUp.Body, modelName, jsonPayload)
	if err != nil {
		return respUp.StatusCode, false, nil, upstreamCT, err
	}
	if metrics != nil {
		metrics.SetFirstTokenTime(time.Now())
	}
	c.Header("Content-Type", "application/json")
	c.Header("X-Octopus-Image-Bridge", "gemini-images")
	c.Status(http.StatusOK)
	if err := json.NewEncoder(c.Writer).Encode(imageResp); err != nil {
		return respUp.StatusCode, c.Writer.Written(), usage, upstreamCT, err
	}
	return respUp.StatusCode, true, usage, upstreamCT, nil
}

func geminiImageRequestBodyAndURL(baseURL, modelName, key string, payload map[string]any, prompt string) ([]byte, string, error) {
	if strings.HasPrefix(strings.ToLower(modelName), "imagen") {
		reqBody := geminiImagePredictRequest{
			Instances: []geminiImageInstance{{Prompt: prompt}},
			Parameters: geminiImageParameters{
				SampleCount:      positiveIntFromPayload(payload, "n", 1),
				AspectRatio:      geminiAspectRatioFromImagePayload(payload),
				PersonGeneration: "allow_adult",
				ImageSize:        geminiImageSizeFromQuality(stringFromPayload(payload, "quality")),
			},
		}
		body, err := json.Marshal(reqBody)
		if err != nil {
			return nil, "", fmt.Errorf("failed to marshal gemini imagen request: %w", err)
		}
		targetURL, err := joinGeminiModelMethod(baseURL, modelName, "predict", key)
		return body, targetURL, err
	}

	reqBody := transformerModel.GeminiGenerateContentRequest{
		Contents: []*transformerModel.GeminiContent{{
			Role: "user",
			Parts: []*transformerModel.GeminiPart{{
				Text: prompt,
			}},
		}},
		GenerationConfig: &transformerModel.GeminiGenerationConfig{
			ResponseModalities: []string{"TEXT", "IMAGE"},
		},
	}
	if n := positiveIntFromPayload(payload, "n", 1); n > 1 {
		reqBody.GenerationConfig.CandidateCount = n
	}
	if cfg := geminiImageConfigFromImagesPayload(payload); cfg != nil {
		reqBody.GenerationConfig.ImageConfig = cfg
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal gemini image generateContent request: %w", err)
	}
	targetURL, err := joinGeminiModelMethod(baseURL, modelName, "generateContent", key)
	return body, targetURL, err
}

func joinGeminiModelMethod(baseURL, modelName, method, key string) (string, error) {
	modelPath := strings.TrimSpace(modelName)
	if !strings.Contains(modelPath, "/") {
		modelPath = "models/" + modelPath
	}
	targetURL, err := xurl.JoinGeminiPath(baseURL, fmt.Sprintf("/v1beta/%s:%s", modelPath, method))
	if err != nil {
		return "", fmt.Errorf("failed to build gemini image upstream url: %w", err)
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse gemini image upstream url: %w", err)
	}
	q := parsed.Query()
	q.Set("key", key)
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

func openAIImagesResponseFromGemini(body io.Reader, modelName string, payload map[string]any) (*imagesGenerationAPIResponse, *imagesUsage, error) {
	if strings.HasPrefix(strings.ToLower(modelName), "imagen") {
		var geminiResp geminiImagePredictResponse
		if err := json.NewDecoder(body).Decode(&geminiResp); err != nil {
			return nil, nil, fmt.Errorf("failed to decode gemini imagen response: %w", err)
		}
		data := make([]imagesGenerationDatum, 0, len(geminiResp.Predictions))
		for _, prediction := range geminiResp.Predictions {
			if strings.TrimSpace(prediction.BytesBase64Encoded) == "" {
				continue
			}
			data = append(data, geminiImageDatum(prediction.MimeType, prediction.BytesBase64Encoded, payload))
		}
		if len(data) == 0 {
			return nil, nil, errors.New("gemini imagen response did not contain image data")
		}
		return &imagesGenerationAPIResponse{Created: time.Now().Unix(), Data: data}, nil, nil
	}

	var geminiResp transformerModel.GeminiGenerateContentResponse
	if err := json.NewDecoder(body).Decode(&geminiResp); err != nil {
		return nil, nil, fmt.Errorf("failed to decode gemini image response: %w", err)
	}
	data := make([]imagesGenerationDatum, 0)
	for _, candidate := range geminiResp.Candidates {
		if candidate == nil || candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part == nil || part.InlineData == nil || strings.TrimSpace(part.InlineData.Data) == "" {
				continue
			}
			data = append(data, geminiImageDatum(part.InlineData.MimeType, part.InlineData.Data, payload))
		}
	}
	if len(data) == 0 {
		return nil, nil, errors.New("gemini image response did not contain inline image data")
	}
	return &imagesGenerationAPIResponse{
		Created: time.Now().Unix(),
		Data:    data,
		Usage:   imagesUsageFromGeminiUsage(geminiResp.UsageMetadata),
	}, imagesUsageFromGeminiUsage(geminiResp.UsageMetadata), nil
}

func geminiImageDatum(mimeType, b64 string, payload map[string]any) imagesGenerationDatum {
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = imageMediaType(stringFromPayload(payload, "output_format"))
	}
	if strings.EqualFold(strings.TrimSpace(stringFromPayload(payload, "response_format")), "url") {
		return imagesGenerationDatum{URL: "data:" + mimeType + ";base64," + b64}
	}
	return imagesGenerationDatum{B64JSON: b64}
}

func imagesUsageFromGeminiUsage(usage *transformerModel.GeminiUsageMetadata) *imagesUsage {
	if usage == nil {
		return nil
	}
	return &imagesUsage{
		PromptTokens:     usage.PromptTokenCount,
		CompletionTokens: usage.CandidatesTokenCount,
		TotalTokens:      usage.TotalTokenCount,
	}
}

func geminiImageConfigFromImagesPayload(payload map[string]any) *transformerModel.GeminiImageConfig {
	cfg := &transformerModel.GeminiImageConfig{
		AspectRatio: geminiAspectRatioFromImagePayload(payload),
		ImageSize:   geminiImageSizeFromQuality(stringFromPayload(payload, "quality")),
	}
	if cfg.AspectRatio == "" && cfg.ImageSize == "" {
		return nil
	}
	return cfg
}

func geminiAspectRatioFromImagePayload(payload map[string]any) string {
	if imageConfig, ok := payload["image_config"].(map[string]any); ok {
		if aspectRatio := strings.TrimSpace(stringFromPayload(imageConfig, "aspect_ratio")); aspectRatio != "" {
			return aspectRatio
		}
	}
	return geminiAspectRatioFromSize(stringFromPayload(payload, "size"))
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

func stringFromPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	switch v := payload[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}

func positiveIntFromPayload(payload map[string]any, key string, fallback int) int {
	if payload == nil {
		return fallback
	}
	switch v := payload[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case json.Number:
		if parsed, err := strconv.Atoi(v.String()); err == nil && parsed > 0 {
			return parsed
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func isGrokImagesModel(modelName string) bool {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	return strings.Contains(modelName, "grok-imagine-image") ||
		strings.Contains(modelName, "grok-2-image")
}

func normalizeGrokImagesPayload(payload map[string]any) map[string]any {
	normalized := make(map[string]any, 4)
	for _, key := range []string{"model", "prompt", "n", "response_format"} {
		if value, ok := payload[key]; ok {
			normalized[key] = value
		}
	}
	return normalized
}
