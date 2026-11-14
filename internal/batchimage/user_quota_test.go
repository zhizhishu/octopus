package batchimage

import (
	"context"
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func TestUserQuotaBatchIdempotencyConcurrentAdmission(t *testing.T) {
	ctx := setupBatchImageServiceTest(t)
	owner := seedBatchImageUser(t, ctx, 100)
	resolver := staticTestResolver(t, ctx)
	if _, err := op.UserQuotaPolicySave(ctx, model.UserQuotaPolicy{UserID: owner, Enabled: true, Timezone: "UTC", FiveHourRequests: 2}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Settings: func(context.Context) Settings {
		return Settings{Enabled: true, MaxItems: 100, MaxConcurrentJobsPerOwner: 10, UnitPrice: 1, RetentionHours: 48}
	}, Resolver: resolver}
	input := SubmitInput{OwnerUserID: owner, Model: "batch-image-test-model", IdempotencyKey: "same-request", Items: []SubmitItem{{CustomID: "a", Prompt: "test a", OutputCount: 1}, {CustomID: "b", Prompt: "test b", OutputCount: 1}}}
	var wg sync.WaitGroup
	jobs := make(chan *model.BatchImageJob, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := service.Submit(ctx, input)
			if err != nil {
				errs <- err
				return
			}
			jobs <- job
		}()
	}
	wg.Wait()
	close(jobs)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	id := ""
	count := 0
	for job := range jobs {
		count++
		if id != "" && id != job.BatchID {
			t.Fatal("idempotent submissions created different jobs")
		}
		id = job.BatchID
	}
	if count != 2 {
		t.Fatalf("successful callers=%d", count)
	}
	var rows int64
	if err := db.GetDB().Model(&model.BatchImageJob{}).Where("owner_user_id = ?", owner).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("jobs=%d", rows)
	}
	state, err := op.UserQuotaStatusGet(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range state.Windows {
		if w.Accepted != 2 || w.CostUsed != 2 {
			t.Fatalf("duplicate admitted units/cost=%+v", w)
		}
	}
	got, _ := op.UserGet(owner)
	if got.Balance != 98 {
		t.Fatalf("duplicate debit=%v", got.Balance)
	}
}
