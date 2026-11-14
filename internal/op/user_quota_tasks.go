package op

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

var errUserQuotaTaskAdvanced = errors.New("quota task was already advanced")

func UserQuotaImageTaskCreate(ctx context.Context, task *model.ImageTask) (bool, error) {
	if task == nil || task.TaskID == "" || task.ExpiresAt <= 0 {
		return false, fmt.Errorf("invalid image task")
	}
	receipt, err := UserQuotaAdmit(ctx, UserQuotaAdmission{
		SourceKey: "image:" + task.TaskID, SourceKind: "image",
		UserID: task.OwnerUserID, RequestModel: task.RequestModel, Units: 1,
		InitialCost: task.Cost, KnownCost: task.BillingMode != "token",
	}, func(tx *gorm.DB) error {
		task.Status = model.NormalizeImageTaskStatus(task.Status)
		return tx.Create(task).Error
	})
	return receipt != nil, err
}

// Terminal task state, original-window quota accounting and the legacy debit
// or refund arithmetic commit together. No terminal CAS/charge crash gap is
// introduced for configured tasks.
func UserQuotaImageTaskFinish(ctx context.Context, task model.ImageTask, status string, updates map[string]any, targetCost float64) (handled, changed bool, err error) {
	failed, outcome := int64(0), "completed"
	if status != model.ImageTaskStatusCompleted {
		failed, outcome = 1, "failed"
	}
	handled, err = UserQuotaSettle(ctx, "image:"+task.TaskID, task.OwnerUserID, targetCost, failed, outcome, func(tx *gorm.DB) error {
		fields := make(map[string]any, len(updates)+2)
		for key, value := range updates {
			fields[key] = value
		}
		fields["status"], fields["cost"] = status, targetCost
		result := tx.Model(&model.ImageTask{}).Where("id = ? AND status = ?", task.ID, model.ImageTaskStatusProcessing).Updates(fields)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errUserQuotaTaskAdvanced
		}
		changed = true
		return nil
	})
	if errors.Is(err, errUserQuotaTaskAdvanced) {
		return true, false, nil
	}
	if err != nil {
		changed = false
	}
	return
}

func UserQuotaBatchCreate(ctx context.Context, job *model.BatchImageJob, items []model.BatchImageItem) (bool, error) {
	receipt, err := UserQuotaAdmit(ctx, UserQuotaAdmission{
		SourceKey: "batch:" + job.BatchID, SourceKind: "batch",
		UserID: job.OwnerUserID, RequestModel: job.RequestModel,
		Units: int64(len(items)), InitialCost: job.HoldAmount, KnownCost: true, RequireBalance: true,
	}, func(tx *gorm.DB) error {
		if !model.IsBatchImageJobStatusAllowed(job.Status) || !model.IsSupportedBatchImageProvider(job.Provider) {
			return fmt.Errorf("invalid batch job")
		}
		now := time.Now().Unix()
		job.CreatedAt, job.UpdatedAt = now, now
		if err := tx.Create(job).Error; err != nil {
			return err
		}
		return tx.CreateInBatches(items, 40).Error
	})
	return receipt != nil, err
}

func UserQuotaBatchSettle(ctx context.Context, job *model.BatchImageJob, charged float64, success, failed, cancelled int) (handled, changed bool, err error) {
	outcome := "completed"
	if job.Status == model.BatchImageJobStatusFailed {
		outcome = "failed"
	}
	if job.Status == model.BatchImageJobStatusCancelled {
		outcome = "cancelled"
	}
	failedUnits := int64(job.ItemCount - success)
	if failedUnits < 0 {
		failedUnits = 0
	}
	handled, err = UserQuotaSettle(ctx, "batch:"+job.BatchID, job.OwnerUserID, charged, failedUnits, outcome, func(tx *gorm.DB) error {
		now := time.Now().Unix()
		result := tx.Model(&model.BatchImageJob{}).Where("id = ? AND settled_at = 0", job.ID).Updates(map[string]any{
			"settled_at": now, "charged_amount": charged, "hold_amount": job.HoldAmount,
			"success_count": success, "fail_count": failed, "cancelled_count": cancelled, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errUserQuotaTaskAdvanced
		}
		changed = true
		return nil
	})
	if errors.Is(err, errUserQuotaTaskAdvanced) {
		return true, false, nil
	}
	if err != nil {
		changed = false
	}
	return
}
