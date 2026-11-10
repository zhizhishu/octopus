package relay

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/transformer/inbound/openai"
	"github.com/zhizhishu/octopus/internal/transformer/model"
)

func identityUsage() *model.Usage {
	return &model.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
}

func identityBody(content string) *model.InternalLLMResponse {
	text := content
	return &model.InternalLLMResponse{
		ID:                "resp_fixed",
		Object:            "chat.completion",
		Model:             "mapped-b",
		SystemFingerprint: "fp_original",
		ServiceTier:       "tier_original",
		Usage:             identityUsage(),
		Choices: []model.Choice{{
			Index: 0,
			Message: &model.Message{
				Role:    "assistant",
				Content: model.MessageContent{Content: &text},
				ToolCalls: []model.ToolCall{{
					ID:   "call_fixed",
					Type: "function",
					Function: model.FunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"same"}`,
					},
				}},
			},
		}},
	}
}

func newIdentityAttempt(requestModel string) *relayAttempt {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	internal := &model.InternalLLMRequest{Model: "sent-b"}
	return &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inAdapter:       &openai.ChatInbound{},
			requestModel:    requestModel,
			internalRequest: internal,
			metrics:         NewRelayMetrics(1, 1, "127.0.0.1", requestModel, internal),
		},
	}
}

func TestClientIdentitySurvivesRealChatAggregate(t *testing.T) {
	ra := newIdentityAttempt("public-a")
	original := identityBody("hello-body")

	ra.captureUpstreamDeclaredModel(original)
	ra.captureUpstreamIdentity(original)
	projected := ra.projectClientResponse(original)
	if _, err := ra.inAdapter.TransformResponse(context.Background(), projected); err != nil {
		t.Fatalf("transform: %v", err)
	}
	ra.collectResponse()

	if original.Model != "mapped-b" || original.SystemFingerprint != "fp_original" {
		t.Fatalf("original mutated: model=%q fp=%q", original.Model, original.SystemFingerprint)
	}
	if projected.Choices[0].Message != original.Choices[0].Message || projected.Usage != original.Usage {
		t.Fatal("projection rewrote shared nested pointers")
	}
	if ra.metrics.UpstreamDeclaredModel != "mapped-b" {
		t.Fatalf("declared C = %q", ra.metrics.UpstreamDeclaredModel)
	}
	if ra.metrics.ActualModel != "sent-b" {
		t.Fatalf("actual B = %q", ra.metrics.ActualModel)
	}
	if ra.metrics.UpstreamSystemFingerprint != "fp_original" || ra.metrics.UpstreamServiceTier != "tier_original" {
		t.Fatalf("identity lost: fp=%q tier=%q", ra.metrics.UpstreamSystemFingerprint, ra.metrics.UpstreamServiceTier)
	}
	got := ra.metrics.InternalResponse
	if got == nil || got.Model != "public-a" || got.SystemFingerprint != "" {
		t.Fatalf("aggregate = %#v", got)
	}
	if got.ID != "resp_fixed" || got.Choices[0].Message.ToolCalls[0].ID != "call_fixed" {
		t.Fatal("id or tool id changed")
	}
	if got.Choices[0].Message.Content.Content == nil || *got.Choices[0].Message.Content.Content != "hello-body" {
		t.Fatal("body changed")
	}
	if got.Usage == nil || got.Usage.TotalTokens != 18 {
		t.Fatal("usage changed")
	}
	logRow := ra.metrics.buildRelayLog(context.Background(), nil, 0, nil, 9, "fake-channel")
	if logRow.RequestModelName != "public-a" || logRow.ActualModelName != "sent-b" || logRow.UpstreamResponseModel != "mapped-b" {
		t.Fatalf("log ABC = %q / %q / %q", logRow.RequestModelName, logRow.ActualModelName, logRow.UpstreamResponseModel)
	}
	if logRow.UpstreamSystemFingerprint != "fp_original" || logRow.UpstreamServiceTier != "tier_original" {
		t.Fatalf("log identity = %q / %q", logRow.UpstreamSystemFingerprint, logRow.UpstreamServiceTier)
	}
	if !strings.Contains(logRow.ResponseContent, "hello-body") || strings.Contains(logRow.ResponseContent, "fp_original") {
		t.Fatal("response content lost the body or kept the cleared fingerprint")
	}
}

func TestClientIdentitySSEAggregateAndEmptyRequest(t *testing.T) {
	ra := newIdentityAttempt("public-a")
	chunk := identityBody("sse-body")
	chunk.Object = "chat.completion.chunk"
	chunk.Choices[0].Delta = chunk.Choices[0].Message
	chunk.Choices[0].Message = nil

	ra.captureUpstreamDeclaredModel(chunk)
	ra.captureUpstreamIdentity(chunk)
	projected := ra.projectClientResponse(chunk)
	if _, err := ra.inAdapter.TransformStream(context.Background(), projected); err != nil {
		t.Fatalf("stream transform: %v", err)
	}
	if chunk.Model != "mapped-b" || chunk.SystemFingerprint != "fp_original" {
		t.Fatal("sse original mutated")
	}
	ra.collectResponse()
	got := ra.metrics.InternalResponse
	if got == nil || got.Model != "public-a" || got.SystemFingerprint != "" {
		t.Fatalf("sse aggregate = %#v", got)
	}
	if got.ID != "resp_fixed" || got.Usage == nil || got.Usage.CompletionTokens != 7 {
		t.Fatal("sse id or usage changed")
	}
	if ra.metrics.UpstreamSystemFingerprint != "fp_original" {
		t.Fatalf("sse fingerprint = %q", ra.metrics.UpstreamSystemFingerprint)
	}

	// Empty public name is an incomplete internal fixture only. Production request
	// validation rejects an empty model, so this is not a client bypass. The helper
	// leaves Model unchanged when requestModel is empty and still clears fingerprint.
	bare := newIdentityAttempt("")
	raw := identityBody("untouched")
	out := bare.projectClientResponse(raw)
	if out.Model != "mapped-b" || out.SystemFingerprint != "" {
		t.Fatalf("empty request projection = model %q fp %q", out.Model, out.SystemFingerprint)
	}
	if raw.SystemFingerprint != "fp_original" {
		t.Fatal("empty-request projection mutated original")
	}
	if got := (*relayAttempt)(nil).projectClientResponse(raw); got != raw {
		t.Fatal("nil attempt must return the input unchanged")
	}
	if got := (&relayAttempt{}).projectClientResponse(raw); got != raw || raw.SystemFingerprint != "fp_original" {
		t.Fatal("attempt with nil relayRequest must not project or panic")
	}
}

func TestFailedAttemptIdentityDoesNotReachWinner(t *testing.T) {
	ra := newIdentityAttempt("public-a")
	ra.internalRequest.Model = "sent-winner"

	loser := identityBody("loser")
	loser.Model = "loser-c"
	loser.SystemFingerprint = "fp_loser"
	loser.ServiceTier = "tier_loser"
	ra.captureUpstreamDeclaredModel(loser)
	ra.captureUpstreamIdentity(loser)
	if _, err := ra.inAdapter.TransformResponse(context.Background(), ra.projectClientResponse(loser)); err != nil {
		t.Fatalf("loser transform: %v", err)
	}
	// Failed attempt: no collectResponse. A goto retry on this same attempt starts
	// a new response and a fresh inbound aggregate. The reset must drop the loser's
	// declared model, or captureUpstreamDeclaredModel keeps loser-c forever.
	ra.resetResponseObservationForNewResponse()
	ra.inAdapter = &openai.ChatInbound{}

	winner := identityBody("winner-body")
	winner.Model = "winner-c"
	winner.SystemFingerprint = "fp_winner"
	winner.ServiceTier = "tier_winner"
	ra.captureUpstreamDeclaredModel(winner)
	ra.captureUpstreamEnvelope(winner)
	ra.captureUpstreamIdentity(winner)
	projected := ra.projectClientResponse(winner)
	if _, err := ra.inAdapter.TransformResponse(context.Background(), projected); err != nil {
		t.Fatalf("winner transform: %v", err)
	}
	ra.collectResponse()

	if ra.metrics.UpstreamDeclaredModel != "winner-c" {
		t.Fatalf("winner C = %q", ra.metrics.UpstreamDeclaredModel)
	}
	if ra.metrics.UpstreamSystemFingerprint != "fp_winner" || ra.metrics.UpstreamServiceTier != "tier_winner" {
		t.Fatalf("winner identity = %q / %q", ra.metrics.UpstreamSystemFingerprint, ra.metrics.UpstreamServiceTier)
	}
	if ra.metrics.InternalResponse == nil || ra.metrics.InternalResponse.Model != "public-a" {
		t.Fatal("winner aggregate is not public A")
	}
	body := ra.metrics.InternalResponse.Choices[0].Message.Content.Content
	if body == nil || *body != "winner-body" {
		t.Fatal("loser body mixed into winner")
	}
	logRow := ra.metrics.buildRelayLog(context.Background(), nil, 0, nil, 9, "fake-channel")
	if logRow.RequestModelName != "public-a" || logRow.ActualModelName != "sent-winner" || logRow.UpstreamResponseModel != "winner-c" || logRow.UpstreamSystemFingerprint != "fp_winner" {
		t.Fatalf("winner log A/B/C/fp = %q / %q / %q / %q", logRow.RequestModelName, logRow.ActualModelName, logRow.UpstreamResponseModel, logRow.UpstreamSystemFingerprint)
	}
	if strings.Contains(logRow.UpstreamSystemFingerprint, "loser") || strings.Contains(logRow.ResponseContent, "loser") {
		t.Fatal("loser identity or body reached the winner log")
	}
}

func TestSetInternalResponseKeepsUnprojectedFingerprint(t *testing.T) {
	m := NewRelayMetrics(1, 1, "127.0.0.1", "public-a", nil)
	m.SetInternalResponse(&model.InternalLLMResponse{
		Model:             "upstream-c",
		SystemFingerprint: "fp_direct",
		ServiceTier:       "tier_direct",
	}, "sent-b")
	row := m.buildRelayLog(context.Background(), nil, 0, nil, 1, "fake")
	if row.UpstreamSystemFingerprint != "fp_direct" || row.UpstreamServiceTier != "tier_direct" {
		t.Fatalf("direct set lost identity: %q / %q", row.UpstreamSystemFingerprint, row.UpstreamServiceTier)
	}
	if row.UpstreamResponseModel != "" {
		t.Fatal("an unprojected response model must not be logged as declared C")
	}

	m.CommitUpstreamIdentity("", "")
	m.SetInternalResponse(&model.InternalLLMResponse{SystemFingerprint: "fp_projected"}, "sent-b")
	row = m.buildRelayLog(context.Background(), nil, 0, nil, 1, "fake")
	if row.UpstreamSystemFingerprint != "" || row.UpstreamServiceTier != "" {
		t.Fatalf("projected aggregate refilled a committed empty identity: %q / %q", row.UpstreamSystemFingerprint, row.UpstreamServiceTier)
	}
}
