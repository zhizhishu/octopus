package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 统一异步任务接口 /v1/tasks，形状照 new-api 最新版：
//   - router/task-router.go：POST /v1/tasks/:key 提交、GET /v1/tasks/:key 查一条
//     （同一个 :key 位置，提交时是任务类型、查询时是任务 id）；
//   - controller/task.go GetTask：{task_id, platform, status, progress, fail_reason,
//     created_at, finished_at}，按调用者归属查，查不到 404 {"error":{message,type}}；
//   - controller/relay.go presentTaskSubmission 的兜底形状 {id, task_id, status, model, created_at}；
//   - router/video-router.go：GET /v1/video/generations/:task_id 回 {"code":"success","data":TaskDto}，
//     查不到 400 {"code":"task_not_exist"}（relay/relay_task.go videoFetchByIDRespBodyBuilder）。
//
// octopus 没有 new-api 那套任务插件，所以 :key 是 octopus 已有的三类异步任务，提交**原样委托**
// 给已有的处理器（路由、重试、熔断、按次计费、日志全部是那条路径自己的，一行不复制）：
//   video[-generations|-edits|-extensions] → POST /v1/videos[/<kind>]（VideosHandlerKind）
//   image[-edits]                          → POST /v1/images/{generations,edits}/async（ImagesAsyncSubmit）
//   midjourney|mj（?mode=imagine|blend|…）→ POST /mj/submit/<mode>（MidjourneySubmitHandler）
// 委托处理器的回包被截下来：成功且拿得到任务 id 时换成统一任务对象；否则（报错、mj 未受理）
// 原样透传状态码、头和正文，客户端看到的错误与原生端点一字不差。
//
// GET /v1/tasks 列表是 new-api 没有的扩展（new-api 的列表只在控制台 /api/task/self）：
// 用户要求 submit/fetch/list 三件套，列表按同样的归属规则归并三张表。
//
// 归属：与各原生查询端点同一套规则（视频/MJ：同一把 key，存量无 token 行按用户；图片：key 与
// 用户都要对上），所以 /v1/tasks 永远不会比原生端点看到更多。另外按 key 的端点族过滤：
// 只开了 video 族的 key 看不到图片/MJ 任务，反之亦然。

const (
	TaskKindVideo      = "video"
	TaskKindImage      = "image"
	TaskKindMidjourney = "midjourney"

	// new-api model.TaskStatus 的取值。
	TaskStatusNotStart   = "NOT_START"
	TaskStatusSubmitted  = "SUBMITTED"
	TaskStatusQueued     = "QUEUED"
	TaskStatusInProgress = "IN_PROGRESS"
	TaskStatusFailure    = "FAILURE"
	TaskStatusSuccess    = "SUCCESS"
	TaskStatusUnknown    = "UNKNOWN"

	taskListDefaultPageSize = 20
	taskListMaxPageSize     = 100
	// taskListMaxWindow 限制 page*page_size：统一列表要从每张表取到 offset+page_size 行再归并，
	// 不设上限的深翻页会让一次请求读上万行。
	taskListMaxWindow = 2000

	// APIKeyEndpointFamiliesContextKey 由 APIKeyAuth 写入：当前 key 允许的端点族。
	APIKeyEndpointFamiliesContextKey = "api_key_endpoint_families"
)

// TaskView 是统一任务对象。前七个字段与 new-api GetTask 同名同义；其余是 octopus 的补充。
type TaskView struct {
	TaskID     string `json:"task_id"`
	Platform   string `json:"platform"`
	Status     string `json:"status"`
	Progress   string `json:"progress"`
	FailReason string `json:"fail_reason"`
	CreatedAt  int64  `json:"created_at"`
	FinishedAt int64  `json:"finished_at"`

	ID        string `json:"id"`
	Object    string `json:"object"`
	Kind      string `json:"kind"`
	Action    string `json:"action,omitempty"`
	Model     string `json:"model"`
	ResultURL string `json:"result_url,omitempty"`
	SubmitAt  int64  `json:"submit_time,omitempty"`
	StartAt   int64  `json:"start_time,omitempty"`
}

func newTaskView(kind, id string) TaskView {
	return TaskView{TaskID: id, ID: id, Platform: kind, Kind: kind, Object: "task", Status: TaskStatusUnknown}
}

// taskError 是 new-api videoProxyError 的形状。
func taskError(c *gin.Context, status int, errType, message string) {
	c.Header("Cache-Control", "private, no-store")
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": errType}})
}

