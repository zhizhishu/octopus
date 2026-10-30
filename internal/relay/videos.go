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
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/helper"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// videoResponseBodyLimit caps the buffered create/poll response. These are tiny
// JSON status objects (task id + url), so this is only a safety ceiling.
const videoResponseBodyLimit = 8 << 20 // 8MB

// videoMediaKind* 是「这条视频任务在哪个子路径上创建」的三个取值。
//
// 为什么需要它：上游是按子路径分组管理任务的 —— 在 /videos/edits 创建的任务
// 只能回到 /videos/edits/{id} 查询，去 /videos/generations/{id} 问永远是 404。
//
// 空值 "" 留给兼容别名 /videos，同时也是存量任务的零值（改动前创建、库里没有
// 这一列）：零值必须等于老行为，否则一次重启就让所有在途任务的查询路径全错。
const (
	VideoMediaKindGenerations = "generations"
	VideoMediaKindEdits       = "edits"
	VideoMediaKindExtensions  = "extensions"
)

// videoUpstreamPath 返回某个 media kind 下「创建 / 查询 / 取成品」的上游路径。
// 三种能力共用一个 kind，是因为上游就是按 kind 分组的 —— 分开写三张表会在
// 新增 kind 时漏掉一处（这个仓库栽过三次：header 顺序、stats 鉴权、视频计费）。
//
// taskID 为空表示「创建」，此时 wantContent 无意义（还没东西可取）。
// 未知 kind **一律退回 "" 分支**：库里的行可能被手工改过，一条看不懂的 kind
// 不许把整条轮询链拼成畸形 URL，更不许直接报错。
func videoUpstreamPath(kind string, taskID string, wantContent bool) string {
	base := videoUpstreamDefaultPrefix
	switch kind {
	case VideoMediaKindGenerations, VideoMediaKindEdits, VideoMediaKindExtensions:
		base += "/" + kind
	}
	id := strings.TrimSpace(taskID)
	if id == "" {
		return base
	}
	base += "/" + url.PathEscape(id)
	if wantContent {
		base += "/content"
	}
	return base
}

// videoUpstreamDefaultPrefix 是视频端点标准路径的前缀。渠道逃生口
// （OpenAIVideosPath）只替换**这一段**，kind/id/content 依旧拼在它后面——
// 丢了 kind 子路径，一次 edits 任务就会打到 generations 上去。
const videoUpstreamDefaultPrefix = "/v1/videos"

// videoUpstreamPathForChannel 把渠道的路径逃生口与 videoUpstreamPath 的标准路径
// 组合成最终要落给 JoinOpenAIPath 的路径。
//
// 分工是刻意的：videoUpstreamPath 保持**纯函数**（kind/id/content → canonical
// /v1/videos… 路径），这里只做一次前缀替换。两边都空时结果与改动前逐字节相同。
//
// 任务查询优先走 OpenAITasksPath：有些上游创建在 /v1/videos，查询却在
// /v1/tasks/{id}，两者不同源。那一支不带 kind（查询路径由上游自己定）。
func videoUpstreamPathForChannel(channel *model.Channel, kind string, taskID string, wantContent bool) string {
	id := strings.TrimSpace(taskID)
	if id != "" && channel != nil {
		if tasksPrefix := channel.GetOpenAITasksPath(); tasksPrefix != "" {
			taskPath := tasksPrefix + "/" + url.PathEscape(id)
			if wantContent {
				taskPath += "/content"
			}
			return taskPath
		}
	}
	standard := videoUpstreamPath(kind, id, wantContent)
	prefix := videoUpstreamDefaultPrefix
	if channel != nil {
		prefix = channel.GetOpenAIVideosPath()
	}
	return prefix + strings.TrimPrefix(standard, videoUpstreamDefaultPrefix)
}

// VideosHandler relays an asynchronous video-generation create call
// (POST /v1/videos). It routes by model exactly like the other relays, forwards
// the request to the chosen channel/key, and on success records which
// channel+key owns each returned task/video id so a later poll can return to
// the same upstream account (the task only exists there).
//
// 这是兼容别名入口，media kind 为空（= 上游 /v1/videos）。
func VideosHandler(c *gin.Context) {
	VideosHandlerKind(c, "")
}

