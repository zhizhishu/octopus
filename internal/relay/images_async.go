package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/helper"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 图片异步任务（/v1/images/{generations,edits}/async + /v1/images/tasks/{id}）。
//
// 为什么要有这条路：同步版把"生成一张图"的全部 20–60 秒压在那一次 HTTP 请求上，
// 客户端切后台/网络抖动就断，断了之后用户不知道图到底画成没有，我们那边还占着一个
// goroutine 干等。异步版立刻回 202 + 任务号，真正的上游调用交给后台执行器
// （internal/task/image_task_poll.go），客户端过几秒来问一次。
//
// 【本文件不碰同步路径的转发】：新的图都从下面这几个出口进出。同步与异步只共用
// 计费口径（images_billing.go：配了按张价按张收，否则按 token 收）。
const (
	// imageTaskTTL 是任务从提交到"再也查不到"的存活时间。
	// 取 24h 与契约给出的示例一致（created_at → expires_at 差 86400 秒）。
	// 图片几秒到几十秒就出结果，24h 只是给"客户端过一晚再来问"的情况兜底。
	imageTaskTTL = 24 * time.Hour

	// imageTaskSubmitBodyLimit 是提交时允许的请求体上限（1MB）。
	// 图片生成的 JSON body 是模型名 + prompt + size/n 这些参数，正常几 KB。
	// 卡在 1MB 是因为这份 body 要整段存进 image_tasks.request_body（text 列）：
	// 放行 256MB（bodycache 的默认上限）等于允许一条请求把库写爆。
	imageTaskSubmitBodyLimit = 1 << 20

	// imageTaskUpstreamReadLimit 是执行器读上游响应体的上限（8MB）。
	// 上游可能回 b64_json（一张图几 MB 的 base64），必须留得下；
	// 但超过 8MB 的响应一定不是我们想要的图，读下去只会打爆内存。
	imageTaskUpstreamReadLimit = 8 << 20

	// imageTaskResultLimit 是落库的 result_json 上限（64KB）。
	// b64 图像数据在写入前就被剔除（见 imageTaskResultPayload），剩下的元数据很小；
	// 这个上限是"上游回了别的大字段"的硬保险丝。
	imageTaskResultLimit = 64 << 10

	// imageTaskPollRetryAfter 是提交响应里建议客户端的轮询间隔（秒）。
	imageTaskPollRetryAfter = "3"

	// imageTaskObjectName 是对外契约里的 object 字段，逐字固定。
	imageTaskObjectName = "image.generation.task"

	// imageTaskPollPathPrefix 是客户端轮询用的路径前缀（也是 Location 头与 poll_url）。
	imageTaskPollPathPrefix = "/v1/images/tasks/"
)

// ImageTaskAsyncEnabled 报告图片异步任务开关是否打开。
//
// 开关读不到（settingCache 里没有这一行）一律当**关闭**：这个功能会因为配置不当
// 变成"扣钱但不干活"，任何不确定都必须落在"不对外存在"这一侧。
func ImageTaskAsyncEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyAsyncImageEnabled)
	if err != nil {
		return false
	}
	return enabled
}

// imageTaskNewID 生成客户端持有的任务号：imgtask_ + 24 位十六进制。
//
// 用 crypto/rand 而不是自增 id：任务号是**句柄**，可枚举就等于可探测别人
// 有没有任务（虽然查询侧还有归属过滤，但没必要把内部计数暴露出去）。
func imageTaskNewID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// rand 失败极少见；退回时间戳保证不返回空 id（空 id 会撞 uniqueIndex 的 default）
		return fmt.Sprintf("imgtask_%x", time.Now().UnixNano())
	}
	return "imgtask_" + hex.EncodeToString(buf)
}

// imageTaskEndpointPath 把任务行上的 Endpoint 换算成上游子路径。
// 空值 = generations（存量/默认，见 model.ImageTask.Endpoint）。
func imageTaskEndpointPath(endpoint string) string {
	switch strings.TrimSpace(endpoint) {
	case model.ImageTaskEndpointEdits:
		return "/images/edits"
	default:
		return "/images/generations"
	}
}