// ---------- 归属与端点族 ----------

func taskScope(c *gin.Context) op.TaskOwnerScope {
	return op.TaskOwnerScope{TokenID: c.GetInt("api_key_id"), UserID: taskOwnerUserID(c)}
}

// allowedTaskKinds 按 key 的端点族决定能看哪些任务。上下文里没有族信息 → 一个都不给（fail-closed）。
func allowedTaskKinds(c *gin.Context) map[string]bool {
	out := map[string]bool{}
	raw, ok := c.Get(APIKeyEndpointFamiliesContextKey)
	if !ok {
		return out
	}
	families, _ := raw.([]model.APIKeyEndpointFamily)
	for _, f := range families {
		switch f {
		case model.APIKeyEndpointFamilyVideo:
			out[TaskKindVideo] = true
		case model.APIKeyEndpointFamilyOpenAICompatible:
			out[TaskKindImage] = true
			out[TaskKindMidjourney] = true
		}
	}
	return out
}

func normalizeTaskKind(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "video", "videos":
		return TaskKindVideo, true
	case "image", "images":
		return TaskKindImage, true
	case "midjourney", "mj":
		return TaskKindMidjourney, true
	}
	return "", false
}

// ---------- 状态映射 ----------

func videoTaskUnifiedStatus(s model.VideoTaskStatus) string {
	switch s {
	case model.VideoTaskNotStart:
		return TaskStatusNotStart
	case model.VideoTaskSubmitted:
		return TaskStatusSubmitted
	case model.VideoTaskQueued:
		return TaskStatusQueued
	case model.VideoTaskInProgress:
		return TaskStatusInProgress
	case model.VideoTaskSuccess:
		return TaskStatusSuccess
	case model.VideoTaskFailure:
		return TaskStatusFailure
	case "":
		// status 列允许 NULL（见 op.videoTaskNotTerminalWhere），等同 not_start。
		return TaskStatusNotStart
	}
	return TaskStatusUnknown
}

func imageTaskUnifiedStatus(s string) string {
	switch model.NormalizeImageTaskStatus(s) {
	case model.ImageTaskStatusCompleted:
		return TaskStatusSuccess
	case model.ImageTaskStatusFailed:
		return TaskStatusFailure
	default:
		return TaskStatusInProgress
	}
}

func midjourneyUnifiedStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "NOT_START":
		return TaskStatusNotStart
	case "SUBMITTED":
		return TaskStatusSubmitted
	case "IN_PROGRESS", "MODAL":
		// MODAL = 等用户在弹窗里补参数（/mj/submit/modal），任务仍未结束。
		return TaskStatusInProgress
	case "SUCCESS":
		return TaskStatusSuccess
	case "FAILURE", "CANCEL":
		return TaskStatusFailure
	}
	return TaskStatusUnknown
}

// unifiedStatusFilter 把 ?status= 换算成两张本地表的原生状态。MJ 的状态只在上游，
// 带 status 过滤时 MJ 不参与列表（文档写明）。
func unifiedStatusFilter(raw string) (video []model.VideoTaskStatus, image []string, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case TaskStatusNotStart:
		return []model.VideoTaskStatus{model.VideoTaskNotStart}, nil, true
	case TaskStatusSubmitted:
		return []model.VideoTaskStatus{model.VideoTaskSubmitted}, nil, true
	case TaskStatusQueued:
		return []model.VideoTaskStatus{model.VideoTaskQueued}, nil, true
	case TaskStatusInProgress:
		return []model.VideoTaskStatus{model.VideoTaskInProgress}, []string{model.ImageTaskStatusProcessing}, true
	case TaskStatusSuccess:
		return []model.VideoTaskStatus{model.VideoTaskSuccess}, []string{model.ImageTaskStatusCompleted}, true
	case TaskStatusFailure:
		return []model.VideoTaskStatus{model.VideoTaskFailure}, []string{model.ImageTaskStatusFailed}, true
	case TaskStatusUnknown:
		return []model.VideoTaskStatus{model.VideoTaskUnknown}, nil, true
	}
	return nil, nil, false
}

// ---------- 行 → 统一对象 ----------

