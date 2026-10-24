package op

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

// 图片批处理的持久层。
//
// 【这一层的职责边界】只管"这个状态能不能写进去、写得对不对"，不碰 HTTP、
// 不碰上游协议、不碰计费公式（金额由调用方算好传进来）。所有并发正确性集中在
// 这里，因为写者有四个：提交路径、后台推进器、客户端取消、过期清理。
//
// 【为什么全是 CAS 而不是无条件 Update】见 image_task.go 顶部那段：一条已经
// 走到 completed 的任务被慢一拍的推进器写成 failed，用户看到的是"图生成了但任务
// 失败"。并发写只能用"条件更新 + 看影响了行数"来裁判，谁抢到算谁的。

// BatchImageNewID 生成客户端持有的批号：imgbatch_ + 32 位十六进制。
//
// 用 crypto/rand 而不是自增 id：批号是**句柄**，可枚举就等于可探测别人有没有批次
// （虽然查询侧有归属过滤，但没必要把内部计数暴露出去）。与 relay 的 imgtask_ 同规矩。
func BatchImageNewID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// rand 失败极少见；退回纳秒时间戳保证不返回空批号（空值会撞 uniqueIndex 的 default）。
		return fmt.Sprintf("imgbatch_%x", time.Now().UnixNano())
	}
	return "imgbatch_" + hex.EncodeToString(buf)
}

// BatchImageRequestHash 计算归一化请求的指纹，用于幂等判定。
//
// 【为什么要指纹】同一个 Idempotency-Key 配不同请求体，是一个**客户端 bug**，
// 而我们默默返回旧批次会让它以为新请求生效了。指纹对不上时如实报冲突。
func BatchImageRequestHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// BatchImageCreate 落一条新任务行并返回它。批号为空时自动生成。
//
// 【为什么状态是 queued 而不是 submitted】这一行落库时我们还没调上游（提交路径里
// 不许等上游）。等推进器真的把 JSONL 交出去，才 CAS 到 submitted。
func BatchImageCreate(ctx context.Context, job *model.BatchImageJob) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}
	if strings.TrimSpace(job.BatchID) == "" {
		job.BatchID = BatchImageNewID()
	}
	if !model.IsBatchImageJobStatusAllowed(job.Status) {
		return fmt.Errorf("invalid batch image job status %q", job.Status)
	}
	if !model.IsSupportedBatchImageProvider(job.Provider) {
		return fmt.Errorf("invalid batch image provider %q", job.Provider)
	}
	now := time.Now().Unix()
	if job.CreatedAt == 0 {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	return db.GetDB().WithContext(ctx).Create(job).Error
}

