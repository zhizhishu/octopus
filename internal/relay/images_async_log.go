package relay

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 图片异步任务的历史日志。
//
// 以前 /v1/images/{generations,edits}/async 整条链路在 relay_logs 里一行都没有：
// 提交时不打上游所以没写，执行器（internal/task/image_task_poll.go）打了上游也没写。
// 于是同一张图，走同步端点能在日志页看到请求、出站请求体、结果和费用，走异步端点
// 什么都查不到。
//
// 现在每条任务在**到达终态时写一行**（完成 / 失败 / 过期各一行，只由赢得 CAS 的那个
// 写者写，所以一条任务恰好一行），字段口径与同步图片日志（imagesRelayMetrics.saveLog）
// 一致：
//   - 请求内容：客户端提交的 JSON，同一个 buildImagesRequestContentForLog + redactLogBody；
//   - 出站请求体：与同步路径同一个开关、同一个 captureOutboundJSONBody，原样不脱敏，
//     内联图片按「图片另存」规则存文件；开关关闭时不读 body；
//   - 结果：落库的 result_json（b64_json 已在 ImageTaskResultPayload 里剔除）；
//     失败写错误原因（已脱敏）；
//   - 费用：完成时结算出来的最终价格（按张 = 提交时的预扣；按 token = 上游用量 × 冻结的价，
//     见 ImageTaskSettle）。失败/过期已退款，记 0。
//
// 【不扣钱】这里只记账给人看。钱由提交时的预扣 + 完成时的差额结算（ImageTaskSettlement.Charge）
// 收齐、失败时已经退回；这里如果像同步路径的 Save 那样再调 UserRecordUsage 就是重复扣费。
//
// 【用时】UseTime 只算执行器那一次上游调用的耗时，不含排队等待：渠道测速/吞吐统计
// 读的就是这一列，把排队时间算进去会让一个快渠道看起来很慢。

// imageTaskLogEndpointSuffix 让异步任务的日志与同步端点区分开，同时保留 "images_"
// 前缀——日志页的端点筛选、被动检测按前缀识别图片类请求（见 op/log_passive.go）。
const imageTaskLogEndpointSuffix = "_async"

// ImageTaskLog 收集一条异步图片任务的日志字段，由执行器在终态时调用 Finish 写入。
// 方法都接受 nil 接收者（什么都不做），调用方不必判空。
type ImageTaskLog struct {
	metrics *imagesRelayMetrics
	task    model.ImageTask

	channelID   int
	channelName string
	keyID       int
	callStart   time.Time
	callDur     time.Duration
	called      bool

	settlement    ImageTaskSettlement
	hasSettlement bool
}

// SetSettlement 记下完成时结算出来的最终价格与用量（成功路径在 Finish 之前调用）。
func (l *ImageTaskLog) SetSettlement(s ImageTaskSettlement) {
	if l == nil {
		return
	}
	l.settlement = s
	l.hasSettlement = true
}

// NewImageTaskLog 按任务行准备一条日志。
func NewImageTaskLog(task model.ImageTask) *ImageTaskLog {
	requestModel := strings.TrimSpace(task.RequestModel)
	if requestModel == "" {
		// 存量任务（加列之前提交的）没有记客户端模型名，退回上游模型名。
		requestModel = task.Model
	}
	endpointPath := imageTaskEndpointPath(task.Endpoint) // /images/generations | /images/edits
	m := newImagesRelayMetrics(task.OwnerTokenID, task.OwnerUserID, task.RequestIP, requestModel,
		imageEndpointName(endpointPath)+imageTaskLogEndpointSuffix, "/v1"+endpointPath+"/async")
	if task.CreatedAt > 0 {
		// 日志时间 = 客户端提交的时刻，与同步请求按"请求发生时间"排序的口径一致。
		m.StartTime = time.Unix(task.CreatedAt, 0)
	}
	m.RequestUserAgent = task.RequestUserAgent
	m.ActualModel = task.Model

	var payload map[string]any
	if err := json.Unmarshal([]byte(task.RequestBody), &payload); err == nil {
		m.RequestContent = m.upstreamBodyLog.contentWithImages(func(capture *imageCapture) string {
			return buildImagesRequestContentForLogWith(false, nil, payload, capture)
		})
	}
	// 调试模式：执行器这一次上游调用的明细。耗时以「开始执行」为起点（不含排队）；
	// 异步任务没有入站请求头可记（提交时的请求早就结束了）。
	m.debug = newDebugRecorder(time.Now(), nil)
	return &ImageTaskLog{metrics: m, task: task, channelID: task.ChannelID, keyID: task.ChannelKeyID}
}

