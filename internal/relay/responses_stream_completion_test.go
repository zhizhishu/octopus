package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	openaiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
	"github.com/gin-gonic/gin"
)

func TestResponsesStreamDoneBeforeCompletedFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(`data: [DONE]` + "\n\n")),
	}

	err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{})
	if err == nil || !strings.Contains(err.Error(), "without internal response") {
		t.Fatalf("expected empty responses stream failure, got %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("empty upstream stream should not write downstream data before retry, got %q", body)
	}
}

func TestResponsesStreamCreatedOnlyBeforeDoneFailsWithoutWriting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		)),
	}

	err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{})
	if err == nil || !strings.Contains(err.Error(), "without internal response") {
		t.Fatalf("expected empty stream failure, got %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("created-only upstream stream should not write before retry, got %q", body)
	}
}

func TestResponsesStreamTextThenDoneSynthesizesCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","delta":"OK"}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		)),
	}

	if err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}); err != nil {
		t.Fatalf("handle stream response: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"response.completed"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("expected synthesized completed and DONE events, got %q", body)
	}
	if !strings.Contains(body, `"delta":"OK"`) {
		t.Fatalf("expected text delta to survive, got %q", body)
	}
}

func TestResponsesStreamCompletedThenDoneSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","delta":"OK"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		)),
	}

	if err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}); err != nil {
		t.Fatalf("handle stream response: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.completed") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected completed and DONE events, got %q", body)
	}
}

// An empty-but-complete stream (response.completed with zero output tokens and no
// content) is a legitimate empty completion and must SUCCEED like the non-stream
// path, not be mistaken for a dropped/empty stream. Regression guard for the
// [DONE]-vs-real-completion distinction (audit #1): [DONE] alone is not a
// completion, but a response.completed event is — even at output_tokens=0.
func TestResponsesStreamCompletedWithZeroTokensSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		)),
	}

	if err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}); err != nil {
		t.Fatalf("empty-but-complete stream should succeed, got: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, "response.completed") {
		t.Fatalf("expected the completed event forwarded downstream, got %q", body)
	}
}

func TestResponsesStreamToolCallDoneSynthesizesTerminalForSequentialCLI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	parallel := false

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
		internalRequest: &transformermodel.InternalLLMRequest{
			Model:             "gpt-5.5",
			ParallelToolCalls: &parallel,
		},
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.created","response":{"id":"resp_tool","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n" +
				`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"shell_command"}}` + "\n\n" +
				`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":\"pwd\"}"}` + "\n\n" +
				`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"shell_command","arguments":"{\"command\":\"pwd\"}"}}` + "\n\n",
		)),
	}

	if err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}); err != nil {
		t.Fatalf("handle tool call stream: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"response.function_call_arguments.done"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
		"data: [DONE]\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected synthesized terminal body to contain %q, got %s", want, body)
		}
	}
}

func TestResponsesStreamClientCancelAfterCompletedSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(pw,
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}`+"\n\n"+
				`data: {"type":"response.output_text.delta","delta":"OK"}`+"\n\n"+
				`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}}}`+"\n\n",
		)
		for range 100 {
			if strings.Contains(rec.Body.String(), "response.completed") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		writeDone <- err
	}()

	if err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}); err != nil {
		t.Fatalf("handle stream response after completed cancel: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write stream fixture: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("expected completed event before client cancel, got %q", body)
	}
}

// TestForcedResponsesNonStreamStopsSilentUpstream guards the data-interval idle
// cutoff on the forced non-stream aggregation path: a non-streaming client that
// posts to an OpenAI Responses channel must not hang forever when the upstream
// goes silent (or only emits a prelude) and never reaches a terminal event.
func TestForcedResponsesNonStreamStopsSilentUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "1"); err != nil {
		t.Fatalf("set stream data timeout setting: %v", err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	// Emit a single prelude, then stay silent: the upstream never sends a
	// terminal completed/DONE event, so without the idle cutoff this hangs.
	go func() {
		_, _ = io.WriteString(pw,
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}`+"\n\n",
		)
	}()

	startedAt := time.Now()
	err := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{})
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("expected silent upstream to be cut quickly, elapsed %s", elapsed)
	}
	if err == nil {
		t.Fatalf("expected idle-timeout error for silent upstream, got nil")
	}
	var relayErr *localRelayError
	if !errors.As(err, &relayErr) || relayErr.code != "octopus_upstream_stream_timeout" {
		t.Fatalf("expected octopus_upstream_stream_timeout, got %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("forced non-stream aggregation should not write downstream data, got %q", body)
	}
}

