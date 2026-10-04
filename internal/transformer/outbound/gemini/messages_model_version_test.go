package gemini

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// M2: convertGeminiToLLMResponse must carry the upstream self-declared model
// (Gemini's modelVersion) into InternalLLMResponse.Model so the relay audit path
// (captureUpstreamDeclaredModel) can compare it against the sent model. Blank or
// missing modelVersion must stay unset — an absent self-report is never
// backfilled with the request model.
func TestConvertGeminiToLLMResponseCarriesModelVersion(t *testing.T) {
	minimalCandidate := []*transformerModel.GeminiCandidate{{Index: 0}}

	t.Run("non_stream_non_empty_sets_model", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "gemini-2.5-pro",
		}, false)
		if resp.Model != "gemini-2.5-pro" {
			t.Fatalf("expected Model=%q, got %q", "gemini-2.5-pro", resp.Model)
		}
	})

	t.Run("non_stream_trims_surrounding_whitespace", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "  gemini-2.5-flash  ",
		}, false)
		if resp.Model != "gemini-2.5-flash" {
			t.Fatalf("expected trimmed Model=%q, got %q", "gemini-2.5-flash", resp.Model)
		}
	})

	t.Run("non_stream_missing_leaves_model_empty", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates: minimalCandidate,
		}, false)
		if resp.Model != "" {
			t.Fatalf("expected empty Model for missing modelVersion, got %q", resp.Model)
		}
	})

	t.Run("non_stream_whitespace_only_leaves_model_empty", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "   ",
		}, false)
		if resp.Model != "" {
			t.Fatalf("expected empty Model for whitespace-only modelVersion, got %q", resp.Model)
		}
	})

	t.Run("stream_non_empty_sets_model", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "gemini-2.5-pro",
		}, true)
		if resp.Model != "gemini-2.5-pro" {
			t.Fatalf("expected Model=%q, got %q", "gemini-2.5-pro", resp.Model)
		}
	})

	t.Run("stream_missing_leaves_model_empty", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates: minimalCandidate,
		}, true)
		if resp.Model != "" {
			t.Fatalf("expected empty Model for missing modelVersion, got %q", resp.Model)
		}
	})
}

// End-to-end through the outbound entry points: the JSON "modelVersion" field
// must reach resp.Model for both the non-stream and stream parse paths.
func TestGeminiOutboundParsesModelVersionIntoResponseModel(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.5-pro"}`)

	t.Run("non_stream_TransformResponse", func(t *testing.T) {
		httpResp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
		}
		resp, err := (&MessagesOutbound{}).TransformResponse(context.Background(), httpResp)
		if err != nil {
			t.Fatalf("TransformResponse returned error: %v", err)
		}
		if resp.Model != "gemini-2.5-pro" {
			t.Fatalf("expected Model=%q, got %q", "gemini-2.5-pro", resp.Model)
		}
	})

	t.Run("stream_TransformStream", func(t *testing.T) {
		resp, err := (&MessagesOutbound{}).TransformStream(context.Background(), body)
		if err != nil {
			t.Fatalf("TransformStream returned error: %v", err)
		}
		if resp == nil {
			t.Fatalf("TransformStream returned nil response")
		}
		if resp.Model != "gemini-2.5-pro" {
			t.Fatalf("expected Model=%q, got %q", "gemini-2.5-pro", resp.Model)
		}
	})

	t.Run("non_stream_TransformResponse_missing_modelVersion", func(t *testing.T) {
		httpResp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"index":0}]}`))),
		}
		resp, err := (&MessagesOutbound{}).TransformResponse(context.Background(), httpResp)
		if err != nil {
			t.Fatalf("TransformResponse returned error: %v", err)
		}
		if resp.Model != "" {
			t.Fatalf("expected empty Model when modelVersion absent, got %q", resp.Model)
		}
	})
}