// VideosHandlerKind 是带 media kind 的创建入口：/v1/videos/generations |
// edits | extensions 各自把 kind 一路带到内存归属、库里那一行和后台轮询上。
//
// 为什么 kind 必须跟着走完全程：一个任务在哪个子路径创建，后续查询就必须
// 回到同一个子路径（见 videoUpstreamPath）。只记在创建时的局部变量里，
// 轮询与重启后的恢复都会回到 /v1/videos/{id}，对上上游就是必然 404。
func VideosHandlerKind(c *gin.Context, mediaKind string) {
	ctx := c.Request.Context()
	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")

	bc, err := bodycache.New(c.Request.Body)
	if err != nil {
		var tooLarge *bodycache.BodyTooLargeError
		if errors.As(err, &tooLarge) {
			resp.Error(c, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() {
		if cerr := bc.Close(); cerr != nil {
			log.Warnf("failed to close video body cache: %v", cerr)
		}
	}()

	payload, requestModel, _, perr := parseJSONModelAndStreamWithDefault(bc, "")
	if perr != nil {
		resp.Error(c, http.StatusBadRequest, perr.Error())
		return
	}

	supportedModels := strings.TrimSpace(c.GetString("supported_models"))
	if !op.IsModelSupported(supportedModels, requestModel) {
		resp.Error(c, http.StatusBadRequest, "model not supported")
		return
	}

	routeResult, status, message, err := selectRouteGroup(c, apiKeyID, requestModel)
	if err != nil || status != 0 {
		if message == "" && err != nil {
			message = err.Error()
		}
		resp.Error(c, status, message)
		return
	}
	group := enrichGroupForSmartRouting(ctx, routeResult.Group, false)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, requestModel, "", false)
	if iter.Len() == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	metrics := newRawProtocolRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), requestModel, "videos", c.Request.URL.Path)
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	metrics.RequestContent = redactLogBody(buildRawProtocolRequestContent(false, bc, payload, "videos"))

	var (
		lastErr          error
		allAttempts      []model.ChannelAttempt
		triedReturnGroup bool
	)

runIterator:
	for iter.Next() {
		select {
		case <-ctx.Done():
			log.Infof("request context canceled, stopping video create retry")
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
		// 视频创建用**视频专用**判据，不是 raw 协议那张表：raw 那张含 Embedding
		// （音频/completions/rerank 等共用），视频不许让嵌入渠道进来。
		if !isVideoCapableChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("unsupported channel type for video: %d", channel.Type))
			continue
		}
		if channel.Type == outbound.OutboundTypeCustomOpenAIChat {
			if _, ok := customChannelAbsoluteURL(channel.OpenAIVideosPath); !ok {
				iter.Skip(channel.ID, 0, channel.Name, CustomChannelNoFullURLReason)
				continue
			}
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
			log.Infof("video create model %s, forwarding to channel: %s model: %s (attempt %d/%d, key %d/%d)",
				requestModel, channel.Name, item.ModelName, iter.Index()+1, iter.Len(), keyIndex+1, len(availableKeys))

			span := iter.StartAttempt(channel.ID, usedKey.ID, channel.Name)
			recordAttemptProxy(span, channel)
			statusCode, body, upstreamCT, fwdErr := func() (int, []byte, string, error) {
				finish := balancer.BeginRuntimeAttempt(channel.ID, usedKey.ID, item.ModelName)
				defer finish()
				return videoForward(ctx, c, http.MethodPost, videoUpstreamPathForChannel(channel, mediaKind, "", false), payload, item.ModelName, channel, usedKey.ChannelKey)
			}()
			usedAt := time.Now().Unix()

			if fwdErr == nil {
				ids := extractVideoTaskIDs(body)
				recordVideoTaskOwner(ids, channel.ID, usedKey.ID, item.ModelName, apiKeyID, userID, mediaKind)
				recordVideoTaskRow(ctx, ids, channel.ID, usedKey.ID, item.ModelName, apiKeyID, userID, mediaKind, payload)

				metrics.ActualModel = item.ModelName
				metrics.ResponseContent = buildVideoResponseNote(ids)
				// 🔴 视频是唯一"上游按条收费、响应里却没有 token 用量"的端点。
				// 不在这里落成本，metrics.Save 收到的 InputCost+OutputCost 就是 0，
				// op.UserRecordUsage 第一行 `if cost <= 0 return nil` 直接返回 ——
				// 余额、月卡、单 key 的 MaxCost 三个口子全都不动，持任意 key 可无限刷。
				applyVideoCallCost(metrics, item.ModelName)
				op.ChannelKeyRecordUse(usedKey, statusCode, usedAt, 0)
				span.End(model.AttemptSuccess, statusCode, "")
				balancer.RecordRuntimeSuccess(channel.ID, usedKey.ID, item.ModelName, balancer.AttemptRuntimeMetrics{
					Duration:   span.Duration(),
					FirstToken: span.Duration(),
					Stream:     false,
				})
				op.StatsChannelUpdate(channel.ID, model.StatsMetrics{
					WaitTime:       span.Duration().Milliseconds(),
					RequestSuccess: 1,
				})
				balancer.RecordSuccess(channel.ID, usedKey.ID, item.ModelName)

				writeVideoResponse(c, statusCode, upstreamCT, body)
				metrics.Save(ctx, true, nil, append(allAttempts, iter.Attempts()...))
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
			}
			op.StatsChannelUpdate(channel.ID, channelStats)
			if breakerCounted {
				balancer.RecordFailureWithStatus(channel.ID, usedKey.ID, item.ModelName, recordStatusCode)
			}

			// The upstream answered 2xx: it has already accepted (and will bill) the
			// video task, so failing over here would create a SECOND task and lose the
			// first one's id (recordVideoTaskOwner only runs on the success branch
			// above). The images and raw pass-through paths already refuse to fail over
			// once the upstream has answered; video creation is the same class of
			// money-costing, side-effecting operation. Fail here instead of silently
			// creating a duplicate.
			if statusCode >= 200 && statusCode < 300 {
				metrics.Save(ctx, false, fwdErr, append(allAttempts, iter.Attempts()...))
				return
			}

			lastErr = fmt.Errorf("channel %s failed: %w", channel.Name, fwdErr)
			if !shouldTryNextChannelKey(recordStatusCode) {
				break
			}
		}
	}

	allAttempts = append(allAttempts, iter.Attempts()...)
	if shouldReturnToOriginalGroup(routeResult, triedReturnGroup) {
		triedReturnGroup = true
		fallbackGroup, err := op.GroupGetEnabledMap(requestModel, ctx)
		if err != nil {
			lastErr = err
		} else {
			fallbackGroup = enrichGroupForSmartRouting(ctx, fallbackGroup, false)
			fallbackIter := balancer.NewIteratorWithSession(fallbackGroup, apiKeyID, requestModel, "", false)
			if fallbackIter.Len() > 0 {
				group = fallbackGroup
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
	status, code, message := relayErrorResponse(finalErr)
	resp.ErrorWithCode(c, status, code, message)
}

// VideoPollHandler relays a poll (GET /v1/videos/:id) back to the exact
// channel+key that created the task, so it always hits the upstream account
// that owns it. Returns 404 when the id is unknown/expired or belongs to a
// different tenant (fail-closed).
func VideoPollHandler(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := strings.TrimSpace(c.Param("id"))
	if taskID == "" {
		resp.Error(c, http.StatusBadRequest, "video task id required")
		return
	}

	entry, channel, channelKey, ok := videoTaskRelayTarget(c, ctx, taskID)
	if !ok {
		return
	}

	// 查询路径只认**记录下来的** kind。客户端可以用 /v1/videos/{id} 这个
	// 兼容别名去轮询一条在 /videos/edits 创建的任务，但它在上游仍然只存在于
	// /videos/edits/{id} 下面；反过来，存量任务（kind 为空）也不能因为客户端
	// 走了一个新子路径就被改送到新子路径上去。
	metrics := newRawProtocolRelayMetrics(c.GetInt("api_key_id"), c.GetInt("user_id"), c.GetString("request_ip"), entry.model, "videos_poll", c.Request.URL.Path)
	metrics.ActualModel = entry.model

	statusCode, body, upstreamCT, fwdErr := videoForward(ctx, c, http.MethodGet, videoUpstreamPathForChannel(channel, entry.mediaKind, taskID, false), nil, entry.model, channel, channelKey)
	if fwdErr != nil {
		metrics.Save(ctx, false, fwdErr, nil)
		status, code, message := relayErrorResponse(fwdErr)
		resp.ErrorWithCode(c, status, code, message)
		return
	}
	metrics.ResponseContent = "video task status"
	writeVideoResponse(c, statusCode, upstreamCT, body)
	metrics.Save(ctx, true, nil, nil)
}

// VideoContentHandler relays GET /v1/videos/<kind>/{id}/content：上游的成品
// 二进制（典型是 mp4）。
//
// 与查询共用同一套「归属查找 + 租户校验 + 渠道/key 解析」，区别只有两点：
// wantContent=true，以及响应体必须**流式**转发。绝不 ReadAll：
// videoResponseBodyLimit 是给几百字节 JSON 状态对象设的 8MB 上限，套在几十 MB
// 的视频上只会产出一个被截断的、放不出来的文件。
//
// 计费：取成品不计费（创建时已按次计过），但必须落一条 metrics.Save ——
// 否则用户拿不到成品时，库里连一条痕迹都查不到。
func VideoContentHandler(c *gin.Context) {
	ctx := c.Request.Context()
	taskID := strings.TrimSpace(c.Param("id"))
	if taskID == "" {
		resp.Error(c, http.StatusBadRequest, "video task id required")
		return
	}

	entry, channel, channelKey, ok := videoTaskRelayTarget(c, ctx, taskID)
	if !ok {
		return
	}

	metrics := newRawProtocolRelayMetrics(c.GetInt("api_key_id"), c.GetInt("user_id"), c.GetString("request_ip"), entry.model, "videos_content", c.Request.URL.Path)
	metrics.ActualModel = entry.model

	statusCode, fwdErr := videoForwardContent(ctx, c, videoUpstreamPathForChannel(channel, entry.mediaKind, taskID, true), channel, channelKey)
	if fwdErr != nil {
		metrics.Save(ctx, false, fwdErr, nil)
		status, code, message := relayErrorResponse(fwdErr)
		resp.ErrorWithCode(c, status, code, message)
		return
	}
	metrics.OpaqueResponse = true
	metrics.ResponseContent = "video content streamed"
	metrics.Save(ctx, true, nil, nil)
	log.Debugf("video content streamed for task %s (upstream status %d)", taskID, statusCode)
}

// videoTaskRelayTarget 是「查询」与「取成品」共用的入口：两级归属查找
// （内存 → 库）、租户校验、渠道与 key 解析。任何一步失败都会写好响应
// （404 / 502）并返回 false，调用方直接 return。
//
// 抽成一处不是为了省几行：这两条路径里任何一处少了一条校验，就是越权取别人的
// 成品；复制两份的代码，第二份迟早会漏掉一条。
func videoTaskRelayTarget(c *gin.Context, ctx context.Context, taskID string) (videoTaskEntry, *model.Channel, string, bool) {
	// Two-level owner lookup: memory first (it also carries every alias id the
	// create returned), then the row. The row is what makes a poll survive a
	// restart; memory stays first because it is the record that was already
	// authoritative before the table existed, so an insert that failed (see
	// recordVideoTaskRow) cannot regress a task that memory can still serve.
	entry, ok := videoTaskOwner(taskID)
	if !ok {
		entry, ok = videoTaskOwnerFromDB(ctx, taskID)
	}
	// The tenant check is applied to BOTH sources: a row on disk is not a licence
	// to poll someone else's task. Fail-closed 404, same as an unknown id.
	if !ok || !videoTaskOwnerMatches(entry, c.GetInt("api_key_id"), c.GetInt("user_id")) {
		resp.Error(c, http.StatusNotFound, "unknown or expired video task; create the task again")
		return videoTaskEntry{}, nil, "", false
	}

	channel, err := op.ChannelGet(entry.channelID, ctx)
	if err != nil {
		resp.Error(c, http.StatusBadGateway, "video task channel unavailable")
		return videoTaskEntry{}, nil, "", false
	}
	channelKey, ok := videoChannelKeyByID(channel, entry.channelKeyID)
	if !ok {
		resp.Error(c, http.StatusBadGateway, "video task channel key unavailable")
		return videoTaskEntry{}, nil, "", false
	}
	return entry, channel, channelKey, true
}

// videoForward builds and sends a single upstream request (POST create or GET
// poll), buffering the small JSON response so the caller can inspect it (to
// record task ownership) before writing it downstream. It reuses the shared
// image/raw helpers for auth headers and the channel HTTP client.
func videoForward(ctx context.Context, c *gin.Context, method, upstreamPath string, payload map[string]any, actualModel string, channel *model.Channel, channelKey string) (int, []byte, string, error) {
	targetURL, err := openAIUpstreamURL(channel, upstreamPath)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to build video url: %w", err)
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to parse video url: %w", err)
	}
	mergeRequestQuery(parsedURL, c.Request.URL.Query())

	var bodyReader io.Reader
	contentType := ""
	if method == http.MethodPost && payload != nil {
		attemptPayload := make(map[string]any, len(payload)+1)
		for k, v := range payload {
			attemptPayload[k] = v
		}
		attemptPayload["model"] = actualModel
		b, merr := json.Marshal(attemptPayload)
		if merr != nil {
			return 0, nil, "", fmt.Errorf("failed to marshal video payload: %w", merr)
		}
		bodyReader = bytes.NewReader(b)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), bodyReader)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to create video request: %w", err)
	}
	copyHeadersToUpstream(req, c, channel, channelKey, contentType, false)

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, nil, "", err
	}
	respUp, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		return 0, nil, "", fmt.Errorf("failed to send video request: %w", err)
	}
	defer respUp.Body.Close()

	upstreamCT := respUp.Header.Get("Content-Type")
	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		return respUp.StatusCode, nil, upstreamCT, newUpstreamError(respUp.StatusCode, b)
	}
	body, err := io.ReadAll(io.LimitReader(respUp.Body, videoResponseBodyLimit))
	if err != nil {
		return respUp.StatusCode, nil, upstreamCT, fmt.Errorf("failed to read video response: %w", err)
	}
	return respUp.StatusCode, body, upstreamCT, nil
}

