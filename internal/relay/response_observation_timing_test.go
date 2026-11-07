package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	openaiInbound "github.com/zhizhishu/octopus/internal/transformer/inbound/openai"
	m "github.com/zhizhishu/octopus/internal/transformer/model"
	chatOut "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

// 首字时间（FirstTokenTime）的归属回归：它是「这一份回复」的观测，而存储是请求级的
// （metrics.Save 把它当 ftut 落库、日志卡片据此算生成速度）。失败那一次盖过章之后，
// 胜出的这一份回复必须能自己盖章，否则 ftut 来自 A、output_tokens 来自 B，
// 算出来的“生成窗口”横跨两次尝试。这条回归只保证不同响应的观测不会被混在一起，
// 不主张它解释过任何线上现象（线上实测到的是阈值把值挡掉，不是首字时间串了的证据）。
//
// 三份断言覆盖三条真实入口：
//  1. 非流式客户端聚合 SSE（handleStreamResponseAsNonStream）：旧章必须被新章顶掉；
//  2. 纯非流式缓冲 JSON（handleResponse）：不产生首字，落库必须是 0（前端显示「TPS —」）；
//  3. 正常流式（handleStreamResponse）：照常盖章，修不能把这条路径改坏。
func newObservationTestAttempt(t *testing.T, stream bool) (*relayAttempt, *httptest.ResponseRecorder, *RelayMetrics) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	internalRequest := &m.InternalLLMRequest{Model: "test-model"}
	if stream {
		streamOn := true
		internalRequest.Stream = &streamOn
	}
	metrics := NewRelayMetrics(1, 1, "127.0.0.1", "test-model", internalRequest)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeOpenAIChat,
			inAdapter:       &openaiInbound.ChatInbound{},
			internalRequest: internalRequest,
			metrics:         metrics,
			requestModel:    "test-model",
		},
	}
	return ra, rec, metrics
}

// TestAggregatedSSEWinnerReplacesStaleFirstToken 覆盖故障转移后的残留：
// 上一条流已盖过章（失败那次），本条聚合回复的 IsZero 守卫本会拒绝盖章。
func TestAggregatedSSEWinnerReplacesStaleFirstToken(t *testing.T) {
	ra, rec, metrics := newObservationTestAttempt(t, false)
	stale := time.Now().Add(-3 * time.Second)
	metrics.SetFirstTokenTime(stale)

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(selfHealSSEChatBody)),
	}
	if err := ra.handleStreamResponseAsNonStream(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{}, 0); err != nil {
		t.Fatalf("aggregated SSE handling failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 aggregated response, got %d", rec.Code)
	}
	if metrics.FirstTokenTime.IsZero() {
		t.Fatal("FirstTokenTime = zero, want this response's own stamp (stale one must not survive as a placeholder)")
	}
	if !metrics.FirstTokenTime.After(stale) {
		t.Fatalf("FirstTokenTime = %v, want replaced (stale %v must not be inherited)", metrics.FirstTokenTime, stale)
	}
}

// TestBufferedJSONWinnerClearsStaleFirstToken 覆盖无监测窗口的胜出路径：
// 缓冲 JSON 没有可观测的首字，落库 ftut 必须是 0——绝不能沿用失败那次的时间戳。
func TestBufferedJSONWinnerClearsStaleFirstToken(t *testing.T) {
	const bufferedJSON = `{"id":"chatcmpl-json","object":"chat.completion","created":1,"model":"test-model",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Hi there"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`
	ra, rec, metrics := newObservationTestAttempt(t, false)
	metrics.SetFirstTokenTime(time.Now().Add(-3 * time.Second))

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(bufferedJSON)),
	}
	if err := ra.handleResponse(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{}); err != nil {
		t.Fatalf("buffered JSON handling failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 JSON response, got %d", rec.Code)
	}
	if !metrics.FirstTokenTime.IsZero() {
		t.Fatalf("FirstTokenTime = %v, want zero: a buffered non-stream reply has no first token, "+
			"carrying the previous attempt's stamp fabricates a generation window", metrics.FirstTokenTime)
	}
}

// TestStreamResponseStillStampsFirstToken 保证修法没把正常流式改坏（盖章仍在首内容处）。
func TestStreamResponseStillStampsFirstToken(t *testing.T) {
	ra, rec, metrics := newObservationTestAttempt(t, true)
	stale := time.Now().Add(-3 * time.Second)
	metrics.SetFirstTokenTime(stale)

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(selfHealSSEChatBody)),
	}
	if err := ra.handleStreamResponse(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{}); err != nil {
		t.Fatalf("normal streaming failed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "Hello") {
		t.Fatalf("stream content lost; wire=%q", rec.Body.String())
	}
	if metrics.FirstTokenTime.IsZero() {
		t.Fatal("FirstTokenTime = zero, want a stamp at the first content chunk")
	}
	if !metrics.FirstTokenTime.After(stale) {
		t.Fatalf("FirstTokenTime = %v, want this stream's own stamp (stale %v kept)", metrics.FirstTokenTime, stale)
	}
}