// BatchImageGetByBatchID 按批号取任务（**不带归属过滤**）。
//
// 调用方必须在拿到之后自己核对归属，或者直接用 BatchImageGetForOwner。
// 这个函数只给推进器用——它按批号推进，本来就不属于任何请求方。
func BatchImageGetByBatchID(ctx context.Context, batchID string) (*model.BatchImageJob, error) {
	batchID = strings.TrimSpace(batchID)
	if batchID == "" {
		return nil, fmt.Errorf("batch_id is required")
	}
	var job model.BatchImageJob
	err := db.GetDB().WithContext(ctx).
		Where("batch_id = ?", batchID).
		First(&job).Error
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// BatchImageGetForOwner 按批号取任务，且**必须**属于给定用户与令牌。
//
// 【归属过滤写在 SQL 里，不靠调用方后置判断】写在 WHERE 里就漏不掉；
// 多一层 if 就多一处会写错的地方。不属于自己的一律返回"不存在"（不是 403）——
// 403 会告诉探测者"这个批号是真的，只是不是你的"。
func BatchImageGetForOwner(ctx context.Context, batchID string, ownerUserID, ownerTokenID int) (*model.BatchImageJob, error) {
	batchID = strings.TrimSpace(batchID)
	if batchID == "" {
		return nil, fmt.Errorf("batch_id is required")
	}
	if ownerUserID <= 0 {
		return nil, fmt.Errorf("invalid owner_user_id: %d", ownerUserID)
	}
	var job model.BatchImageJob
	query := db.GetDB().WithContext(ctx).
		Where("batch_id = ? AND owner_user_id = ?", batchID, ownerUserID).
		Where("expires_at = 0 OR expires_at > ?", time.Now().Unix())
	if ownerTokenID > 0 {
		query = query.Where("owner_token_id = ?", ownerTokenID)
	}
	if err := query.First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// "不存在"、"不是你的"、"已过期"三种情况必须返回**同一个**结果。
			// 返回 (nil, nil) 而不是 ErrRecordNotFound 是为了让上层只有一条分支：
			// 一旦这三种情况在调用方看来有区别，就会有人不小心写出"这个批号存在
			// 但不是你的"这种可探测的响应。
			//
			// 与 ImageTaskGetForOwner 完全同一个口径。
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// BatchImageGetByIdempotencyKey 按幂等键取已存在的任务。找不到返回 (nil, nil)。
//
// 找不到不返回 error：对调用方来说"没有"是正常分支（新建），不是错误。
func BatchImageGetByIdempotencyKey(ctx context.Context, ownerUserID, ownerTokenID int, key string) (*model.BatchImageJob, error) {
	key = strings.TrimSpace(key)
	if key == "" || ownerUserID <= 0 {
		return nil, nil
	}
	var job model.BatchImageJob
	err := db.GetDB().WithContext(ctx).
		Where("owner_user_id = ? AND owner_token_id = ? AND idempotency_key = ?", ownerUserID, ownerTokenID, key).
		Order("id DESC").
		First(&job).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// BatchImageStatusCAS 条件推进任务状态。返回是否真的推进了。
//
// 【非法目标值一律拒绝，不归一】理由同 ImageTaskStatusCAS：把一次推进悄悄改写成
// queued 会让推进器无限重跑同一条任务，比报错危险得多。
//
// updates 里的 status 会被忽略（以 to 为准），避免"两个地方都能改状态"。
func BatchImageStatusCAS(ctx context.Context, id int64, from, to string, updates map[string]any) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid batch image job id: %d", id)
	}
	if !model.IsBatchImageJobStatusAllowed(to) {
		return false, fmt.Errorf("invalid batch image job status %q", to)
	}
	if !model.CanTransitionBatchImageJob(from, to) {
		return false, fmt.Errorf("illegal batch image job transition %q -> %q", from, to)
	}

	fields := make(map[string]any, len(updates)+2)
	for k, v := range updates {
		if k == "status" {
			continue
		}
		fields[k] = v
	}
	fields["status"] = to
	fields["updated_at"] = time.Now().Unix()

	res := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ? AND status = ?", id, from).
		Updates(fields)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// BatchImageUpdateProgress 写"上游状态变了但阶段没变"的观测值（raw state / 句柄）。
//
// 【刻意不带状态条件】这些字段是**观测**，不是推进：上游从 PENDING 变 RUNNING，
// 我们的 status 可能还是 submitted。用 CAS 反而会因为状态没变而写不进去。
// 但它必须带 updated_at，让"这条任务还在动"可观测。
func BatchImageUpdateProgress(ctx context.Context, id int64, updates map[string]any) error {
	if id <= 0 {
		return fmt.Errorf("invalid batch image job id: %d", id)
	}
	if len(updates) == 0 {
		return nil
	}
	fields := make(map[string]any, len(updates)+1)
	for k, v := range updates {
		if k == "status" || k == "updated_at" {
			continue
		}
		fields[k] = v
	}
	fields["updated_at"] = time.Now().Unix()
	return db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ?", id).
		Updates(fields).Error
}

// BatchImageListExecutable 取"推进器这一轮该动"的任务。
//
// 【为什么按 updated_at 而不是 created_at 排】一条提交了很久但一直没轮到的任务
// 会被永远排在后面。按 updated_at 排 = 越久没动过的越优先，天然带上公平性。
func BatchImageListExecutable(ctx context.Context, limit int) ([]model.BatchImageJob, error) {
	if limit <= 0 {
		limit = 20
	}
	now := time.Now().Unix()
	var jobs []model.BatchImageJob
	err := db.GetDB().WithContext(ctx).
		Where("status IN ?", []string{
			model.BatchImageJobStatusQueued,
			model.BatchImageJobStatusSubmitted,
			model.BatchImageJobStatusIndexing,
		}).
		Where("expires_at = 0 OR expires_at > ?", now).
		// 只有 queued 的任务需要"已发布"这道闸：提交是先落主记录、再落子项，
		// 中间那个空窗里推进器若捡起它，loadItems 会看到 0 条子项而 abort+退款，
		// 客户端却可能随后收到一个成功响应。已提交给上游的任务（submitted/indexing）
		// 必然早就过了这一步，但仍加同样的条件——语义是"没有子项的任务不可执行"，
		// 这比"只在 queued 时检查"更难写错。
		Where("EXISTS (SELECT 1 FROM batch_image_items WHERE batch_image_items.batch_id = batch_image_jobs.batch_id)").
		Order("updated_at ASC").
		Limit(limit).
		Find(&jobs).Error
	return jobs, err
}

// BatchImageListExpired 取已过保留期、但还没收口的任务。
//
// 只挑"非终态或产物还在"的：completed 且产物已删的不必再动。
func BatchImageListExpired(ctx context.Context, limit int) ([]model.BatchImageJob, error) {
	if limit <= 0 {
		limit = 50
	}
	now := time.Now().Unix()
	var jobs []model.BatchImageJob
	err := db.GetDB().WithContext(ctx).
		Where("expires_at > 0 AND expires_at <= ?", now).
		Where("status NOT IN ?", []string{
			model.BatchImageJobStatusFailed,
			model.BatchImageJobStatusCancelled,
			model.BatchImageJobStatusOutputDeleted,
		}).
		Order("expires_at ASC").
		Limit(limit).
		Find(&jobs).Error
	return jobs, err
}

// BatchImageListForOwner 分页列出某人的批次。
//
// cursor 用自增 id 而非 offset：offset 分页在"边翻边有新任务进来"时会漏行/重行。
// 返回的 nextCursor 为 0 表示没有下一页。
func BatchImageListForOwner(ctx context.Context, ownerUserID, ownerTokenID int, status string, limit int, cursor int64) ([]model.BatchImageJob, int64, error) {
	if ownerUserID <= 0 {
		return nil, 0, fmt.Errorf("invalid owner_user_id: %d", ownerUserID)
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	now := time.Now().Unix()
	query := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("owner_user_id = ?", ownerUserID).
		Where("expires_at = 0 OR expires_at > ?", now)
	if ownerTokenID > 0 {
		query = query.Where("owner_token_id = ?", ownerTokenID)
	}
	if status = strings.TrimSpace(status); status != "" {
		query = query.Where("status = ?", status)
	}
	if cursor > 0 {
		query = query.Where("id < ?", cursor)
	}
	var jobs []model.BatchImageJob
	if err := query.Order("id DESC").Limit(limit + 1).Find(&jobs).Error; err != nil {
		return nil, 0, err
	}
	var nextCursor int64
	if len(jobs) > limit {
		jobs = jobs[:limit]
		nextCursor = jobs[len(jobs)-1].ID
	}
	return jobs, nextCursor, nil
}

// BatchImageItemCreateBatch 批量落子项行（提交路径用）。
//
// 分批 Create 而不是逐条：200 条子项逐条 INSERT 在远端库上会明显拖慢提交。
// 库的占位符上限（SQLite 默认 999 变量）用 40 一批来避——按 12 列出头算是 480，
// 留足余量。
func BatchImageItemCreateBatch(ctx context.Context, items []model.BatchImageItem) error {
	if len(items) == 0 {
		return nil
	}
	const chunk = 40
	return db.GetDB().WithContext(ctx).CreateInBatches(items, chunk).Error
}

// BatchImageItemsByBatchID 取一批的全部子项，按 custom_id 稳定排序。
//
// 排序是契约的一部分：客户端按提交顺序读，我们按主键顺序给，两者一致。
func BatchImageItemsByBatchID(ctx context.Context, batchID string) ([]model.BatchImageItem, error) {
	batchID = strings.TrimSpace(batchID)
	if batchID == "" {
		return nil, fmt.Errorf("batch_id is required")
	}
	var items []model.BatchImageItem
	err := db.GetDB().WithContext(ctx).
		Where("batch_id = ?", batchID).
		Order("id ASC").
		Find(&items).Error
	return items, err
}

// BatchImageItemGet 按 (批号, custom_id) 取一条子项。
func BatchImageItemGet(ctx context.Context, batchID, customID string) (*model.BatchImageItem, error) {
	var item model.BatchImageItem
	err := db.GetDB().WithContext(ctx).
		Where("batch_id = ? AND custom_id = ?", strings.TrimSpace(batchID), strings.TrimSpace(customID)).
		First(&item).Error
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// BatchImageItemIndexResult 把一条子项的索引结果写进去（**只在还是 pending 时**）。
//
// 【为什么要条件】推进器可能因为超时重跑同一轮的索引；无条件写会把已经定稿的
// 成功行改成失败行（或者反过来），客户端看到的结果就在两次查询之间跳变。
func BatchImageItemIndexResult(ctx context.Context, id int64, fields map[string]any) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid batch image item id: %d", id)
	}
	status, _ := fields["status"].(string)
	if status != "" && !model.IsBatchImageItemStatusAllowed(status) {
		return false, fmt.Errorf("invalid batch image item status %q", status)
	}
	payload := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		payload[k] = v
	}
	payload["indexed_at"] = time.Now().Unix()
	res := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageItem{}).
		Where("id = ? AND status = ?", id, model.BatchImageItemStatusPending).
		Updates(payload)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// BatchImageCountItems 统计一批里各状态的子项数量。
func BatchImageCountItems(ctx context.Context, batchID string) (success, failed, cancelled int, err error) {
	type row struct {
		Status string
		Total  int
	}
	var rows []row
	err = db.GetDB().WithContext(ctx).
		Model(&model.BatchImageItem{}).
		Select("status, COUNT(*) AS total").
		Where("batch_id = ?", strings.TrimSpace(batchID)).
		Group("status").
		Scan(&rows).Error
	if err != nil {
		return 0, 0, 0, err
	}
	for _, r := range rows {
		switch r.Status {
		case model.BatchImageItemStatusSuccess:
			success = r.Total
		case model.BatchImageItemStatusFailed:
			failed = r.Total
		case model.BatchImageItemStatusCancelled:
			cancelled = r.Total
		}
	}
	return success, failed, cancelled, nil
}

// BatchImageCancelPending 把一批里还在 pending 的子项标记成 cancelled。
// 返回被标记的条数。
func BatchImageCancelPending(ctx context.Context, batchID string) (int64, error) {
	res := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageItem{}).
		Where("batch_id = ? AND status = ?", strings.TrimSpace(batchID), model.BatchImageItemStatusPending).
		Updates(map[string]any{
			"status":     model.BatchImageItemStatusCancelled,
			"indexed_at": time.Now().Unix(),
		})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// BatchImageCountActiveForOwner 统计某人当前还在跑的批次数（用于并发上限）。
func BatchImageCountActiveForOwner(ctx context.Context, ownerUserID int) (int64, error) {
	if ownerUserID <= 0 {
		return 0, nil
	}
	var total int64
	err := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("owner_user_id = ?", ownerUserID).
		Where("status IN ?", []string{
			model.BatchImageJobStatusQueued,
			model.BatchImageJobStatusSubmitted,
			model.BatchImageJobStatusIndexing,
		}).
		Count(&total).Error
	return total, err
}

// BatchImageClaimSettle 抢"这一单的结算权"，抢到的那一次才动钱。
//
// 与 ImageTaskClaimRefund 同一套裁判：一条带条件的 UPDATE 把 settled_at 从 0 写成
// 非零，只有真正写成的那一个写者拿到 true。退款/扣款都必须先抢这个权，
// 否则失败路径 + 过期清扫 + 重启补扫各退一次 = 凭空给用户加钱。
func BatchImageClaimSettle(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid batch image job id: %d", id)
	}
	res := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ? AND settled_at = 0", id).
		Update("settled_at", time.Now().Unix())
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// BatchImageWriteSettlement 在**已抢到结算权**之后写入最终金额。
//
// 刻意不带 settled_at 条件：抢权那一步已经把它从 0 改成了非零，这里再要求 0
// 会永远写不进去。它只写 charged_amount / hold_amount 两列。
func BatchImageWriteSettlement(ctx context.Context, id int64, charged, hold float64) error {
	if id <= 0 {
		return fmt.Errorf("invalid batch image job id: %d", id)
	}
	return db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"charged_amount": charged,
			"hold_amount":    hold,
			"updated_at":     time.Now().Unix(),
		}).Error
}

