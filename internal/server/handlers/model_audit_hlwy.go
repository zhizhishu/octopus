package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// hlwy 基准对照的四个接口：基准目录、导入、存为本轮基准、离线重算。
//
// 这里刻意**没有**「直接跑一次对照」的接口：跑一次对照真的会打上游、真的会花钱。
// 所以只有一条创建路径——走既有的 /enqueue 队列（kind=hlwy_compare），由后台
// 串行 worker 执行。列表/基准/重算都是零上游请求的纯读操作。

const (
	// hlwyRequestBodyLimit 是我们自己接收 JSON 请求体的上限（基准导入另有限制）。
	hlwyRequestBodyLimit = 1 << 20
)

func init() {
	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/hlwy-baselines", http.MethodGet).
				Handle(listHlwyBaselines),
		)

	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/hlwy-baselines/import", http.MethodPost).
				Handle(importHlwyBaseline),
		).
		AddRoute(
			router.NewRoute("/hlwy-baselines/from-result", http.MethodPost).
				Handle(createHlwyBaselineFromResult),
		).
		AddRoute(
			router.NewRoute("/hlwy-compare", http.MethodPost).
				Handle(compareHlwyResult),
		)
}

// hlwyFromResultRequest 把一次已完成的对照存成基准。
type hlwyFromResultRequest struct {
	ResultID int64 `json:"result_id"`
	// Label 可选的人类可读名字，缺省时由服务器按模型+时间生成。
	Label string `json:"label"`
}

// hlwyCompareRequest 用已采到的数字对另一份基准离线重算。
//
// 没有任何金额字段（samples/channel_id/model）：重算用的永远是那次运行已经
// 采到的 raw_numbers，不允许在这里“再采一点”。
type hlwyCompareRequest struct {
	ResultID   int64  `json:"result_id"`
	BaselineID string `json:"baseline_id"`
}

// hlwyBindStrictJSON 严格解码一个小请求体。
//
// 为什么不用 ShouldBindJSON：它会把未知字段静默丢掉。而这些接口带着「会不会花钱」
// 「写不写库」的语义，静默丢掉一个字段等于按用户的另一半意图执行。所以这里显式
// 拒绝未知字段、拒绝多个 JSON 值、限制体积，并且错误信息只说“字段不认识”，
// 不回显上游/库里的任何内容。
func hlwyBindStrictJSON(c *gin.Context, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, hlwyRequestBodyLimit))
	if err != nil {
		return errors.New("请求体不可读或超过上限")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("请求体为空")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求体格式不支持: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("请求体只能是一个 JSON 对象")
	}
	return nil
}

func listHlwyBaselines(c *gin.Context) {
	entries, err := op.HlwyBaselineList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"baselines": entries})
}

// importHlwyBaseline 导入一份**旧版基准 JSON 原文**（一个对象，或只有一个元素的数组）。
//
// 刻意收原文而不是"解析成结构再传"：解析后重新序列化会丢掉我们没认识的字段，
// 也会让"导入的到底是哪份文件"变得不可核对。校验全在 op 层（分布长度/总量/
// 结构/哈希），这里只负责不把超大文件读进内存。
func importHlwyBaseline(c *gin.Context) {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body,
		numberdist.DefaultMaxImportBytes))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, "基准内容不可读或超过上限")
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		resp.Error(c, http.StatusBadRequest, "基准内容为空")
		return
	}
	saved, err := op.HlwyBaselineImport(c.Request.Context(), raw, time.Now())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, saved)
}

func createHlwyBaselineFromResult(c *gin.Context) {
	var req hlwyFromResultRequest
	if err := hlwyBindStrictJSON(c, &req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.ResultID <= 0 {
		resp.Error(c, http.StatusBadRequest, "result_id is required")
		return
	}
	saved, err := op.HlwyBaselineFromResult(c.Request.Context(), req.ResultID, req.Label)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, saved)
}