// TestForcedResponsesNonStreamCompletedAggregates is a regression guard for the
// select-based read loop: a completed-then-DONE stream must still aggregate into
// a single 200 JSON response.
func TestForcedResponsesNonStreamCompletedAggregates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n" +
				`data: {"type":"response.output_text.delta","delta":"OK"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		)),
	}

	if err := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}); err != nil {
		t.Fatalf("handle forced non-stream aggregation: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 aggregated response, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"object":"response"`) {
		t.Fatalf("expected aggregated responses JSON body, got %q", body)
	}
}

// TestForcedResponsesNonStreamFirstTokenTimeout guards the non-stream aggregate
// path's first-token cutoff. The production hang was a ping-only upstream that
// never sends a meaningful token: every prelude/ping resets the idle timer, so
// the idle cutoff never fires and the request stalls until the client's own
// deadline. The first-token guard must catch it even while the idle timer keeps
// being reset.
func TestForcedResponsesNonStreamFirstTokenTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	// Idle cutoff stays generous and is reset by every prelude below, exactly like
	// a ping-only upstream, so only the first-token guard can end this stall.
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30"); err != nil {
		t.Fatalf("set stream data timeout setting: %v", err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:            c,
			inboundType:  inbound.InboundTypeOpenAIResponse,
			inAdapter:    &openaiInbound.ResponseInbound{},
			requestModel: "gpt-5.5",
		},
		firstTokenTimeOutSec: 1,
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	// Emit a prelude-only event repeatedly (never a meaningful token) so the idle
	// timer is reset on every tick; the first-token guard must still fire.
	stop := make(chan struct{})
	go func() {
		prelude := `data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}` + "\n\n"
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := io.WriteString(pw, prelude); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	startedAt := time.Now()
	err := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{})
	close(stop)
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("expected first-token timeout to cut the stall quickly, elapsed %s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "first token timeout") {
		t.Fatalf("expected first token timeout error, got %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("forced non-stream aggregation should not write downstream data, got %q", body)
	}
}

// TestForcedResponsesNonStreamKeepaliveThenAggregates verifies the blank-line keepalive:
// while a slow upstream buffers (reasoning), the aggregate path must flush "\n" heartbeats
// to keep the non-stream client alive, then still emit one valid JSON body. The leading
// newlines are JSON insignificant whitespace, so the whole response stays parseable.
func TestForcedResponsesNonStreamKeepaliveThenAggregates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "1"); err != nil {
		t.Fatalf("set keepalive setting: %v", err)
	}
	// Generous idle cutoff so only the keepalive (not the data timeout) acts here.
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30"); err != nil {
		t.Fatalf("set stream data timeout setting: %v", err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{relayRequest: &relayRequest{
		c:            c,
		inboundType:  inbound.InboundTypeOpenAIResponse,
		inAdapter:    &openaiInbound.ResponseInbound{},
		requestModel: "gpt-5.5",
	}}
	pr, pw := io.Pipe()
	defer pr.Close()
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	// Prelude arrives fast, then the upstream "reasons" silently past the 1s keepalive
	// before finally producing content — exactly the buffered-reasoning shape.
	go func() {
		_, _ = io.WriteString(pw, `data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}`+"\n\n")
		time.Sleep(1500 * time.Millisecond)
		_, _ = io.WriteString(pw, `data: {"type":"response.output_text.delta","delta":"OK"}`+"\n\n")
		_, _ = io.WriteString(pw, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}}}`+"\n\n")
		_, _ = io.WriteString(pw, `data: [DONE]`+"\n\n")
		_ = pw.Close()
	}()

	if err := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}); err != nil {
		t.Fatalf("handle forced non-stream aggregation with keepalive: %v", err)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "\n") {
		t.Fatalf("expected a leading blank-line keepalive before the JSON body, got %q", body[:min(20, len(body))])
	}
	if !strings.Contains(body, `"object":"response"`) {
		t.Fatalf("expected aggregated responses JSON body, got %q", body)
	}
	// The whole response (leading newlines + JSON) must still parse as valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("keepalive-prefixed body is not valid JSON: %v (body=%q)", err, body)
	}
}
