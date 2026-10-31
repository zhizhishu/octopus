package op

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// ImageTaskCreate 落一条新任务。执行器靠它在重启后重新接管，所以创建即入库。
//
// 状态为空的兜底成 processing（见 model.NormalizeImageTaskStatus）：磁盘上这一列
// 有 default，但写入侧主动制造"空状态"的行会让它既不在待执行集合里、也不是终态，
// 永远没人推进。
func ImageTaskCreate(ctx context.Context, task *model.ImageTask) error {
	if task == nil {
		return fmt.Errorf("image task is nil")
	}
	if strings.TrimSpace(task.TaskID) == "" {
		return fmt.Errorf("image task_id is required")
	}
	// ExpiresAt 必须由创建方给：它落 0 的行既不在待执行集合里（要 expires_at > now），
	// 也不在过期清扫里（要 expires_at > 0）—— 两不管，会永远停在 processing 且钱不退。
	// 与其默认补一个 24h（把一个调用方的 bug 静默成“看起来正常”），不如当场报错。
	if task.ExpiresAt <= 0 {
		return fmt.Errorf("image task expires_at is required")
	}
	task.Status = model.NormalizeImageTaskStatus(task.Status)
	return db.GetDB().WithContext(ctx).Create(task).Error
}

// ErrImageTaskOwnerScopeRequired 是"查询没给归属"时的显式错误。
//
// 为什么不是"空列表"：调用方（HTTP handler）必须据此回 404，而不是把一次越权
// 尝试渲染成"你没有任务"。两者在日志和告警里必须能区分开。
var ErrImageTaskOwnerScopeRequired = errors.New("image task owner scope is required")

// ImageTaskGetForOwner 按 task_id + 归属取一条任务。
//
// 【红线】归属是**必须**的，不是可选参数：这个函数的唯一用途就是回答
// "这个调用者自己的那条任务"。只按 task_id 查等于"知道 id 就能看别人的图"。
// 归属全为 0 直接报错，绝不退化成"不限归属"。
//
// 归还 (nil, nil) 表示没有这条记录（不存在 / 不属于你 / 已过期，三者对调用方一律 404）。
func ImageTaskGetForOwner(ctx context.Context, taskID string, ownerTokenID, ownerUserID int) (*model.ImageTask, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("image task_id is required")
	}
	if ownerTokenID <= 0 && ownerUserID <= 0 {
		return nil, ErrImageTaskOwnerScopeRequired
	}
	q := db.GetDB().WithContext(ctx).Model(&model.ImageTask{}).Where("task_id = ?", taskID)
	// 提交时两个归属一起写，所以两个条件一起比：任何一个对不上都不是"你的任务"。
	if ownerTokenID > 0 {
		q = q.Where("owner_token_id = ?", ownerTokenID)
	}
	if ownerUserID > 0 {
		q = q.Where("owner_user_id = ?", ownerUserID)
	}
	var task model.ImageTask
	res := q.Limit(1).Find(&task)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	return &task, nil
}

// ImageTaskListExecutable 取一批"该去上游干活"的任务：还没出结果、且还没过期。
//
// limit 是保险丝：单轮最多处理这么多条，积压时一轮不跑到天亮。
//
// 按 id 升序（先进先出）而不是像视频那样按 updated_at：本表没有 updated_at
// 列（契约给的结构体只有 created_at/completed_at/expires_at），而且这里的"一轮"
// 会把批次内的任务**都执行完**（受并发上限与单条超时约束）才返回，不存在视频那边
// "上游永远说还在跑"导致同一行盘踞队头的情形——一次执行必出终态。
func ImageTaskListExecutable(ctx context.Context, now int64, limit int) ([]model.ImageTask, error) {
	if limit <= 0 {
		limit = 20
	}
	var tasks []model.ImageTask
	res := db.GetDB().WithContext(ctx).
		Where("status = ?", model.ImageTaskStatusProcessing).
		Where("expires_at > ?", now).
		Order("id").
		Limit(limit).
		Find(&tasks)
	if res.Error != nil {
		return nil, res.Error
	}
	return tasks, nil
}

