package relay

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
)

// 按次计费的 raw 端点没有 token，但确实扣了钱：日志的 Cost 必须与扣费一致，
// 不能因为"没有 token"就显示 0（改动前 saveLog 只在 token>0 时写 Cost）。
func TestRawPerCallCostIsShownInRelayLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("mp3"))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "grok-tts", "grok-tts", "k")
	price.SetLLMPrice("grok-tts", dbmodel.LLMPrice{PerCall: 0.25})
	t.Cleanup(func() { price.SetLLMPrice("grok-tts", dbmodel.LLMPrice{}) })

	c, rec := newGrokVoiceContext(http.MethodPost, "/v1/tts", strings.NewReader(`{"text":"hi","language":"en"}`), "application/json", 0)
	RawProtocolHandler(grokTTSOptions(), c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil || len(logs) == 0 {
		t.Fatalf("relay log missing: %v", err)
	}
	if math.Abs(logs[0].Cost-0.25) > 1e-9 {
		t.Fatalf("log cost %f, want the per_call charge 0.25", logs[0].Cost)
	}
	if logs[0].InputTokens != 0 || logs[0].OutputTokens != 0 {
		t.Fatalf("no tokens should be invented: in=%d out=%d", logs[0].InputTokens, logs[0].OutputTokens)
	}
}
