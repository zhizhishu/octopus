package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/helper"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// Midjourney（midjourney-proxy 协议）中转。
//
// 照抄的是 new-api（QuantumNous/new-api）的 /mj 中转：
//   - 路由表：router/relay-router.go:228-243；
//   - 动作 → 模型名：service/midjourney.go CovertMjpActionToModelName（"mj_"+小写动作，
//     SWAP_FACE → "swap_face"），customId → 动作：CoverPlusActionToNormalAction，
//     content → 动作：ConvertSimpleChangeParams；
//   - 上游请求：service/midjourney.go DoMidjourneyHttpRequest（删 accountFilter /
//     notifyHook，鉴权头是 `mj-api-secret: <渠道 key>`）；
//   - 结果判定与计费：relay/mjproxy_handler.go RelayMidjourneySubmit（code 1/21/22 为受理，
//     21/22 改写成 1；INPAINT / CUSTOM_ZOOM 不计费；受理后续动作必须回到原任务的渠道）；
//   - 提交只打一次：service/midjourney.go RecordMidjourneyPolicyResponse 的注释——
//     含糊的传输失败绝不能在另一个渠道上再建一条重复任务。
//
// 与 new-api 不同的地方（刻意的）：
//   - 不新增渠道类型：MJ 走 OpenAI 兼容 / OpenAI Response 渠道，base 填 mj-proxy 根地址
//     （末尾的 /v1 会被去掉，填成 …/mj 也不会叠成 /mj/mj）。
//   - fetch / image-seed / list-by-condition 原样转发给归属渠道，不在本地维护一份任务状态，
//     所以不需要 new-api 那个后台轮询器。
//   - 上游 HTTP 非 2xx（明确没受理）按 octopus 惯例换渠道；传输层错误不换（见上）。

const (
	midjourneyResponseBodyLimit = 8 << 20
	// midjourneyRequestErrorCode 是 mj-proxy 协议里的「请求错误」（new-api constant.MjRequestError）。
	midjourneyRequestErrorCode = 4
	// 内存归属只是库的前一级缓存；库里的行没有 TTL。
	midjourneyTaskMemoryTTL = 24 * time.Hour
)

// 提交类路由的模式（= /mj/submit/<mode> 的最后一段；swap-face 走 /mj/insight-face/swap）。
const (
	MidjourneyModeImagine      = "imagine"
	MidjourneyModeChange       = "change"
	MidjourneyModeSimpleChange = "simple-change"
	MidjourneyModeDescribe     = "describe"
	MidjourneyModeBlend        = "blend"
	MidjourneyModeEdits        = "edits"
	MidjourneyModeVideo        = "video"
	MidjourneyModeAction       = "action"
	MidjourneyModeModal        = "modal"
	MidjourneyModeShorten      = "shorten"
	MidjourneyModeUpload       = "upload-discord-images"
	MidjourneyModeSwapFace     = "swap-face"
)

// mj-proxy 动作名（new-api constant/midjourney.go）。
const (
	mjActionImagine       = "IMAGINE"
	mjActionDescribe      = "DESCRIBE"
	mjActionBlend         = "BLEND"
	mjActionUpscale       = "UPSCALE"
	mjActionVariation     = "VARIATION"
	mjActionReRoll        = "REROLL"
	mjActionInPaint       = "INPAINT"
	mjActionModal         = "MODAL"
	mjActionZoom          = "ZOOM"
	mjActionCustomZoom    = "CUSTOM_ZOOM"
	mjActionShorten       = "SHORTEN"
	mjActionHighVariation = "HIGH_VARIATION"
	mjActionLowVariation  = "LOW_VARIATION"
	mjActionPan           = "PAN"
	mjActionSwapFace      = "SWAP_FACE"
	mjActionUpload        = "UPLOAD"
	mjActionVideo         = "VIDEO"
	mjActionEdits         = "EDITS"
)

// 日志端点名落在 images 族下：日志页现有的 images 筛选按前缀匹配就能筛到，不必动 UI。
const (
	midjourneySubmitLogName = "images_midjourney"
	midjourneyTaskLogName   = "images_midjourney_task"
)

func midjourneySubmitUpstreamPath(mode string) string {
	if mode == MidjourneyModeSwapFace {
		return "/mj/insight-face/swap"
	}
	return "/mj/submit/" + mode
}

// midjourneyModelName = new-api CovertMjpActionToModelName。
func midjourneyModelName(action string) string {
	if action == mjActionSwapFace {
		return "swap_face"
	}
	return "mj_" + strings.ToLower(action)
}

type midjourneySubmitPlan struct {
	action       string
	model        string
	originTaskID string
	billable     bool
}

