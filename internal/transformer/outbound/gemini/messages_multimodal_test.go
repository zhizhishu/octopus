package gemini

import (
	"context"
	"encoding/json"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func ptr[T any](v T) *T { return &v }

func decodeGeminiRequest(t *testing.T, req *transformerModel.InternalLLMRequest) *transformerModel.GeminiGenerateContentRequest {
	t.Helper()
	httpReq, err := (&MessagesOutbound{}).TransformRequest(context.Background(), req, "https://generativelanguage.googleapis.com/v1beta/models", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	var body transformerModel.GeminiGenerateContentRequest
	if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
		t.Fatalf("decode gemini request: %v", err)
	}
	return &body
}

// Fix 1: assistant multimodal content (e.g. images from prior drawing turns)
// must survive instead of being silently dropped.
func TestOutboundAssistantMultipleContentRoundTrips(t *testing.T) {
	stream := false
	req := &transformerModel.InternalLLMRequest{
		Model:  "gemini-2.5-flash-image",
		Stream: &stream,
		Messages: []transformerModel.Message{{
			Role: "assistant",
			Content: transformerModel.MessageContent{
				MultipleContent: []transformerModel.MessageContentPart{
					{Type: "text", Text: ptr("here is your sticker")},
					{Type: "image_url", ImageURL: &transformerModel.ImageURL{
						URL: "data:image/png;base64,QUJD",
					}},
				},
			},
		}},
	}

	body := decodeGeminiRequest(t, req)
	if len(body.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(body.Contents))
	}
	c := body.Contents[0]
	if c.Role != "model" {
		t.Fatalf("expected model role, got %q", c.Role)
	}
	if len(c.Parts) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d: %#v", len(c.Parts), c.Parts)
	}
	if c.Parts[0].Text != "here is your sticker" {
		t.Fatalf("unexpected text part: %#v", c.Parts[0])
	}
	if c.Parts[1].InlineData == nil || c.Parts[1].InlineData.MimeType != "image/png" || c.Parts[1].InlineData.Data != "QUJD" {
		t.Fatalf("expected inlineData image, got %#v", c.Parts[1])
	}
}

// Fix 2: remote http(s) image URLs must round-trip as fileData rather than
// being skipped.
func TestOutboundRemoteImageURLBecomesFileData(t *testing.T) {
	stream := false
	const remote = "https://example.com/cat.png"
	req := &transformerModel.InternalLLMRequest{
		Model:  "gemini-pro",
		Stream: &stream,
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				MultipleContent: []transformerModel.MessageContentPart{
					{Type: "image_url", ImageURL: &transformerModel.ImageURL{URL: remote}},
				},
			},
		}},
	}

	body := decodeGeminiRequest(t, req)
	parts := body.Contents[0].Parts
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].FileData == nil || parts[0].FileData.FileURI != remote {
		t.Fatalf("expected fileData with remote uri, got %#v", parts[0])
	}
	if parts[0].InlineData != nil {
		t.Fatalf("remote url should not produce inlineData: %#v", parts[0])
	}
}

// Fix 3: file parts referenced by FileURL / FileID, plus MediaType mime hint.
func TestOutboundFileRemoteAndIDBecomeFileData(t *testing.T) {
	stream := false
	const docURL = "https://example.com/report.pdf"
	const fileID = "files/abc123"
	req := &transformerModel.InternalLLMRequest{
		Model:  "gemini-pro",
		Stream: &stream,
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				MultipleContent: []transformerModel.MessageContentPart{
					{Type: "file", File: &transformerModel.File{
						FileURL:   docURL,
						MediaType: "application/pdf",
					}},
					{Type: "file", File: &transformerModel.File{
						FileID:    fileID,
						MediaType: "application/pdf",
					}},
				},
			},
		}},
	}

	body := decodeGeminiRequest(t, req)
	parts := body.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d: %#v", len(parts), parts)
	}
	if parts[0].FileData == nil || parts[0].FileData.FileURI != docURL || parts[0].FileData.MimeType != "application/pdf" {
		t.Fatalf("expected fileData from FileURL with mime, got %#v", parts[0])
	}
	if parts[1].FileData == nil || parts[1].FileData.FileURI != fileID || parts[1].FileData.MimeType != "application/pdf" {
		t.Fatalf("expected fileData from FileID with mime, got %#v", parts[1])
	}
}

