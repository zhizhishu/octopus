package task

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 队头阻塞：上游永报非终态的老行不许把新任务挤出批次窗口。
//
// 场景是与真实故障同形的：150 条「上游一直说 running」的老行（id 小、3 小时前提交
// → 既在未完成集合里、又在超时扫描的 stale 窗口里）+ 5 条刚提交的新行。
//
// 旧实现（op.VideoTaskListUnfinished 按 id 升序）的死锁链：
//  1. Limit(100) 取到的是最老的 100 条 —— 全是那 150 条卡死行；
//  2. 超时扫描这一轮刚问过它们（polled 集合里都有），
//     dropVideoTasksAlreadyPolledThisRound 把它们全剔掉 → 批次为空；
//  3. 本轮直接 return，id 更大的 5 条新行一条也进不了批次，
//     永久停在 submitted，前端拿不到结果。
//
// 修复后（按 updated_at 升序 + 问过即推进 updated_at）被问过的老行退到队尾，
// 新行必然进入本轮批次并被推进到终态。
func TestVideoTaskPollStuckTasksDoNotBlockNewOnes(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)
	channel := createVideoPollTestChannel(t, ctx, upstream.URL)

	// 150 条卡死老行：稳态就是「已经是 in_progress、进度也没变、上游永远回 running」，
	// 于是每轮问完上游一个字段都不用写。updated_at 也很旧（3 小时前）—— 这正是它们在
	// 旧实现里盘踞队头的原因，也让「问过就推进 updated_at」成为修复里承重的一环。
	const stuckCount = 150
	stuckUpdatedAt := time.Now().Add(-3 * time.Hour).Unix()
	for i := 0; i < stuckCount; i++ {
		id := fmt.Sprintf("stuck-%03d", i)
		stub.set(id, http.StatusOK, `{"status":"running","progress":"50%"}`)
		createVideoPollTestTask(t, ctx, model.VideoTask{
			UpstreamTaskID: id,
			ChannelID:      channel.ID,
			ChannelKeyID:   channel.Keys[0].ID,
			Status:         model.VideoTaskInProgress,
			Progress:       "50%",
			SubmitTime:     time.Now().Add(-3 * time.Hour).Unix(),
			UpdatedAt:      stuckUpdatedAt,
		})
	}

	// 5 条新行：id 更大、刚提交，上游会给终态。
	const freshCount = 5
	for i := 0; i < freshCount; i++ {
		id := fmt.Sprintf("fresh-%d", i)
		stub.set(id, http.StatusOK, `{"status":"succeeded","content":{"video_url":"https://cdn.example/f.mp4"}}`)
		createVideoPollTestTask(t, ctx, model.VideoTask{
			UpstreamTaskID: id,
			ChannelID:      channel.ID,
			ChannelKeyID:   channel.Keys[0].ID,
			Status:         model.VideoTaskSubmitted,
			SubmitTime:     time.Now().Unix(),
		})
	}

	runVideoTaskPollOnce()

	for i := 0; i < freshCount; i++ {
		id := fmt.Sprintf("fresh-%d", i)
		fresh := loadVideoPollTestTask(t, ctx, id)
		if fresh.Status != model.VideoTaskSuccess {
			t.Fatalf("队头阻塞：新任务 %s 状态 = %q, want %q（%d 条卡死老行把它挤出了批次窗口）",
				id, fresh.Status, model.VideoTaskSuccess, stuckCount)
		}
		// 新行这一轮只该被问一次：超时扫描问过的行会被剔掉，不许重复问。
		if got := stub.hitCount(id); got != 1 {
			t.Fatalf("新任务 %s 本轮被上游查询 %d 次, want 1", id, got)
		}
	}

	// 卡死行本身仍是非终态（没被这轮误判死；硬上限是 24h，它们只有 3h），
	// 且这一轮问过后 updated_at 被推进了——它们因此退到队尾，把窗口让给新任务。
	stuck := loadVideoPollTestTask(t, ctx, "stuck-000")
	if model.IsVideoTaskTerminal(stuck.Status) {
		t.Fatalf("卡死老行被提前判终态: %q (%s)", stuck.Status, stuck.FailReason)
	}
	if stuck.UpdatedAt <= stuckUpdatedAt {
		t.Fatalf("问过上游后 updated_at 没推进: %d <= %d（下一轮它又会盘踞队头）",
			stuck.UpdatedAt, stuckUpdatedAt)
	}
}