// planMidjourneySubmit 把一次提交解析成「动作 / 计费模型 / 是否要回到原任务渠道」。
// 返回的 string 非空表示请求本身有误（mj-proxy 风格的 description）。
func planMidjourneySubmit(mode string, payload map[string]any) (midjourneySubmitPlan, string) {
	var plan midjourneySubmitPlan
	switch mode {
	case MidjourneyModeImagine:
		if strings.TrimSpace(mjString(payload, "prompt")) == "" {
			return plan, "prompt_is_required"
		}
		plan.action = mjActionImagine
	case MidjourneyModeDescribe:
		plan.action = mjActionDescribe
	case MidjourneyModeEdits:
		plan.action = mjActionEdits
	case MidjourneyModeShorten:
		plan.action = mjActionShorten
	case MidjourneyModeBlend:
		plan.action = mjActionBlend
	case MidjourneyModeUpload:
		plan.action = mjActionUpload
	case MidjourneyModeSwapFace:
		plan.action = mjActionSwapFace
	case MidjourneyModeVideo:
		// 视频可以从一张已有任务的图出发（带 taskId，必须回原渠道），也可以不带。
		plan.action = mjActionVideo
		plan.originTaskID = strings.TrimSpace(mjString(payload, "taskId"))
	case MidjourneyModeChange:
		plan.action = strings.ToUpper(strings.TrimSpace(mjString(payload, "action")))
		plan.originTaskID = strings.TrimSpace(mjString(payload, "taskId"))
		if plan.originTaskID == "" {
			return plan, "task_id_is_required"
		}
		if plan.action == "" {
			return plan, "action_is_required"
		}
		if mjInt(payload, "index") == 0 && plan.action != mjActionReRoll {
			return plan, "index_is_required"
		}
	case MidjourneyModeSimpleChange:
		content := strings.TrimSpace(mjString(payload, "content"))
		if content == "" {
			return plan, "content_is_required"
		}
		taskID, action, ok := parseMidjourneySimpleChange(content)
		if !ok {
			return plan, "content_parse_failed"
		}
		plan.action = action
		plan.originTaskID = taskID
	case MidjourneyModeModal:
		plan.action = mjActionModal
		plan.originTaskID = strings.TrimSpace(mjString(payload, "taskId"))
		if plan.originTaskID == "" {
			return plan, "task_id_is_required"
		}
	case MidjourneyModeAction:
		action, desc := midjourneyActionFromCustomID(mjString(payload, "customId"))
		if desc != "" {
			return plan, desc
		}
		plan.action = action
		plan.originTaskID = strings.TrimSpace(mjString(payload, "taskId"))
		if plan.originTaskID == "" {
			return plan, "task_id_is_required"
		}
	default:
		return plan, "unknown_relay_action"
	}
	plan.model = midjourneyModelName(plan.action)
	// new-api relay/mjproxy_handler.go:515：局部重绘 / 自定义缩放是弹窗的前置步骤，不收钱。
	plan.billable = plan.action != mjActionInPaint && plan.action != mjActionCustomZoom
	return plan, ""
}

// parseMidjourneySimpleChange = new-api ConvertSimpleChangeParams："<taskId> U1|V3|R"。
func parseMidjourneySimpleChange(content string) (taskID, action string, ok bool) {
	split := strings.Split(content, " ")
	if len(split) != 2 || split[0] == "" || split[1] == "" {
		return "", "", false
	}
	verb := strings.ToLower(split[1])
	switch {
	case verb == "r":
		return split[0], mjActionReRoll, true
	case verb[0] == 'u':
		action = mjActionUpscale
	case verb[0] == 'v':
		action = mjActionVariation
	default:
		return "", "", false
	}
	if len(verb) < 2 {
		return "", "", false
	}
	index, err := strconv.Atoi(verb[1:2])
	if err != nil || index < 1 || index > 4 {
		return "", "", false
	}
	return split[0], action, true
}

// midjourneyActionFromCustomID = new-api CoverPlusActionToNormalAction 的动作部分。
// "MJ::JOB::upsample::2::<uuid>" → UPSCALE。
func midjourneyActionFromCustomID(customID string) (string, string) {
	customID = strings.TrimSpace(customID)
	if customID == "" {
		return "", "custom_id_is_required"
	}
	splits := strings.Split(customID, "::")
	if len(splits) < 2 {
		return "", "unknown_action:" + customID
	}
	action := splits[1]
	if splits[1] == "JOB" {
		if len(splits) < 3 {
			return "", "unknown_action:" + customID
		}
		action = splits[2]
	}
	if action == "" {
		return "", "unknown_action"
	}
	indexOK := func() bool {
		if len(splits) < 4 {
			return false
		}
		_, err := strconv.Atoi(splits[3])
		return err == nil
	}
	switch {
	case strings.Contains(action, "upsample"):
		if !indexOK() {
			return "", "index_parse_failed"
		}
		return mjActionUpscale, ""
	case strings.Contains(action, "variation"):
		switch action {
		case "variation":
			if !indexOK() {
				return "", "index_parse_failed"
			}
			return mjActionVariation, ""
		case "low_variation":
			return mjActionLowVariation, ""
		case "high_variation":
			return mjActionHighVariation, ""
		}
		return "", "unknown_action:" + customID
	case strings.Contains(action, "pan"):
		return mjActionPan, ""
	case strings.Contains(action, "reroll"):
		return mjActionReRoll, ""
	case action == "Outpaint":
		return mjActionZoom, ""
	case action == "CustomZoom":
		return mjActionCustomZoom, ""
	case action == "Inpaint":
		return mjActionInPaint, ""
	}
	return "", "unknown_action:" + customID
}