func videoTaskView(t model.VideoTask) TaskView {
	v := newTaskView(TaskKindVideo, t.UpstreamTaskID)
	v.Status = videoTaskUnifiedStatus(t.Status)
	v.Progress = t.Progress
	v.FailReason = t.FailReason
	v.CreatedAt = t.CreatedAt
	v.FinishedAt = t.FinishTime
	v.Model = t.Model
	v.Action = t.MediaKind
	if v.Action == "" {
		v.Action = "generate"
	}
	v.ResultURL = t.ResultURL
	v.SubmitAt = t.SubmitTime
	v.StartAt = t.StartTime
	if v.Status == TaskStatusSuccess && v.Progress == "" {
		v.Progress = "100%"
	}
	return v
}

func imageTaskView(t model.ImageTask) TaskView {
	v := newTaskView(TaskKindImage, t.TaskID)
	v.Status = imageTaskUnifiedStatus(t.Status)
	v.CreatedAt = t.CreatedAt
	v.SubmitAt = t.CreatedAt
	v.Model = t.Model
	v.Action = t.Endpoint
	if v.Action == "" {
		v.Action = model.ImageTaskEndpointGenerations
	}
	switch v.Status {
	case TaskStatusSuccess:
		v.Progress = "100%"
		v.FinishedAt = t.CompletedAt
		v.ResultURL = imageTaskImageURL(t.ResultJSON)
	case TaskStatusFailure:
		v.Progress = "100%"
		v.FinishedAt = t.CompletedAt
		// 与 /v1/images/tasks/:id 一样只给一句话，不外露上游原始返回。
		v.FailReason = strings.TrimSpace(t.ErrorMessage)
		if v.FailReason == "" {
			v.FailReason = "image generation failed"
		}
	default:
		v.Progress = "0%"
	}
	return v
}

func midjourneyRowView(t model.MidjourneyTask) TaskView {
	v := newTaskView(TaskKindMidjourney, t.UpstreamTaskID)
	v.CreatedAt = t.CreatedAt
	v.Model = t.Model
	v.Action = strings.ToLower(t.Action)
	return v
}

