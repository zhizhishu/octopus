package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/redteamlab"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"gorm.io/gorm"
)

// V2 deliberately exposes no arbitrary HTTP target or remote attacker provider.
// The only target sender remains the existing (CLI shape preserving) ProbeSender.
func init() {
	router.NewGroupRouter("/api/v1/redteam-lab/v2").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(router.NewRoute("/cases", http.MethodGet).Handle(redTeamV2ListCases)).
		AddRoute(router.NewRoute("/cases", http.MethodPost).Use(middleware.RequireJSON()).Handle(redTeamV2SaveCase)).
		AddRoute(router.NewRoute("/cases/import", http.MethodPost).Use(middleware.RequireJSON()).Handle(redTeamV2Import)).
		AddRoute(router.NewRoute("/cases/:caseID", http.MethodGet).Handle(redTeamV2GetCase)).
		AddRoute(router.NewRoute("/cases/:caseID/revisions", http.MethodGet).Handle(redTeamV2ListRevisions)).
		AddRoute(router.NewRoute("/runs", http.MethodGet).Handle(redTeamV2ListRuns)).
		AddRoute(router.NewRoute("/runs", http.MethodPost).Use(middleware.RequireJSON()).Handle(redTeamV2StartRun)).
		AddRoute(router.NewRoute("/runs/:runID", http.MethodGet).Handle(redTeamV2GetRun)).
		AddRoute(router.NewRoute("/runs/:runID/cancel", http.MethodPost).Handle(redTeamV2CancelRun)).
		AddRoute(router.NewRoute("/runs/:runID/reviews", http.MethodPost).Use(middleware.RequireJSON()).Handle(redTeamV2ReviewRun))
}

type redTeamV2CaseView struct {
	Spec      redteamlab.V2CaseSpec `json:"spec"`
	Revision  int                   `json:"revision"`
	SHA256    string                `json:"sha256"`
	CreatedAt time.Time             `json:"created_at"`
}

func redTeamV2Revision(c *gin.Context, id string, revision int) (redTeamV2CaseView, bool) {
	var item model.RedTeamV2Revision
	query := db.GetDB().WithContext(c.Request.Context()).Where("case_id = ?", id)
	if revision > 0 {
		query = query.Where("revision = ?", revision)
	} else {
		query = query.Order("revision DESC")
	}
	if err := query.First(&item).Error; err != nil {
		resp.Error(c, http.StatusNotFound, "案例版本不存在")
		return redTeamV2CaseView{}, false
	}
	var spec redteamlab.V2CaseSpec
	if err := json.Unmarshal([]byte(item.SpecJSON), &spec); err != nil {
		resp.Error(c, http.StatusInternalServerError, "案例版本数据损坏")
		return redTeamV2CaseView{}, false
	}
	return redTeamV2CaseView{Spec: spec, Revision: item.Revision, SHA256: item.SpecSHA256, CreatedAt: item.CreatedAt}, true
}

func redTeamV2ListCases(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var cases []model.RedTeamV2Case
	if err := db.GetDB().WithContext(c.Request.Context()).Order("updated_at DESC").Limit(100).Find(&cases).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "读取案例库失败")
		return
	}
	out := make([]redTeamV2CaseView, 0, len(cases))
	for _, item := range cases {
		var revision model.RedTeamV2Revision
		if err := db.GetDB().WithContext(c.Request.Context()).Where("case_id = ? AND revision = ?", item.ID, item.CurrentRevision).First(&revision).Error; err != nil {
			resp.Error(c, http.StatusInternalServerError, "案例库版本不一致")
			return
		}
		var spec redteamlab.V2CaseSpec
		if err := json.Unmarshal([]byte(revision.SpecJSON), &spec); err != nil {
			resp.Error(c, http.StatusInternalServerError, "案例 JSON 损坏")
			return
		}
		out = append(out, redTeamV2CaseView{Spec: spec, Revision: revision.Revision, SHA256: revision.SpecSHA256, CreatedAt: revision.CreatedAt})
	}
	resp.Success(c, out)
}

