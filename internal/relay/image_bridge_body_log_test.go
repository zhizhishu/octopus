package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 出站 body 快照只覆盖标准 relay、竞速、以及这条 chat→images 桥。
// Gemini 图片桥在同一函数里提前返回，不许被顺手采进去。
func TestChatImageBridgeSnapshotsUpstreamBodyWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.SettingSetInt(dbmodel.SettingKeyUpstreamRequestBodyLog, 1); err != nil {
		t.Fatalf("enable body log: %v", err)
	}

	const secret = "sk-live-bridge-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"Y2hhdC1pbWc="}]}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gpt-image-2", "gpt-image-2", outbound.OutboundTypeOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"Draw `+secret+` please"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("bridge failed: %d %s", rec.Code, rec.Body.String())
	}

	body := latestUpstreamRequestBody(t, ctx)
	if !strings.Contains(body, "gpt-image-2") || !strings.Contains(body, "Draw ") || !strings.Contains(body, secret) {
		t.Fatalf("chat->images snapshot must store the body as sent, got %s", body)
	}
	if strings.Contains(body, "[redacted]") {
		t.Fatalf("chat->images snapshot must not mask credentials, got %s", body)
	}
}

func TestChatImageBridgeOmitsUpstreamBodyWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"Y2hhdC1pbWc="}]}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gpt-image-2", "gpt-image-2", outbound.OutboundTypeOpenAIChat)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-image-2",
		"messages":[{"role":"user","content":"Create a pixel art crab"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("bridge failed: %d %s", rec.Code, rec.Body.String())
	}
	if body := latestUpstreamRequestBody(t, ctx); body != "" {
		t.Fatalf("disabled setting must not store an upstream body, got %s", body)
	}
}

func TestGeminiImageBridgeDoesNotSnapshotUpstreamBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.SettingSetInt(dbmodel.SettingKeyUpstreamRequestBodyLog, 1); err != nil {
		t.Fatalf("enable body log: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates":[{
				"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"Z2VtaW5pLWltZw=="}}]},
				"finishReason":"STOP"
			}]
		}`))
	}))
	t.Cleanup(upstream.Close)
	createImageBridgeGroup(t, ctx, upstream.URL, "gemini-image-chat", "gemini-2.5-flash-image", outbound.OutboundTypeGemini)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gemini-image-chat",
		"messages":[{"role":"user","content":"Create a gemini image"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIChat, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("gemini bridge failed: %d %s", rec.Code, rec.Body.String())
	}
	if body := latestUpstreamRequestBody(t, ctx); body != "" {
		t.Fatalf("gemini image bridge must not capture an upstream body, got %s", body)
	}
}

func latestUpstreamRequestBody(t *testing.T, ctx context.Context) string {
	t.Helper()
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 5, nil)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected a relay log")
	}
	full, err := op.RelayLogGetByID(ctx, logs[0].ID, nil)
	if err != nil {
		t.Fatalf("get log %d: %v", logs[0].ID, err)
	}
	return full.UpstreamRequestBody
}
