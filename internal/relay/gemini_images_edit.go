package relay

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/zhizhishu/octopus/internal/relay/bodycache"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// geminiImageEditInputs converts the existing OpenAI Images edits input surface into
// Gemini image parts. Generations do not enter this path, so their wire shape stays
// unchanged. The body cache already enforces the configurable media upload limit.
func geminiImageEditInputs(isMultipart bool, bc *bodycache.BodyCache, boundary string, payload map[string]any) (map[string]any, []*transformerModel.GeminiPart, error) {
	if !isMultipart {
		if payload == nil {
			return nil, nil, errors.New("nil image edit payload")
		}
		if payload["mask"] != nil {
			return nil, nil, errors.New("Gemini image edits do not support OpenAI masks")
		}
		parts, err := geminiImageEditJSONParts(payload)
		return payload, parts, err
	}
	if bc == nil {
		return nil, nil, errors.New("missing multipart image body")
	}
	reader, err := bc.NewReader()
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()

	parsed := make(map[string]any)
	parts := make([]*transformerModel.GeminiPart, 0, 2)
	mr := multipart.NewReader(reader, boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("parse image edit multipart: %w", err)
		}
		name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(part.FormName())), "[]")
		if name == "mask" {
			_ = part.Close()
			return nil, nil, errors.New("Gemini image edits do not support OpenAI masks")
		}
		if name == "image" || name == "images" {
			if len(parts) >= 16 {
				_ = part.Close()
				return nil, nil, errors.New("too many source images (maximum 16)")
			}
			raw, readErr := io.ReadAll(part)
			if closeErr := part.Close(); readErr == nil {
				readErr = closeErr
			}
			if readErr != nil {
				return nil, nil, fmt.Errorf("read source image: %w", readErr)
			}
			if len(raw) == 0 {
				return nil, nil, errors.New("source image is empty")
			}
			imageType := strings.TrimSpace(part.Header.Get("Content-Type"))
			if idx := strings.IndexByte(imageType, ';'); idx >= 0 {
				imageType = imageType[:idx]
			}
			if !strings.HasPrefix(strings.ToLower(imageType), "image/") {
				imageType = mime.TypeByExtension(strings.ToLower(filepath.Ext(part.FileName())))
			}
			if !strings.HasPrefix(strings.ToLower(imageType), "image/") {
				imageType = http.DetectContentType(raw)
			}
			if !strings.HasPrefix(strings.ToLower(imageType), "image/") {
				return nil, nil, errors.New("source file is not an image")
			}
			parts = append(parts, &transformerModel.GeminiPart{InlineData: &transformerModel.GeminiBlob{
				MimeType: imageType, Data: base64.StdEncoding.EncodeToString(raw),
			}})
			continue
		}
		// Only scalar editing controls are needed. Large files are handled in
		// the branch above; a malformed multi-megabyte form field is rejected.
		raw, readErr := io.ReadAll(io.LimitReader(part, 64*1024+1))
		_ = part.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if len(raw) > 64*1024 {
			return nil, nil, errors.New("image edit field is too large")
		}
		if name == "image_config" || name == "extra_body" {
			var nested map[string]any
			if err := json.Unmarshal(raw, &nested); err != nil {
				return nil, nil, fmt.Errorf("invalid %s JSON: %w", name, err)
			}
			parsed[name] = nested
		} else if name != "" {
			parsed[name] = string(raw)
		}
	}
	if len(parts) == 0 {
		return nil, nil, errors.New("image edit requires at least one source image")
	}
	return parsed, parts, nil
}

func geminiImageEditJSONParts(payload map[string]any) ([]*transformerModel.GeminiPart, error) {
	refs := make([]any, 0, 2)
	if v, ok := payload["image"]; ok {
		refs = append(refs, v)
	}
	if v, ok := payload["images"]; ok {
		switch arr := v.(type) {
		case []any:
			refs = append(refs, arr...)
		default:
			refs = append(refs, arr)
		}
	}
	if len(refs) == 0 {
		return nil, errors.New("image edit requires at least one source image")
	}
	if len(refs) > 16 {
		return nil, errors.New("too many source images (maximum 16)")
	}
	parts := make([]*transformerModel.GeminiPart, 0, len(refs))
	for _, ref := range refs {
		var source string
		switch v := ref.(type) {
		case string:
			source = v
		case map[string]any:
			if _, hasFileID := v["file_id"]; hasFileID {
				return nil, errors.New("Gemini image edits require image data, not OpenAI file_id")
			}
			switch imageURL := v["image_url"].(type) {
			case string:
				source = imageURL
			case map[string]any:
				source, _ = imageURL["url"].(string)
			}
		}
		dataURL := xurl.ParseDataURL(strings.TrimSpace(source))
		if dataURL == nil || !dataURL.IsBase64 || !strings.HasPrefix(strings.ToLower(dataURL.MediaType), "image/") {
			return nil, errors.New("Gemini image edits require a base64 image data URL or multipart image file")
		}
		if _, err := base64.StdEncoding.DecodeString(dataURL.Data); err != nil {
			return nil, fmt.Errorf("invalid base64 source image: %w", err)
		}
		parts = append(parts, &transformerModel.GeminiPart{InlineData: &transformerModel.GeminiBlob{
			MimeType: dataURL.MediaType, Data: dataURL.Data,
		}})
	}
	return parts, nil
}
