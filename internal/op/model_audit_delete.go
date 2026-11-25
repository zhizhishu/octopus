package op

import (
	"context"
	"errors"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// 检测记录的手动清理：单条删、勾选批量删、一键清空已完成。
//
// 90 天保留期（auditResultRetention）不变，这里只是让管理员自己决定提前删掉哪些。
// 删除是**真删**（任务行 + 它的全部结果行），删了就找不回来，所以页面上要二次确认。
//
// 🔴 还在花钱的任务一律不删：排队中/进行中的任务、以及已取消但还有一行在收尾的
// 任务（取消只拦住新的请求，已发出去的那一次还在跑，见 ModelAuditJobCancel）。
// 删掉它们等于让 worker 手里的那一行写进一张已经不存在的任务，进度、回收都会乱；
// 用户要的是「不想看到这条记录」，不是「中断一次已付费的请求」。

// ErrAuditJobBusy 表示任务还在排队或在跑，不能删。
var ErrAuditJobBusy = errors.New("这个任务还在排队或在跑，先取消、等它停下再删")

// ModelAuditDeleteResult 是一次删除的结果。
type ModelAuditDeleteResult struct {
	// Deleted 实际删掉的任务数。
	Deleted int `json:"deleted"`
	// SkippedBusy 因为还在排队/在跑而没删的任务 id。
	SkippedBusy []int64 `json:"skipped_busy"`
}

// auditBusyJobIDs 返回 ids 里还不能删的任务：任务本身 queued/running，
// 或者还有结果行处于 pending/running。
func auditBusyJobIDs(tx *gorm.DB, ids []int64) (map[int64]struct{}, error) {
	busy := make(map[int64]struct{})
	if len(ids) == 0 {
		return busy, nil
	}
	var jobBusy []int64
	if err := tx.Model(&model.ModelAuditJob{}).
		Where("id IN ? AND status IN ?", ids, []string{model.AuditJobQueued, model.AuditJobRunning}).
		Pluck("id", &jobBusy).Error; err != nil {
		return nil, err
	}
	for _, id := range jobBusy {
		busy[id] = struct{}{}
	}
	var rowBusy []int64
	if err := tx.Model(&model.ModelAuditResult{}).
		Where("job_id IN ? AND status IN ?", ids, []string{model.AuditResultPending, model.AuditResultRunning}).
		Distinct().Pluck("job_id", &rowBusy).Error; err != nil {
		return nil, err
	}
	for _, id := range rowBusy {
		busy[id] = struct{}{}
	}
	return busy, nil
}

// ModelAuditJobsDelete 删除一批任务（连同结果行），跳过还在跑的。
func ModelAuditJobsDelete(ctx context.Context, ids []int64) (ModelAuditDeleteResult, error) {
	out := ModelAuditDeleteResult{SkippedBusy: []int64{}}
	clean := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	if len(clean) == 0 {
		return out, errors.New("没有要删的任务")
	}
	if len(clean) > 500 {
		return out, errors.New("一次最多删 500 条")
	}
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		busy, err := auditBusyJobIDs(tx, clean)
		if err != nil {
			return err
		}
		del := make([]int64, 0, len(clean))
		for _, id := range clean {
			if _, b := busy[id]; b {
				out.SkippedBusy = append(out.SkippedBusy, id)
				continue
			}
			del = append(del, id)
		}
		if len(del) == 0 {
			return nil
		}
		if err := tx.Where("job_id IN ?", del).Delete(&model.ModelAuditResult{}).Error; err != nil {
			return err
		}
		res := tx.Where("id IN ?", del).Delete(&model.ModelAuditJob{})
		if res.Error != nil {
			return res.Error
		}
		out.Deleted = int(res.RowsAffected)
		return nil
	})
	return out, err
}

// ModelAuditJobDelete 删一个任务。还在跑就返回 ErrAuditJobBusy；不存在返回 gorm.ErrRecordNotFound。
func ModelAuditJobDelete(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("任务 id 无效")
	}
	var n int64
	if err := db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).Where("id = ?", id).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return gorm.ErrRecordNotFound
	}
	res, err := ModelAuditJobsDelete(ctx, []int64{id})
	if err != nil {
		return err
	}
	if len(res.SkippedBusy) > 0 {
		return ErrAuditJobBusy
	}
	return nil
}

// ModelAuditJobsClearFinished 清空所有已经停下的任务（完成/已取消且没有在收尾的行）。
// 排队中、进行中的一律不动。分批删，单次最多清 5000 条，防止一次锁库太久。
func ModelAuditJobsClearFinished(ctx context.Context) (ModelAuditDeleteResult, error) {
	total := ModelAuditDeleteResult{SkippedBusy: []int64{}}
	for round := 0; round < 10; round++ {
		var ids []int64
		if err := db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).
			Where("status IN ?", []string{model.AuditJobDone, model.AuditJobCanceled}).
			Where("id NOT IN (?)", db.GetDB().Model(&model.ModelAuditResult{}).Select("job_id").
				Where("status IN ?", []string{model.AuditResultPending, model.AuditResultRunning})).
			Order("id ASC").Limit(500).Pluck("id", &ids).Error; err != nil {
			return total, err
		}
		if len(ids) == 0 {
			break
		}
		res, err := ModelAuditJobsDelete(ctx, ids)
		if err != nil {
			return total, err
		}
		total.Deleted += res.Deleted
		if res.Deleted == 0 {
			break
		}
	}
	return total, nil
}
