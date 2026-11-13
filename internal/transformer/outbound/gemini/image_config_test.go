package gemini

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// 客户端显式写的 image_config 必须压过 OpenAI image_generation 工具参数：这是
// CPA / new-api / Requesty 一致的优先级，也是"客户端要 4K 却拿到 1K"的根子。
func TestGeminiImageConfigExplicitMetadataWinsOverTool(t *testing.T) {
	request := &model.InternalLLMRequest{
		Model:               "gemini-3-pro-image",
		TransformerMetadata: map[string]string{"gemini_image_config": `{"aspect_ratio":"16:9","image_size":"4K"}`},
		Tools: []model.Tool{{
			Type:            "image_generation",
			ImageGeneration: &model.ImageGeneration{Size: "1024x1024", Quality: "1K"},
		}},
	}

	cfg := geminiImageConfigFromRequest(request)
	if cfg == nil {
		t.Fatal("expected image config")
	}
	if cfg.ImageSize != "4K" {
		t.Fatalf("imageSize = %q, want 4K (explicit image_config must win)", cfg.ImageSize)
	}
	if cfg.AspectRatio != "16:9" {
		t.Fatalf("aspectRatio = %q, want 16:9", cfg.AspectRatio)
	}
}

// 客户端没写 image_config 时，工具参数照旧生效——不能因为新优先级把老路径打断。
func TestGeminiImageConfigFallsBackToTool(t *testing.T) {
	request := &model.InternalLLMRequest{
		Model: "gemini-3-pro-image",
		Tools: []model.Tool{{
			Type:            "image_generation",
			ImageGeneration: &model.ImageGeneration{Size: "1792x1024", Quality: "hd"},
		}},
	}

	cfg := geminiImageConfigFromRequest(request)
	if cfg == nil {
		t.Fatal("expected image config from tool params")
	}
	if cfg.AspectRatio != "16:9" || cfg.ImageSize != "2K" {
		t.Fatalf("got aspectRatio=%q imageSize=%q, want 16:9 / 2K", cfg.AspectRatio, cfg.ImageSize)
	}
}

// 什么都没有时不发 imageConfig，保持上游默认档。
func TestGeminiImageConfigAbsentWithoutHints(t *testing.T) {
	request := &model.InternalLLMRequest{Model: "gemini-3-pro-image"}
	if cfg := geminiImageConfigFromRequest(request); cfg != nil {
		t.Fatalf("expected nil config, got %#v", cfg)
	}
}
