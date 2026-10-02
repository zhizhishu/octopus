package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// S07 — no canvas rule: live traffic must follow the fleet-wide two-mode default.
// Auto-created field tests are not this: we send real chat requests and count hits.
func TestNoCanvasRuleFollowsGlobalFillFirstThenSpread(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayKeyRetryDB(t)
	const modelName = "no-canvas-live"
	if err := op.SettingSetString(dbmodel.SettingKeyRouteModeOverride, "fill_first"); err != nil {
		t.Fatal(err)
	}

	var leftCount int64
	var rightCount int64
	left := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&leftCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-left","object":"chat.completion","created":1,"model":"no-canvas-live","choices":[{"index":0,"message":{"role":"assistant","content":"left"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(left.Close)
	right := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&rightCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-right","object":"chat.completion","created":1,"model":"no-canvas-live","choices":[{"index":0,"message":{"role":"assistant","content":"right"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(right.Close)

	leftChannel := dbmodel.Channel{
		Name:     "no-canvas-left",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    modelName,
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: left.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "left-key"}},
	}
	if err := op.ChannelCreate(&leftChannel, ctx); err != nil {
		t.Fatal(err)
	}
	rightChannel := dbmodel.Channel{
		Name:     "no-canvas-right",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    modelName,
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: right.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "right-key"}},
	}
	if err := op.ChannelCreate(&rightChannel, ctx); err != nil {
		t.Fatal(err)
	}

	plan, err := op.AccessPlanSelect(0, "", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := op.AccessPlanGroupForModel(plan, modelName, ctx); err != nil || ok {
		t.Fatalf("S07 requires no canvas rule: ok=%v err=%v", ok, err)
	}

	send := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"no-canvas-live","messages":[{"role":"user","content":"ping"}]}`))
		req.Header.Set("Content-Type", "application/json")
		c.Request = req
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		Handler(inbound.InboundTypeOpenAIChat, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat status=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	for i := 0; i < 6; i++ {
		send()
	}
	if leftCount+rightCount != 6 {
		t.Fatalf("fill_first completed %d/6", leftCount+rightCount)
	}
	if leftCount != 0 && rightCount != 0 {
		t.Fatalf("no-canvas fill_first must concentrate, got left=%d right=%d", leftCount, rightCount)
	}

	if err := op.SettingSetString(dbmodel.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&leftCount, 0)
	atomic.StoreInt64(&rightCount, 0)
	for i := 0; i < 6; i++ {
		send()
	}
	if leftCount == 0 || rightCount == 0 {
		t.Fatalf("no-canvas spread must use both channels, got left=%d right=%d", leftCount, rightCount)
	}
}