func mjString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload[key].(string); ok {
		return v
	}
	return ""
}

func mjInt(payload map[string]any, key string) int {
	if payload == nil {
		return 0
	}
	switch v := payload[key].(type) {
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}

// writeMidjourneyError 写 mj-proxy 风格的错误体（new-api controller/relay.go RelayMidjourney）。
func writeMidjourneyError(c *gin.Context, status int, description string) {
	c.JSON(status, gin.H{
		"code":        midjourneyRequestErrorCode,
		"description": description,
		"type":        "upstream_error",
	})
}

// isMidjourneyCapableChannelType：只有填得出 mj-proxy 根地址的 OpenAI 兼容类渠道。
// 自定义渠道（type 6）没有可填的 MJ 完整地址字段，与 raw 透传同样跳过。
func isMidjourneyCapableChannelType(channelType outbound.OutboundType) bool {
	switch channelType {
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeOpenAIResponse:
		return true
	default:
		return false
	}
}

// midjourneyUpstreamURL 拼 <渠道 base 去掉末尾 /v1> + /mj/...。
// new-api 是 baseURL + "/mj/submit/imagine" 直拼；octopus 渠道的 base 常常填成 …/v1
// （同一个渠道还要跑 chat），去掉它才能落到 mj-proxy 的根路由上。
func midjourneyUpstreamURL(channel *model.Channel, upstreamPath string, query url.Values) (string, error) {
	base := ""
	if channel != nil {
		base = channel.GetBaseUrl()
	}
	cleaned, err := xurl.TrimPathSuffixes(base, "/v1")
	if err != nil {
		return "", err
	}
	joined, err := xurl.JoinPath(cleaned, upstreamPath)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(joined)
	if err != nil {
		return "", err
	}
	mergeRequestQuery(parsed, query)
	return parsed.String(), nil
}

// midjourneyForward 发一次上游请求。头部是**逐个手工设置**的，不复用
// copyHeadersToUpstream：客户端可能用 mj-api-secret 头带着自己的 octopus key 进来，
// 镜像客户端头会把它原样送给上游。
func midjourneyForward(ctx context.Context, c *gin.Context, method, upstreamPath string, body []byte, channel *model.Channel, channelKey string) (int, []byte, string, error) {
	var query url.Values
	if c != nil && c.Request != nil {
		query = c.Request.URL.Query()
	}
	targetURL, err := midjourneyUpstreamURL(channel, upstreamPath, query)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to build midjourney url: %w", err)
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to create midjourney request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	accept := ""
	if c != nil && c.Request != nil {
		accept = strings.TrimSpace(c.Request.Header.Get("Accept"))
	}
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("mj-api-secret", strings.TrimPrefix(channelKey, "Bearer "))
	for _, h := range channel.CustomHeader {
		req.Header.Set(h.HeaderKey, h.HeaderValue)
	}
	setHeaderIfMissing(req.Header, "User-Agent", genericUAForChannel(channel))

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, nil, "", err
	}
	respUp, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to send midjourney request: %w", err)
	}
	defer respUp.Body.Close()

	upstreamCT := respUp.Header.Get("Content-Type")
	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		return respUp.StatusCode, nil, upstreamCT, newUpstreamError(respUp.StatusCode, b)
	}
	respBody, err := io.ReadAll(io.LimitReader(respUp.Body, midjourneyResponseBodyLimit))
	if err != nil {
		return respUp.StatusCode, nil, upstreamCT, fmt.Errorf("failed to read midjourney response: %w", err)
	}
	return respUp.StatusCode, respBody, upstreamCT, nil
}

// midjourneySubmitResponse 是 mj-proxy 提交响应。result 对 upload-discord-images
// 是字符串数组，其余是任务 id 字符串，所以用 any 接。
type midjourneySubmitResponse struct {
	Code        int    `json:"code"`
	Description string `json:"description"`
	Result      any    `json:"result"`
	Properties  any    `json:"properties"`
}

