package task

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
)

// 异步生图计费（照 new-api 任务计费）：提交时按冻结的口径预扣/预留，完成时按上游用量
// 结算差额，失败/过期原路退回；日志里的费用就是最终收的那一笔，记日志本身不再扣钱。

// imgBillSnapshot 模拟提交时 ImagesAsyncSubmit 冻结到任务行上的计费快照。
func imgBillSnapshot(t *testing.T, input, output, multiplier float64) string {
	t.Helper()
	raw, err := json.Marshal(model.AccessPlanBillingSnapshot{
		BillingModelName:  "image-poll-test-model",
		DefaultMultiplier: multiplier,
		ModelMultiplier:   1,
		FinalMultiplier:   multiplier,
		BaseInputPrice:    input,
		BaseOutputPrice:   output,
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(raw)
}

// newImgBillTask 建一条带计费口径的任务：先按 precharge 扣钱（提交时的预扣），
// 再占一笔在途预留，与 ImagesAsyncSubmit 的顺序一致。
func newImgBillTask(t *testing.T, ctx context.Context, upstreamURL, taskID string, balance, precharge float64, mode string, count int, snapshot string) (model.ImageTask, model.User) {
	t.Helper()
	channel := createImageTaskPollTestChannel(t, ctx, upstreamURL)
	user := createImageTaskPollTestUser(t, ctx, balance)
	if _, err := op.ImageTaskReservationTake(ctx, taskID, user.ID); err != nil {
		t.Fatalf("take reservation: %v", err)
	}
	if precharge > 0 {
		if err := op.UserRecordUsage(user.ID, precharge, ctx); err != nil {
			t.Fatalf("pre-charge: %v", err)
		}
	}
	task := newImageTaskPollTestTask(t, ctx, channel, user, taskID, precharge, time.Hour)
	if err := db.GetDB().WithContext(ctx).Model(&model.ImageTask{}).Where("id = ?", task.ID).Updates(map[string]any{
		"billing_mode":     mode,
		"image_count":      count,
		"billing_snapshot": snapshot,
		"request_model":    "image-poll-test-model",
	}).Error; err != nil {
		t.Fatalf("seed billing: %v", err)
	}
	return reloadImageTask(t, ctx, taskID), user
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

const imgBillUsageBody = `{"created":1700000000,"data":[{"url":"https://images.upstream.example/u.png"}],` +
	`"usage":{"input_tokens":1000,"output_tokens":4000,"total_tokens":5000}}`

// 按 token 计费的模型：提交不预扣，完成时按上游 usage × 冻结的价结算，只扣一次；
// 跑两轮执行器也不会再扣；中途改价不影响已受理的任务；日志只有一行、费用 = 结算价。
func TestImageTaskBillingTokenPricedSettlesFromUsageOnce(t *testing.T) {
	ctx := setupImgLogTest(t, false)
	up := &imgLogUpstream{status: http.StatusOK, body: imgBillUsageBody}
	// 提交时：输入 $5 / 输出 $40 每百万 token，倍率 1.5。
	task, user := newImgBillTask(t, ctx, up.server(t).URL, "imgtask_bill_token", 1.0, 0, "token", 1, imgBillSnapshot(t, 5, 40, 1.5))
	// 提交之后管理员改了价：已受理的任务仍按提交时的价结算。
	price.SetLLMPrice("image-poll-test-model", model.LLMPrice{Input: 500, Output: 500})
	t.Cleanup(func() { price.SetLLMPrice("image-poll-test-model", model.LLMPrice{}) })

	runImageTaskPollOnce()
	runImageTaskPollOnce()

	want := (1000*5 + 4000*40) * 1e-6 * 1.5 // 0.2475
	stored := reloadImageTask(t, ctx, task.TaskID)
	if stored.Status != model.ImageTaskStatusCompleted || !nearly(stored.Cost, want) {
		t.Fatalf("task = %s cost %v, want completed with cost %v", stored.Status, stored.Cost, want)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; !nearly(got, 1.0-want) {
		t.Fatalf("balance = %v, want %v (settled exactly once)", got, 1.0-want)
	}
	rows := imgLogRows(t, ctx)
	if len(rows) != 1 {
		t.Fatalf("want exactly one log row, got %d", len(rows))
	}
	l := rows[0]
	if !nearly(l.Cost, want) || l.InputTokens != 1000 || l.OutputTokens != 4000 {
		t.Fatalf("log cost/tokens = %v %d %d, want %v 1000 4000", l.Cost, l.InputTokens, l.OutputTokens, want)
	}
	if l.BaseOutputPrice != 40 || l.FinalMultiplier != 1.5 || l.UsageMissingReason != "" {
		t.Fatalf("log should carry the frozen price: out %v mult %v missing %q", l.BaseOutputPrice, l.FinalMultiplier, l.UsageMissingReason)
	}
	if op.ImageTaskReservationRelease(task.TaskID) {
		t.Fatal("reservation must already be released at completion")
	}
}

// 按 token 计费但上游没回 usage：与同步图片同一口径，按 0 收，并在日志里标明「上游没给用量」。
func TestImageTaskBillingTokenPricedWithoutUsageIsFlagged(t *testing.T) {
	ctx := setupImgLogTest(t, false)
	up := &imgLogUpstream{status: http.StatusOK, body: `{"data":[{"url":"https://images.upstream.example/n.png"}]}`}
	task, user := newImgBillTask(t, ctx, up.server(t).URL, "imgtask_bill_nousage", 1.0, 0, "token", 1, imgBillSnapshot(t, 5, 40, 1))

	runImageTaskPollOnce()

	if stored := reloadImageTask(t, ctx, task.TaskID); stored.Status != model.ImageTaskStatusCompleted || stored.Cost != 0 {
		t.Fatalf("task = %s cost %v", stored.Status, stored.Cost)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 1.0 {
		t.Fatalf("balance = %v, want 1.0", got)
	}
	rows := imgLogRows(t, ctx)
	if len(rows) != 1 || rows[0].UsageMissingReason != model.RelayLogUsageMissingReasonUpstreamUsageMissing {
		t.Fatalf("log must flag the missing usage: %+v", rows)
	}
}

// 按张计费的模型：提交时预扣「单价 × 张数 × 倍率」，完成不结算、不再扣（即使上游回了 usage）。
func TestImageTaskBillingPerImageKeepsPrechargeAndChargesOnce(t *testing.T) {
	ctx := setupImgLogTest(t, false)
	up := &imgLogUpstream{status: http.StatusOK, body: imgBillUsageBody}
	// 0.5/张 × 2 张 × 倍率 1 = 1.0，提交时已从 3.0 扣成 2.0。
	task, user := newImgBillTask(t, ctx, up.server(t).URL, "imgtask_bill_perimage", 3.0, 1.0, "per_image", 2, imgBillSnapshot(t, 5, 40, 1))

	runImageTaskPollOnce()
	runImageTaskPollOnce()

	if stored := reloadImageTask(t, ctx, task.TaskID); stored.Status != model.ImageTaskStatusCompleted || stored.Cost != 1.0 {
		t.Fatalf("task = %s cost %v, want completed 1.0", stored.Status, stored.Cost)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 2.0 {
		t.Fatalf("balance = %v, want 2.0 (pre-charge only, nothing more)", got)
	}
	rows := imgLogRows(t, ctx)
	if len(rows) != 1 || rows[0].Cost != 1.0 || rows[0].FinalOutputCost != 1.0 || rows[0].OutputTokens != 4000 {
		t.Fatalf("log = %+v, want one row with cost 1.0 (tokens shown only)", rows)
	}
}

// 失败：预扣原路退回，任务 cost 清零，日志费用 0。
func TestImageTaskBillingFailureRefundsPrecharge(t *testing.T) {
	ctx := setupImgLogTest(t, false)
	up := &imgLogUpstream{status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`}
	task, user := newImgBillTask(t, ctx, up.server(t).URL, "imgtask_bill_fail", 3.0, 1.0, "per_image", 2, imgBillSnapshot(t, 0, 0, 1))

	runImageTaskPollOnce()
	runImageTaskPollOnce()

	if stored := reloadImageTask(t, ctx, task.TaskID); stored.Status != model.ImageTaskStatusFailed || stored.Cost != 0 {
		t.Fatalf("task = %s cost %v, want failed 0", stored.Status, stored.Cost)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 3.0 {
		t.Fatalf("balance = %v, want 3.0 (refunded exactly once)", got)
	}
	rows := imgLogRows(t, ctx)
	if len(rows) != 1 || rows[0].Cost != 0 || rows[0].FinalOutputCost != 0 {
		t.Fatalf("log = %+v, want one row with cost 0", rows)
	}
}

// 过期：不打上游，预扣原路退回，日志费用 0。
func TestImageTaskBillingExpiryRefundsPrecharge(t *testing.T) {
	ctx := setupImgLogTest(t, false)
	up := &imgLogUpstream{status: http.StatusOK, body: imgBillUsageBody}
	task, user := newImgBillTask(t, ctx, up.server(t).URL, "imgtask_bill_expired", 3.0, 1.0, "per_image", 2, imgBillSnapshot(t, 0, 0, 1))
	if err := db.GetDB().WithContext(ctx).Model(&model.ImageTask{}).Where("id = ?", task.ID).
		Update("expires_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
		t.Fatalf("expire: %v", err)
	}

	runImageTaskPollOnce()
	runImageTaskPollOnce()

	if stored := reloadImageTask(t, ctx, task.TaskID); stored.Status != model.ImageTaskStatusFailed || stored.Cost != 0 {
		t.Fatalf("task = %s cost %v, want failed 0", stored.Status, stored.Cost)
	}
	if got := reloadImageTaskUser(t, user.ID).Balance; got != 3.0 {
		t.Fatalf("balance = %v, want 3.0 (refunded)", got)
	}
	up.mu.Lock()
	hits := len(up.bodies)
	up.mu.Unlock()
	if hits != 0 {
		t.Fatalf("expired task must not call upstream, got %d", hits)
	}
	rows := imgLogRows(t, ctx)
	if len(rows) != 1 || rows[0].Cost != 0 {
		t.Fatalf("log = %+v, want one row with cost 0", rows)
	}
}