type redTeamV2RevisionMeta struct {
	Revision  int       `json:"revision"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy int       `json:"created_by"`
}

func redTeamV2ListRevisions(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	id := c.Param("caseID")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`).MatchString(id) {
		resp.Error(c, http.StatusBadRequest, "案例 ID 不合法")
		return
	}
	var revisions []model.RedTeamV2Revision
	if err := db.GetDB().WithContext(c.Request.Context()).Where("case_id = ?", id).Order("revision DESC").Limit(100).
		Select("revision", "spec_sha256", "created_at", "created_by").
		Find(&revisions).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "读取版本记录失败")
		return
	}
	result := make([]redTeamV2RevisionMeta, 0, len(revisions))
	for _, item := range revisions {
		result = append(result, redTeamV2RevisionMeta{Revision: item.Revision, SHA256: item.SpecSHA256, CreatedAt: item.CreatedAt, CreatedBy: item.CreatedBy})
	}
	resp.Success(c, result)
}

func redTeamV2GetCase(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	revision := 0
	if v := c.Query("revision"); v != "" {
		var err error
		revision, err = strconv.Atoi(v)
		if err != nil || revision < 1 {
			resp.Error(c, http.StatusBadRequest, "版本号不合法")
			return
		}
	}
	item, ok := redTeamV2Revision(c, c.Param("caseID"), revision)
	if ok {
		resp.Success(c, item)
	}
}

func redTeamV2WriteCase(tx *gorm.DB, spec redteamlab.V2CaseSpec, operatorID int) (redTeamV2CaseView, error) {
	raw, sum, err := spec.Canonical()
	if err != nil {
		return redTeamV2CaseView{}, err
	}
	var head model.RedTeamV2Case
	err = tx.Where("id = ?", spec.ID).First(&head).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		head = model.RedTeamV2Case{ID: spec.ID, CurrentRevision: 0}
	case err != nil:
		return redTeamV2CaseView{}, err
	}
	// Identical saves do not silently mint extra revisions.
	if head.CurrentRevision > 0 {
		var previous model.RedTeamV2Revision
		if err := tx.Where("case_id = ? AND revision = ?", head.ID, head.CurrentRevision).First(&previous).Error; err != nil {
			return redTeamV2CaseView{}, err
		}
		if previous.SpecSHA256 == sum {
			return redTeamV2CaseView{Spec: spec, Revision: head.CurrentRevision, SHA256: sum, CreatedAt: previous.CreatedAt}, nil
		}
	}
	next := head.CurrentRevision + 1
	rev := model.RedTeamV2Revision{CaseID: spec.ID, Revision: next, SpecJSON: raw, SpecSHA256: sum, CreatedBy: operatorID}
	if err := tx.Create(&rev).Error; err != nil {
		return redTeamV2CaseView{}, err
	}
	head.CurrentRevision = next
	if next == 1 {
		if err := tx.Create(&head).Error; err != nil {
			return redTeamV2CaseView{}, err
		}
	} else {
		// Optimistic CAS rejects concurrent stale editors rather than overwriting their revisions.
		if changed := tx.Model(&model.RedTeamV2Case{}).Where("id = ? AND current_revision = ?", spec.ID, next-1).
			Updates(map[string]any{"current_revision": next, "updated_at": time.Now()}); changed.Error != nil || changed.RowsAffected != 1 {
			if changed.Error != nil {
				return redTeamV2CaseView{}, changed.Error
			}
			return redTeamV2CaseView{}, errors.New("案例版本并发冲突，请刷新后重试")
		}
	}
	return redTeamV2CaseView{Spec: spec, Revision: next, SHA256: sum, CreatedAt: rev.CreatedAt}, nil
}

func redTeamV2SaveCase(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var spec redteamlab.V2CaseSpec
	if err := c.ShouldBindJSON(&spec); err != nil {
		resp.Error(c, http.StatusBadRequest, "案例格式无效")
		return
	}
	if err := spec.Validate(); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	var view redTeamV2CaseView
	err := db.GetDB().WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) (err error) {
		view, err = redTeamV2WriteCase(tx, spec, middleware.CurrentUserID(c))
		return err
	})
	if err != nil {
		resp.Error(c, http.StatusConflict, "案例版本无法保存：可能存在并发修改")
		return
	}
	resp.Success(c, view)
}