func writeVideoResponse(c *gin.Context, statusCode int, contentType string, body []byte) {
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/json"
	}
	c.Data(statusCode, contentType, body)
}

// videoForwardContent 是取成品专用的上游请求：状态码、Content-Type 和响应体
// 原样透传，body 用 io.Copy 流式转发。
//
// 为什么不复用 videoForward：那条路径为了解析 task id 会把响应读进内存，并套上
// videoResponseBodyLimit（8MB 的 JSON 上限）。视频成品套上去只有一个结果 ——
// 一份被截断的、放不出来的 mp4。
//
// 返回的 error 只在「还没向客户端写过任何字节」时出现：非 2xx 的错误体在这里
// 被读成 upstream error（与查询路径一致）；2xx 之后的读写错误只能记日志，
// 不能再回头改响应头。
func videoForwardContent(ctx context.Context, c *gin.Context, upstreamPath string, channel *model.Channel, channelKey string) (int, error) {
	if channel == nil {
		return 0, fmt.Errorf("video content: channel is nil")
	}
	targetURL, err := openAIUpstreamURL(channel, upstreamPath)
	if err != nil {
		return 0, fmt.Errorf("failed to build video content url: %w", err)
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return 0, fmt.Errorf("failed to parse video content url: %w", err)
	}
	mergeRequestQuery(parsedURL, c.Request.URL.Query())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create video content request: %w", err)
	}
	copyHeadersToUpstream(req, c, channel, channelKey, "", false)
	// 成品是二进制，不是 JSON：客户端没带 Accept 时不能像查询那样默认要
	// application/json，否则一部分上游会按 Accept 回一个 JSON 错误体当成品。
	if strings.TrimSpace(c.Request.Header.Get("Accept")) == "" {
		req.Header.Set("Accept", "*/*")
	}

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, err
	}
	respUp, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		return 0, fmt.Errorf("failed to send video content request: %w", err)
	}
	defer respUp.Body.Close()

	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		return respUp.StatusCode, newUpstreamError(respUp.StatusCode, b)
	}

	contentType := strings.TrimSpace(respUp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Status(respUp.StatusCode)
	if _, cerr := io.Copy(c.Writer, respUp.Body); cerr != nil {
		// 响应头已经发出去了，这里只能留痕：客户端拿到的是一份不完整的流。
		log.Warnf("video content stream aborted (upstream status %d): %v", respUp.StatusCode, cerr)
	}
	return respUp.StatusCode, nil
}