// ImageTaskListExpired 取"到 expires_at 还没出结果"的任务，供过期清扫统一判失败。
//
// 与 ImageTaskListExecutable 是互补的两条查询：那条要 expires_at > now，这条要
// <= now，任何一条 processing 的行恰好落在其中一边。
//
// expires_at <= 0 的行（外部导入/手工 SQL 造出来的）刻意**不**扫：没有时间证据时
// 按本地时钟判死是错的，宁可留着。
func ImageTaskListExpired(ctx context.Context, now int64, limit int) ([]model.ImageTask, error) {
	if limit <= 0 {
		limit = 50
	}
	var tasks []model.ImageTask
	res := db.GetDB().WithContext(ctx).
		Where("status = ?", model.ImageTaskStatusProcessing).
		Where("expires_at > 0 AND expires_at <= ?", now).
		Order("id").
		Limit(limit).
		Find(&tasks)
	if res.Error != nil {
		return nil, res.Error
	}
	return tasks, nil
}

// ImageTaskStatusCAS 以「当前状态必须还是 from」为前提把任务推进到 to，同时写入
// updates 里的附加列，返回是否真的更新了一行。
//
// 为什么必须是 CAS 而不是无条件 UPDATE：提交、后台执行、过期清扫、进程重启后的
// 补扫会同时看到同一条任务。无条件写会互相覆盖——一条已经 completed 的任务被慢
// 一拍的清扫写成 failed，用户就看到"图已经生成了但任务失败"。
//
// 【非法目标值一律拒绝，不归一】：状态只有三个合法值（见 model.ImageTaskStatus*）。
// 把一次推进悄悄改写成 processing 会让执行器无限重跑同一条任务，比报错危险得多。
func ImageTaskStatusCAS(ctx context.Context, id int64, from, to string, updates map[string]any) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid image task id: %d", id)
	}
	if !model.IsImageTaskStatusAllowed(to) {
		return false, fmt.Errorf("invalid image task status %q", to)
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
		Model(&model.ImageTask{}).
		Where("id = ? AND status = ?", id, from).
		Updates(fields)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ImageTaskClaimRefund 抢"这一笔预扣的退款权"，抢到的那一次才真退钱。
//
// 退款必须幂等：失败路径、过期清扫、进程重启后的补扫都可能撞上同一条任务，
// 重复退款等于凭空给用户加钱。这里用一条带条件的 UPDATE 做**唯一**的裁判：
// `... SET cost = 0 WHERE id = ? AND cost > 0`，只有真正把 cost 从非零改成 0 的
// 那一个写者拿到 true，其余的 RowsAffected = 0。
//
// 三种方言都成立：MySQL 默认计"真正改动的行数"，cost 由非零变 0 一定算改动；
// PostgreSQL / SQLite 计"匹配到的行数"，同样 > 0。
func ImageTaskClaimRefund(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid image task id: %d", id)
	}
	res := db.GetDB().WithContext(ctx).
		Model(&model.ImageTask{}).
		Where("id = ? AND cost > 0", id).
		Update("cost", 0)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ImageTaskRefund 退掉一条任务的预扣（幂等）。返回是否真的退了钱。
//
// 顺序是"先抢退款权、再动钱"，不是反的：先付后退会让两个写者各退一次。
// 万一钱没退成（DB 抖动），必须把退款权写回去——否则 cost 已经被清零，
// 之后任何一次重试都抢不到权，用户永远拿不回这笔钱。
func ImageTaskRefund(ctx context.Context, task model.ImageTask) (bool, error) {
	if task.Cost <= 0 || task.OwnerUserID <= 0 {
		return false, nil
	}
	claimed, err := ImageTaskClaimRefund(ctx, task.ID)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	if err := UserRefundUsage(task.OwnerUserID, task.Cost, ctx); err != nil {
		if restoreErr := db.GetDB().WithContext(ctx).
			Model(&model.ImageTask{}).
			Where("id = ? AND cost = 0", task.ID).
			Update("cost", task.Cost).Error; restoreErr != nil {
			log.Errorf("image task refund: restore claimed cost for task %s failed: %v (original error: %v)", task.TaskID, restoreErr, err)
		}
		return false, err
	}
	return true, nil
}

// imageTaskReservation 是一条异步任务占用的在途额度记录。
type imageTaskReservation struct {
	userID int
	amount float64
	// keyID / keyAmount：在 API Key 花费上限里占的那一笔（见 apikey_quota.go）。
	// 访客 Key 的归属是管理员、amount 恒为 0，上限只靠这一笔管住。
	keyID     int
	keyAmount float64
}

// imageTaskReservations 记录每个异步图片任务占用的**在途额度**（task_id → 预留）。
//
// 为什么必须单独记账：UserCanRelay 的预留在 HTTP 请求结束时就由中间件归还了
// （middleware.APIKeyAuth 的 defer），而异步任务的真实生命周期比那次请求长得多
// （几十秒到几小时）。不另外占一笔，用户就能拿同一笔余额同时开 N 个任务。
//
// 为什么是内存态：与 userQuotaInflight 同一语义——进程重启后不存在在途任务，
// 清零即正确。任何"落库续期"的写法都会在崩溃后留下一批需要人来清理的幽灵占用。
var imageTaskReservations = make(map[string]imageTaskReservation)

var imageTaskReservationsLock sync.Mutex

// ImageTaskReservationTake 为一条异步任务在途占一笔额度，返回预留金额（0 = 未占）。
//
// 它走的就是 UserCanRelay —— 与同步图片路径、与所有其它中继路径同一道闸，**不是**
// 另写一套判据。拿不到就返回错误，调用方必须据此拒绝提交（此时任务行还没落库）。
func ImageTaskReservationTake(ctx context.Context, taskID string, userID int) (float64, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return 0, fmt.Errorf("image task_id is required")
	}
	reservation, err := UserCanRelay(userID, ctx)
	if err != nil {
		return 0, err
	}
	if reservation <= 0 {
		return 0, nil
	}
	imageTaskReservationsLock.Lock()
	imageTaskReservations[taskID] = imageTaskReservation{userID: userID, amount: reservation}
	imageTaskReservationsLock.Unlock()
	return reservation, nil
}

