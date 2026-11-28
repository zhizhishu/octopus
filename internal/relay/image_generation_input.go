package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"sort"
	"strings"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// Only the image-generation bridge reads these inputs. Ordinary chat and
// Responses requests retain their original multi-part content and wire shape.
func (ra *relayAttempt) imageEditSources(ctx context.Context) ([]*transformerModel.GeminiPart, error) {
	if ra == nil || ra.internalRequest == nil {
		return nil, nil
	}
	sources := make([]any, 0, 2)
	for _, msg := range ra.internalRequest.Messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		for _, part := range msg.Content.MultipleContent {
			if !strings.EqualFold(part.Type, "image_url") && !strings.EqualFold(part.Type, "input_image") {
				continue
			}
			if part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
				return nil, errors.New("image generation reference image is missing image_url")
			}
			if len(sources) >= 16 {
				return nil, errors.New("image generation accepts a maximum of 16 reference images")
			}
			source := strings.TrimSpace(part.ImageURL.URL)
			if strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://") {
				// Use the existing public-only image client, not the upstream
				// channel proxy. Its dialer checks redirects against SSRF.
				downloaded, err := ra.downloadImageToBase64DataURL(ctx, source)
				if err != nil {
					return nil, fmt.Errorf("could not load image generation reference: %s", err.Error())
				}
				source = downloaded
			}
			sources = append(sources, source)
		}
	}
	if len(sources) == 0 {
		return nil, nil
	}
	// Reuse the exact Images edits data-URL parser and Gemini inlineData type.
	parts, err := geminiImageEditJSONParts(map[string]any{"images": sources})
	if err != nil {
		return nil, err
	}
	var totalBytes int
	for _, part := range parts {
		decoded, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			return nil, fmt.Errorf("invalid reference image bytes: %w", err)
		}
		totalBytes += len(decoded)
		if totalBytes > maxDownloadedImageBytes {
			return nil, errors.New("image generation reference images exceed 32 MiB in total")
		}
	}
	return parts, nil
}

// OpenAI Images edits expects multipart image files, unlike generations.
// Reusing the same inbound image parts avoids creating a second parser.
func openAIImageEditMultipart(payload map[string]any, parts []*transformerModel.GeminiPart) ([]byte, string, error) {
	if len(parts) == 0 {
		return nil, "", errors.New("image edit needs at least one reference image")
	}
	if payload["input_image_mask"] != nil || payload["mask"] != nil {
		return nil, "", errors.New("image generation input_image_mask requires a separately supported mask upload")
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for _, key := range []string{"model", "prompt"} {
		if value := strings.TrimSpace(stringFromPayload(payload, key)); value != "" {
			if err := writer.WriteField(key, value); err != nil {
				return nil, "", err
			}
		}
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		if key == "model" || key == "prompt" || key == "stream" || key == "input_image_mask" || key == "mask" || key == "image" || key == "images" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if payload[key] == nil {
			continue
		}
		var value string
		switch v := payload[key].(type) {
		case map[string]any, []any:
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, "", err
			}
			value = string(raw)
		default:
			value = fmt.Sprint(v)
		}
		if err := writer.WriteField(key, value); err != nil {
			return nil, "", err
		}
	}
	formKey := "image"
	if len(parts) > 1 {
		formKey = "image[]"
	}
	for idx, part := range parts {
		mimeType := strings.TrimSpace(part.InlineData.MimeType)
		ext := "png"
		switch mimeType {
		case "image/jpeg":
			ext = "jpg"
		case "image/webp":
			ext = "webp"
		case "image/gif":
			ext = "gif"
		case "image/avif":
			ext = "avif"
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf("form-data; name=%q; filename=%q", formKey, fmt.Sprintf("reference-%d.%s", idx+1, ext)))
		header.Set("Content-Type", mimeType)
		dst, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		raw, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			return nil, "", err
		}
		if _, err := dst.Write(raw); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

func appendGeminiImageConfigFromMetadata(payload map[string]any, req *transformerModel.InternalLLMRequest) {
	if req == nil || req.TransformerMetadata == nil {
		return
	}
	raw := strings.TrimSpace(req.TransformerMetadata["gemini_image_config"])
	if raw == "" {
		return
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err == nil && len(config) > 0 {
		payload["image_config"] = config
	}
}
