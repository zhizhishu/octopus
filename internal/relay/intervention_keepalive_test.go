package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/gin-gonic/gin"
)

// TestStartInterventionKeepaliveFiresShortDelay verifies the hold-loop keepalive uses
// the dedicated short intervention delay (not the 20s first-byte delay): after 1s it
// commits the SSE headers and writes an ignorable ":\n\n" heartbeat.
func TestStartInterventionKeepaliveFiresShortDelay(t *testing.T) {
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyInterventionKeepaliveDelaySeconds, "1"); err != nil {
		t.Fatalf("set intervention delay: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "1"); err != nil {
		t.Fatalf("set interval: %v", err)
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	stop := startInterventionKeepalive(context.Background(), c)
	time.Sleep(1300 * time.Millisecond)
	stop()

	if body := rec.Body.String(); !strings.Contains(body, ":\n\n") {
		t.Fatalf("expected injected SSE comment keepalive within the short intervention delay, got %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream content-type committed before heartbeat, got %q", ct)
	}
}

// TestStartInterventionKeepaliveNoOpWhenDelayZero verifies intervention delay=0
// disables the hold heartbeats entirely (byte-identical to before).
func TestStartInterventionKeepaliveNoOpWhenDelayZero(t *testing.T) {
	t.Setenv("OCTOPUS_RELAY_INTERVENTION_KEEPALIVE_DELAY_SECONDS", "0")

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	stop := startInterventionKeepalive(context.Background(), c)
	stop()

	if rec.Body.Len() != 0 {
		t.Fatalf("expected no bytes when intervention delay=0, got %d: %q", rec.Body.Len(), rec.Body.String())
	}
}

// TestInterventionHoldEmitsDownstreamHeartbeats proves the production bug this fix
// closes: during an intervention hold (all upstream attempts failing, machine rescue
// retrying at its fixed 1s cadence) the downstream client must actually receive SSE
// comment heartbeats. Before the fix, the hold loop stopped and restarted the shared
// 20s-delay keepalive every round, so its delay timer never fired and the held client
// sat byte-silent until the rescue budget died — racing client stream-idle timeouts
// (codex TUI ~300s). The keepalive is stopped BEFORE each round's attempt (to keep a
// single writer on gin.Writer), so its delay must fire inside the backoff wait — the
// no-breaker hold clamps the delay to half a round, which makes one heartbeat land
// per round deterministically instead of racing the round boundary.
func TestInterventionHoldEmitsDownstreamHeartbeats(t *testing.T) {
	setupRescueDeadlineDB(t)
	// setupRescueDeadlineDB disables the stream keepalive interval; re-enable both the
	// interval and the dedicated intervention delay so hold heartbeats can fire.
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "1"); err != nil {
		t.Fatalf("set interval: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyInterventionKeepaliveDelaySeconds, "1"); err != nil {
		t.Fatalf("set intervention delay: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "6"); err != nil {
		t.Fatalf("set budget: %v", err)
	}

	// Upstream 500s after a short delay. The keepalive is stopped before each attempt,
	// so the beat must land inside the ~1s backoff wait: the hold clamps the 1s delay
	// to half a round, giving the timer a ~500ms margin instead of a DB-query sliver
	// (the unclamped 1s/1s pairing raced the round boundary and flaked under load).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	newRescueChannel(t, upstream.URL)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inbound.InboundTypeOpenAIChat, c)
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	balancer.ResetRuntimeTelemetry()

	body := `{"model":"request-model","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	// Read the streamed body for up to 5s and count heartbeats. The rescue budget is
	// 6s; heartbeats must start flowing long before it dies.
	type chunk struct {
		data []byte
		err  error
	}
	chunks := make(chan chunk, 16)
	go func() {
		b := make([]byte, 512)
		for {
			n, err := resp.Body.Read(b)
			if n > 0 {
				c := make([]byte, n)
				copy(c, b[:n])
				chunks <- chunk{data: c}
			}
			if err != nil {
				chunks <- chunk{err: err}
				return
			}
		}
	}()
	// 新契约(大领导 18:38 定的): 救援期间**不许**发预内容心跳。心跳会把响应提交成 HTTP 200,
	// 提交之后"全渠道失败"就只能写成带内错误帧 —— 调用方看到 200 + 空内容, 比报错更糟。
	// 心跳让位给真错误码: 请求保持未提交, 收尾时如实回上游状态。这条测试的上游一直失败,
	// 所以调用方必须拿到真错误码, 且正文里不许出现任何心跳注释。
	var received []byte
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-chunks:
			if c.data != nil {
				received = append(received, c.data...)
			}
			if c.err != nil {
				if n := strings.Count(string(received), ":\n\n"); n != 0 {
					t.Fatalf("a failing-over request must not send pre-content heartbeats (got %d) — they commit HTTP 200 and turn "+
						"the failure into a fake success; received=%q", n, received)
				}
				if resp.StatusCode == http.StatusOK {
					t.Fatalf("the caller got HTTP 200 for a request that never got content: body=%q", received)
				}
				return
			}
		case <-deadline:
			t.Fatalf("the failing request neither finished nor failed within 5s: received=%q", received)
		}
	}
}
