package task

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// 超时扫描不能把整轮预算吃光。
//
// 场景：一批"上游连得上但对端永不返回"的挂起任务。它们同时满足
// 「已超时」和「问上游时吊住」，是把这个轮询器锁死的最短路径：
//  1. 扫描逐条问上游，每条都被吊住（单条 20s 封顶）
//  2. 6 条就吃光 120s 整轮预算
//  3. op.VideoTaskListUnfinished 拿到过期 ctx → deadline exceeded → 本轮直接返回
//  4. 下一轮 Order("id") 还是从那批最老的开始 → 自锁，新任务永远排不上
//
// 修复后扫描有独立子预算（videoTaskPollSweepBudget=40s），主轮询始终拿得到
// 剩下的时间，新任务一定能推进。
func TestVideoTaskPollSweepCannotStarveMainRound(t *testing.T) {
	ctx := setupVideoPollTestDB(t)

	// 把一个健康渠道和一条正常任务放进库：扫描把预算烧光后，它必须仍被推进。
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)
	channel := createVideoPollTestChannel(t, ctx, upstream.URL)

	// 挂起上游：收到请求后一直不返回，直到客户端放弃。
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hang.Close)
	hangChannel := createVideoPollTestChannelNamed(t, ctx, "video-poll-hang", hang.URL)

	// 5 条已超时的挂起任务（id 更小 → 排在队头）。
	for i := 0; i < 5; i++ {
		createVideoPollTestTask(t, ctx, model.VideoTask{
			UpstreamTaskID: "hang-" + string(rune('a'+i)),
			ChannelID:      hangChannel.ID,
			ChannelKeyID:   hangChannel.Keys[0].ID,
			Status:         model.VideoTaskInProgress,
			SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
		})
	}
	// 一条健康的新任务（id 更大 → 排在后面）。
	stub.set("cgt-healthy", http.StatusOK, `{"status":"succeeded","content":{"video_url":"https://cdn.example/v.mp4"}}`)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-healthy",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskInProgress,
		SubmitTime:     time.Now().Unix(),
	})

	// 压小时间刻度，让"挂起任务吃光预算"在毫秒级复现：
	// 整轮 400ms、扫描预算 100ms、单条 80ms。
	// 5 条挂起任务 × 80ms = 400ms > 整轮预算 → 若扫描没有独立预算，整轮必被吃光。
	oldRound, oldSweep, oldTask := videoTaskPollRoundTimeout, videoTaskPollSweepBudget, videoTaskPollTaskTimeout
	videoTaskPollRoundTimeout = 400 * time.Millisecond
	videoTaskPollSweepBudget = 100 * time.Millisecond
	videoTaskPollTaskTimeout = 80 * time.Millisecond
	t.Cleanup(func() {
		videoTaskPollRoundTimeout, videoTaskPollSweepBudget, videoTaskPollTaskTimeout = oldRound, oldSweep, oldTask
	})

	// 主轮询实际推进健康任务。
	runVideoTaskPollOnce()
	healthy := loadVideoPollTestTask(t, ctx, "cgt-healthy")
	if healthy.Status != model.VideoTaskSuccess {
		t.Fatalf("队头挂起任务饿死了健康任务：cgt-healthy 状态 = %q, want %q",
			healthy.Status, model.VideoTaskSuccess)
	}
}
