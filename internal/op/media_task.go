package op

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zhizhishu/octopus/internal/db"
)

// media_task.go 是「图片/视频任务」合并列表的查询侧（fix-ui1127a）。
//
// 三个来源表合成一条时间线：image_tasks（异步图片）、midjourney_tasks（MJ 归属行）、
// video_tasks（异步视频）。对外字段与 videoTaskListItem 逐字对齐（不造新词表）：
//   - midjourney_tasks 不存状态（fetch 透传上游、本地没有副本，见 model/midjourney_task.go），
//     所以 MJ 行的 status 用既有词表里的 unknown——「不知道」照实说，不发明新状态；
//     progress/fail_reason/result_url 一律空。
//   - image_tasks 没有上游时间点和进度：progress 空、submit/start 0、finish 用 completed_at。
//   - image 的 result_url 由 handler 从 ResultJSON 提取（写库时已剔除 b64），SQL 层不解析 JSON。
//
// 合并发生在 SQL 层（UNION ALL 子查询 + ORDER/LIMIT/OFFSET）：分页与总数对「合并后的
// 整列」成立——若在应用层拼两个分页接口，混合类型下页码永远对不上。

// MediaTaskKind 是合并列表的类型维度：image / video（filter 的「全部」= 不过滤）。
type MediaTaskKind string

const (
	MediaTaskKindImage MediaTaskKind = "image"
	MediaTaskKindVideo MediaTaskKind = "video"
)

// MediaTaskRow 是 UNION 出来的一行（字段名与 video_tasks 对齐）。
type MediaTaskRow struct {
	OwnerUserID int64
	Kind        string
	TaskID      string
	Model       string
	Status      string
	Progress    string
	FailReason  string
	ResultURL   string
	SubmitTime  int64
	StartTime   int64
	FinishTime  int64
	CreatedAt   int64
}

// MediaTaskListFilter 是合并列表的过滤条件。归属只认 OwnerUserID（同视频任务口径）。
type MediaTaskListFilter struct {
	OwnerUserID int
	// AllUsers=true 时 owner 约束整体放开（管理员看全部用户的任务，fix-ui1127a-r1 第 6 条）；
	// 此时 OwnerUserID>0 仍按该用户过滤（筛选排「用户」下拉），=0 才是真全部。
	AllUsers  bool
	Kind      MediaTaskKind // 空 = 全部
	Status    string        // 空 = 全部；按行自身 status 精确匹配
	Model     string        // 空 = 全部；子串匹配
	StartTime int64         // created_at 下界（unix 秒），0 = 不限
	EndTime   int64         // created_at 上界（unix 秒），0 = 不限
	Page      int
	PageSize  int
}

type mediaTaskSource struct {
	selectSQL string
	args      []any
	countSQL  string
	countArgs []any
	alias     string
}

