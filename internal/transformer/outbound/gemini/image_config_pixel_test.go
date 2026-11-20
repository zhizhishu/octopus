package gemini

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// image_generation 工具只给像素 size（quality=auto）时，按长边推 2K/4K，比例按最近标准值。
func TestGeminiImageConfigToolPixelSize(t *testing.T) {
	request := &model.InternalLLMRequest{
		Model: "gemini-3-pro-image",
		Tools: []model.Tool{{
			Type:            "image_generation",
			ImageGeneration: &model.ImageGeneration{Size: "2400x1792", Quality: "auto"},
		}},
	}
	cfg := geminiImageConfigFromRequest(request)
	if cfg == nil {
		t.Fatal("expected image config")
	}
	if cfg.AspectRatio != "4:3" || cfg.ImageSize != "2K" {
		t.Fatalf("got aspectRatio=%q imageSize=%q, want 4:3 / 2K", cfg.AspectRatio, cfg.ImageSize)
	}
}
