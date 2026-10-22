package op

import (
	"context"
	"fmt"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// VideoTaskStatusCAS 以「当前状态必须还是 from」为前提，把任务推进到 to，
// 同时写入 updates 里的附加列，返回是否真的更新了一行。
//
// 为什么必须是 CAS 而不是无条件 UPDATE：轮询进程、超时清理、用户取消、进程重启
// 后的补扫可能同时看到同一条任务。无条件写会互相覆盖——一个已经 success 的任务
// 被慢一拍的轮询写回 in_progress，用户就白等一轮。把「状态没变」放进 WHERE，
// 数据库行级锁保证只有一个写者赢。
//
// updates 由调用方给（进度、结果地址、失败原因……），status 由本函数统一写入；
// 调用方就算在 updates 里塞了 status 也会被 to 覆盖，避免两个来源各写一套。
// 该 map 不会被修改。
func VideoTaskStatusCAS(ctx context.Context, id int64, from, to model.VideoTaskStatus, updates map[string]any) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid video task id: %d", id)
	}

	fields := make(map[string]any, len(updates)+1)
	for k, v := range updates {
		if k == "status" {
			continue
		}
		fields[k] = v
	}
	fields["status"] = to

	res := db.GetDB().WithContext(ctx).
		Model(&model.VideoTask{}).
		Where("id = ? AND status = ?", id, from).
		Updates(fields)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// VideoTaskCreate 落一条新任务。轮询进程靠它在重启后重新接管，所以创建即入库。
func VideoTaskCreate(ctx context.Context, task *model.VideoTask) error {
	if task == nil {
		return fmt.Errorf("video task is nil")
	}
	return db.GetDB().WithContext(ctx).Create(task).Error
}

// VideoTaskGetByUpstreamID 按上游任务 id 取一条任务。
// 归还 (nil, nil) 表示没有这条记录，调用方据此决定新建还是更新。
func VideoTaskGetByUpstreamID(ctx context.Context, upstreamTaskID string) (*model.VideoTask, error) {
	if upstreamTaskID == "" {
		return nil, fmt.Errorf("upstream task id is empty")
	}
	var task model.VideoTask
	res := db.GetDB().WithContext(ctx).
		Where("upstream_task_id = ?", upstreamTaskID).
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

// VideoTaskListUnfinished 取所有还没进终态的任务，供轮询进程挨个查上游。
// limit 是保险丝：单轮最多处理这么多条，避免积压时一轮跑到天亮。
func VideoTaskListUnfinished(ctx context.Context, limit int) ([]model.VideoTask, error) {
	if limit <= 0 {
		limit = 200
	}
	var tasks []model.VideoTask
	res := db.GetDB().WithContext(ctx).
		Where("status NOT IN ?", []model.VideoTaskStatus{model.VideoTaskSuccess, model.VideoTaskFailure}).
		Order("id").
		Limit(limit).
		Find(&tasks)
	if res.Error != nil {
		return nil, res.Error
	}
	return tasks, nil
}
