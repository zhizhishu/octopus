package modeltest

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

func newProbeReq(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://upstream.example/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// 观察器不得改动出站请求：Body 一个字节都不能被消费；同时它报告的必须是
// 最终出站值（参数覆盖后的结果）。
func TestObserverLeavesWireUnchangedReflectsFinalValues(t *testing.T) {
	body := `{"stream":true,"temperature":0.25,"max_tokens":321}`
	req := newProbeReq(t, body)

	var got numberdist.Conditions
	ctx := WithProbeConditionsObserver(context.Background(), func(c numberdist.Conditions) { got = c })
	observeFinalOutboundConditions(ctx, "openai-chat", req)

	after, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(after) != body {
		t.Fatalf("original wire changed: %q", string(after))
	}
	if got.Stream == nil || !*got.Stream {
		t.Fatalf("stream=%v want true", got.Stream)
	}
	if got.Temperature == nil || *got.Temperature != 0.25 {
		t.Fatalf("temperature=%v want 0.25", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 321 {
		t.Fatalf("max_tokens=%v want 321", got.MaxTokens)
	}

	// Gemini: thinkingLevel 嵌在 generationConfig.thinkingConfig 里，应映射为 effort=high。
	gem := newProbeReq(t, `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`)
	var gemCond numberdist.Conditions
	observeFinalOutboundConditions(
		WithProbeConditionsObserver(context.Background(), func(c numberdist.Conditions) { gemCond = c }),
		"gemini-generate-content", gem)
	if gemCond.ReasoningEffort != "high" {
		t.Fatalf("gemini reasoning_effort=%q want high", gemCond.ReasoningEffort)
	}
}

// 没有 GetBody 时：判为未知，且 Body 不被消费。
func TestObserverWithoutGetBodyUnknownAndUnconsumed(t *testing.T) {
	body := `{"stream":true}`
	req := newProbeReq(t, body)
	req.GetBody = nil

	var got numberdist.Conditions
	ctx := WithProbeConditionsObserver(context.Background(), func(c numberdist.Conditions) { got = c })
	observeFinalOutboundConditions(ctx, "openai-chat", req)

	if got.Completeness != "unknown" {
		t.Fatalf("completeness=%q want unknown", got.Completeness)
	}
	after, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(after) != body {
		t.Fatalf("body consumed: %q", string(after))
	}
}

// 没有观察者时：连 GetBody 都不碰，Body 也不读。
func TestNoObserverNoExtraRead(t *testing.T) {
	body := `{"stream":true}`
	req := newProbeReq(t, body)
	req.GetBody = func() (io.ReadCloser, error) {
		t.Fatal("no observer must not call GetBody")
		return nil, nil
	}

	observeFinalOutboundConditions(context.Background(), "openai-chat", req)

	after, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(after) != body {
		t.Fatalf("body consumed: %q", string(after))
	}
}