// imageTaskPollURL 是客户端用来查询的路径（同时也是 Location 头的值）。
func imageTaskPollURL(taskID string) string {
	return imageTaskPollPathPrefix + taskID
}

// imageTaskSubmitResponse 是提交成功（202）的响应体。字段与契约逐字对齐。
type imageTaskSubmitResponse struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Object    string `json:"object"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	PollURL   string `json:"poll_url"`
}

// imageTaskErrorPayload 是失败任务的 error 字段。刻意只有 code + message：
// 上游原始返回（ResultJSON）可能含上游内网信息，不对外展示。
type imageTaskErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// imageTaskResponse 是查询接口的响应体。三种状态共用这一个结构，
// 用 omitempty 表达"哪些字段在哪个状态才有值"。
type imageTaskResponse struct {
	ID          string                 `json:"id"`
	TaskID      string                 `json:"task_id"`
	Object      string                 `json:"object"`
	Status      string                 `json:"status"`
	ImageURL    string                 `json:"image_url,omitempty"`
	Result      json.RawMessage        `json:"result,omitempty"`
	Error       *imageTaskErrorPayload `json:"error,omitempty"`
	CreatedAt   int64                  `json:"created_at"`
	CompletedAt int64                  `json:"completed_at,omitempty"`
	ExpiresAt   int64                  `json:"expires_at"`
	PollURL     string                 `json:"poll_url"`
}

// ImagesAsyncSubmit 处理 POST /v1/images/{generations,edits}/async。
//
// 它只做三件事：走**同一套**准入、把请求存成一行任务、立刻回 202。
// 真正的上游调用不在这里——这里等它，就等于把同步版那 20–60 秒的等待搬到
// 一个"异步"接口上，客户端该断还是会断。
//
// endpoint 取 model.ImageTaskEndpoint* 的那套值（"generations" / "edits"），
// 与任务行上的 Endpoint 列、与后台执行器的重放路径同一套定义。
func ImagesAsyncSubmit(endpoint string, c *gin.Context) {
	if !ImageTaskAsyncEnabled() {
		// 关闭时对外不存在：404，不是 403（403 等于"有这个功能，你没权限"）。
		resp.Error(c, http.StatusNotFound, "not found")
		return
	}

	ctx := c.Request.Context()
	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")

	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if strings.Contains(contentType, "multipart/form-data") {
		// multipart 的请求体里有图片二进制，存不进 text 列，也无法原样重放。
		// 明确拒绝，而不是存一份缺了图片的 body 去跑一个"必然画错"的任务。
		resp.Error(c, http.StatusBadRequest, "async image tasks require an application/json body")
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, imageTaskSubmitBodyLimit+1))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(rawBody) > imageTaskSubmitBodyLimit {
		resp.Error(c, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	requestPayload, requestModel, stream, perr := parseJSONModelAndStreamBytes(rawBody)
	if perr != nil {
		resp.Error(c, http.StatusBadRequest, perr.Error())
		return
	}
	if stream {
		// 异步任务的结果只能"查"，没有可流式交付的通道。静默去掉 stream 会让
		// 客户端以为自己在收流；明确拒绝更诚实。
		resp.Error(c, http.StatusBadRequest, "async image tasks do not support stream=true")
		return
	}

	// supported_models 校验：与同步版同一个判据（复用 APIKeyAuth 注入的值）。
	supportedModels := strings.TrimSpace(c.GetString("supported_models"))
	if !op.IsModelSupported(supportedModels, requestModel) {
		resp.Error(c, http.StatusBadRequest, "model not supported")
		return
	}

	// 选路：与同步版同一个入口（AccessPlan 规则 / 分组回退都在里面）。
	routeResult, status, message, routeErr := selectRouteGroup(c, apiKeyID, requestModel)
	if routeErr != nil || status != 0 {
		if message == "" && routeErr != nil {
			message = routeErr.Error()
		}
		resp.Error(c, status, message)
		return
	}
	group := enrichGroupForSmartRouting(ctx, routeResult.Group, false)
	clientSession := deriveClientSessionInfo(c.Request.Header, nil)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, requestModel, clientSession.Key, routeStickyEnabled(group.Mode, clientSession.Source))
	if iter.Len() == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	channel, channelKey, upstreamModel, ok := pickImageTaskChannel(iter, ctx)
	if !ok {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	// 0) 定价：照 new-api（ModelPriceHelperPerCall）——没价的模型直接拒绝，而不是
	//    "先受理、按 0 收"。异步任务一旦受理就会真花上游的钱，没价等于白送。
	//    管理员从不扣费（op.UserRecordUsage 对管理员直接返回），相当于 new-api 的自用模式，放行。
	billing := imageTaskBillingPlan(routeResult, requestModel, upstreamModel, endpoint, c, imageRequestCount(requestPayload))
	// 访客 Key 虽然归属管理员，但有自己的花费上限：没价的模型按 0 记，上限形同虚设，所以照普通用户拒绝。
	if !billing.priced && (isGuestKeyRequest(c) || !imageTaskUserIsAdmin(userID)) {
		resp.Error(c, http.StatusBadRequest, imageModelPriceNotConfiguredMessage(requestModel))
		return
	}

	taskID := imageTaskNewID()

	// 1) 在途预留：走的就是 UserCanRelay（与同步图片路径、与所有中继路径同一道闸）。
	//    中间件那一份预留只在这次 HTTP 请求里有效，异步任务活得比它长得多，
	//    所以这里必须为任务本身再占一笔，并在终态释放（见 task 包）。
	// 预留金额本身不用在这里记账（op 里按 taskID 存着），只要确保拿到了。
	if _, rerr := op.ImageTaskReservationTake(ctx, taskID, userID); rerr != nil {
		resp.Error(c, http.StatusPaymentRequired, rerr.Error())
		return
	}
	// 1b) Key 花费上限也要为任务本身占一笔，一直占到终态（与上面同一个 taskID 一起归还）。
	//    访客 Key 归属管理员、上面那笔恒为 0，没有这一笔就能一次提交 N 个任务冲过上限。
	//    按张计费的占预扣金额（事先就知道要花多少），按 token 计费的占默认量子。
	if apiKeyID > 0 {
		keyObj, kerr := op.APIKeyGet(apiKeyID, ctx)
		if kerr != nil {
			op.ImageTaskReservationRelease(taskID)
			resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
			return
		}
		if _, kerr := op.ImageTaskKeyReservationTake(taskID, keyObj, billing.precharge); kerr != nil {
			op.ImageTaskReservationRelease(taskID)
			resp.Error(c, http.StatusUnauthorized, kerr.Error())
			return
		}
	}

	// 2) 预扣：按张计费的模型在创建任务时就扣掉「单价 × 张数 × 倍率」（new-api 的
	//    PreConsumeBilling）；按 token 计费的模型此时算不出钱，只靠上面那笔在途预留占住
	//    余额，完成时按上游用量结算（见 relay.ImageTaskSettle）。失败/过期原路退回。
	cost := billing.precharge
	if cost > 0 {
		if derr := op.UserRecordUsage(userID, cost, ctx); derr != nil {
			op.ImageTaskReservationRelease(taskID)
			log.Warnf("image task submit: pre-charge failed for task %s: %v", taskID, derr)
			resp.Error(c, http.StatusInternalServerError, "failed to charge for the image task")
			return
		}
	}

	now := time.Now()
	task := model.ImageTask{
		TaskID:       taskID,
		CreatedAt:    now.Unix(),
		ExpiresAt:    now.Add(imageTaskTTL).Unix(),
		Status:       model.ImageTaskStatusProcessing,
		OwnerTokenID: apiKeyID,
		OwnerUserID:  userID,
		ChannelID:    channel.ID,
		ChannelKeyID: channelKey.ID,
		Model:        upstreamModel,
		Endpoint:     imageTaskEndpoint(endpoint),
		// 请求体原文（含 prompt）：供执行器重放；绝不进进程日志/错误串。
		// 历史日志行由执行器按同步图片的同一套规则写（见 images_async_log.go）。
		RequestBody:      string(rawBody),
		Cost:             cost,
		RequestModel:     requestModel,
		RequestIP:        c.GetString("request_ip"),
		RequestUserAgent: truncateUserAgentForLog(c.Request.UserAgent()),
		BillingMode:      billing.mode,
		ImageCount:       billing.count,
		BillingSnapshot:  billing.snapshotJSON(),
	}
	if cerr := op.ImageTaskCreate(ctx, &task); cerr != nil {
		op.ImageTaskReservationRelease(taskID)
		if cost > 0 {
			if rerr := op.UserRefundUsage(userID, cost, ctx); rerr != nil {
				log.Errorf("image task submit: refund after create failure for task %s failed: %v", taskID, rerr)
			}
		}
		log.Warnf("image task submit: create task %s failed: %v", taskID, cerr)
		resp.Error(c, http.StatusInternalServerError, "failed to create the image task")
		return
	}

	log.Infof("image task submit: task %s created (channel=%d, model=%s, endpoint=%s)",
		taskID, channel.ID, upstreamModel, task.Endpoint)

	c.Header("Cache-Control", "no-store")
	c.Header("Location", imageTaskPollURL(taskID))
	c.Header("Retry-After", imageTaskPollRetryAfter)
	c.JSON(http.StatusAccepted, imageTaskSubmitResponse{
		ID:        taskID,
		TaskID:    taskID,
		Object:    imageTaskObjectName,
		Status:    model.ImageTaskStatusProcessing,
		CreatedAt: task.CreatedAt,
		ExpiresAt: task.ExpiresAt,
		PollURL:   imageTaskPollURL(taskID),
	})
}

// imageTaskEndpoint 把 handler 传进来的 endpoint 归一成任务行上的取值。
func imageTaskEndpoint(endpoint string) string {
	if strings.Contains(strings.Trim(strings.ToLower(endpoint), "/"), "edits") {
		return model.ImageTaskEndpointEdits
	}
	return model.ImageTaskEndpointGenerations
}

// imageTaskBilling 是提交时定下来的计费口径，会冻结到任务行上。
type imageTaskBilling struct {
	mode      string // imageBillingModePerImage / imageBillingModeToken
	count     int
	snapshot  model.AccessPlanBillingSnapshot
	priced    bool    // 计费模型有没有任何价格
	precharge float64 // 提交时预扣的钱（只有按张计费才 > 0）
}

func (b imageTaskBilling) snapshotJSON() string {
	raw, err := json.Marshal(b.snapshot)
	if err != nil {
		return ""
	}
	return string(raw)
}

// imageTaskBillingPlan 算出这条任务怎么收钱。
//
// 计费模型名、访问计划倍率**直接复用** imagesRelayMetrics.currentBillingSnapshot：
// 同步图片按哪个模型名、哪套倍率收，异步就按哪个收（路由规则里的 billing_model_source
// 也一并生效），抄一份过来迟早两边不一致。
//   - 价格行 PerCall > 0 → 按张：预扣 PerCall × n × 倍率，完成不再结算；
//   - 否则 → 按 token：不预扣，完成时按上游 usage 结算。
func imageTaskBillingPlan(routeResult routeGroupResult, requestModel, upstreamModel, endpoint string, c *gin.Context, count int) imageTaskBilling {
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	metrics := newImagesRelayMetrics(0, 0, "", requestModel, imageEndpointName(endpoint), path)
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	snapshot := metrics.currentBillingSnapshot(upstreamModel)
	if count < 1 {
		count = 1
	}
	plan := imageTaskBilling{
		mode:     imageBillingModeToken,
		count:    count,
		snapshot: snapshot,
		priced:   imageModelPriced(snapshot.BillingModelName),
	}
	if unit := imagePerImagePrice(snapshot.BillingModelName); unit > 0 {
		plan.mode = imageBillingModePerImage
		plan.precharge = imagePerImageCost(unit, count, snapshot)
	}
	return plan
}

// imageTaskUserIsAdmin 报告提交者是不是管理员（管理员从不扣费，没价模型对他放行）。
func imageTaskUserIsAdmin(userID int) bool {
	user, err := op.UserGet(userID)
	return err == nil && user.IsAdmin()
}

// pickImageTaskChannel 按同步图片路径**同一批判据**选出一个可用渠道 + 一把可用 key。
//
// 判据逐个都是同步循环里用的那几个（渠道启用 / 端点族兼容 / key 可用），
// 没有另写一套：`isImagesEndpointCompatibleChannelType`、`GetAvailableChannelKeys`、
// `PrioritizeChannelKeysByHealth` 都是同步路径的原函数。
//
// 与同步循环的区别只有一处：**不转发**。异步提交只是把"该去哪个渠道问"记下来，
// 真正的调用由后台执行器用同一组 ChannelID/ChannelKeyID 重放。
func pickImageTaskChannel(iter *balancer.Iterator, ctx context.Context) (*model.Channel, model.ChannelKey, string, bool) {
	for iter.Next() {
		select {
		case <-ctx.Done():
			return nil, model.ChannelKey{}, "", false
		default:
		}

		item := iter.Item()
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
			continue
		}
		if !channel.Enabled {
			iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
			continue
		}
		if !isImagesEndpointCompatibleChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("unsupported channel type: %d", channel.Type))
			continue
		}
		if channel.Type == outbound.OutboundTypeCustomOpenAIChat {
			if _, ok := customChannelAbsoluteURL(channel.OpenAIImagesPath); !ok {
				iter.Skip(channel.ID, 0, channel.Name, CustomChannelNoFullURLReason)
				continue
			}
		}
		upstreamModel := item.ModelName
		if mapped, ok := channel.ModelMapping[item.ModelName]; ok && mapped != "" {
			upstreamModel = mapped
		}

		availableKeys := channel.GetAvailableChannelKeys()
		if len(availableKeys) == 0 {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		if stickyKeyID := iter.StickyKeyIDForCurrentChannel(channel.ID); stickyKeyID > 0 {
			availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, upstreamModel, stickyKeyID)
		} else {
			availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, upstreamModel, 0)
		}
		for _, usedKey := range availableKeys {
			if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
				continue
			}
			return channel, usedKey, upstreamModel, true
		}
	}
	return nil, model.ChannelKey{}, "", false
}

// ImagesAsyncGet 处理 GET /v1/images/tasks/:task_id。
//
// 【fail-closed】任务不存在 / 已过期 / 不属于你，三者一律 404，原因不区分：
// 区分了就等于告诉探测者"这个 id 存在，只是不是你的"。
//
// 【失败也返回 200】任务本身查询成功了，只是它的结果是"失败"。用 5xx 表示"任务
// 失败"会让客户端以为查询接口坏了从而重试查询，越重试越乱。
func ImagesAsyncGet(c *gin.Context) {
	if !ImageTaskAsyncEnabled() {
		resp.Error(c, http.StatusNotFound, "not found")
		return
	}

	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")
	taskID := c.Param("task_id")

	task, err := op.ImageTaskGetForOwner(c.Request.Context(), taskID, apiKeyID, userID)
	if err != nil {
		if errors.Is(err, op.ErrImageTaskOwnerScopeRequired) {
			resp.Error(c, http.StatusNotFound, "image task not found")
			return
		}
		log.Warnf("image task query: read task %s failed: %v", strings.TrimSpace(taskID), err)
		resp.Error(c, http.StatusInternalServerError, "failed to read the image task")
		return
	}
	if task == nil {
		resp.Error(c, http.StatusNotFound, "image task not found")
		return
	}
	// 过期即不可见：即使后台清扫还没来得及把它标成 failed，客户端也查不到
	// （契约：不存在 / 已过期 / 不是你的，三者一律 404）。
	if task.ExpiresAt > 0 && task.ExpiresAt <= time.Now().Unix() {
		resp.Error(c, http.StatusNotFound, "image task not found")
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, buildImageTaskResponse(*task))
}

// buildImageTaskResponse 把一行任务翻译成对外响应体。
func buildImageTaskResponse(task model.ImageTask) imageTaskResponse {
	out := imageTaskResponse{
		ID:        task.TaskID,
		TaskID:    task.TaskID,
		Object:    imageTaskObjectName,
		Status:    model.NormalizeImageTaskStatus(task.Status),
		CreatedAt: task.CreatedAt,
		ExpiresAt: task.ExpiresAt,
		PollURL:   imageTaskPollURL(task.TaskID),
	}
	switch out.Status {
	case model.ImageTaskStatusCompleted:
		out.CompletedAt = task.CompletedAt
		out.ImageURL = imageTaskImageURL(task.ResultJSON)
		if json.Valid([]byte(task.ResultJSON)) && strings.TrimSpace(task.ResultJSON) != "" {
			out.Result = json.RawMessage(task.ResultJSON)
		}
	case model.ImageTaskStatusFailed:
		message := strings.TrimSpace(task.ErrorMessage)
		if message == "" {
			message = "image generation failed"
		}
		out.Error = &imageTaskErrorPayload{Code: "image_generation_failed", Message: message}
	}
	return out
}

// imageTaskImageURL 从落库的 result_json 里取出结果图片地址（没有就空串）。
//
// 取不到不算错：有的上游只回 b64_json，那种情况下 result 里仍有内容，
// 只是没有可直接访问的 URL。客户端拿 result 自己处理。
func imageTaskImageURL(resultJSON string) string {
	if strings.TrimSpace(resultJSON) == "" {
		return ""
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &raw); err != nil {
		return ""
	}
	return imageTaskURLFromPayload(raw)
}

// imageTaskURLFromPayload 在常见的两种形状里找图片地址：
// 顶层 url，或 data[0].url（OpenAI images 的标准形状）。
func imageTaskURLFromPayload(raw map[string]any) string {
	if url, ok := raw["url"].(string); ok && strings.TrimSpace(url) != "" {
		return strings.TrimSpace(url)
	}
	items, ok := raw["data"].([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if url, ok := entry["url"].(string); ok && strings.TrimSpace(url) != "" {
			return strings.TrimSpace(url)
		}
	}
	return ""
}

// ImageTaskResultPayload 把上游响应体整理成要落库的 result_json，并返回图片地址。
//
// 【必须剔除 b64_json】有的上游把整张图 base64 塞在 data[i].b64_json 里（几 MB）。
// 原样落库会让这一列无限膨胀、日志页每次读一行都拖几百 KB。这里把它换成长度提示，
// 其余字段（created / revised_prompt / usage ...）原样保留，诊断价值不受影响。
//
// 返回的 resultJSON 一定是**合法 JSON 或空串**：查询接口直接把它当 json.RawMessage
// 编码，非法 JSON 会让整条响应编码失败。
func ImageTaskResultPayload(body []byte) (resultJSON string, imageURL string) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		// 上游回了个读不懂的东西：不落原始字节（可能是 HTML 错误页/内网信息），
		// 只留一句可读的说明，真正的状态码由调用方写进 ErrorMessage。
		return "", ""
	}
	imageURL = imageTaskURLFromPayload(raw)
	if items, ok := raw["data"].([]any); ok {
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if b64, ok := entry["b64_json"].(string); ok {
				entry["b64_json"] = fmt.Sprintf("(omitted: %d bytes of base64 image data)", len(b64))
			}
		}
	}
	marshaled, err := json.Marshal(raw)
	if err != nil {
		return "", imageURL
	}
	if len(marshaled) > imageTaskResultLimit {
		return "", imageURL
	}
	return string(marshaled), imageURL
}

// ImageTaskRunUpstream 重放一条异步图片任务到它的上游，返回状态码、响应体与
// 失败原因（已脱敏）。
//
// 它是执行器（internal/task/image_task_poll.go）唯一的出口，等价于同步路径的
// imagesAttempt —— 同一套 base_url 拼接（xurl.JoinOpenAIPath + canonical /v1）、
// 同一份鉴权/自定义头/UA 默认，只是不写下游 response writer（后台没有下游）。
//
// 【不做的事】不改 outboundTLSFace / 指纹 / shape：这条链路的出口租户与同步图片
// 路径完全一样，多一层"异步专属指纹"反而是新的可识别面。
//
// 【日志红线】本函数**绝不**打印 requestBody：它含用户的 prompt。
//
// taskLog 非 nil 时，在请求发出前拍一份真实出站请求体快照（与同步图片同一开关、
// 同一套规则；开关关闭时不读 body）。快照只读、原样挂回，出站字节不变。
func ImageTaskRunUpstream(
	ctx context.Context,
	channel *model.Channel,
	channelKey string,
	endpoint string,
	modelName string,
	requestBody []byte,
	taskLog *ImageTaskLog,
) (statusCode int, body []byte, failReason string, err error) {
	if channel == nil {
		return 0, nil, "", errors.New("channel is nil")
	}
	if strings.TrimSpace(channelKey) == "" {
		return 0, nil, "", errors.New("channel key is empty")
	}

	var payload map[string]any
	if uerr := json.Unmarshal(requestBody, &payload); uerr != nil {
		return 0, nil, "", fmt.Errorf("stored request body is not valid json: %w", uerr)
	}
	// 与同步路径一致：上游拿到的是**映射后**的模型名。
	payload["model"] = modelName
	upstreamBody, merr := json.Marshal(payload)
	if merr != nil {
		return 0, nil, "", fmt.Errorf("failed to marshal upstream image payload: %w", merr)
	}

	targetURL, uerr := openAIUpstreamURL(channel, channel.GetOpenAIImagesPath(imageTaskEndpointPath(endpoint)))
	if uerr != nil {
		return 0, nil, "", fmt.Errorf("failed to build upstream image url: %w", uerr)
	}

	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if rerr != nil {
		return 0, nil, "", fmt.Errorf("failed to create upstream image request: %w", rerr)
	}
	// 真实出站请求体快照：与同步图片路径（imagesAttempt）同一个采集函数。
	if cerr := captureOutboundJSONBody(req, taskLog.bodyLog()); cerr != nil {
		return 0, nil, "", cerr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+channelKey)
	for _, h := range channel.CustomHeader {
		req.Header.Set(h.HeaderKey, h.HeaderValue)
	}
	// 与同步图片路径同一个 UA 兜底：不补这一层，出站会带 Go 默认 UA（"我是 Go 中转"）。
	setHeaderIfMissing(req.Header, "User-Agent", genericUAForChannel(channel))

	httpClient, herr := helper.ChannelHttpClient(channel)
	if herr != nil {
		return 0, nil, "", herr
	}
	respUp, derr := httpClient.Do(req)
	if derr != nil {
		return 0, nil, "", fmt.Errorf("failed to send upstream image request: %w", derr)
	}
	defer func() {
		if cerr := respUp.Body.Close(); cerr != nil {
			log.Debugf("image task upstream: close response body failed: %v", cerr)
		}
	}()

	respBody, readErr := io.ReadAll(io.LimitReader(respUp.Body, imageTaskUpstreamReadLimit))

	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		// 上游错误体的摘要（经 xredact 脱敏 + 截断）留给运维诊断；
		// 它进的是 image_tasks.error_message 与本次 Warn 日志，不是 result_json。
		summary := upstreamBodySummary(string(respBody))
		if summary == "" {
			summary = http.StatusText(respUp.StatusCode)
		}
		return respUp.StatusCode, respBody, fmt.Sprintf("upstream returned status %d: %s", respUp.StatusCode, summary), nil
	}
	if readErr != nil {
		return respUp.StatusCode, respBody, "", fmt.Errorf("failed to read upstream image response: %w", readErr)
	}
	return respUp.StatusCode, respBody, "", nil
}
