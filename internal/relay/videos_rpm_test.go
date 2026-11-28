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
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 视频创建成功计渠道 RPM；轮询不计（fix-1128 件2）。
func TestVideosCreateCountsChannelRPMPollDoesNot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)

	// 🔴 必须清掉本用例的日志，否则会污染同包其它测试（同 videos_billing_e2e_test.go）：
	// relayLogAddPending 把日志放进**进程级**待落库队列 relayLogCache，这个队列
	// 不随 db.InitDB 的临时库切换而重置；而 op.RelayLogList 会先读这个队列。
	// 本用例写一条 model="rpm-video-model" 的创建日志后，videos_test.go:228 那条
	// 「取全局前 10 条日志再找 create 记录」的用例就会抓到本用例的记录，在
	// videos_test.go:249 报 `expected request model alias-sora, got "rpm-video-model"`。
	// 单独跑各自都绿、一起跑必红。用生产已有的 RelayLogClear 收尾。
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/videos" {
			_, _ = w.Write([]byte(`{"id":"vid_rpm_task","status":"queued"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/videos/vid_rpm_task" {
			_, _ = w.Write([]byte(`{"id":"vid_rpm_task","status":"completed","video_url":"https://example.com/v.mp4"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-rpm-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "rpm-video-model",
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "rpm-secret-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	recCreate := httptest.NewRecorder()
	cCreate, _ := gin.CreateTestContext(recCreate)
	reqCreate := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"rpm-video-model","prompt":"generate a sunset video"}`))
	reqCreate.Header.Set("Content-Type", "application/json")
	cCreate.Request = reqCreate
	cCreate.Set("api_key_id", 0)
	cCreate.Set("user_id", videoTestOwnerUserID)
	cCreate.Set("request_ip", "127.0.0.1")

	VideosHandler(cCreate)
	if recCreate.Code != http.StatusOK {
		t.Fatalf("expected video create to succeed, got %d body %s", recCreate.Code, recCreate.Body.String())
	}
	if got := balancer.ChannelGateCurrentRPM(ch.ID); got != 1 {
		t.Fatalf("channel RPM after create = %d, want 1", got)
	}

	// 轮询不计渠道 RPM。
	recPoll := httptest.NewRecorder()
	cPoll, _ := gin.CreateTestContext(recPoll)
	cPoll.Params = []gin.Param{{Key: "id", Value: "vid_rpm_task"}}
	reqPoll := httptest.NewRequest(http.MethodGet, "/v1/videos/vid_rpm_task", nil)
	cPoll.Request = reqPoll
	cPoll.Set("api_key_id", 0)
	cPoll.Set("user_id", videoTestOwnerUserID)
	cPoll.Set("request_ip", "127.0.0.1")

	VideoPollHandler(cPoll)
	if recPoll.Code != http.StatusOK {
		t.Fatalf("expected video poll to succeed, got %d body %s", recPoll.Code, recPoll.Body.String())
	}
	if got := balancer.ChannelGateCurrentRPM(ch.ID); got != 1 {
		t.Fatalf("channel RPM after poll = %d, want 1 (poll must not count)", got)
	}
}

// 视频创建走「入站 body len/4」的 TPM 估算输入：非 chat 协议不能再按 0 进闸。
func TestVideosCreateCountsEstimatedInputTPM(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vid_tpm_task","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)

	ch := dbmodel.Channel{
		Name:     "video-tpm-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "tpm-video-model",
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "tpm-secret-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	body := `{"model":"tpm-video-model","prompt":"generate a sunset video"}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", videoTestOwnerUserID)
	c.Set("request_ip", "127.0.0.1")

	VideosHandler(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected video create to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	want := int64(len(body)) / 4
	if got := balancer.ChannelGateCurrentTPM(ch.ID); got != want {
		t.Fatalf("channel TPM after create = %d, want estimated input %d", got, want)
	}
}