func mediaTaskStatusCond(raw string) (string, []any) {
	statuses := []string{}
	unknown := false
	for _, part := range strings.Split(raw, ",") {
		st := strings.TrimSpace(part)
		if st == "" {
			continue
		}
		statuses = append(statuses, st)
		if st == "unknown" {
			unknown = true
		}
	}
	if len(statuses) == 0 {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	args := make([]any, 0, len(statuses))
	for _, st := range statuses {
		args = append(args, st)
	}
	cond := "status IN (" + placeholders + ")"
	return " AND " + cond, args
}

func mediaTaskAppendTime(sql string, args []any, filter MediaTaskListFilter) (string, []any) {
	if filter.StartTime > 0 {
		sql += " AND created_at >= ?"
		args = append(args, filter.StartTime)
	}
	if filter.EndTime > 0 {
		sql += " AND created_at <= ?"
		args = append(args, filter.EndTime)
	}
	return sql, args
}

// mediaTaskBuildQuery 拼三来源各自的 top-N 子查询、UNION 分页 SQL 和三段 COUNT。
// top-N 必须先下推到每个来源表：管理员全量时外层 UNION 排序不必扫全表；因为每个
// 来源最多只需要 offset+limit 条候选，合并后再排序分页结果不变。COUNT 分表相加，
// 不对 UNION 结果做整包 COUNT。派生表全部显式命名，兼容 SQLite/MySQL/Postgres。
func mediaTaskBuildQuery(filter MediaTaskListFilter) (string, []any, []mediaTaskSource, error) {
	ownerCond := `owner_user_id = ?`
	ownerArgs := []any{filter.OwnerUserID}
	if filter.AllUsers && filter.OwnerUserID <= 0 {
		ownerCond = `1 = 1`
		ownerArgs = nil
	}
	branches := []MediaTaskKind{}
	switch filter.Kind {
	case "":
		branches = []MediaTaskKind{MediaTaskKindImage, MediaTaskKindVideo}
	case MediaTaskKindImage, MediaTaskKindVideo:
		branches = []MediaTaskKind{filter.Kind}
	default:
		return "", nil, nil, fmt.Errorf("未知任务类型 %q", string(filter.Kind))
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	pageSize := filter.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	topN := page * pageSize

	sources := []mediaTaskSource{}
	if slices.Contains(branches, MediaTaskKindImage) {
		statusCond, statusArgs := mediaTaskStatusCond(filter.Status)

		imageSQL := `SELECT owner_user_id, 'image' AS kind, task_id, model, status, '' AS progress, error_message AS fail_reason, '' AS result_url, 0 AS submit_time, 0 AS start_time, completed_at AS finish_time, created_at FROM image_tasks WHERE ` + ownerCond
		imageArgs := append([]any{}, ownerArgs...)
		imageCountSQL := `SELECT COUNT(*) FROM image_tasks WHERE ` + ownerCond
		imageCountArgs := append([]any{}, ownerArgs...)
		imageSQL += statusCond
		imageArgs = append(imageArgs, statusArgs...)
		imageCountSQL += statusCond
		imageCountArgs = append(imageCountArgs, statusArgs...)
		if filter.Model != "" {
			imageSQL += " AND model LIKE ?"
			imageArgs = append(imageArgs, "%"+filter.Model+"%")
			imageCountSQL += " AND model LIKE ?"
			imageCountArgs = append(imageCountArgs, "%"+filter.Model+"%")
		}
		imageSQL, imageArgs = mediaTaskAppendTime(imageSQL, imageArgs, filter)
		imageCountSQL, imageCountArgs = mediaTaskAppendTime(imageCountSQL, imageCountArgs, filter)
		sources = append(sources, mediaTaskSource{selectSQL: imageSQL, args: imageArgs, countSQL: imageCountSQL, countArgs: imageCountArgs, alias: "media_image_top"})

		mjSQL := `SELECT owner_user_id, 'image' AS kind, upstream_task_id AS task_id, model, 'unknown' AS status, '' AS progress, '' AS fail_reason, '' AS result_url, 0 AS submit_time, 0 AS start_time, 0 AS finish_time, created_at FROM midjourney_tasks WHERE ` + ownerCond
		mjArgs := append([]any{}, ownerArgs...)
		mjCountSQL := `SELECT COUNT(*) FROM midjourney_tasks WHERE ` + ownerCond
		mjCountArgs := append([]any{}, ownerArgs...)
		includeMJ := true
		if filter.Status != "" {
			includeMJ = false
			for _, st := range strings.Split(filter.Status, ",") {
				if strings.TrimSpace(st) == "unknown" {
					includeMJ = true
					break
				}
			}
		}
		if !includeMJ {
			mjSQL += " AND 1=0"
			mjCountSQL += " AND 1=0"
		}
		if filter.Model != "" {
			mjSQL += " AND model LIKE ?"
			mjArgs = append(mjArgs, "%"+filter.Model+"%")
			mjCountSQL += " AND model LIKE ?"
			mjCountArgs = append(mjCountArgs, "%"+filter.Model+"%")
		}
		mjSQL, mjArgs = mediaTaskAppendTime(mjSQL, mjArgs, filter)
		mjCountSQL, mjCountArgs = mediaTaskAppendTime(mjCountSQL, mjCountArgs, filter)
		sources = append(sources, mediaTaskSource{selectSQL: mjSQL, args: mjArgs, countSQL: mjCountSQL, countArgs: mjCountArgs, alias: "media_midjourney_top"})
	}

	if slices.Contains(branches, MediaTaskKindVideo) {
		statusCond, statusArgs := mediaTaskStatusCond(filter.Status)
		videoSQL := `SELECT owner_user_id, 'video' AS kind, upstream_task_id AS task_id, model, status, progress, fail_reason, result_url, submit_time, start_time, finish_time, created_at FROM video_tasks WHERE ` + ownerCond
		videoArgs := append([]any{}, ownerArgs...)
		videoCountSQL := `SELECT COUNT(*) FROM video_tasks WHERE ` + ownerCond
		videoCountArgs := append([]any{}, ownerArgs...)
		videoSQL += statusCond
		videoArgs = append(videoArgs, statusArgs...)
		videoCountSQL += statusCond
		videoCountArgs = append(videoCountArgs, statusArgs...)
		if filter.Model != "" {
			videoSQL += " AND model LIKE ?"
			videoArgs = append(videoArgs, "%"+filter.Model+"%")
			videoCountSQL += " AND model LIKE ?"
			videoCountArgs = append(videoCountArgs, "%"+filter.Model+"%")
		}
		videoSQL, videoArgs = mediaTaskAppendTime(videoSQL, videoArgs, filter)
		videoCountSQL, videoCountArgs = mediaTaskAppendTime(videoCountSQL, videoCountArgs, filter)
		sources = append(sources, mediaTaskSource{selectSQL: videoSQL, args: videoArgs, countSQL: videoCountSQL, countArgs: videoCountArgs, alias: "media_video_top"})
	}

	pageParts := []string{}
	pageArgs := []any{}
	countSources := []mediaTaskSource{}
	for _, src := range sources {
		pageParts = append(pageParts, "SELECT * FROM ("+src.selectSQL+" ORDER BY created_at DESC, task_id ASC LIMIT ?) AS "+src.alias)
		pageArgs = append(pageArgs, append(append([]any{}, src.args...), topN)...)
		countSources = append(countSources, src)
	}
	pageSQL := "SELECT * FROM (" + strings.Join(pageParts, " UNION ALL ") + ") AS media_tasks_union ORDER BY created_at DESC, kind ASC, task_id ASC LIMIT ? OFFSET ?"
	pageArgs = append(pageArgs, pageSize, (page-1)*pageSize)
	return pageSQL, pageArgs, countSources, nil
}

// MediaTaskListByOwner 合并查询当前调用者的图片/视频任务（分页 + 类型/状态/模型/时间过滤）。
func MediaTaskListByOwner(ctx context.Context, filter MediaTaskListFilter) ([]MediaTaskRow, int64, error) {
	query, queryArgs, countSources, err := mediaTaskBuildQuery(filter)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	for _, src := range countSources {
		var count int64
		if err := db.GetDB().WithContext(ctx).Raw(src.countSQL, src.countArgs...).Scan(&count).Error; err != nil {
			return nil, 0, err
		}
		total += count
	}
	rows := []MediaTaskRow{}
	if err := db.GetDB().WithContext(ctx).Raw(query, queryArgs...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}
