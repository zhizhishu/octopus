package relay

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	openaiInbound "github.com/zhizhishu/octopus/internal/transformer/inbound/openai"
	openaiOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

// A non-stream caller is served by aggregating a streaming upstream when a
// Responses backend insists on SSE or when the chat self-heal branch is used.
// A genuine partial tool call must never be returned as an HTTP 200 "success".
func TestForcedNonStreamPartialToolArgumentsDoNotBecomeSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ra := &relayAttempt{relayRequest: &relayRequest{
		c: c, inboundType: inbound.InboundTypeOpenAIChat,
		inAdapter:    &openaiInbound.ChatInbound{},
		requestModel: truncationTestModel,
	}}
	partial := `{"id":"chatcmpl_trunc","object":"chat.completion.chunk","created":1,"model":"trunc-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_partial","type":"function","function":{"name":"calculate","arguments":"{\"value\":"}}]}}]}`
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("data: " + chatRoleChunk() + "\n\n" +
			"data: " + partial + "\n\n")),
	}
	err := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ChatOutbound{}, 0)
	var local *localRelayError
	if !errors.As(err, &local) || local.code != "octopus_upstream_stream_truncated" {
		t.Fatalf("partial tool call without terminal must be rejected with existing upstream error code, got %v, body=%q", err, rec.Body.String())
	}
	if ra.outputIncompleteReason != dbmodel.RelayLogOutputIncompleteUpstreamMissingTerminal {
		t.Fatalf("truncated tool call must be visible to administrators; incomplete reason=%q", ra.outputIncompleteReason)
	}
	if strings.Contains(rec.Body.String(), "call_partial") {
		t.Fatalf("partial tool call must never be delivered as successful JSON: %q", rec.Body.String())
	}
}

func TestForcedNonStreamFinishReasonWithoutDoneIsComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ra := &relayAttempt{relayRequest: &relayRequest{
		c: c, inboundType: inbound.InboundTypeOpenAIChat,
		inAdapter:    &openaiInbound.ChatInbound{},
		requestModel: truncationTestModel,
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("data: " + chatRoleChunk() + "\n\n" +
			"data: " + chatChunk("complete") + "\n\n" +
			"data: " + chatFinishChunk() + "\n\n")),
	}
	err := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ChatOutbound{}, 0)
	if err != nil {
		t.Fatalf("valid finish_reason must retain old successful aggregate behavior: %v", err)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "complete") {
		t.Fatalf("expected complete non-stream JSON response, status=%d body=%q", rec.Code, rec.Body.String())
	}
	if ra.outputIncompleteReason != "" {
		t.Fatalf("valid finish_reason must not be marked truncated, got %q", ra.outputIncompleteReason)
	}
}

// A Responses backend that sends a prelude and partial generated content but
// closes before response.completed / [DONE] must not be silently promoted to a
// success body. The existing JSON failure envelope is kept by the outer relay.
func TestForcedNonStreamResponsesTextEOFDoesNotBecomeSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ra := &relayAttempt{relayRequest: &relayRequest{
		c: c, inboundType: inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.created","response":{"id":"resp_partial","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","delta":"half"}` + "\n\n",
		)),
	}
	err := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}, 0)
	var local *localRelayError
	if !errors.As(err, &local) || local.code != "octopus_upstream_stream_truncated" {
		t.Fatalf("partial Responses stream without terminal must be an upstream truncation, got %v, body=%q", err, rec.Body.String())
	}
	if ra.outputIncompleteReason != dbmodel.RelayLogOutputIncompleteUpstreamMissingTerminal {
		t.Fatalf("partial Responses stream missing log diagnostic: %q", ra.outputIncompleteReason)
	}
}
