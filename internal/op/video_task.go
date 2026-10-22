package op

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// videoTaskNotTerminalWhere 是「这一行还没进终态」的 WHERE 片段。
//
// 【为什么必须显式写 IS NULL】status 列允许 NULL（见 model.VideoTask.Status 的注释）：
// SQL 里 `NULL NOT IN ('success','failure')` 求值为 UNKNOWN，WHERE 只保留 TRUE 的行，
// 于是状态为 NULL 的任务既不在「未完成」里、也不在「终态」里——它会永远卡住，
// 客户端永远等不到结果。MySQL / PostgreSQL / SQLite 在这条语义上完全一致（都遵循
// SQL 标准的三值逻辑），所以这不是某个方言的怪癖，只给其中一种库打补丁没有意义。
//
// 【为什么括号是必需的】GORM 只在多个 Where 之间做 AND 拼接，不会替这里面的 OR 补
// 括号。VideoTaskListStale 在这条之后还要再 Where 一条超时条件，少了括号就会变成
// `... AND status IS NULL OR status NOT IN (...)`，把终态任务也捞出来。
//
// 【三方言语义差异，按本项目的三方言支持范围核对】
//   - NOT NULL：本项目刻意**不**给 video_tasks.status 加 not null tag，因为它在存量库上
//     根本不生效。GORM 的 MigrateColumn 只在「列当前是 NOT NULL、要放松成可空」时才判定
//     需要改列，反方向的收紧不判（gorm.io/gorm v1.31.1 migrator/migrator.go:535-541 的内层
//     条件是 `!nullable`）。实测（glebarez/sqlite v1.11.0）：已有表 + not null tag +
//     AutoMigrate → err=nil，列仍然 nullable=true；把同样的 tag 用在一张新建的表上才会
//     得到 NOT NULL。也就是说 tag 只能救未来新建的库，救不了**恰恰出过这个 bug 的存量库**。
//     而真去改列（Migrator.AlterColumn）在 SQLite 上是整表重建（glebarez/sqlite
//     Migrator.AlterColumn → recreateTable），存量有 NULL 时实测报
//     `NOT NULL constraint failed: video_tasks__temp.status`；db.go 的 AutoMigrate(:70) 又
//     早于 AfterAutoMigration(:101)，归一化根本还没跑。MySQL / PostgreSQL 能就地改列，
//     但为此去动生产表的列约束，收益不抵风险——查询侧兜住更便宜也更全面。
//   - RowsAffected：PostgreSQL 与 SQLite 的 UPDATE 计「匹配到的行数」（写入值与原值相同
//     也算匹配，SQLite 侧已实测：同值 CAS 返回 ok=true）；MySQL 默认（未开
//     CLIENT_FOUND_ROWS）计「真正被改动的行数」，同值写入返回 0。本层的 CAS 里 to 永远由
//     轮询结果决定、与 from 不同，所以不受影响；把差异记在这里，是为了将来有人拿 CAS 写回
//     同一个状态值时，不会把它误判成「没抢到」。
const videoTaskNotTerminalWhere = "(status IS NULL OR status NOT IN ?)"

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

	// from 非空时条件就是原样的 `status = ?`（NULL 行匹配不上，这正是 CAS 该有的语义：
	// 以为自己读到的是 'queued'，就不该去推一行其实是 NULL 的任务）。
	// from 为空时说明调用方读到的就是一个空状态——GORM 把 NULL 列扫成零值 ""，
	// 而 `status = ''` 在三种库上都匹配不到 NULL，所以这里显式展开成
	// `status IS NULL OR status = ''`，否则 NULL 行读得出、推不动，还是卡死。
	where := "id = ? AND status = ?"
	args := []any{id, from}
	if from == "" {
		where = "id = ? AND (status IS NULL OR status = '')"
		args = []any{id}
	}

	res := db.GetDB().WithContext(ctx).
		Model(&model.VideoTask{}).
		Where(where, args...).
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
	// 状态为空就兜底成 not_start。status 列在磁盘上允许 NULL（见 videoTaskNotTerminalWhere），
	// 写入侧漏一次，这一行就落进「既不是未完成、也不是终态」的悬空态。查询侧现在已经兜得住，
	// 但写入侧不该主动制造需要兜的局面。
	if task.Status == "" {
		task.Status = model.VideoTaskNotStart
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
		Where(videoTaskNotTerminalWhere, []model.VideoTaskStatus{model.VideoTaskSuccess, model.VideoTaskFailure}).
		Order("id").
		Limit(limit).
		Find(&tasks)
	if res.Error != nil {
		return nil, res.Error
	}
	return tasks, nil
}

