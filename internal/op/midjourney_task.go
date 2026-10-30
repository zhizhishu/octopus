package op

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// MidjourneyTaskCreate 落一条任务归属。同一个上游 id 已经有行时不再重复写：
// mj-proxy 对重复提交会回 code 21（任务已存在）并给出同一个 id。
func MidjourneyTaskCreate(ctx context.Context, task *model.MidjourneyTask) error {
	if task == nil {
		return fmt.Errorf("midjourney task is nil")
	}
	task.UpstreamTaskID = strings.TrimSpace(task.UpstreamTaskID)
	if task.UpstreamTaskID == "" {
		return fmt.Errorf("midjourney upstream task id is empty")
	}
	existing, err := MidjourneyTaskGetByUpstreamID(ctx, task.UpstreamTaskID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	return db.GetDB().WithContext(ctx).Create(task).Error
}

// MidjourneyTaskGetByUpstreamID 按上游任务 id 取归属。(nil, nil) = 没有这条记录。
func MidjourneyTaskGetByUpstreamID(ctx context.Context, upstreamTaskID string) (*model.MidjourneyTask, error) {
	upstreamTaskID = strings.TrimSpace(upstreamTaskID)
	if upstreamTaskID == "" {
		return nil, fmt.Errorf("upstream task id is empty")
	}
	var task model.MidjourneyTask
	res := db.GetDB().WithContext(ctx).
		Where("upstream_task_id = ?", upstreamTaskID).
		Order("id ASC").
		Limit(1).
		Find(&task)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	return &task, nil
}
