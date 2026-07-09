package relay

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

// fakeFirstTokenMetrics 满足 firstTokenMetrics 接口，仅记录首 token 时间。
type fakeFirstTokenMetrics struct {
	set bool
	at  time.Time
}

func (f *fakeFirstTokenMetrics) SetFirstTokenTime(t time.Time) {
	f.set = true
	f.at = t
}

// TestProxySSEIdleDataTimeoutCutsSilentUpstream 验证 raw/SSE 透传路径
// proxySSEWithOptions 在上游吐了首 token 后中途卡死时，会按上游空闲超时断流，
// 而不是无限挂起；并返回与主路径一致的 octopus_upstream_stream_timeout 语义错误。
func TestProxySSEIdleDataTimeoutCutsSilentUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "1"); err != nil {
		t.Fatalf("set stream data timeout setting: %v", err)
	}
	// 关掉 keepalive，专注验证空闲闸，避免心跳干扰。
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "0"); err != nil {
		t.Fatalf("disable keepalive setting: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	respUp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	// 先吐一个首事件（触发 firstToken 计时关闭），随后保持沉默：没有空闲闸
	// 时这会永久挂起。
	go func() {
		_, _ = io.WriteString(pw, "data: {\"type\":\"image_generation.partial\"}\n\n")
	}()

	metrics := &fakeFirstTokenMetrics{}
	startedAt := time.Now()
	_, written, _, err := proxySSEWithOptions(c.Request.Context(), c, respUp, 0, metrics, proxySSEOptions{})
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
	// 首事件应已透传到下游，证明这是“首 token 后卡死”而非首 token 超时。
	if !written {
		t.Fatalf("expected first event to be forwarded downstream before idle cutoff")
	}
	if !metrics.set {
		t.Fatalf("expected first token time to be recorded for the forwarded event")
	}
	if body := rec.Body.String(); !strings.Contains(body, "image_generation.partial") {
		t.Fatalf("expected forwarded first event in downstream body, got %q", body)
	}
}

// TestProxySSEKeepaliveInjectedWhenUpstreamIdle 验证当上游在首 token 后短暂安静、
// 但仍未超过空闲上限时，proxySSEWithOptions 会向下游注入合法的 SSE 注释心跳
// (":\n\n")，且心跳不污染最终透传的数据流。
func TestProxySSEKeepaliveInjectedWhenUpstreamIdle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	// 心跳间隔很短以便快速观察；空闲上限设较大，避免提前断流。
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "1"); err != nil {
		t.Fatalf("set keepalive setting: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "60"); err != nil {
		t.Fatalf("set data timeout setting: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	pr, pw := io.Pipe()
	defer pr.Close()

	respUp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	go func() {
		// 首事件。
		_, _ = io.WriteString(pw, "data: {\"type\":\"image_generation.partial\"}\n\n")
		// 安静一段时间（远大于 1s 心跳间隔），给 keepalive ticker 多次触发机会——
		// 旧值 1500ms 仅留 0.5s 余量，满负载(本地/CI)下 goroutine 调度一飘就错过、
		// 偶发 flaky。3.5s 让心跳有 ~3 次机会，即便调度延迟 1-2s 仍稳。
		time.Sleep(3500 * time.Millisecond)
		// 终止事件并关闭上游，正常结束。
		_, _ = io.WriteString(pw, "data: {\"type\":\"image_generation.completed\"}\n\n")
		_ = pw.Close()
	}()

	metrics := &fakeFirstTokenMetrics{}
	_, _, _, err := proxySSEWithOptions(c.Request.Context(), c, respUp, 0, metrics, proxySSEOptions{})
	if err != nil {
		t.Fatalf("expected clean completion, got %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, ":\n\n") {
		t.Fatalf("expected injected SSE comment keepalive in downstream body, got %q", body)
	}
	// 心跳不应破坏正常的事件透传。
	if !strings.Contains(body, "image_generation.partial") || !strings.Contains(body, "image_generation.completed") {
		t.Fatalf("expected both upstream events forwarded intact, got %q", body)
	}
}
