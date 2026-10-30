package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 视频入口的日志也要带调用端 User-Agent（用户页「客户端」靠它）。只读入站请求头，
// 走的是不会打上游的路径（嵌入渠道被跳过），所以这里不涉及任何出站请求。
func TestVideoCreateLogsInboundUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called")
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })
	const modelName = "embedding-video-ua-model"
	newVideoChannelTypeTestChannel(t, ctx, "video-ua-embedding-channel", outbound.OutboundTypeOpenAIEmbedding, upstream.URL, modelName)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"`+modelName+`","prompt":"a sunset"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CherryStudio/1.5.0")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	VideosHandler(c)

	logs, err := op.RelayLogList(context.Background(), nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 || logs[0].RequestUserAgent != "CherryStudio/1.5.0" || logs[0].RequestIP != "127.0.0.1" {
		t.Fatalf("video log must carry inbound ip/ua, got %+v", logs)
	}
}