type redTeamV2ImportBody struct {
	Cases            []redteamlab.V2CaseSpec `json:"cases"`
	ConfirmedLicense bool                    `json:"confirmed_license"`
}

func redTeamV2Import(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var request redTeamV2ImportBody
	if err := c.ShouldBindJSON(&request); err != nil || !request.ConfirmedLicense || len(request.Cases) == 0 || len(request.Cases) > redteamlab.V2MaxCasesImported {
		resp.Error(c, http.StatusBadRequest, "导入需确认来源许可，且每批1-20个案例")
		return
	}
	seen := map[string]bool{}
	for _, item := range request.Cases {
		if err := item.Validate(); err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		if seen[item.ID] {
			resp.Error(c, http.StatusBadRequest, "导入案例 ID 重复")
			return
		}
		seen[item.ID] = true
	}
	views := make([]redTeamV2CaseView, 0, len(request.Cases))
	err := db.GetDB().WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		for _, item := range request.Cases {
			view, err := redTeamV2WriteCase(tx, item, middleware.CurrentUserID(c))
			if err != nil {
				return err
			}
			views = append(views, view)
		}
		return nil
	})
	if err != nil {
		resp.Error(c, http.StatusConflict, "导入事务未完成，案例版本可能有冲突")
		return
	}
	resp.Success(c, views)
}

type redTeamV2StartBody struct {
	CaseID         string `json:"case_id"`
	Revision       int    `json:"revision"`
	ChannelID      int    `json:"channel_id"`
	Model          string `json:"model"`
	ConfirmCost    bool   `json:"confirm_cost"`
	IdempotencyKey string `json:"idempotency_key"`
}

