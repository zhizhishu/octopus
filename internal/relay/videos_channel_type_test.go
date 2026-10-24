package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 这些用例钉住的是「视频创建用哪张表」，不是表本身（表本身在
// channel_type_capability_matrix_test.go 里逐格钉）。
//
// 为什么要走真实 handler 而不是只测函数：`isVideoCapableChannelType` 判断对了
// 但调用点忘了改，单测照样全绿 —— 而生产缺陷正是调用点用错表。

func newVideoChannelTypeTestChannel(t *testing.T, ctx context.Context, name string, typ outbound.OutboundType, upstreamURL string, model string) {
	t.Helper()
	ch := dbmodel.Channel{
		Name:     name,
		Type:     typ,
		Enabled:  true,
		Model:    model,
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "video-channel-type-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
}

func postVideoCreate(t *testing.T, modelName string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"`+modelName+`","prompt":"a sunset"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	VideosHandler(c)
	return rec
}

// 嵌入渠道（type=OpenAIEmbedding）发起视频创建：必须被跳过，且**一次上游请求都不许发**。
//
// 嵌入渠道是算向量的账号，生成视频无意义。放它进去只会把"配置错了"推迟成上游 502，
// 用户看到一个说不清原因的失败。这条用例就是那个"配置错了要当场说清楚"的守门人。
func TestVideoCreateSkipsEmbeddingChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_embed_1","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	const modelName = "embedding-video-model"
	newVideoChannelTypeTestChannel(t, ctx, "video-embedding-channel", outbound.OutboundTypeOpenAIEmbedding, upstream.URL, modelName)

	rec := postVideoCreate(t, modelName)

	if rec.Code == http.StatusOK {
		t.Fatalf("嵌入渠道创建视频居然成功了：%s", rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("嵌入渠道被跳过时仍然打了上游 %d 次（跳过必须发生在转发之前）", got)
	}

	// 跳过原因在**路由尝试记录**里，不在 HTTP 响应体里：客户端只看得到
	// 「no available channel」这一层脱敏后的 503（upstream_error.go 故意不把内部
	// 选路细节透给客户端）。所以"错误信息含 unsupported channel type for video"
	// 这条要断在日志的 attempts 上——那正是运维排障时看的那一行。
	logs, err := op.RelayLogList(context.Background(), nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("期望恰好 1 条视频创建日志，实际 %d 条", len(logs))
	}
	found := false
	for _, attempt := range logs[0].Attempts {
		if strings.Contains(attempt.Msg, "unsupported channel type for video") {
			found = true
		}
	}
	if !found {
		t.Fatalf("跳过原因里没有 \"unsupported channel type for video\"，实际 attempts=%+v", logs[0].Attempts)
	}
}

// 正面对照：同一个入口，OpenAIChat 渠道必须照旧能创建视频并真的打到上游。
//
// 没有这条，上面那条只需要把所有渠道都拒掉就能变绿 —— 那等于把视频功能整体砍了。
func TestVideoCreateStillForwardsForChatChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_chat_1","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	const modelName = "chat-video-model"
	newVideoChannelTypeTestChannel(t, ctx, "video-chat-channel", outbound.OutboundTypeOpenAIChat, upstream.URL, modelName)

	rec := postVideoCreate(t, modelName)

	if rec.Code != http.StatusOK {
		t.Fatalf("chat 渠道创建视频失败：%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("上游命中次数 = %d, want 1", got)
	}
}

// 反向守门：把 Embedding 从视频表里摘掉，**不许顺手摘掉它在 raw 协议里的能力**。
//
// isOpenAIWireChannelType 同时服务音频 speech/transcriptions/translations、
// completions、moderations、rerank、responses_compact。若有人图省事直接从那张表
// 删掉 Embedding，嵌入渠道这些端点会一起失效 —— 那才是扩大伤害。
func TestEmbeddingChannelKeepsRawProtocolAbility(t *testing.T) {
	if !isOpenAIWireChannelType(outbound.OutboundTypeOpenAIEmbedding) {
		t.Fatal("isOpenAIWireChannelType 必须仍然收 Embedding：它还服务音频/completions/moderations/rerank/responses_compact")
	}
	if isVideoCapableChannelType(outbound.OutboundTypeOpenAIEmbedding) {
		t.Fatal("isVideoCapableChannelType 必须拒 Embedding")
	}

	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	const modelName = "embedding-raw-model"
	newVideoChannelTypeTestChannel(t, ctx, "raw-embedding-channel", outbound.OutboundTypeOpenAIEmbedding, upstream.URL, modelName)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"model":"`+modelName+`","query":"hello","documents":["world"]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/rerank", Name: "rerank"}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("嵌入渠道的 raw rerank 被拒了（这次拆分不许砍它的 raw 能力）：%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("上游命中次数 = %d, want 1", got)
	}
}
