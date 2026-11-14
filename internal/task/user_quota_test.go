package task

import (
	"net/http"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func TestUserQuotaImagePollSettlesOrRefundsExactlyOnce(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "completed"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := setupImgLogTest(t, false)
			up := &imgLogUpstream{status: http.StatusOK, body: imgBillUsageBody}
			if failed {
				up.status = http.StatusInternalServerError
				up.body = `{"error":{"message":"controlled failure"}}`
			}
			channel := createImageTaskPollTestChannel(t, ctx, up.server(t).URL)
			user := createImageTaskPollTestUser(t, ctx, 3)
			if _, err := op.UserQuotaPolicySave(ctx, model.UserQuotaPolicy{UserID: user.ID, Enabled: true, Timezone: "UTC", FiveHourCost: 10, FiveHourRequests: 2}); err != nil {
				t.Fatal(err)
			}
			task := model.ImageTask{
				TaskID: "imgtask_quota_" + name, CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
				Status: model.ImageTaskStatusProcessing, OwnerUserID: user.ID,
				ChannelID: channel.ID, ChannelKeyID: channel.Keys[0].ID,
				Model: "image-poll-test-model", RequestModel: "image-poll-test-model",
				Endpoint: model.ImageTaskEndpointGenerations, RequestBody: `{"model":"image-poll-test-model","prompt":"test image"}`,
				BillingMode: "token", ImageCount: 1, BillingSnapshot: imgBillSnapshot(t, 5, 40, 1),
			}
			if failed {
				task.BillingMode = "per_image"
				task.Cost = 0.5
			}
			if _, err := op.ImageTaskReservationTake(ctx, task.TaskID, user.ID); err != nil {
				t.Fatal(err)
			}
			if handled, err := op.UserQuotaImageTaskCreate(ctx, &task); err != nil || !handled {
				t.Fatalf("create=%v %v", handled, err)
			}
			runImageTaskPollOnce()
			runImageTaskPollOnce()
			want := 0.165
			wantStatus := model.ImageTaskStatusCompleted
			wantFailed := int64(0)
			if failed {
				want = 0
				wantStatus = model.ImageTaskStatusFailed
				wantFailed = 1
			}
			stored := reloadImageTask(t, ctx, task.TaskID)
			if stored.Status != wantStatus || !nearly(stored.Cost, want) {
				t.Fatalf("terminal=%s cost=%v", stored.Status, stored.Cost)
			}
			if got := reloadImageTaskUser(t, user.ID).Balance; !nearly(got, 3-want) {
				t.Fatalf("balance=%v want=%v", got, 3-want)
			}
			state, err := op.UserQuotaStatusGet(ctx, user.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state.PendingReceipts != 0 {
				t.Fatalf("terminal receipt pending=%d", state.PendingReceipts)
			}
			for _, w := range state.Windows {
				if w.Accepted != 1 || w.FailedAccepted != wantFailed || !nearly(w.CostUsed, want) || w.CostReserved != 0 {
					t.Fatalf("window=%+v", w)
				}
			}
			up.mu.Lock()
			hits := len(up.bodies)
			up.mu.Unlock()
			if hits != 1 {
				t.Fatalf("poll replayed generation: %d", hits)
			}
		})
	}
}
