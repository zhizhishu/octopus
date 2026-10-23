package task

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// 【走真实调用路径·后台轮询】行上的 media_kind 必须一路传到发往上游的那条 URL 上。
//
// 为什么单独守这一条：relay.VideoTaskPollStatus 自己按 kind 拼路径是对的，但真正
// 决定「传什么 kind」的是本包 queryVideoTaskUpstream 的调用点。调用点漏传，edits
// 任务会在重启后的第一轮被上游 404 判死 —— 正是「测了 helper 没测调用点」的典型。
// 这条用例只走 runVideoTaskPollOnce，不直接调 relay，所以它抓的就是调用点。
func TestVideoTaskPollUsesRowMediaKind(t *testing.T) {
	ctx := setupVideoPollTestDB(t)

	var (
		mu   sync.Mutex
		path string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cgt-edit","status":"success","video_url":"https://upstream.example/out.mp4"}`))
	}))
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-edit",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Model:          "video-upstream-model",
		Status:         model.VideoTaskSubmitted,
		MediaKind:      "edits",
		SubmitTime:     time.Now().Unix(),
	})

	runVideoTaskPollOnce()

	mu.Lock()
	gotPath := path
	mu.Unlock()
	if gotPath != "/v1/videos/edits/cgt-edit" {
		t.Fatalf("background poll hit %q, want /v1/videos/edits/cgt-edit (the row's media_kind was not used)", gotPath)
	}
	// 反向对照：上游认了这条路径，任务因此真的被推进到终态。
	// 若 kind 被漏传，请求会打到 /v1/videos/cgt-edit（这个 stub 也回 200），
	// 但上面的路径断言已经把它抓住了；这里再确认结果确实落库。
	stored := loadVideoPollTestTask(t, ctx, "cgt-edit")
	if stored.Status != model.VideoTaskSuccess {
		t.Fatalf("status = %q, want %q", stored.Status, model.VideoTaskSuccess)
	}
	if stored.ResultURL != "https://upstream.example/out.mp4" {
		t.Fatalf("result url = %q", stored.ResultURL)
	}
}

// 存量行的 media_kind 是空串：后台轮询必须继续走 /v1/videos/{id} 的老路径。
// 这一条和上面那条互为对照 —— 少了它，「把空串也送去 /videos/edits」之类的
// 回归会让所有老任务在重启后失效。
func TestVideoTaskPollLegacyRowKeepsAliasPath(t *testing.T) {
	ctx := setupVideoPollTestDB(t)

	var (
		mu   sync.Mutex
		path string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cgt-legacy","status":"in_progress"}`))
	}))
	t.Cleanup(upstream.Close)

	channel := createVideoPollTestChannel(t, ctx, upstream.URL)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-legacy",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Model:          "video-upstream-model",
		Status:         model.VideoTaskQueued,
		SubmitTime:     time.Now().Unix(),
	})

	runVideoTaskPollOnce()

	mu.Lock()
	gotPath := path
	mu.Unlock()
	if gotPath != "/v1/videos/cgt-legacy" {
		t.Fatalf("legacy row polled %q, want /v1/videos/cgt-legacy", gotPath)
	}
}
