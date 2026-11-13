package openai

import (
	"context"
	"testing"
)

// chat 客户端（Cherry Studio / OpenRouter 风格）把分辨率写在顶层 image_config，
// 这个字段不属于 InternalLLMRequest，必须在入站单独收进元数据，否则被静默丢掉。
func TestChatInboundCapturesImageConfig(t *testing.T) {
	body := []byte(`{
		"model":"gemini-3-pro-image",
		"messages":[{"role":"user","content":"draw a 4K apple"}],
		"image_config":{"aspect_ratio":"16:9","image_size":"4K"}
	}`)

	request, err := (&ChatInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	got := request.TransformerMetadata["gemini_image_config"]
	want := `{"aspect_ratio":"16:9","image_size":"4K"}`
	if got != want {
		t.Fatalf("metadata = %q, want %q", got, want)
	}
}

// 驼峰写法也认（参考实现里两种都出现在生态中）。
func TestChatInboundCapturesCamelCaseImageConfig(t *testing.T) {
	body := []byte(`{"model":"gemini-3-pro-image","messages":[{"role":"user","content":"x"}],"image_config":{"aspectRatio":"1:1","imageSize":"2k"}}`)
	request, err := (&ChatInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	if got := request.TransformerMetadata["gemini_image_config"]; got != `{"aspect_ratio":"1:1","image_size":"2k"}` {
		t.Fatalf("metadata = %q", got)
	}
}

// 没写生图档位的普通 chat 请求不能平添元数据。
func TestChatInboundLeavesMetadataEmptyWithoutImageConfig(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	request, err := (&ChatInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	if _, ok := request.TransformerMetadata["gemini_image_config"]; ok {
		t.Fatalf("unexpected metadata for plain chat request")
	}
}
