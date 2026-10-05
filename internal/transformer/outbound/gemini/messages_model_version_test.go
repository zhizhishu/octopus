package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// M2: convertGeminiToLLMResponse must carry the upstream self-declared model
// (Gemini's modelVersion) into the audit-only field
// InternalLLMResponse.UpstreamDeclaredModel so the relay audit path
// (captureUpstreamDeclaredModel) can compare it against the sent model — WITHOUT
// touching the client-visible Model. Blank or missing modelVersion must stay
// unset on both: an absent self-report is never backfilled with the request model.
func TestConvertGeminiToLLMResponseCarriesModelVersion(t *testing.T) {
	minimalCandidate := []*transformerModel.GeminiCandidate{{Index: 0}}

	t.Run("non_stream_non_empty_sets_audit_field_only", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "gemini-2.5-pro",
		}, false)
		if resp.UpstreamDeclaredModel != "gemini-2.5-pro" {
			t.Fatalf("expected UpstreamDeclaredModel=%q, got %q", "gemini-2.5-pro", resp.UpstreamDeclaredModel)
		}
		// The client-visible Model must stay untouched (old contract): writing the
		// upstream version here would leak it into an unmapped Gemini→Chat body.
		if resp.Model != "" {
			t.Fatalf("client-visible Model must stay empty, got %q", resp.Model)
		}
	})

	t.Run("non_stream_trims_surrounding_whitespace", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "  gemini-2.5-flash  ",
		}, false)
		if resp.UpstreamDeclaredModel != "gemini-2.5-flash" {
			t.Fatalf("expected trimmed UpstreamDeclaredModel=%q, got %q", "gemini-2.5-flash", resp.UpstreamDeclaredModel)
		}
	})

	t.Run("non_stream_missing_leaves_audit_field_empty", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates: minimalCandidate,
		}, false)
		if resp.UpstreamDeclaredModel != "" {
			t.Fatalf("expected empty UpstreamDeclaredModel for missing modelVersion, got %q", resp.UpstreamDeclaredModel)
		}
	})

	t.Run("non_stream_whitespace_only_leaves_audit_field_empty", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "   ",
		}, false)
		if resp.UpstreamDeclaredModel != "" {
			t.Fatalf("expected empty UpstreamDeclaredModel for whitespace-only modelVersion, got %q", resp.UpstreamDeclaredModel)
		}
	})

	t.Run("stream_non_empty_sets_audit_field_only", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates:   minimalCandidate,
			ModelVersion: "gemini-2.5-pro",
		}, true)
		if resp.UpstreamDeclaredModel != "gemini-2.5-pro" {
			t.Fatalf("expected UpstreamDeclaredModel=%q, got %q", "gemini-2.5-pro", resp.UpstreamDeclaredModel)
		}
		if resp.Model != "" {
			t.Fatalf("client-visible Model must stay empty, got %q", resp.Model)
		}
	})

	t.Run("stream_missing_leaves_audit_field_empty", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates: minimalCandidate,
		}, true)
		if resp.UpstreamDeclaredModel != "" {
			t.Fatalf("expected empty UpstreamDeclaredModel for missing modelVersion, got %q", resp.UpstreamDeclaredModel)
		}
	})
}

// End-to-end through the outbound entry points: the JSON "modelVersion" field
// must reach the audit field for both the non-stream and stream parse paths, and
// must not appear in a serialized response object (json:"-").
func TestGeminiOutboundParsesModelVersionIntoAuditField(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.5-pro"}`)

	assertAuditFieldNotSerialized := func(t *testing.T, resp *transformerModel.InternalLLMResponse) {
		t.Helper()
		if resp.UpstreamDeclaredModel != "gemini-2.5-pro" {
			t.Fatalf("expected UpstreamDeclaredModel=%q, got %q", "gemini-2.5-pro", resp.UpstreamDeclaredModel)
		}
		encoded, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		if bytes.Contains(encoded, []byte("gemini-2.5-pro")) {
			t.Fatalf("audit-only field leaked into serialized response: %s", encoded)
		}
		if bytes.Contains(encoded, []byte("UpstreamDeclaredModel")) {
			t.Fatalf("audit-only field name leaked into serialized response: %s", encoded)
		}
	}

	t.Run("non_stream_TransformResponse", func(t *testing.T) {
		httpResp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
		}
		resp, err := (&MessagesOutbound{}).TransformResponse(context.Background(), httpResp)
		if err != nil {
			t.Fatalf("TransformResponse returned error: %v", err)
		}
		assertAuditFieldNotSerialized(t, resp)
		if resp.Model != "" {
			t.Fatalf("client-visible Model must stay empty, got %q", resp.Model)
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
		assertAuditFieldNotSerialized(t, resp)
		if resp.Model != "" {
			t.Fatalf("client-visible Model must stay empty, got %q", resp.Model)
		}
	})

	t.Run("non_stream_TransformResponse_missing_modelVersion", func(t *testing.T) {
		httpResp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"candidates":[{"index":0}]}`)),
		}
		resp, err := (&MessagesOutbound{}).TransformResponse(context.Background(), httpResp)
		if err != nil {
			t.Fatalf("TransformResponse returned error: %v", err)
		}
		if resp.UpstreamDeclaredModel != "" || resp.Model != "" {
			t.Fatalf("expected both model fields empty when modelVersion absent, got audit=%q model=%q", resp.UpstreamDeclaredModel, resp.Model)
		}
	})
}