// midjourneySubmitAccepted = new-api RecordMidjourneyPolicyResponse 的受理判定：
// HTTP 200 + code ∈ {1,21,22}，且 properties.status 不是 FAILURE。
func midjourneySubmitAccepted(statusCode int, parsed midjourneySubmitResponse) bool {
	if statusCode != http.StatusOK {
		return false
	}
	if parsed.Code != 1 && parsed.Code != 21 && parsed.Code != 22 {
		return false
	}
	if props, ok := parsed.Properties.(map[string]any); ok {
		if status, _ := props["status"].(string); status == "FAILURE" {
			return false
		}
	}
	return true
}

// rewriteMidjourneySubmitBody = new-api 对 21/22 的回包改写（relay/mjproxy_handler.go:623, 676）。
func rewriteMidjourneySubmitBody(body []byte, code int, action string) []byte {
	switch code {
	case 21:
		if action != mjActionInPaint && action != mjActionCustomZoom {
			return []byte(strings.ReplaceAll(string(body), `"code":21`, `"code":1`))
		}
	case 22:
		return []byte(strings.ReplaceAll(string(body), `"code":22`, `"code":1`))
	}
	return body
}

// ---- 归属：内存（一级） + midjourney_tasks 表（二级，扛重启） ----

type midjourneyTaskEntry struct {
	channelID    int
	channelKeyID int
	model        string
	ownerTokenID int
	ownerUserID  int
	expiresAt    time.Time
}

var midjourneyTaskStore = struct {
	sync.Mutex
	items map[string]midjourneyTaskEntry
}{items: make(map[string]midjourneyTaskEntry)}

func recordMidjourneyTaskOwner(ctx context.Context, taskID string, entry midjourneyTaskEntry, action string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || entry.channelID == 0 || entry.channelKeyID == 0 {
		return
	}
	now := time.Now()
	entry.expiresAt = now.Add(midjourneyTaskMemoryTTL)
	midjourneyTaskStore.Lock()
	for id, e := range midjourneyTaskStore.items {
		if now.After(e.expiresAt) {
			delete(midjourneyTaskStore.items, id)
		}
	}
	midjourneyTaskStore.items[taskID] = entry
	midjourneyTaskStore.Unlock()

	if err := op.MidjourneyTaskCreate(ctx, &model.MidjourneyTask{
		UpstreamTaskID: taskID,
		ChannelID:      entry.channelID,
		ChannelKeyID:   entry.channelKeyID,
		Model:          entry.model,
		Action:         action,
		OwnerTokenID:   entry.ownerTokenID,
		OwnerUserID:    entry.ownerUserID,
		CreatedAt:      now.Unix(),
	}); err != nil {
		// 上游已经受理（并计费），客户端马上拿到 id：落库失败只记日志，内存仍能服务本进程。
		log.Warnf("midjourney task persist failed (task %s, channel %d): %v", taskID, entry.channelID, err)
	}
}

func midjourneyTaskOwner(ctx context.Context, taskID string) (midjourneyTaskEntry, bool) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return midjourneyTaskEntry{}, false
	}
	midjourneyTaskStore.Lock()
	entry, ok := midjourneyTaskStore.items[taskID]
	if ok && time.Now().After(entry.expiresAt) {
		delete(midjourneyTaskStore.items, taskID)
		ok = false
	}
	midjourneyTaskStore.Unlock()
	if ok {
		return entry, true
	}
	row, err := op.MidjourneyTaskGetByUpstreamID(ctx, taskID)
	if err != nil {
		// 读不出归属 = 拒绝，绝不能变成放行。
		log.Warnf("midjourney task owner lookup failed (task %s): %v", taskID, err)
		return midjourneyTaskEntry{}, false
	}
	if row == nil || row.ChannelID == 0 || row.ChannelKeyID == 0 {
		return midjourneyTaskEntry{}, false
	}
	return midjourneyTaskEntry{
		channelID:    row.ChannelID,
		channelKeyID: row.ChannelKeyID,
		model:        row.Model,
		ownerTokenID: row.OwnerTokenID,
		ownerUserID:  row.OwnerUserID,
	}, true
}

// MJ 任务与视频任务同一套租户规则（fail-closed，见 taskOwnerMatchesRequest）：
// 有 token 归属就只认同一个 token，否则认同一个用户；无归属的旧任务只给管理员控制台会话。

// midjourneyTaskTarget 解析一条已有任务的归属渠道与 key。失败时已写好响应。
func midjourneyTaskTarget(c *gin.Context, ctx context.Context, taskID string) (midjourneyTaskEntry, *model.Channel, string, bool) {
	entry, ok := midjourneyTaskOwner(ctx, taskID)
	if !ok || !taskOwnerMatchesRequest(c, entry.ownerTokenID, entry.ownerUserID) {
		writeMidjourneyError(c, http.StatusBadRequest, "task_not_found")
		return midjourneyTaskEntry{}, nil, "", false
	}
	channel, ok := midjourneyTaskChannel(ctx, entry)
	if !ok {
		writeMidjourneyError(c, http.StatusBadGateway, "task_channel_unavailable")
		return midjourneyTaskEntry{}, nil, "", false
	}
	key, ok := videoChannelKeyByID(channel, entry.channelKeyID)
	if !ok {
		writeMidjourneyError(c, http.StatusBadGateway, "task_channel_key_unavailable")
		return midjourneyTaskEntry{}, nil, "", false
	}
	return entry, channel, key, true
}

