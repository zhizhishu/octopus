package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 视频任务列表的分页默认值与上限，和日志列表（/api/v1/log/list）保持一致：
// 同一个页面上的两个视图用两套分页口径只会让人怀疑自己看错了。
const (
	videoTaskDefaultPageSize = 20
	videoTaskMaxPageSize     = 100
)

func init() {
	// 只读接口，挂在用户鉴权（JWT / 管理 token）之下。
	// 为什么不是 APIKeyAuth：这是给浏览器里的日志页用的列表，
	// 前端带的是登录态 JWT，没有 sk- key；而任务的归属用户与发起它的 key
	// 归属同一个用户（见 relay.recordVideoTaskRow），按 owner_user_id 过滤即可。
	router.NewGroupRouter("/api/v1/video-task").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listVideoTasks),
		)
}

// videoTaskListItem 是一条视频任务的对外形状。
//
// 字段名刻意对齐 new-api 的 dto.TaskDto 语义（task_id / status / progress /
// fail_reason / result_url / submit_time / finish_time）：从别的系统迁过来的
// 用户不该为了「看自己的视频任务」重新学一套字段名。
//
// 故意**不**返回的字段：channel_id / channel_key_id（key 的引用也不外露）、
// owner_*（调用方只看自己的任务，返回归属没有意义），以及任何计费字段
// ——本接口只负责「我有哪些任务、结果在哪」，不参与计费。
type videoTaskListItem struct {
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

type videoTaskListResponse struct {
	Items    []videoTaskListItem `json:"items"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

// videoTaskKnownStatuses 是 ?status= 的白名单，与 model.VideoTask* 常量一一对应。
// 新增状态时这里必须跟着加，否则新状态会被当成非法参数拒掉（失败会立刻可见，
// 不会像「静默不过滤」那样悄悄返回全部）。
var videoTaskKnownStatuses = []model.VideoTaskStatus{
	model.VideoTaskNotStart,
	model.VideoTaskSubmitted,
	model.VideoTaskQueued,
	model.VideoTaskInProgress,
	model.VideoTaskSuccess,
	model.VideoTaskFailure,
	model.VideoTaskUnknown,
}

// videoTaskStatusFromQuery 解析 ?status= 并**严格**校验取值。
//
// 为什么不在不认识的状态上「当作不过滤」：那样一个拼错的状态会静默返回
// 全部任务，用户看到的是「筛选不生效」，而真正的错被吃掉。这里直接 400。
// 空值表示不限状态（唯一的「全部」写法）。
func videoTaskStatusFromQuery(c *gin.Context) (model.VideoTaskStatus, bool) {
	raw := strings.TrimSpace(c.Query("status"))
	if raw == "" {
		return "", true
	}
	for _, known := range videoTaskKnownStatuses {
		if raw == string(known) {
			return known, true
		}
	}
	return "", false
}

// listVideoTasks 返回**当前调用者自己的**视频任务（分页 + 状态过滤）。
//
// 【红线】归属只认会话身份：不接受任何客户端传来的 user_id / token_id。
// 一旦允许客户端指定归属，这个接口就成了「查别人任务」的入口。
func listVideoTasks(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	if userID <= 0 {
		// Fail-closed：没有身份就没有「自己的任务」。这里**不能**退化成不过滤。
		resp.Error(c, http.StatusForbidden, "video task owner is required")
		return
	}

	status, ok := videoTaskStatusFromQuery(c)
	if !ok {
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

	tasks, total, err := op.VideoTaskListByOwner(c.Request.Context(), op.VideoTaskListFilter{
		OwnerUserID: userID,
		Status:      status,
		Page:        page,
		PageSize:    pageSize,
	})
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]videoTaskListItem, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, videoTaskListItem{
			TaskID:     task.UpstreamTaskID,
			Model:      task.Model,
			Status:     string(task.Status),
			Progress:   task.Progress,
			FailReason: task.FailReason,
			ResultURL:  task.ResultURL,
			SubmitTime: task.SubmitTime,
			StartTime:  task.StartTime,
			FinishTime: task.FinishTime,
			CreatedAt:  task.CreatedAt,
		})
	}

	resp.Success(c, videoTaskListResponse{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}