// ImageTaskReservationRelease 归还某条任务的在途预留，返回是否真的归还了一笔。
//
// 【幂等】同一 task_id 重复调用只有第一次返回 true：账本里取不到就是已经还过了。
// 这一点很重要——终态推进可能被多个写者尝试，重复归还会被 UserReleaseQuota 夹到 0，
// 于是"多退"看起来无害，实际上会把别的任务占用的额度一起放掉。
func ImageTaskReservationRelease(taskID string) bool {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false
	}
	imageTaskReservationsLock.Lock()
	held, ok := imageTaskReservations[taskID]
	if ok {
		delete(imageTaskReservations, taskID)
	}
	imageTaskReservationsLock.Unlock()
	if !ok {
		return false
	}
	if held.keyAmount > 0 {
		APIKeyReleaseReserve(held.keyID, held.keyAmount)
	}
	if held.amount <= 0 {
		return held.keyAmount > 0
	}
	UserReleaseQuota(held.userID, held.amount)
	return true
}

// ImageTaskKeyReservationTake 在 API Key 的花费上限里为这条异步任务占一笔，一直占到任务终态
// （ImageTaskReservationRelease 一并归还）。amount 是已知的预扣金额（按张计费），
// <= 0 时按默认量子占。Key 没设上限时不占。拿不到就返回错误，调用方必须拒绝提交。
func ImageTaskKeyReservationTake(taskID string, key model.APIKey, amount float64) (float64, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return 0, fmt.Errorf("image task_id is required")
	}
	reservation, err := APIKeyReserve(key, amount)
	if err != nil || reservation <= 0 {
		return 0, err
	}
	imageTaskReservationsLock.Lock()
	held := imageTaskReservations[taskID]
	if held.keyAmount > 0 {
		// 同一 task_id 不会占两次；万一发生，先还掉旧的，不让占用越积越多。
		APIKeyReleaseReserve(held.keyID, held.keyAmount)
	}
	held.keyID, held.keyAmount = key.ID, reservation
	imageTaskReservations[taskID] = held
	imageTaskReservationsLock.Unlock()
	return reservation, nil
}