// 同一个队头阻塞的第二个触发条件：每轮都查失败的行。
//
// 上游回 5xx / 响应读不懂时 pollOneVideoTask 直接 return（下一轮再试），什么都没写。
// 如果这一条返回路径不推进 updated_at，这些行就永远站在队列最前面：每轮 100 个名额
// 全被它们占光，id 更大的新任务一样进不了批次。
//
// 注意这批行刻意只有 10 分钟龄：没到 2h 超时，所以超时扫描不碰它们（不会被判死收口），
// 只能靠排序公平性把它们排到队尾。
//
// 这里必须跑**两轮**：批次窗口是本轮开头一次性查出来的，而「问过就推进 updated_at」
// 发生在查完之后。所以公平性给的保证是「卡在队头的行下一轮被换出去」，而不是「本轮就
// 把剩下的人塞进来」（窗口已经定了，做不到）。真正坏的是旧实现下它们**永远**换不出去：
// 第二轮、第三轮……窗口永远是同一批老行，新任务永远停在 submitted。
func TestVideoTaskPollFailingUpstreamTasksDoNotBlockNewOnes(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)
	channel := createVideoPollTestChannel(t, ctx, upstream.URL)

	const brokenCount = 150
	brokenUpdatedAt := time.Now().Add(-10 * time.Minute).Unix()
	for i := 0; i < brokenCount; i++ {
		id := fmt.Sprintf("broken-%03d", i)
		stub.set(id, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
		createVideoPollTestTask(t, ctx, model.VideoTask{
			UpstreamTaskID: id,
			ChannelID:      channel.ID,
			ChannelKeyID:   channel.Keys[0].ID,
			Status:         model.VideoTaskSubmitted,
			SubmitTime:     brokenUpdatedAt,
			UpdatedAt:      brokenUpdatedAt,
		})
	}

	const freshCount = 5
	for i := 0; i < freshCount; i++ {
		id := fmt.Sprintf("errfresh-%d", i)
		stub.set(id, http.StatusOK, `{"status":"succeeded","content":{"video_url":"https://cdn.example/f.mp4"}}`)
		createVideoPollTestTask(t, ctx, model.VideoTask{
			UpstreamTaskID: id,
			ChannelID:      channel.ID,
			ChannelKeyID:   channel.Keys[0].ID,
			Status:         model.VideoTaskSubmitted,
			SubmitTime:     time.Now().Unix(),
		})
	}

	// 第一轮：窗口被那 150 条失败行占满（它们最久没被问过），新行还没轮上。
	runVideoTaskPollOnce()
	// 第二轮：它们上一轮已经问过 → updated_at 被推进 → 退到队尾，新行进入批次。
	runVideoTaskPollOnce()

	for i := 0; i < freshCount; i++ {
		id := fmt.Sprintf("errfresh-%d", i)
		if fresh := loadVideoPollTestTask(t, ctx, id); fresh.Status != model.VideoTaskSuccess {
			t.Fatalf("队头阻塞：新任务 %s 状态 = %q, want %q（%d 条每轮都失败的行占满了批次窗口）",
				id, fresh.Status, model.VideoTaskSuccess, brokenCount)
		}
	}

	// 失败行本身仍是未完成（没到超时），但这一轮问过后必须退到队尾。
	broken := loadVideoPollTestTask(t, ctx, "broken-000")
	if model.IsVideoTaskTerminal(broken.Status) {
		t.Fatalf("10 分钟龄的失败行被提前判终态: %q (%s)", broken.Status, broken.FailReason)
	}
	if broken.UpdatedAt <= brokenUpdatedAt {
		t.Fatalf("查询失败后 updated_at 没推进: %d <= %d（下一轮它又占满队头）",
			broken.UpdatedAt, brokenUpdatedAt)
	}
}

// 兜底终态：上游一直报非终态、且已远超本地超时的行必须有硬上限。
//
// 没有硬上限时 videoTaskTimeoutResolvedUpstream 对这类行永远返回 true
// （「上游还在干活，本地判超时是错的」），行就永久留在未完成集合里，每轮白烧一次
// 上游查询和一份批次窗口。超过 videoTaskPollHardTimeout 后不再问上游，直接判失败。
func TestVideoTaskPollHardCapsEndlesslyRunningTask(t *testing.T) {
	ctx := setupVideoPollTestDB(t)
	stub := newUpstreamStub()
	upstream := httptest.NewServer(stub.handler())
	t.Cleanup(upstream.Close)
	channel := createVideoPollTestChannel(t, ctx, upstream.URL)

	// 26 小时前提交，上游仍报 running —— 远超本地 2h 超时、也超过 24h 硬上限。
	const upstreamID = "cgt-forever-running"
	stub.set(upstreamID, http.StatusOK, `{"status":"running"}`)
	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: upstreamID,
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskInProgress,
		SubmitTime:     time.Now().Add(-26 * time.Hour).Unix(),
	})

	runVideoTaskPollOnce()

	stored := loadVideoPollTestTask(t, ctx, upstreamID)
	if stored.Status != model.VideoTaskFailure {
		t.Fatalf("超过硬上限仍在跑的任务状态 = %q, want %q（它会永久占着未完成集合）",
			stored.Status, model.VideoTaskFailure)
	}
	if !strings.Contains(stored.FailReason, "abandoned") {
		t.Fatalf("硬上限的失败原因不对: %q", stored.FailReason)
	}
	if stored.FinishTime == 0 {
		t.Fatalf("硬上限判死没有写 finish_time")
	}
	// 判死前不再问上游：问多少次都只会得到同一句「还在跑」。
	if got := stub.hitCount(upstreamID); got != 0 {
		t.Fatalf("超过硬上限仍向上游查询了 %d 次, want 0", got)
	}
	unfinished, err := op.VideoTaskListUnfinished(ctx, 10)
	if err != nil {
		t.Fatalf("list unfinished: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("硬上限判死的行仍在未完成集合里: %+v", unfinished)
	}

	// 反方向：还没到硬上限（26h 关掉、只超本地 2h）时不许判死，旧语义必须保住。
	oldHard := videoTaskPollHardTimeout
	videoTaskPollHardTimeout = 200 * time.Hour
	t.Cleanup(func() { videoTaskPollHardTimeout = oldHard })

	createVideoPollTestTask(t, ctx, model.VideoTask{
		UpstreamTaskID: "cgt-still-running",
		ChannelID:      channel.ID,
		ChannelKeyID:   channel.Keys[0].ID,
		Status:         model.VideoTaskInProgress,
		SubmitTime:     time.Now().Add(-26 * time.Hour).Unix(),
	})
	stub.set("cgt-still-running", http.StatusOK, `{"status":"running"}`)
	runVideoTaskPollOnce()
	if alive := loadVideoPollTestTask(t, ctx, "cgt-still-running"); alive.Status == model.VideoTaskFailure {
		t.Fatalf("未到硬上限的 running 任务被判死: %q", alive.FailReason)
	}
}