// BatchImageRestoreSettleClaim 把抢到手的结算权**还回去**，让下一轮可以重试。
//
// 【什么时候必须调】抢权成功之后、真正动钱之前失败了（数不出条目数、退款写库失败）。
// 不还回去的话 settled_at 已经非零，这条任务从此再也结算不了——钱既没退也没记，
// 而且没有任何后续流程会碰它。这是"抢权式幂等"唯一的已知代价，必须显式补偿。
//
// 【调用纪律】只允许在 BatchImageWriteSettlement **成功之前**调用。
// 结算金额一旦落库，再还权就会让下一轮重复结算，等于重复退款。
func BatchImageRestoreSettleClaim(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("invalid batch image job id: %d", id)
	}
	return db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ?", id).
		Update("settled_at", 0).Error
}

// BatchImageMarkOutputDeleted 把产物标记为已删（终态改写）。
//
// 【为什么不走 CanTransition】completed → output_deleted 是**产物生命周期**的改写，
// 不是任务推进：钱已经结清了，只是图没了。把它塞进推进表会让状态机同时表达两件事，
// 而"completed 还能不能再变"这个问题的答案会变得没法一眼看出来。这里用一条
// 带条件的原子 UPDATE 单独表达：只有还挂着的产物能被删一次。
func BatchImageMarkOutputDeleted(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid batch image job id: %d", id)
	}
	now := time.Now().Unix()
	res := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ? AND output_deleted_at = 0", id).
		Updates(map[string]any{
			"status":            model.BatchImageJobStatusOutputDeleted,
			"output_deleted_at": now,
			"updated_at":        now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// BatchImageMarkDownloaded 记一笔"这一批被下载过"。
//
// 只是观测，不参与任何判断；失败不报错（下载本身已经成功了，不该因为打个戳失败
// 就把文件的响应搞坏）。
func BatchImageMarkDownloaded(ctx context.Context, id int64) {
	if id <= 0 {
		return
	}
	if err := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ? AND downloaded_at = 0", id).
		Update("downloaded_at", time.Now().Unix()).Error; err != nil {
		log.Debugf("batch image: mark downloaded for job %d failed: %v", id, err)
	}
}

// BatchImageDeleteRecord 删掉一条任务记录连同它的子项。返回实际删掉的任务数。
//
// 【只删记录，不删上游产物】上游的结果对象由上游按保留期自己清。这里删的是
// 我们的索引。调用前必须已经确认归属（见 BatchImageGetForOwner）。
func BatchImageDeleteRecord(ctx context.Context, id int64, batchID string) (bool, error) {
	if id <= 0 || strings.TrimSpace(batchID) == "" {
		return false, fmt.Errorf("invalid batch image job reference")
	}
	deleted := false
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("batch_id = ?", batchID).Delete(&model.BatchImageItem{}).Error; err != nil {
			return err
		}
		res := tx.Where("id = ?", id).Delete(&model.BatchImageJob{})
		if res.Error != nil {
			return res.Error
		}
		deleted = res.RowsAffected > 0
		return nil
	})
	return deleted, err
}

