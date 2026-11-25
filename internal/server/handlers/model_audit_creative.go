package handlers

import (
	"context"
	"errors"
	"net/http"
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
)

func init() {
	router.NewGroupRouter("/api/v1/model-audit/creative").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(router.NewRoute("/rounds", http.MethodGet).Handle(listCreativeRounds)).
		AddRoute(router.NewRoute("/rounds/:round", http.MethodGet).Handle(getCreativeRound)).
		AddRoute(router.NewRoute("/items/:id", http.MethodDelete).Handle(deleteCreativeItem))

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

var creativeWorkerStarted bool

// StartModelAuditCreativeWorker 拉起鹈鹕测试的执行车道。可重复调用。
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
	})
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

	channel, err := op.ChannelGet(row.ChannelID, ctx)
	if err != nil || channel == nil {
		finish(op.CreativeOutcome{Status: model.CreativeFailed, Error: "渠道已不存在"})
		return
	}
	upstreamModel := row.Model
	if mapped, ok := channel.ModelMapping[upstreamModel]; ok && mapped != "" {
		upstreamModel = mapped
	}
	sender, err := modeltest.NewProbeSender(*channel, upstreamModel, op.AuditEndpointForChannel(*channel))
	if err != nil {
		finish(op.CreativeOutcome{Status: model.CreativeFailed, Error: err.Error()})
		return
	}

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
