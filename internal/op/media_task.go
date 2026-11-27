package op

import (
	"context"
	"fmt"
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

// mediaTaskBuildQuery 拼出 UNION 子查询 + 参数。status/model 过滤逐段应用（包进子查询里，
// SQL 层过滤，不让总行数虚涨）；时间过滤在 UNION 外层。参数按占位符在 SQL 文本里的
// 出现顺序追加——image 分支是两段 SELECT（image_tasks 在前、midjourney_tasks 在后），
// 各自的过滤参数必须跟在自己那段后面。
func mediaTaskBuildQuery(filter MediaTaskListFilter) (string, []any, error) {
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
		return "", nil, fmt.Errorf("未知任务类型 %q", string(filter.Kind))
	}

	args := []any{}
	parts := []string{}
	for _, kind := range branches {
		if kind == MediaTaskKindImage {
			seg1 := `SELECT owner_user_id, 'image' AS kind, task_id, model, status, '' AS progress, error_message AS fail_reason, '' AS result_url, 0 AS submit_time, 0 AS start_time, completed_at AS finish_time, created_at FROM image_tasks WHERE ` + ownerCond
			seg1Args := append([]any{}, ownerArgs...)
			seg2 := `SELECT owner_user_id, 'image' AS kind, upstream_task_id AS task_id, model, 'unknown' AS status, '' AS progress, '' AS fail_reason, '' AS result_url, 0 AS submit_time, 0 AS start_time, 0 AS finish_time, created_at FROM midjourney_tasks WHERE ` + ownerCond
			seg2Args := append([]any{}, ownerArgs...)
			if filter.Status != "" {
				if filter.Status == "unknown" {
					// MJ 行恒定 unknown：过滤 unknown 时 image_tasks 段整段排除。
					seg1 += " AND 1=0"
				} else {
					statuses := strings.Split(filter.Status, ",")
					placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
					args := make([]any, 0, len(statuses))
					for _, st := range statuses {
						args = append(args, strings.TrimSpace(st))
					}
					seg1 += " AND status IN (" + placeholders + ")"
					seg1Args = append(seg1Args, args...)
					seg2 += " AND 1=0"
				}
			}
			if filter.Model != "" {
				seg1 += " AND model LIKE ?"
				seg1Args = append(seg1Args, "%"+filter.Model+"%")
				seg2 += " AND model LIKE ?"
				seg2Args = append(seg2Args, "%"+filter.Model+"%")
			}
			parts = append(parts, seg1+" UNION ALL "+seg2)
			args = append(args, seg1Args...)
			args = append(args, seg2Args...)
		} else {
			seg := `SELECT owner_user_id, 'video' AS kind, upstream_task_id AS task_id, model, status, progress, fail_reason, result_url, submit_time, start_time, finish_time, created_at FROM video_tasks WHERE ` + ownerCond
			segArgs := append([]any{}, ownerArgs...)
			if filter.Status != "" {
				statuses := strings.Split(filter.Status, ",")
				placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
				seg += " AND status IN (" + placeholders + ")"
				for _, st := range statuses {
					segArgs = append(segArgs, strings.TrimSpace(st))
				}
			}
			if filter.Model != "" {
				seg += " AND model LIKE ?"
				segArgs = append(segArgs, "%"+filter.Model+"%")
			}
			parts = append(parts, seg)
			args = append(args, segArgs...)
		}
	}

	union := strings.Join(parts, " UNION ALL ")
	var outer strings.Builder
	outer.WriteString("SELECT * FROM (")
	outer.WriteString(union)
	outer.WriteString(")")
	where := []string{}
	if filter.StartTime > 0 {
		where = append(where, "created_at >= ?")
		args = append(args, filter.StartTime)
	}
	if filter.EndTime > 0 {
		where = append(where, "created_at <= ?")
		args = append(args, filter.EndTime)
	}
	if len(where) > 0 {
		outer.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	return outer.String(), args, nil
}

// MediaTaskListByOwner 合并查询当前调用者的图片/视频任务（分页 + 类型/状态/模型/时间过滤）。
func MediaTaskListByOwner(ctx context.Context, filter MediaTaskListFilter) ([]MediaTaskRow, int64, error) {
	base, args, err := mediaTaskBuildQuery(filter)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := db.GetDB().WithContext(ctx).Raw("SELECT COUNT(*) FROM ("+base+")", args...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	page := filter.Page
	if page < 1 {
		page = 1
	}
	pageSize := filter.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	rows := []MediaTaskRow{}
	query := base + " ORDER BY created_at DESC, task_id ASC LIMIT ? OFFSET ?"
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	if err := db.GetDB().WithContext(ctx).Raw(query, queryArgs...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}