// BatchImageMetadataJSON 把 metadata map 序列化成稳定字符串（用于幂等指纹）。
func BatchImageMetadataJSON(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	// 先按 key 排序再序列化：encoding/json 对 map 已经按 key 排序，
	// 这里显式依赖那个保证（写出来是为了让读代码的人知道这是有意为之）。
	raw, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(raw)
}

// BatchImageCursorFromString 解析客户端传回来的 cursor；非法值返回 0（= 第一页）。
//
// 不报错：cursor 是**游标**不是身份，坏了退回第一页比 400 更友好，
// 也不会让一个过期的游标把整个列表页卡死。
func BatchImageCursorFromString(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// BatchImageWriteCounts 写一批的统计字段。
//
// 【为什么不是 CAS】计数是观测数据，不是状态推进。用 StatusCAS(from == to) 去写
// 会被状态机以"同状态不是推进"拒掉——那正是它该做的事，错的是调用方。
func BatchImageWriteCounts(ctx context.Context, id int64, success, failed, cancelled int) error {
	if id <= 0 {
		return fmt.Errorf("invalid batch image job id: %d", id)
	}
	return db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"success_count":   success,
			"fail_count":      failed,
			"cancelled_count": cancelled,
			"updated_at":      time.Now().Unix(),
		}).Error
}

