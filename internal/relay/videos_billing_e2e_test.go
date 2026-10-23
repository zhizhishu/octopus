package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 端到端：一次成功的 /v1/videos 必须真的从用户余额里扣掉 per_call 那笔钱。
//
// 为什么必须有这条：辅助函数的单元测试只能证明"算得对"，无法证明**调用点存在**。
// 而原始缺陷恰恰是「videos.go 从不调用任何计费」——把调用删掉，单元测试依然全绿。
// 这条测试走真实 handler → upstream → metrics.Save → op.UserRecordUsage 全链路，
// 断言余额变化，删掉调用点它就红。
func TestVideosHandlerChargesUserBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_bill_1","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)

	const modelName = "billing-video-model"
	ch := dbmodel.Channel{
		Name:     "video-billing-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    modelName,
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "billing-secret"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// 配置按次单价。
	const perCall = 0.25
	price.SetLLMPrice(modelName, dbmodel.LLMPrice{PerCall: perCall})
	t.Cleanup(func() { price.SetLLMPrice(modelName, dbmodel.LLMPrice{}) })

	// 造一个有钱的真实用户，让 UserRecordUsage 真的扣。
	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "video-billing-user",
		Password: "test-password-123",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
		Balance:  10,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// 🔴 必须清掉本用例的日志，否则会污染同包其它测试。
	//
	// relayLogAddPending 把日志放进**进程级**待落库队列 relayLogCache，这个队列
	// 不随 db.InitDB 的临时库切换而重置；而 op.RelayLogList 会先读这个队列。
	// 本用例写一条 endpoint="videos" 的日志后，videos_test.go:228 那条
	// "取全局前 10 条日志再找 create 记录" 的用例就会抓到**本用例的记录**，
	// 报 `expected request model alias-sora, got "billing-video-model"`。
	// 单独跑各自都绿、一起跑必红 —— 实测确认。用生产已有的 RelayLogClear 收尾。
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	before := user.Balance

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(`{"model":"`+modelName+`","prompt":"a sunset"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", user.ID)
	c.Set("request_ip", "127.0.0.1")

	VideosHandler(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("video create failed: %d body=%s", rec.Code, rec.Body.String())
	}

	after, err := op.UserGet(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	charged := before - after.Balance
	if charged <= 0 {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		t.Fatalf("视频创建成功但用户余额没有变化（before=%v after=%v）："+
			"最贵的端点完全免费，持任意 key 可无限刷", before, after.Balance)
	}
	if want := perCall; charged != want {
		t.Fatalf("扣费 = %v, want %v（per_call 原值，倍率 1）", charged, want)
	}
}