// Fix 3 (cont): base64 file with no data-url mime falls back to File.MediaType.
func TestOutboundFileBase64UsesMediaTypeFallback(t *testing.T) {
	stream := false
	req := &transformerModel.InternalLLMRequest{
		Model:  "gemini-pro",
		Stream: &stream,
		Messages: []transformerModel.Message{{
			Role: "user",
			Content: transformerModel.MessageContent{
				MultipleContent: []transformerModel.MessageContentPart{
					// data URL without an explicit media type
					{Type: "file", File: &transformerModel.File{
						FileData:  "data:;base64,QUJD",
						MediaType: "application/pdf",
					}},
				},
			},
		}},
	}

	body := decodeGeminiRequest(t, req)
	parts := body.Contents[0].Parts
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].InlineData == nil || parts[0].InlineData.Data != "QUJD" {
		t.Fatalf("expected inlineData base64, got %#v", parts[0])
	}
	if parts[0].InlineData.MimeType != "application/pdf" {
		t.Fatalf("expected MediaType fallback mime, got %q", parts[0].InlineData.MimeType)
	}
}

// Fix 5: tool-call Index must count tool calls only, not the parts-loop index,
// so interleaved text/thought/functionCall parts do not cause index jumps or
// collisions.
func TestOutboundToolCallIndexIsContiguous(t *testing.T) {
	reason := "STOP"
	geminiResp := &transformerModel.GeminiGenerateContentResponse{
		Candidates: []*transformerModel.GeminiCandidate{{
			Index:        0,
			FinishReason: &reason,
			Content: &transformerModel.GeminiContent{
				Role: "model",
				Parts: []*transformerModel.GeminiPart{
					{Text: "thinking out loud", Thought: true},
					{Text: "let me call tools"},
					{FunctionCall: &transformerModel.GeminiFunctionCall{Name: "first", Args: map[string]interface{}{"a": 1}}},
					{Text: "and another"},
					{FunctionCall: &transformerModel.GeminiFunctionCall{Name: "second", Args: map[string]interface{}{"b": 2}}},
				},
			},
		}},
	}

	resp := convertGeminiToLLMResponse(geminiResp, true)
	if len(resp.Choices) != 1 || resp.Choices[0].Delta == nil {
		t.Fatalf("unexpected choices: %#v", resp.Choices)
	}
	tcs := resp.Choices[0].Delta.ToolCalls
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(tcs), tcs)
	}
	if tcs[0].Index != 0 || tcs[1].Index != 1 {
		t.Fatalf("expected contiguous indices 0,1 got %d,%d", tcs[0].Index, tcs[1].Index)
	}
	if tcs[0].Function.Name != "first" || tcs[1].Function.Name != "second" {
		t.Fatalf("unexpected tool call names: %#v", tcs)
	}
	if tcs[0].ID == tcs[1].ID {
		t.Fatalf("tool call IDs must not collide: %q", tcs[0].ID)
	}
}

// Fix 6: an explicit ReasoningBudget (e.g. round-tripped from inbound Gemini
// thinkingConfig.thinkingBudget) must be honored over ReasoningEffort.
func TestOutboundReasoningBudgetReadBack(t *testing.T) {
	stream := false
	content := "ping"
	budget := int64(2048)
	req := &transformerModel.InternalLLMRequest{
		Model:           "gemini-pro",
		Stream:          &stream,
		ReasoningEffort: "high", // would map to 24576 if budget were ignored
		ReasoningBudget: &budget,
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}

	body := decodeGeminiRequest(t, req)
	if body.GenerationConfig == nil || body.GenerationConfig.ThinkingConfig == nil {
		t.Fatalf("expected thinkingConfig, got %#v", body.GenerationConfig)
	}
	tb := body.GenerationConfig.ThinkingConfig.ThinkingBudget
	if tb == nil || *tb != 2048 {
		t.Fatalf("expected thinkingBudget 2048, got %#v", tb)
	}
}

// Guard: with only ReasoningEffort set, the effort mapping still applies.
func TestOutboundReasoningEffortStillMapsWithoutBudget(t *testing.T) {
	stream := false
	content := "ping"
	req := &transformerModel.InternalLLMRequest{
		Model:           "gemini-pro",
		Stream:          &stream,
		ReasoningEffort: "low",
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}

	body := decodeGeminiRequest(t, req)
	if body.GenerationConfig == nil || body.GenerationConfig.ThinkingConfig == nil {
		t.Fatalf("expected thinkingConfig, got %#v", body.GenerationConfig)
	}
	tb := body.GenerationConfig.ThinkingConfig.ThinkingBudget
	if tb == nil || *tb != 1024 {
		t.Fatalf("expected thinkingBudget 1024 from low effort, got %#v", tb)
	}
}