// IsUniqueViolation 判断错误是否为唯一约束冲突。
//
// 【为什么不按具体错误类型判】三种方言说法完全不同：SQLite 是
// "UNIQUE constraint failed"、PostgreSQL 是 "duplicate key value violates
// unique constraint"（SQLSTATE 23505）、MySQL 是 "Duplicate entry"。
// 这里按关键字识别，宁可漏判成普通错误（那也只是退化成"报错给客户端"），
// 不可把普通错误误判成幂等命中（那会让两个不同请求共享一条任务）。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, needle := range []string{
		"unique constraint failed",
		"duplicate key value violates unique constraint",
		"duplicate entry",
		"sqlite_constraint_unique",
	} {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// BatchImageListUnsettledTerminal 列出"已经是终态、但还没结算"的任务。
//
// 【为什么必须有这个查询】结算流程是"抢权 → 退款 → 写金额"。抢权那一步就把
// settled_at 写成了非零，所以进程若在"抢权成功、退款还没写"之间退出（崩溃、重启、
// 请求 context 被取消），那条记录看起来**已经结算过了**：settled_at 非零、
// charged_amount 仍是 0，而退款从未发生。用户的钱就这么少了，且没有任何流程会再碰它。
//
// 判据用 charged_amount = 0 AND hold_amount > 0：预扣过钱、但结算金额从未落库。
// 这比"看 settled_at"可靠——settled_at 恰恰是被抢权提前写脏的那个字段。
func BatchImageListUnsettledTerminal(ctx context.Context, limit int) ([]model.BatchImageJob, error) {
	if limit <= 0 {
		limit = 50
	}
	var jobs []model.BatchImageJob
	err := db.GetDB().WithContext(ctx).
		Where("status IN ?", []string{
			model.BatchImageJobStatusCompleted,
			model.BatchImageJobStatusFailed,
			model.BatchImageJobStatusCancelled,
		}).
		Where("charged_amount = 0 AND hold_amount > 0").
		Order("updated_at ASC").
		Limit(limit).
		Find(&jobs).Error
	return jobs, err
}

// BatchImageReclaimSettle 强制夺回一条"已抢权但从未写入结算金额"的结算权。
//
// 与 BatchImageClaimSettle 的区别：那个要求 settled_at = 0（正常首次抢权），
// 这个专门对付被抢权写脏的残留行。条件里带 charged_amount = 0，保证
// **绝不会覆盖已经真正结算过的记录**——那是重复退款的风险点。
func BatchImageReclaimSettle(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("invalid batch image job id: %d", id)
	}
	res := db.GetDB().WithContext(ctx).
		Model(&model.BatchImageJob{}).
		Where("id = ? AND charged_amount = 0 AND hold_amount > 0", id).
		Update("settled_at", time.Now().Unix())
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
