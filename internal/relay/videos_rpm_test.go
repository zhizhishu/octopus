package relay

import (
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