func midjourneyTaskChannel(ctx context.Context, entry midjourneyTaskEntry) (*model.Channel, bool) {
	channel, err := op.ChannelGet(entry.channelID, ctx)
	if err != nil || channel == nil || !channel.Enabled || !isMidjourneyCapableChannelType(channel.Type) {
		return nil, false
	}
	return channel, true
}

func midjourneySingleAttempt(channel *model.Channel, keyID int, modelName string, success bool, started time.Time, msg string) []model.ChannelAttempt {
	status := model.AttemptFailed
	if success {
		status = model.AttemptSuccess
	}
	return []model.ChannelAttempt{{
		ChannelID:    channel.ID,
		ChannelKeyID: keyID,
		ChannelName:  channel.Name,
		ModelName:    modelName,
		AttemptNum:   1,
		Status:       status,
		Duration:     int(time.Since(started).Milliseconds()),
		Msg:          msg,
	}}
}

// ---- 提交 ----

// MidjourneySubmitHandler 处理 POST /mj/submit/<mode> 与 /mj/insight-face/swap。
func MidjourneySubmitHandler(c *gin.Context, mode string) {
	ctx := c.Request.Context()
	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")

	bc, err := bodycache.New(c.Request.Body)
	if err != nil {
		var tooLarge *bodycache.BodyTooLargeError
		if errors.As(err, &tooLarge) {
			writeMidjourneyError(c, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeMidjourneyError(c, http.StatusInternalServerError, "read_request_body_failed")
		return
	}
	defer func() {
		if cerr := bc.Close(); cerr != nil {
			log.Warnf("failed to close midjourney body cache: %v", cerr)
		}
	}()
	reader, err := bc.NewReader()
	if err != nil {
		writeMidjourneyError(c, http.StatusInternalServerError, "read_request_body_failed")
		return
	}
	var payload map[string]any
	dec := json.NewDecoder(reader)
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil || payload == nil {
		writeMidjourneyError(c, http.StatusBadRequest, "bind_request_body_failed")
		return
	}

	plan, desc := planMidjourneySubmit(mode, payload)
	if desc != "" {
		writeMidjourneyError(c, http.StatusBadRequest, desc)
		return
	}
	if !op.IsModelSupported(strings.TrimSpace(c.GetString("supported_models")), plan.model) {
		writeMidjourneyError(c, http.StatusBadRequest, "model not supported: "+plan.model)
		return
	}

	// new-api DoMidjourneyHttpRequest：MjAccountFilterEnabled / MjNotifyEnabled 默认关，
	// 两个字段一律删掉。notifyHook 尤其不能放：它让上游回调一个客户端随意指定的地址。
	delete(payload, "accountFilter")
	delete(payload, "notifyHook")
	body, err := json.Marshal(payload)
	if err != nil {
		writeMidjourneyError(c, http.StatusInternalServerError, "marshal_request_body_failed")
		return
	}

	metrics := newRawProtocolRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), plan.model, midjourneySubmitLogName, c.Request.URL.Path)
	metrics.RequestUserAgent = c.Request.UserAgent() // 仅入站 UA 快照，供日志 / 用户页识别客户端
	metrics.RequestContent = metrics.upstreamBodyLog.contentWithImages(func(capture *imageCapture) string {
		return buildRawProtocolRequestContentWith(false, bc, payload, midjourneySubmitLogName, capture)
	})
	upstreamPath := midjourneySubmitUpstreamPath(mode)

	if plan.originTaskID != "" {
		midjourneySubmitPinned(c, ctx, plan, upstreamPath, body, metrics)
		return
	}
	midjourneySubmitRouted(c, ctx, plan, upstreamPath, body, metrics)
}

