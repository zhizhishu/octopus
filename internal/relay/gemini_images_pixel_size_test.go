package relay

import (
	"encoding/json"
	"testing"
)

func TestGeminiImageConfigFromImagesPayloadPixelSize(t *testing.T) {
	cases := []struct {
		name       string
		payload    map[string]any
		wantAspect string
		wantSize   string
	}{
		{"cherry 2048 square with quality auto", map[string]any{"size": "2048x2048", "quality": "auto"}, "1:1", "2K"},
		{"4K landscape pixels", map[string]any{"size": "4800x3584"}, "4:3", "4K"},
		{"amazon 1200x900 stays default tier", map[string]any{"size": "1200x900"}, "4:3", ""},
		{"quality high still works", map[string]any{"size": "1024x1024", "quality": "high"}, "1:1", "2K"},
		{"explicit image_size beats pixels", map[string]any{"size": "4096x4096", "image_size": "2K"}, "1:1", "2K"},
		{"image_config as JSON string", map[string]any{"image_config": `{"aspect_ratio":"16:9","image_size":"4K"}`}, "16:9", "4K"},
		{"camelCase image_config", map[string]any{"image_config": map[string]any{"aspectRatio": "3:2", "imageSize": "2k"}}, "3:2", "2K"},
		{"new-api extra_body.google", map[string]any{"extra_body": map[string]any{"google": map[string]any{"image_config": map[string]any{"aspect_ratio": "9:16", "image_size": "4K"}}}}, "9:16", "4K"},
		{"explicit config beats pixel size", map[string]any{"size": "2048x2048", "image_config": map[string]any{"aspect_ratio": "16:9", "image_size": "1K"}}, "16:9", "1K"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := geminiImageConfigFromImagesPayload(tc.payload)
			gotAspect, gotSize := "", ""
			if cfg != nil {
				gotAspect, gotSize = cfg.AspectRatio, cfg.ImageSize
			}
			if gotAspect != tc.wantAspect || gotSize != tc.wantSize {
				t.Fatalf("got aspect=%q size=%q, want %q / %q", gotAspect, gotSize, tc.wantAspect, tc.wantSize)
			}
		})
	}
}

func TestGeminiImageRequestBodyCarriesPixelTier(t *testing.T) {
	body, _, err := geminiImageRequestBodyAndURLWithInput("https://example.invalid", "gemini-3-pro-image", "k", map[string]any{"size": "2048x2048", "quality": "auto"}, "a cow", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		GenerationConfig struct {
			ImageConfig struct {
				AspectRatio string `json:"aspectRatio"`
				ImageSize   string `json:"imageSize"`
			} `json:"imageConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.GenerationConfig.ImageConfig.ImageSize != "2K" || got.GenerationConfig.ImageConfig.AspectRatio != "1:1" {
		t.Fatalf("imageConfig = %+v, want 1:1 / 2K; body=%s", got.GenerationConfig.ImageConfig, body)
	}
}

// Imagen predict 保持原行为：不按像素推档位、不做近似比例。
func TestImagenPredictKeepsFixedSizeTable(t *testing.T) {
	payload := map[string]any{"size": "2048x2048"}
	if got := imagenImageSizeFromImagesPayload(payload); got != "" {
		t.Fatalf("imagen size = %q, want empty", got)
	}
	if got := imagenAspectRatioFromImagePayload(map[string]any{"size": "1200x900"}); got != "" {
		t.Fatalf("imagen aspect = %q, want empty", got)
	}
}