// extractVideoTaskIDs pulls every id the upstream returned (task_id / video_id /
// id) so the poll can look up ownership regardless of which id the client keeps.
// applyVideoCallCost 给一次成功的视频创建计入「按次」费用。
//
// 为什么按次而不是按 token：视频上游的创建响应只回 task_id / id，**没有任何
// token 用量字段**（见 extractVideoTaskIDs 解析的字段）。所以按 Input/Output
// 单价算出来的成本恒为 0。上游真正的计价单位是"生成一条片子"，按次才能对上。
//
// 定价从模型价格行的 PerCall 取（管理员可配，见 model.LLMPrice.PerCall）：
//   - PerCall 为 0（默认）→ 不收费，行为与改动前完全一致，不会突然改账单。
//   - PerCall > 0 → 每次成功创建收这么多，乘上访问计划的倍率。
//
// 走 AccessPlan 的倍率是刻意的：同一个模型对不同用户组可能有不同折扣，
// 视频不该绕过这套规则自成一套。
func applyVideoCallCost(metrics *rawProtocolRelayMetrics, actualModel string) {
	if metrics == nil {
		return
	}
	modelPrice := price.GetLLMPrice(actualModel)
	if modelPrice == nil || modelPrice.PerCall <= 0 {
		// 没配按次单价：保持"不计费"的既有行为，但留下可观测痕迹，
		// 让"这个模型在收费却算不出钱"这件事能被发现，而不是静默漏计。
		log.Warnf("video call cost: model %q has no per_call price, billing 0 for this request "+
			"(configure per_call on the model price to bill video generation)", actualModel)
		return
	}
	snapshot := metrics.currentBillingSnapshot(actualModel)
	multiplier := snapshot.FinalMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	metrics.BillingSnapshot = snapshot
	metrics.Stats.OutputCost = modelPrice.PerCall * multiplier
}