// midjourneyUpstreamTask 是 midjourney-proxy 的任务对象里统一视图要用的字段。
type midjourneyUpstreamTask struct {
	ID         string `json:"id"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Progress   string `json:"progress"`
	FailReason string `json:"failReason"`
	SubmitTime int64  `json:"submitTime"`
	StartTime  int64  `json:"startTime"`
	FinishTime int64  `json:"finishTime"`
	ImageURL   string `json:"imageUrl"`
	VideoURL   string `json:"videoUrl"`
}

// mjSeconds：midjourney-proxy 的时间戳是毫秒，统一视图用秒（与另两类一致）。
func mjSeconds(ts int64) int64 {
	if ts > 1e12 {
		return ts / 1000
	}
	return ts
}

func applyMidjourneyUpstream(v *TaskView, u midjourneyUpstreamTask) {
	v.Status = midjourneyUnifiedStatus(u.Status)
	v.Progress = u.Progress
	v.FailReason = u.FailReason
	v.SubmitAt = mjSeconds(u.SubmitTime)
	v.StartAt = mjSeconds(u.StartTime)
	v.FinishedAt = mjSeconds(u.FinishTime)
	if u.Action != "" {
		v.Action = strings.ToLower(u.Action)
	}
	v.ResultURL = u.ImageURL
	if u.VideoURL != "" {
		v.ResultURL = u.VideoURL
	}
}

// ---------- 按 id 取一条（归属内） ----------

func lookupOwnedVideoTask(ctx context.Context, c *gin.Context, id string) (*model.VideoTask, error) {
	row, err := op.VideoTaskGetByUpstreamID(ctx, id)
	if err != nil || row == nil {
		return nil, err
	}
	entry := videoTaskEntry{ownerTokenID: row.OwnerTokenID, ownerUserID: row.OwnerUserID}
	if !taskOwnerMatchesRequest(c, entry.ownerTokenID, entry.ownerUserID) {
		return nil, nil
	}
	return row, nil
}

func lookupOwnedImageTask(ctx context.Context, c *gin.Context, id string) (*model.ImageTask, error) {
	scope := taskScope(c)
	row, err := op.ImageTaskGetForOwner(ctx, id, scope.TokenID, scope.UserID)
	if err != nil || row == nil {
		if err == op.ErrImageTaskOwnerScopeRequired {
			return nil, nil
		}
		return nil, err
	}
	if row.ExpiresAt > 0 && row.ExpiresAt <= time.Now().Unix() {
		return nil, nil
	}
	return row, nil
}

func lookupOwnedMidjourneyTask(ctx context.Context, c *gin.Context, id string) (*model.MidjourneyTask, midjourneyTaskEntry, bool) {
	entry, ok := midjourneyTaskOwner(ctx, id)
	if !ok || !taskOwnerMatchesRequest(c, entry.ownerTokenID, entry.ownerUserID) {
		return nil, midjourneyTaskEntry{}, false
	}
	row, err := op.MidjourneyTaskGetByUpstreamID(ctx, id)
	if err != nil || row == nil {
		row = &model.MidjourneyTask{UpstreamTaskID: id, Model: entry.model, ChannelID: entry.channelID, ChannelKeyID: entry.channelKeyID}
	}
	return row, entry, true
}

// fetchMidjourneyStatus 向任务所属渠道/key 问一次 /mj/task/:id/fetch，并按 MJ 查询端点的口径落一条日志
// （images_midjourney_task，不计费）。失败时 view 保持 UNKNOWN，不让一次上游抖动变成 404。
func fetchMidjourneyStatus(c *gin.Context, ctx context.Context, view *TaskView, entry midjourneyTaskEntry) {
	channel, ok := midjourneyTaskChannel(ctx, entry)
	if !ok {
		return
	}
	key, ok := videoChannelKeyByID(channel, entry.channelKeyID)
	if !ok {
		return
	}
	metrics := newRawProtocolRelayMetrics(c.GetInt("api_key_id"), c.GetInt("user_id"), c.GetString("request_ip"), entry.model, midjourneyTaskLogName, c.Request.URL.Path)
	metrics.ActualModel = entry.model
	// 调试模式：挂记录器（只读观察，关着时 ctx 原样）。
	ctx = metrics.enableDebugCapture(ctx, c.Request.Header)
	metrics.debug.target(channel, entry.channelKeyID)
	started := time.Now()
	// c 传 nil：不把 /v1/tasks 的查询参数（kind= 之类）带到上游。
	_, body, _, err := midjourneyForward(ctx, nil, http.MethodGet, "/mj/task/"+url.PathEscape(view.TaskID)+"/fetch", nil, channel, key)
	if err != nil {
		metrics.Save(ctx, false, err, midjourneySingleAttempt(channel, entry.channelKeyID, entry.model, false, started, auditErrorMessage(err)))
		return
	}
	metrics.ResponseContent = "midjourney task fetch (unified /v1/tasks)"
	metrics.Save(ctx, true, nil, midjourneySingleAttempt(channel, entry.channelKeyID, entry.model, true, started, ""))
	var u midjourneyUpstreamTask
	if json.Unmarshal(body, &u) == nil {
		applyMidjourneyUpstream(view, u)
	}
}

// findOwnedTask 依次在允许的类型里找 id。kindHint 非空时只找那一类。
func findOwnedTask(c *gin.Context, id, kindHint string) (*TaskView, error) {
	ctx := c.Request.Context()
	allowed := allowedTaskKinds(c)
	want := func(kind string) bool { return allowed[kind] && (kindHint == "" || kindHint == kind) }

	if want(TaskKindImage) {
		row, err := lookupOwnedImageTask(ctx, c, id)
		if err != nil {
			return nil, err
		}
		if row != nil {
			v := imageTaskView(*row)
			return &v, nil
		}
	}
	if want(TaskKindVideo) {
		row, err := lookupOwnedVideoTask(ctx, c, id)
		if err != nil {
			return nil, err
		}
		if row != nil {
			v := videoTaskView(*row)
			return &v, nil
		}
	}
	if want(TaskKindMidjourney) {
		if row, entry, ok := lookupOwnedMidjourneyTask(ctx, c, id); ok {
			v := midjourneyRowView(*row)
			fetchMidjourneyStatus(c, ctx, &v, entry)
			return &v, nil
		}
	}
	return nil, nil
}

// TaskGetHandler 处理 GET /v1/tasks/:key（:key 在这里是任务 id）。可选 ?kind=video|image|midjourney。
func TaskGetHandler(c *gin.Context) {
	id := strings.TrimSpace(c.Param("key"))
	if id == "" {
		taskError(c, http.StatusBadRequest, "invalid_request_error", "task id is required")
		return
	}
	kindHint := ""
	if raw := c.Query("kind"); raw != "" {
		k, ok := normalizeTaskKind(raw)
		if !ok {
			taskError(c, http.StatusBadRequest, "invalid_request_error", "unknown task kind: "+raw)
			return
		}
		kindHint = k
	}
	view, err := findOwnedTask(c, id, kindHint)
	if err != nil {
		log.Warnf("unified task query %s failed: %v", id, err)
		taskError(c, http.StatusInternalServerError, "server_error", "Failed to query task")
		return
	}
	if view == nil {
		taskError(c, http.StatusNotFound, "invalid_request_error", "Task not found")
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, view)
}

// VideoGenerationGetHandler 处理 GET /v1/video/generations/:task_id（new-api 路由），
// 回 new-api 的 {"code":"success","data":TaskDto}。只认视频任务。
func VideoGenerationGetHandler(c *gin.Context) {
	id := strings.TrimSpace(c.Param("task_id"))
	var row *model.VideoTask
	if id != "" && allowedTaskKinds(c)[TaskKindVideo] {
		var err error
		row, err = lookupOwnedVideoTask(c.Request.Context(), c, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": "get_task_failed", "message": "get_task_failed", "data": nil})
			return
		}
	}
	if row == nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "task_not_exist", "message": "task_not_exist", "data": nil})
		return
	}
	v := videoTaskView(*row)
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{
		"code": "success",
		"data": gin.H{
			"task_id":     v.TaskID,
			"platform":    v.Platform,
			"action":      v.Action,
			"status":      v.Status,
			"fail_reason": v.FailReason,
			"result_url":  v.ResultURL,
			"submit_time": v.SubmitAt,
			"start_time":  v.StartAt,
			"finish_time": v.FinishedAt,
			"progress":    v.Progress,
			"created_at":  row.CreatedAt,
			"updated_at":  row.UpdatedAt,
			"model":       v.Model,
		},
	})
}

// ---------- 列表 ----------

type taskListResponse struct {
	Object   string     `json:"object"`
	Data     []TaskView `json:"data"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	HasMore  bool       `json:"has_more"`
}