// midjourneySubmitPinned：放大 / 变换 / 弹窗等后续动作必须回到原任务的渠道与 key
// （new-api relay/mjproxy_handler.go:484-501），只打一次。
func midjourneySubmitPinned(c *gin.Context, ctx context.Context, plan midjourneySubmitPlan, upstreamPath string, body []byte, metrics *rawProtocolRelayMetrics) {
	entry, channel, channelKey, ok := midjourneyTaskTarget(c, ctx, plan.originTaskID)
	if !ok {
		metrics.Save(ctx, false, fmt.Errorf("midjourney origin task %s not available", plan.originTaskID), nil)
		return
	}
	actualModel := plan.model
	if mapped, ok := channel.ModelMapping[actualModel]; ok && mapped != "" {
		actualModel = mapped
	}
	started := time.Now()
	statusCode, respBody, upstreamCT, fwdErr := midjourneyForward(ctx, c, http.MethodPost, upstreamPath, body, channel, channelKey)
	if fwdErr != nil {
		metrics.Save(ctx, false, fwdErr, midjourneySingleAttempt(channel, entry.channelKeyID, actualModel, false, started, auditErrorMessage(fwdErr)))
		status, _, message := relayErrorResponse(fwdErr)
		writeMidjourneyError(c, status, message)
		return
	}
	accepted := finishMidjourneySubmit(c, ctx, plan, channel, entry.channelKeyID, actualModel, statusCode, respBody, upstreamCT, metrics)
	metricsErr := error(nil)
	if !accepted {
		metricsErr = midjourneyRejectedError(respBody)
	}
	metrics.Save(ctx, accepted, metricsErr, midjourneySingleAttempt(channel, entry.channelKeyID, actualModel, accepted, started, ""))
}

// midjourneySubmitRouted：新任务按模型名走正常的分组 / 负载均衡。
func midjourneySubmitRouted(c *gin.Context, ctx context.Context, plan midjourneySubmitPlan, upstreamPath string, body []byte, metrics *rawProtocolRelayMetrics) {
	apiKeyID := c.GetInt("api_key_id")
	routeResult, status, message, err := selectRouteGroup(c, apiKeyID, plan.model)
	if err != nil || status != 0 {
		if message == "" && err != nil {
			message = err.Error()
		}
		writeMidjourneyError(c, status, message)
		return
	}
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	group := enrichGroupForSmartRouting(ctx, routeResult.Group, false)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, plan.model, "", false)
	if iter.Len() == 0 {
		writeMidjourneyError(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	var (
		lastErr          error
		allAttempts      []model.ChannelAttempt
		triedReturnGroup bool
	)
runIterator:
	for iter.Next() {
		select {
		case <-ctx.Done():
			metrics.Save(ctx, false, context.Canceled, append(allAttempts, iter.Attempts()...))
			return
		default:
		}
		item := iter.Item()
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
			lastErr = err
			continue
		}
		if !channel.Enabled {
			iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
			continue
		}
		if !isMidjourneyCapableChannelType(channel.Type) {
			reason := fmt.Sprintf("unsupported channel type for midjourney: %d", channel.Type)
			if channel.Type == outbound.OutboundTypeCustomOpenAIChat {
				reason = CustomChannelNoFullURLReason
			}
			iter.Skip(channel.ID, 0, channel.Name, reason)
			continue
		}
		if mapped, ok := channel.ModelMapping[item.ModelName]; ok && mapped != "" {
			item.ModelName = mapped
			iter.RemapCurrentModel(mapped)
		}
		availableKeys := channel.GetAvailableChannelKeys()
		if len(availableKeys) == 0 {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, item.ModelName, 0)

		for keyIndex, usedKey := range availableKeys {
			if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
				continue
			}
			log.Infof("midjourney %s model %s, forwarding to channel: %s model: %s (attempt %d/%d, key %d/%d)",
				plan.action, plan.model, channel.Name, item.ModelName, iter.Index()+1, iter.Len(), keyIndex+1, len(availableKeys))
			span := iter.StartAttempt(channel.ID, usedKey.ID, channel.Name)
			recordAttemptProxy(span, channel)
			statusCode, respBody, upstreamCT, fwdErr := func() (int, []byte, string, error) {
				finish := balancer.BeginRuntimeAttempt(channel.ID, usedKey.ID, item.ModelName)
				defer finish()
				return midjourneyForward(ctx, c, http.MethodPost, upstreamPath, body, channel, usedKey.ChannelKey)
			}()
			usedAt := time.Now().Unix()

			if fwdErr == nil {
				op.ChannelKeyRecordUse(usedKey, statusCode, usedAt, 0)
				span.End(model.AttemptSuccess, statusCode, "")
				balancer.RecordRuntimeSuccess(channel.ID, usedKey.ID, item.ModelName, balancer.AttemptRuntimeMetrics{
					Duration:   span.Duration(),
					FirstToken: span.Duration(),
				})
				op.StatsChannelUpdate(channel.ID, model.StatsMetrics{WaitTime: span.Duration().Milliseconds(), RequestSuccess: 1})
				balancer.RecordSuccess(channel.ID, usedKey.ID, item.ModelName)
				// HTTP 2xx：上游已经给出业务结论（受理或拒绝），无论哪种都不再换渠道。
				accepted := finishMidjourneySubmit(c, ctx, plan, channel, usedKey.ID, item.ModelName, statusCode, respBody, upstreamCT, metrics)
				metricsErr := error(nil)
				if !accepted {
					metricsErr = midjourneyRejectedError(respBody)
				}
				metrics.Save(ctx, accepted, metricsErr, append(allAttempts, iter.Attempts()...))
				return
			}

			recordStatusCode := attemptStatusCode(statusCode, fwdErr)
			op.ChannelKeyRecordUse(usedKey, recordStatusCode, usedAt, 0)
			span.End(model.AttemptFailed, recordStatusCode, auditErrorMessage(fwdErr))
			breakerCounted := shouldRecordBreakerFailure(recordStatusCode, fwdErr)
			if breakerCounted {
				retryAfter, _ := retryAfterFromError(fwdErr)
				balancer.RecordRuntimeFailure(channel.ID, usedKey.ID, item.ModelName, recordStatusCode, span.Duration(), retryAfter)
			}
			channelStats := model.StatsMetrics{WaitTime: span.Duration().Milliseconds()}
			if breakerCounted {
				channelStats.RequestFailed = 1
				balancer.RecordFailureWithStatus(channel.ID, usedKey.ID, item.ModelName, recordStatusCode)
			}
			op.StatsChannelUpdate(channel.ID, channelStats)

			lastErr = fmt.Errorf("channel %s failed: %w", channel.Name, fwdErr)
			// 传输层失败（没拿到任何 HTTP 状态）是含糊的：上游可能已经建了任务。
			// 照 new-api 的 RecordMidjourneyPolicyResponse：绝不在另一个渠道上再提交一次。
			if statusCode == 0 {
				allAttempts = append(allAttempts, iter.Attempts()...)
				metrics.Save(ctx, false, lastErr, allAttempts)
				status, _, message := relayErrorResponse(lastErr)
				writeMidjourneyError(c, status, message)
				return
			}
			if !shouldTryNextChannelKey(recordStatusCode) {
				break
			}
		}
	}

	allAttempts = append(allAttempts, iter.Attempts()...)
	if shouldReturnToOriginalGroup(routeResult, triedReturnGroup) {
		triedReturnGroup = true
		fallbackGroup, err := op.GroupGetEnabledMap(plan.model, ctx)
		if err != nil {
			lastErr = err
		} else {
			fallbackGroup = enrichGroupForSmartRouting(ctx, fallbackGroup, false)
			fallbackIter := balancer.NewIteratorWithSession(fallbackGroup, apiKeyID, plan.model, "", false)
			if fallbackIter.Len() > 0 {
				iter = fallbackIter
				goto runIterator
			}
		}
	}

	finalErr := lastErr
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	metrics.Save(ctx, false, finalErr, allAttempts)
	status, _, message = relayErrorResponse(finalErr)
	writeMidjourneyError(c, status, message)
}