func extractVideoTaskIDs(body []byte) []string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	for _, key := range []string{"task_id", "video_id", "id"} {
		v, ok := m[key].(string)
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		ids = append(ids, v)
	}
	return ids
}

func videoChannelKeyByID(channel *model.Channel, keyID int) (string, bool) {
	for _, k := range channel.GetAvailableChannelKeys() {
		if k.ID == keyID {
			return k.ChannelKey, true
		}
	}
	return "", false
}

func buildVideoResponseNote(ids []string) string {
	if len(ids) == 0 {
		return "video task created"
	}
	return "video task created: " + strings.Join(ids, ",")
}

// recordVideoTaskRow persists the ownership of a freshly created video task.
//
// Why dual write (memory + DB) instead of DB only: see recordVideoTaskOwner's map
// — before this change the in-memory map was the ONLY owner record, so deleting it
// in the same change that introduces the DB path would remove the sole fallback the
// moment an insert fails (DB busy, migration not run, sqlite locked). The poll
// handler reads memory first and the row second, so a failed insert degrades to
// exactly the pre-change behaviour instead of turning a created task into an
// unreachable one.
//
// Why ONE row per create rather than one row per extracted id: extractVideoTaskIDs
// returns the aliases of a single upstream task (task_id / video_id / id of the same
// create response). A row per alias would make the background poller issue one paid
// upstream GET plus one status write per alias for the same task, and could leave the
// aliases in contradictory terminal states. The alias -> owner path that already
// exists (the in-memory map, every alias recorded for defaultVideoTaskTTL) keeps
// working for the process lifetime; the row covers what memory cannot: a restart.
func recordVideoTaskRow(ctx context.Context, ids []string, channelID, channelKeyID int, modelName string, ownerTokenID, ownerUserID int, mediaKind string, requestBody map[string]any) {
	if channelID == 0 || channelKeyID == 0 || len(ids) == 0 {
		return
	}
	upstreamID := strings.TrimSpace(ids[0])
	if upstreamID == "" {
		return
	}

	existing, err := op.VideoTaskGetByUpstreamID(ctx, upstreamID)
	if err != nil {
		log.Warnf("video task row lookup before insert failed (task %s): %v", upstreamID, err)
		return
	}
	if existing != nil {
		// An idempotent upstream (or a retried create that reused the id) must not
		// produce a second row: the poller would then poll the same task twice.
		return
	}

	var requestData json.RawMessage
	if requestBody != nil {
		if b, merr := json.Marshal(requestBody); merr == nil {
			requestData = b
		} else {
			log.Warnf("video task request data marshal failed (task %s): %v", upstreamID, merr)
		}
	}

	task := &model.VideoTask{
		UpstreamTaskID: upstreamID,
		ChannelID:      channelID,
		ChannelKeyID:   channelKeyID,
		Model:          modelName,
		OwnerTokenID:   ownerTokenID,
		OwnerUserID:    ownerUserID,
		Status:         model.VideoTaskSubmitted,
		MediaKind:      mediaKind,
		SubmitTime:     time.Now().Unix(),
		RequestData:    requestData,
	}
	if err := op.VideoTaskCreate(ctx, task); err != nil {
		// The upstream already accepted (and will bill) this task and the client is
		// about to receive its id: a persistence hiccup must be logged, not surfaced
		// as a failed create. Memory still owns the poll for this process.
		log.Warnf("video task persist failed (task %s, channel %d): %v", upstreamID, channelID, err)
	}
}

