package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
	"gorm.io/gorm"
)

// 鹈鹕测试（创作题并排看）的接口与执行体。全部仅管理员。
//
// 执行体和检测队列一样放在 handlers 层：发请求要用 modeltest（它引用 client），
// 放进 op 会成环。它有自己的一条串行车道，不排在长检测后面；但和检测共用
// (渠道, 上游模型) 单飞闸门，同一个模型不会被两边同时打。

const (
	creativePollInterval = 3 * time.Second
	// creativeRunTimeout 单张图最多等多久。画 SVG 的回答可能很长，给足 3 分钟。
	creativeRunTimeout = 180 * time.Second
	// creativeSlotWait 同一模型有检测在跑时最多等多久再放弃。
	creativeSlotWait = 10 * time.Minute
	// creativeLaneRestartBackoff 车道意外退出后重拉的起步间隔。
	creativeLaneRestartBackoff = 5 * time.Second
	// creativeLaneRestartMaxBackoff 重拉间隔的上限。
	creativeLaneRestartMaxBackoff = 5 * time.Minute
	// creativeLaneHealthyAfter 车道活过这么久才算「健康」，重拉间隔从头算。
	creativeLaneHealthyAfter = time.Minute
)

func init() {
	router.NewGroupRouter("/api/v1/model-audit/creative").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(router.NewRoute("/rounds", http.MethodGet).Handle(listCreativeRounds)).
		AddRoute(router.NewRoute("/rounds/:round", http.MethodGet).Handle(getCreativeRound)).
		AddRoute(router.NewRoute("/items/:id", http.MethodDelete).Handle(deleteCreativeItem)).
		// PNG 上传/下载挂在**不挂 RequireJSON** 的这组：上传的请求体是 PNG 原始字节，
		// 不是 JSON；下载是二进制响应。权限沿用 creative 路由（仅管理员）。
		AddRoute(router.NewRoute("/items/:id/png", http.MethodPost).Handle(uploadCreativeItemPNG)).
		AddRoute(router.NewRoute("/items/:id/png", http.MethodGet).Handle(downloadCreativeItemPNG))

	router.NewGroupRouter("/api/v1/model-audit/creative").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(router.NewRoute("/rounds", http.MethodPost).Handle(createCreativeRound)).
		AddRoute(router.NewRoute("/items/:id/pin", http.MethodPost).Handle(pinCreativeItem)).
		AddRoute(router.NewRoute("/clear-unpinned", http.MethodPost).Handle(clearUnpinnedCreatives))
}

type creativeRoundRequest struct {
	Kind    string              `json:"kind"`
	Prompt  string              `json:"prompt"`
	Targets []op.CreativeTarget `json:"targets"`
}

func createCreativeRound(c *gin.Context) {
	var req creativeRoundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	roundID, ids, err := op.ModelAuditCreativeEnqueue(c.Request.Context(), req.Kind, req.Prompt, req.Targets, time.Now())
	if err != nil {
		if errors.Is(err, op.ErrCreativeBudget) {
			resp.Error(c, http.StatusTooManyRequests, err.Error())
			return
		}
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, gin.H{"round_id": roundID, "ids": ids})
}

func listCreativeRounds(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	rounds, err := op.ModelAuditCreativeRounds(c.Request.Context(), limit)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rounds)
}