// finishMidjourneySubmit 处理一次 HTTP 2xx 的提交回包：判定受理、记归属、计费、
// 改写 21/22、写回客户端。返回是否受理。
func finishMidjourneySubmit(c *gin.Context, ctx context.Context, plan midjourneySubmitPlan, channel *model.Channel, channelKeyID int, actualModel string, statusCode int, respBody []byte, upstreamCT string, metrics *rawProtocolRelayMetrics) bool {
	var parsed midjourneySubmitResponse
	_ = json.Unmarshal(respBody, &parsed)
	accepted := midjourneySubmitAccepted(statusCode, parsed)
	metrics.ActualModel = actualModel
	taskID := ""
	if s, ok := parsed.Result.(string); ok {
		taskID = strings.TrimSpace(s)
	}
	if accepted {
		if taskID != "" {
			recordMidjourneyTaskOwner(ctx, taskID, midjourneyTaskEntry{
				channelID:    channel.ID,
				channelKeyID: channelKeyID,
				model:        actualModel,
				ownerTokenID: c.GetInt("api_key_id"),
				ownerUserID:  c.GetInt("user_id"),
			}, plan.action)
		}
		if plan.billable {
			applyRawProtocolPerCallCost(metrics, actualModel)
		}
		respBody = rewriteMidjourneySubmitBody(respBody, parsed.Code, plan.action)
	}
	metrics.ResponseContent = truncateString(fmt.Sprintf(`{"protocol":%q,"action":%q,"code":%d,"task_id":%q,"description":%q}`,
		midjourneySubmitLogName, plan.action, parsed.Code, taskID, parsed.Description), 2048)
	if strings.TrimSpace(upstreamCT) == "" {
		upstreamCT = "application/json"
	}
	c.Data(statusCode, upstreamCT, respBody)
	return accepted
}

func midjourneyRejectedError(respBody []byte) error {
	var parsed midjourneySubmitResponse
	_ = json.Unmarshal(respBody, &parsed)
	if parsed.Description != "" {
		return fmt.Errorf("midjourney submit not accepted: code %d: %s", parsed.Code, parsed.Description)
	}
	return fmt.Errorf("midjourney submit not accepted: code %d", parsed.Code)
}

// ---- 查询 ----