// videoTaskOwnerFromDB is the second level of the poll's owner lookup: the row is
// what survives a restart, when the in-memory map is empty.
//
// A miss AND a lookup error both return false, i.e. the caller answers 404: an
// unreadable ownership store must never become an open door.
func videoTaskOwnerFromDB(ctx context.Context, taskID string) (videoTaskEntry, bool) {
	task, err := op.VideoTaskGetByUpstreamID(ctx, taskID)
	if err != nil {
		log.Warnf("video task owner lookup failed (task %s): %v", taskID, err)
		return videoTaskEntry{}, false
	}
	if task == nil || task.ChannelID == 0 || task.ChannelKeyID == 0 {
		return videoTaskEntry{}, false
	}
	// expiresAt stays zero on purpose: the row has no TTL. Where the memory map
	// forgets a task after defaultVideoTaskTTL, the row lives until the task reaches
	// a terminal state, and a task the upstream no longer knows simply answers with
	// the upstream's own response instead of a locally invented expiry.
	return videoTaskEntry{
		channelID:    task.ChannelID,
		channelKeyID: task.ChannelKeyID,
		model:        task.Model,
		ownerTokenID: task.OwnerTokenID,
		ownerUserID:  task.OwnerUserID,
		// 库里的这一列就是「重启后查询该走哪个子路径」的唯一依据；存量行为空串，
		// 正好回到 /v1/videos 的老行为。
		mediaKind: task.MediaKind,
	}, true
}