// debugBegin 在执行器发往上游之前调用；调试模式关着时返回 nil（后续调用全部空操作）。
func (l *ImageTaskLog) debugBegin(channel *model.Channel) *debugCall {
	if l == nil || l.metrics == nil || l.metrics.debug == nil {
		return nil
	}
	id, name := l.channelID, l.channelName
	if channel != nil {
		id, name = channel.ID, channel.Name
	}
	return l.metrics.debug.begin(id, name, l.keyID)
}

// bodyLog 返回出站请求体快照的存放处；nil 接收者返回 nil（采集函数据此跳过）。
func (l *ImageTaskLog) bodyLog() *upstreamBodyLog {
	if l == nil {
		return nil
	}
	return l.metrics.bodyLog()
}

// SetChannel 记下执行这条任务的渠道（名字用于日志展示）。
func (l *ImageTaskLog) SetChannel(channel *model.Channel) {
	if l == nil || channel == nil {
		return
	}
	l.channelID = channel.ID
	l.channelName = channel.Name
}

// BeginUpstream / EndUpstream 包住那一次上游调用，用来算 UseTime 与生成一条 attempt。
func (l *ImageTaskLog) BeginUpstream() {
	if l == nil {
		return
	}
	l.callStart = time.Now()
}

func (l *ImageTaskLog) EndUpstream() {
	if l == nil || l.callStart.IsZero() {
		return
	}
	l.called = true
	l.callDur = time.Since(l.callStart)
}