// MidjourneyTaskFetchHandler 处理 GET /mj/task/:id/fetch 与 /mj/task/:id/image-seed：
// 原样转发给任务归属的渠道 + key（new-api RelayMidjourneyTaskImageSeed 的做法；fetch 在
// new-api 是读本地库，这里同样回到归属渠道，以上游为准）。
func MidjourneyTaskFetchHandler(c *gin.Context, suffix string) {
	ctx := c.Request.Context()
	taskID := strings.TrimSpace(c.Param("id"))
	if taskID == "" {
		writeMidjourneyError(c, http.StatusBadRequest, "task_id_is_required")
		return
	}
	entry, channel, channelKey, ok := midjourneyTaskTarget(c, ctx, taskID)
	if !ok {
		return
	}
	metrics := newRawProtocolRelayMetrics(c.GetInt("api_key_id"), c.GetInt("user_id"), c.GetString("request_ip"), entry.model, midjourneyTaskLogName, c.Request.URL.Path)
	metrics.RequestUserAgent = c.Request.UserAgent() // 仅入站 UA 快照，供日志 / 用户页识别客户端
	metrics.ActualModel = entry.model
	started := time.Now()
	statusCode, body, upstreamCT, fwdErr := midjourneyForward(ctx, c, http.MethodGet, "/mj/task/"+url.PathEscape(taskID)+"/"+suffix, nil, channel, channelKey)
	if fwdErr != nil {
		metrics.Save(ctx, false, fwdErr, midjourneySingleAttempt(channel, entry.channelKeyID, entry.model, false, started, auditErrorMessage(fwdErr)))
		status, _, message := relayErrorResponse(fwdErr)
		writeMidjourneyError(c, status, message)
		return
	}
	metrics.ResponseContent = "midjourney task " + suffix
	if strings.TrimSpace(upstreamCT) == "" {
		upstreamCT = "application/json"
	}
	c.Data(statusCode, upstreamCT, body)
	metrics.Save(ctx, true, nil, midjourneySingleAttempt(channel, entry.channelKeyID, entry.model, true, started, ""))
}

// MidjourneyTaskListByConditionHandler 处理 POST /mj/task/list-by-condition {"ids":[...]}。
//
// new-api 从本地库按「当前用户 + ids」查（model.GetByMJIds），别人的 id 和不认识的 id
// 直接不出现在结果里。这里同样先按归属过滤，再按「渠道 + key」分组各问一次上游，
// 把各组返回的数组拼起来。某一组上游失败只丢那一组（记日志），不让一个坏渠道拖垮整次查询。
func MidjourneyTaskListByConditionHandler(c *gin.Context) {
	ctx := c.Request.Context()
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20)).Decode(&req); err != nil {
		writeMidjourneyError(c, http.StatusBadRequest, "bind_request_body_failed")
		return
	}
	type groupKey struct{ channelID, keyID int }
	groups := make(map[groupKey][]string)
	var order []groupKey
	seen := make(map[string]bool)
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry, ok := midjourneyTaskOwner(ctx, id)
		if !ok || !taskOwnerMatchesRequest(c, entry.ownerTokenID, entry.ownerUserID) {
			continue
		}
		k := groupKey{entry.channelID, entry.channelKeyID}
		if _, exists := groups[k]; !exists {
			order = append(order, k)
		}
		groups[k] = append(groups[k], id)
	}

	metrics := newRawProtocolRelayMetrics(c.GetInt("api_key_id"), c.GetInt("user_id"), c.GetString("request_ip"), "", midjourneyTaskLogName, c.Request.URL.Path)
	metrics.RequestUserAgent = c.Request.UserAgent() // 仅入站 UA 快照，供日志 / 用户页识别客户端
	results := make([]json.RawMessage, 0)
	var attempts []model.ChannelAttempt
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
		body, _ := json.Marshal(map[string]any{"ids": groups[k]})
		_, respBody, _, fwdErr := midjourneyForward(ctx, c, http.MethodPost, "/mj/task/list-by-condition", body, channel, key)
		if fwdErr != nil {
			log.Warnf("midjourney list-by-condition on channel %d failed: %v", channel.ID, fwdErr)
			attempts = append(attempts, midjourneySingleAttempt(channel, k.keyID, "", false, started, auditErrorMessage(fwdErr))...)
			continue
		}
		var part []json.RawMessage
		if err := json.Unmarshal(respBody, &part); err != nil {
			log.Warnf("midjourney list-by-condition on channel %d returned a non-array body", channel.ID)
			attempts = append(attempts, midjourneySingleAttempt(channel, k.keyID, "", false, started, "non-array response")...)
			continue
		}
		results = append(results, part...)
		attempts = append(attempts, midjourneySingleAttempt(channel, k.keyID, "", true, started, "")...)
	}
	metrics.ResponseContent = fmt.Sprintf("midjourney list-by-condition: %d of %d ids", len(results), len(req.IDs))
	c.JSON(http.StatusOK, results)
	metrics.Save(ctx, true, nil, attempts)
}