// compareHlwyResult 用已有结果的原始数字对另一份基准重算。
//
// 纯计算：不建任务、不写任何行、不打上游。它存在的意义是「我换一份基准再看看」，
// 而不是「再采一轮」——后者必须走队列，否则任何人都能绕过预算与去重直接开跑。
func compareHlwyResult(c *gin.Context) {
	var req hlwyCompareRequest
	if err := hlwyBindStrictJSON(c, &req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.ResultID <= 0 {
		resp.Error(c, http.StatusBadRequest, "result_id is required")
		return
	}
	baselineID := strings.TrimSpace(req.BaselineID)
	if baselineID == "" {
		resp.Error(c, http.StatusBadRequest, "baseline_id is required")
		return
	}
	report, err := op.HlwyRecompare(c.Request.Context(), req.ResultID, baselineID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, op.ErrHlwyBaselineNotFound) {
			status = http.StatusNotFound
		}
		resp.Error(c, status, err.Error())
		return
	}
	resp.Success(c, gin.H{"report": report})
}

// hlwyAuditEnabled 读 hlwy 对照开关。
//
// 只用来挡「新建一次对照」。已存在的任务照旧跑完、已采到的数字照旧可读可重算：
// 关掉开关的意思是「别再花钱开新的」，不是「把已有证据作废」。
func hlwyAuditEnabled(ctx context.Context) bool {
	enabled, err := op.SettingGetBool(model.SettingKeyModelAuditHlwyEnabled)
	if err != nil {
		// 读不到就当关着：这是一个“要不要花钱”的开关，默认必须是不花。
		return false
	}
	return enabled
}

// hlwyPlanConditionsNote 是计划上固定的一句说明：实际请求体在开跑前无法确知。
const hlwyPlanConditionsNote = "请求体与采样条件在开跑前不伪造预览：跑完按实际观测记录，条件变了会当场停止采样。"

// hlwyPlan 回答 hlwy 对照的「这一单最多花多少次调用、多久」：零上游请求、零写入。
//
// 形状是顶层字段（kind/enabled/samples/max_calls/timeout_seconds/…），与多探针
// 计划完全不同：挤在旧计划的一个嵌套字段里，页面永远读不到 enabled，提交键会
// 一直置灰。校验口径与入队一致（chat 家族渠道、100/200、基准必须能解析），
// 于是「计划通过了、提交却被拒」这种自相矛盾的事不会发生。
func hlwyPlan(c *gin.Context) {
	ctx := c.Request.Context()
	samples, err := strconv.Atoi(strings.TrimSpace(c.Query("samples")))
	if err != nil || (samples != model.HlwyDefaultSamples && samples != model.HlwyMaxSamples) {
		resp.Error(c, http.StatusBadRequest, fmt.Sprintf("采样次数只能 %d 或 %d",
			model.HlwyDefaultSamples, model.HlwyMaxSamples))
		return
	}
	channelID, _ := strconv.Atoi(strings.TrimSpace(c.Query("channel_id")))
	if channelID <= 0 {
		resp.Error(c, http.StatusBadRequest, "channel_id is required")
		return
	}
	targetModel := strings.TrimSpace(c.Query("model"))
	if targetModel == "" {
		resp.Error(c, http.StatusBadRequest, "model is required")
		return
	}
	baselineID := strings.TrimSpace(c.Query("baseline_id"))
	if baselineID == "" {
		resp.Error(c, http.StatusBadRequest, "baseline_id is required")
		return
	}
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil {
		resp.Error(c, http.StatusNotFound, "渠道不存在")
		return
	}
	if !outbound.IsChatChannelType(channel.Type) {
		resp.Error(c, http.StatusBadRequest,
			fmt.Sprintf("渠道类型 %d 不支持 hlwy 数字采样（需要 chat 家族）", int(channel.Type)))
		return
	}
	resolved, err := op.HlwyBaselineResolve(ctx, baselineID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, op.ErrHlwyBaselineNotFound) {
			status = http.StatusNotFound
		}
		resp.Error(c, status, err.Error())
		return
	}
	// 开关关着不是错误：计划照实给出，只是 enabled=false，页面据此置灰提交键。
	resp.Success(c, gin.H{
		"kind":                   model.AuditKindHlwy,
		"enabled":                hlwyAuditEnabled(ctx),
		"samples":                samples,
		"max_calls":              samples,
		"timeout_seconds":        op.HlwyRunTimeout(samples),
		"sample_timeout_seconds": int(op.HlwySampleTimeout / time.Second),
		"protocol_id":            numberdist.ProtocolID,
		"baseline":               resolved.Entry(),
		"conditions_note":        hlwyPlanConditionsNote,
	})
}