// Finish 写这条任务唯一的一行日志。success=false 时 failReason 必须是已脱敏的原因。
// resultJSON 是落库的 result_json（成功时）；statusCode 是上游状态码（没打上游为 0）。
func (l *ImageTaskLog) Finish(success bool, statusCode int, resultJSON string, failReason string) {
	if l == nil {
		return
	}
	ctx, cancel := metricsPersistContext()
	defer cancel()

	if l.channelName == "" && l.channelID > 0 {
		if ch, err := op.ChannelGet(l.channelID, ctx); err == nil && ch != nil {
			l.channelName = ch.Name
		}
	}

	m := l.metrics
	cost := 0.0
	usageSeen := false
	if success {
		cost = l.task.Cost
		m.ResponseContent = resultJSON
		if l.hasSettlement {
			// 最终价格以结算为准（按 token 计费的任务在完成时才算出钱）。
			cost = l.settlement.Cost
			if l.settlement.usage != nil {
				m.SetUsageFromImages(m.ActualModel, *l.settlement.usage)
				usageSeen = true
			}
			if l.settlement.hasSnapshot {
				m.BillingSnapshot = l.settlement.snapshot
			}
		}
	} else if strings.TrimSpace(l.task.BillingSnapshot) != "" {
		// 失败/过期：钱已退回，价格信息（计费模型、单价、倍率）照样展示，费用一律 0。
		var snap model.AccessPlanBillingSnapshot
		if err := json.Unmarshal([]byte(l.task.BillingSnapshot), &snap); err == nil {
			snap.FinalInputCost, snap.FinalOutputCost, snap.FinalCacheReadCost, snap.FinalCacheWriteCost = 0, 0, 0, 0
			m.BillingSnapshot = snap
		}
	}

	var attempts []model.ChannelAttempt
	if l.called {
		status := model.AttemptSuccess
		if !success {
			status = model.AttemptFailed
		}
		attempts = []model.ChannelAttempt{{
			ChannelID:    l.channelID,
			ChannelKeyID: l.keyID,
			ChannelName:  l.channelName,
			ModelName:    l.task.Model,
			AttemptNum:   1,
			Status:       status,
			Duration:     int(l.callDur.Milliseconds()),
			Msg:          failReason,
		}}
	}

	relayLog := model.RelayLog{
		UserID:           m.UserID,
		APIKeyID:         m.APIKeyID,
		RequestIP:        m.RequestIP,
		RequestUserAgent: truncateUserAgentForLog(m.RequestUserAgent),
		Time:             m.StartTime.Unix(),
		RequestEndpoint:  m.RequestEndpoint,
		RequestPath:      m.RequestPath,
		RequestModelName: m.RequestModel,
		ChannelName:      l.channelName,
		ChannelId:        l.channelID,
		ActualModelName:  m.ActualModel,
		UseTime:          int(l.callDur.Milliseconds()),
		Attempts:         attempts,
		TotalAttempts:    len(attempts),
		RequestContent:   m.RequestContent,
		ResponseContent:  m.ResponseContent,
		Cost:             cost,
	}
	m.upstreamBodyLog.applyTo(&relayLog)
	relayLog.DebugInfo = m.debug.build(l.callDur, time.Time{})
	if m.BillingSnapshot.BillingModelName != "" || m.BillingSnapshot.AccessPlanID != 0 {
		applyBillingSnapshotToRelayLog(&relayLog, m.BillingSnapshot)
	}
	relayLog.UsageSource, relayLog.UsageMissingReason = usageAuditFromStats(m.Stats, usageSeen, nil, false)
	if usageSeen {
		relayLog.InputTokens = int(m.Stats.InputToken)
		relayLog.OutputTokens = int(m.Stats.OutputToken)
		relayLog.CacheHitTokens = int(m.Stats.CacheHitToken)
		relayLog.CacheWriteTokens = int(m.Stats.CacheWriteToken)
		relayLog.CacheInputTokens = int(m.Stats.CacheInputToken)
		relayLog.CacheHitRate = cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)
	}
	if !success {
		relayLog.Error = failReason
		if statusCode > 0 {
			relayLog.ErrorStatus = statusCode
		}
	}
	if apiKey, err := op.APIKeyGet(m.APIKeyID, ctx); err == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	if user, err := op.UserGet(m.UserID); err == nil {
		relayLog.UserName = user.Username
	}

	// 统计口径与同步图片一致（请求数、等待时间、费用），但**不**调 UserRecordUsage：
	// 这一单的钱提交时已经扣过（失败已退）。
	stats := model.StatsMetrics{WaitTime: int64(relayLog.UseTime), OutputCost: cost}
	if usageSeen {
		stats.InputToken = m.Stats.InputToken
		stats.OutputToken = m.Stats.OutputToken
		stats.CacheHitToken = m.Stats.CacheHitToken
		stats.CacheWriteToken = m.Stats.CacheWriteToken
		stats.CacheInputToken = m.Stats.CacheInputToken
	}
	if success {
		stats.RequestSuccess = 1
	} else {
		stats.RequestFailed = 1
	}
	op.StatsTotalUpdate(stats)
	op.StatsHourlyUpdate(stats)
	op.StatsDailyUpdate(ctx, stats)
	op.StatsAPIKeyUpdate(m.APIKeyID, stats)
	op.StatsChannelUpdate(l.channelID, stats)
	if err := op.UserRecordRelayIP(m.UserID, m.RequestIP, m.StartTime.Unix(), ctx); err != nil {
		log.Warnf("image task log: record user relay ip failed: %v", err)
	}

	if err := op.RelayLogAdd(ctx, relayLog); err != nil {
		log.Warnf("image task log: save relay log for task %s failed: %v", l.task.TaskID, err)
	}
}
