package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 「图片/视频任务」合并列表接口（fix-ui1127a 第 1 条第三块）。
//
// 字段与 videoTaskListItem 逐字对齐（task_id/model/status/progress/fail_reason/
// result_url/submit_time/start_time/finish_time/created_at），权限口径同视频任务：
// 只认会话身份，永远只看自己的任务。新增一个维度 kind（image/video）供类型筛选。
//
// 为什么不分开两个接口让前端拼：混合类型下两边各自分页，合并后的页码永远对不上；
// 合并必须发生在 SQL 层（见 op/media_task.go）。

func init() {
	router.NewGroupRouter("/api/v1/media-task").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listMediaTasks),
		)
}

type mediaTaskListItem struct {
	// UserID 是归属用户（管理员看全部用户的任务时一清二楚；普通用户只看自己，字段无害）。
	UserID     int64  `json:"user_id"`
	Kind       string `json:"kind"`
	TaskID     string `json:"task_id"`
	Model      string `json:"model"`
	Status     string `json:"status"`
	Progress   string `json:"progress"`
	FailReason string `json:"fail_reason"`
	ResultURL  string `json:"result_url"`
	SubmitTime int64  `json:"submit_time"`
	StartTime  int64  `json:"start_time"`
	FinishTime int64  `json:"finish_time"`
	CreatedAt  int64  `json:"created_at"`
}

type mediaTaskListResponse struct {
	Items    []mediaTaskListItem `json:"items"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

// listMediaTasks 返回图片/视频合并任务列表。权限口径（fix-ui1127a-r1 第 6 条）：
// 普通用户只看自己的任务；管理员看全部用户的任务，可用 user_id 参数收窄到具体用户
// ——octopus 作为中间层，管理员对下游谁跑了什么任务要一清二楚。
func listMediaTasks(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	if userID <= 0 {
		// Fail-closed 与视频任务一致：没有身份就没有「自己的任务」。
		resp.Error(c, http.StatusForbidden, "media task owner is required")
		return
	}
	isAdmin := middleware.CurrentUserIsAdmin(c)
	ownerID := userID
	allUsers := false
	if isAdmin {
		allUsers = true
		if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed <= 0 {
				resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
				return
			}
			ownerID = int(parsed)
		} else {
			ownerID = 0
		}
	}

	kind := op.MediaTaskKind(strings.TrimSpace(c.Query("type")))
	switch kind {
	case "", op.MediaTaskKindImage, op.MediaTaskKindVideo:
	default:
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", strconv.Itoa(videoTaskDefaultPageSize)))
	if pageSize < 1 || pageSize > videoTaskMaxPageSize {
		pageSize = videoTaskDefaultPageSize
	}
	startTime, _ := strconv.ParseInt(c.DefaultQuery("start_time", "0"), 10, 64)
	endTime, _ := strconv.ParseInt(c.DefaultQuery("end_time", "0"), 10, 64)

	rows, total, err := op.MediaTaskListByOwner(c.Request.Context(), op.MediaTaskListFilter{
		OwnerUserID: ownerID,
		AllUsers:    allUsers,
		Kind:        kind,
		Status:      strings.TrimSpace(c.Query("status")),
		Model:       strings.TrimSpace(c.Query("model")),
		StartTime:   startTime,
		EndTime:     endTime,
		Page:        page,
		PageSize:    pageSize,
	})
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	// 图片行的结果地址不在 UNION 里（JSON 提取不该进 SQL）：本页完成的图片任务
	// 一次性补查 ResultJSON 提取（写库时已剔除 b64，见 imageTaskResultPayload）。
	urls := imageTaskResultURLs(c, rows)
	items := make([]mediaTaskListItem, 0, len(rows))
	for _, row := range rows {
		resultURL := row.ResultURL
		if row.Kind == string(op.MediaTaskKindImage) {
			resultURL = urls[row.TaskID]
		}
		items = append(items, mediaTaskListItem{
			UserID:     row.OwnerUserID,
			Kind:       row.Kind,
			TaskID:     row.TaskID,
			Model:      row.Model,
			Status:     row.Status,
			Progress:   row.Progress,
			FailReason: row.FailReason,
			ResultURL:  resultURL,
			SubmitTime: row.SubmitTime,
			StartTime:  row.StartTime,
			FinishTime: row.FinishTime,
			CreatedAt:  row.CreatedAt,
		})
	}
	resp.Success(c, mediaTaskListResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// imageTaskResultURL 从 image_tasks.ResultJSON（写库时已剔除 b64）提取第一张图的
// 对外地址；提取不到就是空串——绝不拿 base64 或上游内网地址充数。
func imageTaskResultURL(resultJSON string) string {
	if resultJSON == "" {
		return ""
	}
	var payload struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &payload); err != nil {
		return ""
	}
	for _, item := range payload.Data {
		if item.URL != "" {
			return item.URL
		}
	}
	return ""
}

// imageTaskResultURLs 批量取本页图片任务的首图地址（task_id → url）。只查 completed 的
// image 行；查不到/解析失败就是空串，不充数。midjourney 行没有本地结果（状态在上游），跳过。
func imageTaskResultURLs(c *gin.Context, rows []op.MediaTaskRow) map[string]string {
	out := map[string]string{}
	ids := []string{}
	for _, row := range rows {
		if row.Kind == string(op.MediaTaskKindImage) && row.Status == "completed" {
			ids = append(ids, row.TaskID)
		}
	}
	if len(ids) == 0 {
		return out
	}
	var tasks []model.ImageTask
	if err := db.GetDB().WithContext(c.Request.Context()).Select("task_id", "result_json").Where("task_id IN ?", ids).Find(&tasks).Error; err != nil {
		return out
	}
	for _, task := range tasks {
		if url := imageTaskResultURL(task.ResultJSON); url != "" {
			out[task.TaskID] = url
		}
	}
	return out
}