var redTeamV2IdemPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,72}$`)
var redTeamV2CancelMu sync.Mutex
var redTeamV2Cancels = map[int64]context.CancelFunc{}

func redTeamV2StartRun(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var req redTeamV2StartBody
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, "运行参数格式错误")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if !req.ConfirmCost || req.ChannelID < 1 || req.Revision < 1 ||
		!redTeamModelName.MatchString(req.Model) || !redTeamV2IdemPattern.MatchString(req.IdempotencyKey) {
		resp.Error(c, http.StatusBadRequest, "请指定案例版本/目标及有效幂等键，并确认可能费用")
		return
	}
	operator := middleware.CurrentUserID(c)
	var previous model.RedTeamV2Run
	if err := db.GetDB().WithContext(c.Request.Context()).Where("idempotency_key = ?", req.IdempotencyKey).First(&previous).Error; err == nil {
		if previous.OperatorID != operator || previous.CaseID != req.CaseID || previous.CaseRevision != req.Revision ||
			previous.ChannelID != req.ChannelID || previous.RequestModel != req.Model {
			resp.Error(c, http.StatusConflict, "幂等键已经用于其他请求")
			return
		}
		resp.Success(c, previous)
		return
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		resp.Error(c, http.StatusInternalServerError, "无法核验幂等键")
		return
	}
	cfg, err := loadRedTeamLabConfig()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "实验配置无法读取")
		return
	}
	if !cfg.Enabled {
		resp.Error(c, http.StatusForbidden, "实验总开关未开启，未发送请求")
		return
	}
	var revision model.RedTeamV2Revision
	if err := db.GetDB().WithContext(c.Request.Context()).Where("case_id = ? AND revision = ?", req.CaseID, req.Revision).First(&revision).Error; err != nil {
		resp.Error(c, http.StatusBadRequest, "目标案例版本不存在")
		return
	}
	var spec redteamlab.V2CaseSpec
	if err := json.Unmarshal([]byte(revision.SpecJSON), &spec); err != nil || spec.Validate() != nil {
		resp.Error(c, http.StatusInternalServerError, "案例版本校验失败")
		return
	}
	channel, err := op.ChannelGet(req.ChannelID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "渠道不存在")
		return
	}
	if !channel.Enabled || !redTeamChannelOffersModel(*channel, req.Model) {
		resp.Error(c, http.StatusBadRequest, "渠道已禁用或未配置目标模型")
		return
	}
	upstreamModel := req.Model
	if mapped, ok := channel.ModelMapping[upstreamModel]; ok && strings.TrimSpace(mapped) != "" {
		upstreamModel = mapped
	}
	endpoint := op.AuditEndpointForChannel(*channel)
	run := model.RedTeamV2Run{
		CaseID: req.CaseID, CaseRevision: req.Revision, SpecSHA256: revision.SpecSHA256,
		OperatorID: operator, IdempotencyKey: req.IdempotencyKey, ChannelID: channel.ID,
		ChannelName: channel.Name, RequestModel: req.Model, Status: "queued", Verdict: "unscored",
	}
	err = db.GetDB().WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		// Fail closed across replicas: the DB lane is acquired in the same
		// transaction as the run. If the process crashes, unknown charge stays
		// locked until explicitly investigated.
		if err := tx.FirstOrCreate(&model.RedTeamV2Lane{ID: 1}, model.RedTeamV2Lane{ID: 1}).Error; err != nil {
			return err
		}
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		changed := tx.Model(&model.RedTeamV2Lane{}).Where("id = ? AND active_run_id = 0", 1).
			Update("active_run_id", run.ID)
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return errors.New("另一个评估运行占用全局执行通道")
		}
		return nil
	})
	if err != nil {
		resp.Error(c, http.StatusConflict, "不能创建本次任务：已有未结束或待核对的运行，请查看历史")
		return
	}
	// Detached only from the HTTP request, not from Octopus process; no retry
	// after a crash or client timeout. Caller polls /runs/:id for evidence.
	go redTeamV2Execute(run.ID, *channel, upstreamModel, endpoint, spec)
	resp.Success(c, run)
}

func redTeamV2ListRuns(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var runs []model.RedTeamV2Run
	if err := db.GetDB().WithContext(c.Request.Context()).Order("id DESC").Limit(50).Omit("response_text").Find(&runs).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "无法读取评估历史")
		return
	}
	if runs == nil {
		runs = []model.RedTeamV2Run{}
	}
	resp.Success(c, runs)
}

type redTeamV2RunView struct {
	Run     model.RedTeamV2Run      `json:"run"`
	Turns   []model.RedTeamV2Turn   `json:"turns"`
	Reviews []model.RedTeamV2Review `json:"reviews"`
}

func redTeamV2RunID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("runID"), 10, 64)
	if err != nil || id < 1 {
		resp.Error(c, http.StatusBadRequest, "运行编号不合法")
		return 0, false
	}
	return id, true
}

func redTeamV2GetRun(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	id, ok := redTeamV2RunID(c)
	if !ok {
		return
	}
	var run model.RedTeamV2Run
	if err := db.GetDB().WithContext(c.Request.Context()).First(&run, id).Error; err != nil {
		resp.Error(c, http.StatusNotFound, "运行记录不存在")
		return
	}
	turns := []model.RedTeamV2Turn{}
	if err := db.GetDB().WithContext(c.Request.Context()).Where("run_id = ?", id).Order("sequence ASC").Find(&turns).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "读取运行轮次失败")
		return
	}
	reviews := []model.RedTeamV2Review{}
	if err := db.GetDB().WithContext(c.Request.Context()).Where("run_id = ?", id).Order("id ASC").Limit(100).Find(&reviews).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "读取人工复核记录失败")
		return
	}
	resp.Success(c, redTeamV2RunView{Run: run, Turns: turns, Reviews: reviews})
}

type redTeamV2ReviewBody struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func redTeamV2ReviewRun(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	id, ok := redTeamV2RunID(c)
	if !ok {
		return
	}
	var request redTeamV2ReviewBody
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, "复核内容格式错误")
		return
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Decision != "confirmed" && request.Decision != "rejected" && request.Decision != "uncertain" {
		resp.Error(c, http.StatusBadRequest, "复核结论需为 confirmed/rejected/uncertain")
		return
	}
	if n := utf8.RuneCountInString(request.Reason); n < 3 || n > 800 {
		resp.Error(c, http.StatusBadRequest, "复核说明须在3-800字")
		return
	}
	var record model.RedTeamV2Review
	err := db.GetDB().WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var run model.RedTeamV2Run
		if err := tx.First(&run, id).Error; err != nil {
			return err
		}
		if run.Status == "queued" || run.Status == "running" {
			return errors.New("评估尚未结束")
		}
		record = model.RedTeamV2Review{RunID: id, ReviewerID: middleware.CurrentUserID(c), Decision: request.Decision, Reason: request.Reason}
		return tx.Create(&record).Error
	})
	if err != nil {
		resp.Error(c, http.StatusConflict, "未能保存复核：运行不存在、仍在执行或写入失败")
		return
	}
	resp.Success(c, record)
}

// A review is an append-only interpretation of evidence. It cannot overwrite
// raw turn observations, deterministic score or any model-identity audit.
func redTeamV2CancelRun(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	id, ok := redTeamV2RunID(c)
	if !ok {
		return
	}
	tx := db.GetDB().WithContext(c.Request.Context()).Model(&model.RedTeamV2Run{}).
		Where("id = ? AND status IN ?", id, []string{"queued", "running"}).
		Update("cancel_requested", true)
	if tx.Error != nil {
		resp.Error(c, http.StatusInternalServerError, "取消操作未保存")
		return
	}
	if tx.RowsAffected == 0 {
		resp.Error(c, http.StatusConflict, "任务不存在或已结束")
		return
	}
	redTeamV2CancelMu.Lock()
	cancel := redTeamV2Cancels[id]
	redTeamV2CancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	resp.Success(c, map[string]any{"id": id, "cancel_requested": true})
}

func redTeamV2Execute(id int64, channel model.Channel, upstreamModel, endpoint string, spec redteamlab.V2CaseSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(spec.Prompts))*90*time.Second)
	redTeamV2CancelMu.Lock()
	redTeamV2Cancels[id] = cancel
	redTeamV2CancelMu.Unlock()
	defer func() {
		cancel()
		redTeamV2CancelMu.Lock()
		delete(redTeamV2Cancels, id)
		redTeamV2CancelMu.Unlock()
		if panicValue := recover(); panicValue != nil {
			// Never log a recovered value; it may carry sensitive provider data.
			redTeamV2Finish(id, "transport_error", "unscored", "", false, 0, 0, "执行组件异常中断；请核对是否已产生上游费用")
		}
	}()
	started := time.Now()
	start := db.GetDB().Model(&model.RedTeamV2Run{}).Where("id = ? AND status = ? AND cancel_requested = ?", id, "queued", false).
		Updates(map[string]any{"status": "running", "started_at": started})
	if start.Error != nil || start.RowsAffected != 1 {
		redTeamV2Finish(id, "cancelled", "unscored", "", false, 0, 0, "开始前已取消")
		return
	}
	if !redTeamLabBusy.CompareAndSwap(false, true) {
		redTeamV2Finish(id, "transport_error", "unscored", "", false, 0, 0, "已有原版实验运行；没有发送上游请求")
		return
	}
	defer redTeamLabBusy.Store(false)

	var turns []modelverify.Turn
	var inTokens, outTokens int
	var lastContent string
	var lastFullContent string // process memory only; never persist raw long replies
	var lastClipped bool
	maxTokens := spec.MaxTokens
	for i, input := range spec.Prompts {
		if ctx.Err() != nil {
			redTeamV2Finish(id, redTeamV2ContextStatus(ctx), "unscored", lastContent, lastClipped, inTokens, outTokens, "实验已取消或超时，不自动重试")
			return
		}
		var latest model.RedTeamV2Run
		if err := db.GetDB().Select("cancel_requested").First(&latest, id).Error; err != nil || latest.CancelRequested {
			redTeamV2Finish(id, "cancelled", "unscored", lastContent, lastClipped, inTokens, outTokens, "操作员取消了后续轮次")
			return
		}
		// Persist the attempt BEFORE calling the target. A crash can charge,
		// so no automatic resume or retry is ever allowed.
		if err := db.GetDB().Model(&model.RedTeamV2Run{}).Where("id = ?", id).Update("target_requests", i+1).Error; err != nil {
			redTeamV2Finish(id, "transport_error", "unscored", lastContent, lastClipped, inTokens, outTokens, "无法记录预算/请求计数，未发新请求")
			return
		}
		turns = append(turns, modelverify.Turn{Role: "user", Content: input})
		ask := modelverify.Ask{Prompt: input, MaxTokens: &maxTokens}
		if i > 0 {
			ask.Prompt = ""
			ask.Turns = turns
		}
		// Single-turn stays on the exact legacy Prompt path. Multi-turn uses
		// the already-existing verified probe replay message builder.
		outcome, sendErr := redTeamLabSend(ctx, channel, upstreamModel, endpoint, ask)
		saved := model.RedTeamV2Turn{RunID: id, Sequence: i + 1, InputText: input, CreatedAt: time.Now()}
		if sendErr != nil {
			status := redTeamV2ContextStatus(ctx)
			if status == "completed" {
				status = "transport_error"
			}
			saved.Status = status
			_ = db.GetDB().Create(&saved).Error
			redTeamV2Finish(id, status, "unscored", lastContent, lastClipped, inTokens, outTokens, "上游未完成这一轮；不要立即重复付费测试")
			return
		}
		saved.Status = "completed"
		saved.ResponseText, saved.ResponseClipped = redteamlab.ClipResponse(outcome.Content)
		saved.InputTokens = outcome.Usage.Input
		if saved.InputTokens == 0 {
			saved.InputTokens = outcome.Usage.Prompt
		}
		saved.OutputTokens = outcome.Usage.Completion
		if err := db.GetDB().Create(&saved).Error; err != nil {
			redTeamV2Finish(id, "transport_error", "unscored", lastContent, lastClipped, inTokens, outTokens, "上游已响应但轮次证据未能持久化，勿自动重试")
			return
		}
		inTokens += saved.InputTokens
		outTokens += saved.OutputTokens
		lastContent, lastClipped = saved.ResponseText, saved.ResponseClipped
		lastFullContent = outcome.Content
		if i+1 < len(spec.Prompts) {
			if saved.ResponseClipped || utf8.RuneCountInString(outcome.Content) > redteamlab.MaxResponseRunes {
				redTeamV2Finish(id, "uncertain", "unscored", lastContent, lastClipped, inTokens, outTokens, "回复超过安全会话保留上限，已停止后续轮次")
				return
			}
			turns = append(turns, modelverify.Turn{Role: "assistant", Content: outcome.Content})
		}
	}
	redTeamV2Finish(id, "completed", spec.Score(lastFullContent), lastContent, lastClipped, inTokens, outTokens, "")
}

func redTeamV2ContextStatus(ctx context.Context) string {
	switch ctx.Err() {
	case context.Canceled:
		return "cancelled"
	case context.DeadlineExceeded:
		return "timeout"
	default:
		return "completed"
	}
}

func redTeamV2Finish(id int64, status, verdict, responseText string, clipped bool, inTokens, outTokens int, message string) {
	now := time.Now()
	// Final state and lane release are atomic: no subsequent run sees an
	// unlocked lane until the preceding result has a durable terminal state.
	_ = db.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.RedTeamV2Run{}).Where("id = ?", id).Updates(map[string]any{
			"status": status, "verdict": verdict, "response_text": responseText, "response_clipped": clipped,
			"input_tokens": inTokens, "output_tokens": outTokens, "error_message": message, "completed_at": now,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&model.RedTeamV2Lane{}).Where("id = ? AND active_run_id = ?", 1, id).
			Update("active_run_id", 0).Error
	})
}