func parsePositiveQuery(c *gin.Context, name string, def int) (int, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// TaskListHandler 处理 GET /v1/tasks?kind=&status=&page=&page_size=。
func TaskListHandler(c *gin.Context) {
	ctx := c.Request.Context()
	page, ok := parsePositiveQuery(c, "page", 1)
	if !ok {
		taskError(c, http.StatusBadRequest, "invalid_request_error", "page must be a positive integer")
		return
	}
	pageSize, ok := parsePositiveQuery(c, "page_size", taskListDefaultPageSize)
	if !ok {
		taskError(c, http.StatusBadRequest, "invalid_request_error", "page_size must be a positive integer")
		return
	}
	if pageSize > taskListMaxPageSize {
		pageSize = taskListMaxPageSize
	}
	window := page * pageSize
	if window > taskListMaxWindow {
		taskError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("page*page_size must not exceed %d", taskListMaxWindow))
		return
	}
	allowed := allowedTaskKinds(c)
	if raw := c.Query("kind"); raw != "" {
		k, ok := normalizeTaskKind(raw)
		if !ok {
			taskError(c, http.StatusBadRequest, "invalid_request_error", "unknown task kind: "+raw)
			return
		}
		allowed = map[string]bool{k: allowed[k]}
	}
	var (
		videoStatuses []model.VideoTaskStatus
		imageStatuses []string
		statusFilter  bool
	)
	if raw := c.Query("status"); raw != "" {
		v, i, ok := unifiedStatusFilter(raw)
		if !ok {
			taskError(c, http.StatusBadRequest, "invalid_request_error", "unknown task status: "+raw)
			return
		}
		videoStatuses, imageStatuses, statusFilter = v, i, true
	}

	scope := taskScope(c)
	w := op.TaskListWindow{Limit: window}
	var (
		views []TaskView
		total int64
	)
	if allowed[TaskKindVideo] {
		rows, n, err := op.VideoTaskListOwned(ctx, scope, videoStatuses, w)
		if err != nil {
			taskListFailed(c, err)
			return
		}
		total += n
		for _, r := range rows {
			views = append(views, videoTaskView(r))
		}
	}
	if allowed[TaskKindImage] && (!statusFilter || len(imageStatuses) > 0) {
		rows, n, err := op.ImageTaskListOwned(ctx, scope, imageStatuses, time.Now().Unix(), w)
		if err != nil {
			taskListFailed(c, err)
			return
		}
		total += n
		for _, r := range rows {
			views = append(views, imageTaskView(r))
		}
	}
	var mjRows []model.MidjourneyTask
	if allowed[TaskKindMidjourney] && !statusFilter {
		rows, n, err := op.MidjourneyTaskListOwned(ctx, scope, w)
		if err != nil {
			taskListFailed(c, err)
			return
		}
		total += n
		mjRows = rows
		for _, r := range rows {
			views = append(views, midjourneyRowView(r))
		}
	}

	sort.SliceStable(views, func(i, j int) bool {
		if views[i].CreatedAt != views[j].CreatedAt {
			return views[i].CreatedAt > views[j].CreatedAt
		}
		return views[i].TaskID > views[j].TaskID
	})
	start := (page - 1) * pageSize
	if start > len(views) {
		start = len(views)
	}
	end := start + pageSize
	if end > len(views) {
		end = len(views)
	}
	pageViews := append([]TaskView(nil), views[start:end]...)
	if len(mjRows) > 0 {
		fillMidjourneyStatuses(c, ctx, pageViews, mjRows)
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, taskListResponse{
		Object:   "list",
		Data:     pageViews,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
		HasMore:  int64(start+len(pageViews)) < total,
	})
}