func getCreativeRound(c *gin.Context) {
	items, err := op.ModelAuditCreativeList(c.Request.Context(), c.Param("round"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, items)
}

func creativeIDParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func pinCreativeItem(c *gin.Context) {
	id, ok := creativeIDParam(c)
	if !ok {
		return
	}
	var req struct {
		Pinned *bool `json:"pinned"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Pinned == nil {
		resp.Error(c, http.StatusBadRequest, "pinned 必须给 true 或 false")
		return
	}
	if err := op.ModelAuditCreativePin(c.Request.Context(), id, *req.Pinned); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp.Error(c, http.StatusNotFound, "这张图不存在（可能已经删了）")
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"id": id, "pinned": *req.Pinned})
}

func deleteCreativeItem(c *gin.Context) {
	id, ok := creativeIDParam(c)
	if !ok {
		return
	}
	switch err := op.ModelAuditCreativeDelete(c.Request.Context(), id); {
	case err == nil:
		resp.Success(c, gin.H{"deleted": 1})
	case errors.Is(err, gorm.ErrRecordNotFound):
		resp.Error(c, http.StatusNotFound, "这张图不存在（可能已经删了）")
	case errors.Is(err, op.ErrCreativeRunning):
		resp.Error(c, http.StatusConflict, err.Error())
	default:
		resp.Error(c, http.StatusInternalServerError, err.Error())
	}
}

func clearUnpinnedCreatives(c *gin.Context) {
	n, err := op.ModelAuditCreativeClearUnpinned(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"deleted": n})
}

// uploadCreativeItemPNG 接收前端渲染完成后自动上传的一张 PNG（SVG 走 canvas 栅格化）。
//
// 请求体是 PNG 原始字节（Content-Type 由前端设 image/png，这里只验文件头）。
// 防重复上传在 op 层做（已存过的这张返回 stored=false，不覆盖）；上限
// CreativeMaxPNGBytes，超限直接 413。
func uploadCreativeItemPNG(c *gin.Context) {
	id, ok := creativeIDParam(c)
	if !ok {
		return
	}
	// 多读 1 字节判超限：正好等于上限的包能过，超出一个字节就拒。
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, model.CreativeMaxPNGBytes+1))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, "读取上传的 PNG 失败")
		return
	}
	if len(data) > model.CreativeMaxPNGBytes {
		resp.Error(c, http.StatusRequestEntityTooLarge, fmt.Sprintf("PNG 太大了（上限 %d MB）", model.CreativeMaxPNGBytes>>20))
		return
	}
	stored, err := op.ModelAuditCreativeSetPNG(c.Request.Context(), id, data)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp.Error(c, http.StatusNotFound, "这张图不存在（可能已经删了）")
			return
		}
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, gin.H{"stored": stored})
}

// downloadCreativeItemPNG 下载/预览一张图自动存的 PNG。仅管理员，与 creative 其余
// 路由同口径。还没有 PNG 的返回 404（只有 SVG 渲染成功后才会自动生成）。
func downloadCreativeItemPNG(c *gin.Context) {
	id, ok := creativeIDParam(c)
	if !ok {
		return
	}
	data, err := op.ModelAuditCreativeGetPNG(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp.Error(c, http.StatusNotFound, "这张图不存在（可能已经删了）")
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if len(data) == 0 {
		resp.Error(c, http.StatusNotFound, "这张图还没有 PNG（SVG 渲染成功后会自动生成）")
		return
	}
	h := c.Writer.Header()
	h.Set("Content-Disposition", fmt.Sprintf(`inline; filename="creative-%d.png"`, id))
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "image/png", data)
}

var creativeWorkerStarted bool

// StartModelAuditCreativeWorker 拉起鹈鹕测试的执行车道。可重复调用。
//
// 车道不能死：车道 goroutine 万一退出（循环里 panic 被兜住、或者车道自己返回），
// 就退避几秒后重新拉起；活过 creativeLaneHealthyAfter 才算健康，重拉间隔从头算。
func StartModelAuditCreativeWorker() {
	if creativeWorkerStarted {
		return
	}
	creativeWorkerStarted = true
	safe.SafeGo("model-audit-creative", func() {
		ctx := context.Background()
		// 上次没出完的先收尾：可能已经花过钱，不重发。
		if n, err := op.ModelAuditCreativeReclaim(ctx, time.Now()); err != nil {
			log.Warnf("creative reclaim failed: %v", err)
		} else if n > 0 {
			log.Warnf("creative reclaimed %d interrupted item(s)", n)
		}
		backoff := creativeLaneRestartBackoff
		for {
			started := time.Now()
			panicked := runCreativeLane(ctx)
			// 活过一阵子再死说明不是持续性故障，退避从头算；短命退出就翻倍往后拖。
			if time.Since(started) >= creativeLaneHealthyAfter {
				backoff = creativeLaneRestartBackoff
			}
			log.Warnf("creative lane stopped (panic=%v), restarting in %s", panicked, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > creativeLaneRestartMaxBackoff {
				backoff = creativeLaneRestartMaxBackoff
			}
		}
	})
}

// runCreativeLane 串行出图的主循环：一直转到进程退出。
// 返回 true 表示是 panic 退出的（已经在这里兜住并记了栈，不让整个 goroutine 死掉）。
func runCreativeLane(ctx context.Context) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			log.Errorf("creative lane panicked: %v\n%s", r, debug.Stack())
		}
	}()
	ticker := time.NewTicker(creativePollInterval)
	defer ticker.Stop()
	for range ticker.C {
		for {
			row, err := op.ModelAuditCreativeClaimNext(ctx)
			if err != nil || row == nil {
				break
			}
			runOneCreative(ctx, row)
		}
	}
	return false
}

// creativePanicSummary 把 panic 值压成一句短话：错误文案要能直接显示在卡片上。
func creativePanicSummary(v any) string {
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" {
		s = "unknown"
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// runOneCreative 出一张图：1 次请求，走渠道检测的出站链路。绝不 panic 外泄。
func runOneCreative(ctx context.Context, row *model.ModelAuditCreative) {
	started := time.Now()
	finish := func(out op.CreativeOutcome) {
		out.DurationMs = time.Since(started).Milliseconds()
		if err := op.ModelAuditCreativeFinish(ctx, row.ID, out, time.Now()); err != nil {
			log.Warnf("creative %d finish failed: %v", row.ID, err)
		}
	}
	// 单张出图 panic 只把这张落成 failed，车道继续跑下一张（defer 包着，不动下面的发送代码）。
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("creative %d panicked: %v\n%s", row.ID, r, debug.Stack())
			finish(op.CreativeOutcome{Status: model.CreativeFailed, Error: "出图时崩了：" + creativePanicSummary(r)})
		}
	}()

	channel, err := op.ChannelGet(row.ChannelID, ctx)
	if err != nil || channel == nil {
		finish(op.CreativeOutcome{Status: model.CreativeFailed, Error: "渠道已不存在"})
		return
	}
	upstreamModel := row.Model
	if mapped, ok := channel.ModelMapping[upstreamModel]; ok && mapped != "" {
		upstreamModel = mapped
	}
	sender, err := modeltest.NewRelaySender(*channel, upstreamModel, op.AuditEndpointForChannel(*channel))
	if err != nil {
		finish(op.CreativeOutcome{Status: model.CreativeFailed, Error: err.Error()})
		return
	}
	// 鹈鹕专用：按真实 CLI 默认开思考（fix-1127 返工第 10 条），只作用于此路。
	sender.SetCreativeThinkingDefaults(true)

	// 同一 (渠道, 上游模型) 有检测在跑时等它一会儿，等不到就放弃（不发请求）。
	deadline := time.Now().Add(creativeSlotWait)
	release, ok := acquireModelAuditSlot(channel.ID, upstreamModel)
	for !ok && time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		release, ok = acquireModelAuditSlot(channel.ID, upstreamModel)
	}
	if !ok {
		finish(op.CreativeOutcome{Status: model.CreativeFailed, Error: "同一模型一直有检测在跑，等了 10 分钟没轮到，这张没发"})
		return
	}
	defer release()

	runCtx, cancel := context.WithTimeout(ctx, creativeRunTimeout)
	defer cancel()
	res, err := sender.Ask(runCtx, modelverify.Ask{Prompt: row.Prompt})
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			finish(op.CreativeOutcome{Status: model.CreativeTimeout, Error: "超时（" + strconv.Itoa(int(creativeRunTimeout.Seconds())) + " 秒没出完）"})
			return
		}
		finish(op.CreativeOutcome{Status: model.CreativeFailed, Error: strings.TrimSpace(err.Error())})
		return
	}
	input := res.Usage.Input
	if input == 0 {
		input = res.Usage.Prompt
	}
	out := op.CreativeOutcome{
		Raw:          res.Content,
		InputTokens:  input,
		OutputTokens: res.Usage.Completion,
	}
	format, content, okExtract, reason := op.ExtractCreative(res.Content)
	if !okExtract {
		out.Status = model.CreativeInvalid
		out.Error = reason
		finish(out)
		return
	}
	out.Status = model.CreativeDone
	out.Format = format
	out.Content = content
	finish(out)
}