// VideoTaskListStale 取「早于 deadline 仍未进终态」的任务，供轮询器统一判超时失败。
//
// 为什么单独一条查询而不是在 VideoTaskListUnfinished 的结果里筛：未完成列表被
// limit 截断（一轮只处理最新鲜的一批），超时任务恰恰是最老的那批；混在一起筛会让
// 「已经卡了很久的任务」看 limit 的脸色才被收口。这里按同样条件单独取一次，
// 只多一条只读查询，收口逻辑就不依赖轮询批次的窗口。
//
// 时间取 COALESCE 语义：submit_time > 0 用它，否则退回 created_at。理由是
// submit_time 是「提交给上游的时刻」，语义上正是该拿来判超时的；但它是可 0 的
// （表默认 0，外部导入/迁移进来的老行也可能没有），此时 created_at 是唯一的时间证据。
// deadline 为 unix 秒。
func VideoTaskListStale(ctx context.Context, deadline int64, limit int) ([]model.VideoTask, error) {
	if limit <= 0 {
		limit = 200
	}
	var tasks []model.VideoTask
	res := db.GetDB().WithContext(ctx).
		Where(videoTaskNotTerminalWhere, []model.VideoTaskStatus{model.VideoTaskSuccess, model.VideoTaskFailure}).
		Where("(CASE WHEN submit_time > 0 THEN submit_time ELSE created_at END) > 0 AND (CASE WHEN submit_time > 0 THEN submit_time ELSE created_at END) < ?", deadline).
		Order("id").
		Limit(limit).
		Find(&tasks)
	if res.Error != nil {
		return nil, res.Error
	}
	return tasks, nil
}

// VideoTaskListFilter 是「按归属分页列任务」的查询条件。
//
// 为什么归属不是可选参数：这个结构体的唯一用途就是回答「**我**有哪些视频任务」。
// 把归属做成可选项，就等于给「忘了传 = 看全部」留了一个口子——那是一次越权事故，
// 不是一个默认值。所以 OwnerTokenID / OwnerUserID 全为 0 直接报错（见
// VideoTaskListByOwner），调用方必须显式给出身份。
type VideoTaskListFilter struct {
	// OwnerTokenID > 0 时按发起任务用的那把 key 归属过滤；
	// 与 OwnerUserID 同时给出时两个条件都要满足（key 本来就属于那个用户）。
	OwnerTokenID int
	// OwnerUserID > 0 时按任务所属用户过滤。
	OwnerUserID int
	// Status 为空表示不限状态。
	Status model.VideoTaskStatus
	// Page 从 1 开始；PageSize 默认 20、上限 100。
	Page     int
	PageSize int
}

// 归属条件缺失时的显式错误。调用方（HTTP handler）据此拒绝请求，
// 而不是把它当成「空列表」——两者在日志和告警里必须能区分开。
var ErrVideoTaskOwnerScopeRequired = errors.New("video task owner scope is required")

// VideoTaskListByOwner 按归属分页列出视频任务，并返回满足条件的总数。
//
// 这是给「日志页的视频任务视图」用的只读查询：轮询器那几条 VideoTaskList*
// 各有各的用途（未完成 / 超时），都不能拿来回答「这个人自己有哪些任务、一共多少条」。
// 表结构上 owner_token_id / owner_user_id 都带索引，分页查询走索引即可。
//
// 【红线】过滤条件里必须有归属：没有归属就报错返回 (nil, 0, err)，
// 绝不退化成「全表」。
func VideoTaskListByOwner(ctx context.Context, f VideoTaskListFilter) ([]model.VideoTask, int64, error) {
	if f.OwnerTokenID <= 0 && f.OwnerUserID <= 0 {
		return nil, 0, ErrVideoTaskOwnerScopeRequired
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	// 每次都从零拼一条新查询：GORM 的 Count/Find 会改写同一条 statement，
	// 拿 Count 之后的对象继续 Find 会把 count(*) 的 select 带过去。
	scoped := func() *gorm.DB {
		q := db.GetDB().WithContext(ctx).Model(&model.VideoTask{})
		if f.OwnerTokenID > 0 {
			q = q.Where("owner_token_id = ?", f.OwnerTokenID)
		}
		if f.OwnerUserID > 0 {
			q = q.Where("owner_user_id = ?", f.OwnerUserID)
		}
		if f.Status != "" {
			q = q.Where("status = ?", f.Status)
		}
		return q
	}

	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var tasks []model.VideoTask
	if err := scoped().
		Order("id DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Find(&tasks).Error; err != nil {
		return nil, 0, err
	}
	return tasks, total, nil
}