func taskListFailed(c *gin.Context, err error) {
	log.Warnf("unified task list failed: %v", err)
	taskError(c, http.StatusInternalServerError, "server_error", "Failed to list tasks")
}

// fillMidjourneyStatuses 只为本页的 MJ 任务问上游：按「渠道 + key」分组各发一次
// /mj/task/list-by-condition（与 MidjourneyTaskListByConditionHandler 同一做法），某组失败只让那组保持 UNKNOWN。
func fillMidjourneyStatuses(c *gin.Context, ctx context.Context, page []TaskView, rows []model.MidjourneyTask) {
	byID := make(map[string]model.MidjourneyTask, len(rows))
	for _, r := range rows {
		byID[r.UpstreamTaskID] = r
	}
	type groupKey struct{ channelID, keyID int }
	groups := map[groupKey][]int{}
	var order []groupKey
	for i := range page {
		if page[i].Kind != TaskKindMidjourney {
			continue
		}
		r, ok := byID[page[i].TaskID]
		if !ok || r.ChannelID == 0 || r.ChannelKeyID == 0 {
			continue
		}
		k := groupKey{r.ChannelID, r.ChannelKeyID}
		if _, exists := groups[k]; !exists {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	if len(order) == 0 {
		return
	}
	metrics := newRawProtocolRelayMetrics(c.GetInt("api_key_id"), c.GetInt("user_id"), c.GetString("request_ip"), "", midjourneyTaskLogName, c.Request.URL.Path)
	// 调试模式：挂记录器（只读观察，关着时 ctx 原样）。
	ctx = metrics.enableDebugCapture(ctx, c.Request.Header)
	var attempts []model.ChannelAttempt
	var lastErr error
	for _, k := range order {
		started := time.Now()
		channel, ok := midjourneyTaskChannel(ctx, midjourneyTaskEntry{channelID: k.channelID})
		if !ok {
			continue
		}
		key, ok := videoChannelKeyByID(channel, k.keyID)
		if !ok {
			continue
		}
		ids := make([]string, 0, len(groups[k]))
		for _, i := range groups[k] {
			ids = append(ids, page[i].TaskID)
		}
		body, _ := json.Marshal(map[string]any{"ids": ids})
		metrics.debug.target(channel, k.keyID)
		_, respBody, _, err := midjourneyForward(ctx, nil, http.MethodPost, "/mj/task/list-by-condition", body, channel, key)
		if err != nil {
			lastErr = err
			attempts = append(attempts, midjourneySingleAttempt(channel, k.keyID, "", false, started, auditErrorMessage(err))...)
			log.Warnf("unified task list: midjourney list-by-condition on channel %d failed: %v", channel.ID, err)
			continue
		}
		attempts = append(attempts, midjourneySingleAttempt(channel, k.keyID, "", true, started, "")...)
		var items []midjourneyUpstreamTask
		if json.Unmarshal(bytes.TrimSpace(respBody), &items) != nil {
			continue
		}
		upstream := make(map[string]midjourneyUpstreamTask, len(items))
		for _, it := range items {
			upstream[it.ID] = it
		}
		for _, i := range groups[k] {
			if u, ok := upstream[page[i].TaskID]; ok {
				applyMidjourneyUpstream(&page[i], u)
			}
		}
	}
	metrics.ResponseContent = "midjourney task list (unified /v1/tasks)"
	metrics.Save(ctx, lastErr == nil, lastErr, attempts)
}