// VideoTaskPollStatus is the background poller's GET /v1/videos/{upstreamTaskID}.
//
// It lives in this package, not in internal/task, because every decision about the
// OUTBOUND identity of a video call (URL join, channel HTTP client, channel
// CustomHeader, generic UA) is made here for videoForward; a second copy of that
// logic in the poller would drift silently and put a differently-shaped request on
// the wire. The background poll has no inbound *gin.Context, so the client-mirrored
// headers videoForward copies are absent by construction — the identity is the
// channel plus its key, exactly the fields videoForward would set anyway
// (Authorization from the key, channel CustomHeader, generic UA).
//
// statusCode is returned even alongside an error so the caller can tell "the
// upstream no longer knows this task" (404 → terminal) from "try again next round"
// (5xx / network / TLS).
//
// mediaKind 是这一行创建时用的子路径（model.VideoTask.MediaKind）：后台轮询
// 和前台轮询一样，必须回到任务真正所在的子路径，否则一条 edits 任务会在第一轮
// 就被上游 404 判死。空串 = 兼容别名 /v1/videos，也是存量行的零值。
func VideoTaskPollStatus(ctx context.Context, channel *model.Channel, channelKey, upstreamTaskID, mediaKind string) (int, []byte, error) {
	if channel == nil {
		return 0, nil, fmt.Errorf("video poll: channel is nil")
	}
	if strings.TrimSpace(channelKey) == "" {
		return 0, nil, fmt.Errorf("video poll: channel key is empty")
	}
	if strings.TrimSpace(upstreamTaskID) == "" {
		return 0, nil, fmt.Errorf("video poll: upstream task id is empty")
	}

	targetURL, err := openAIUpstreamURL(channel, videoUpstreamPathForChannel(channel, mediaKind, upstreamTaskID, false))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to build video poll url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to create video poll request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+channelKey)
	if len(channel.CustomHeader) > 0 {
		for _, h := range channel.CustomHeader {
			req.Header.Set(h.HeaderKey, h.HeaderValue)
		}
	}
	setHeaderIfMissing(req.Header, "User-Agent", genericUAForChannel(channel))

	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, nil, err
	}
	respUp, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to send video poll request: %w", err)
	}
	defer respUp.Body.Close()

	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		return respUp.StatusCode, nil, newUpstreamError(respUp.StatusCode, b)
	}
	body, err := io.ReadAll(io.LimitReader(respUp.Body, videoResponseBodyLimit))
	if err != nil {
		return respUp.StatusCode, nil, fmt.Errorf("failed to read video poll response: %w", err)
	}
	return respUp.StatusCode, body, nil
}
