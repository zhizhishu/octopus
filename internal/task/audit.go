package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

const (
	// scheduledAuditInterval 抽检的节拍（init.go 用它注册 TaskModelAuditQuick）。
	//
	// 节拍比"每渠道每天 N 轮"密得多是有意的：真正限制频率的是按轮数算出来的冷却
	// （auditCooldown），节拍只决定"多久来看一眼有没有渠道到点了"。所以
	// 6 小时一轮不会多打上游，只是让到点的渠道不必等到下一个整天。
	scheduledAuditInterval = 6 * time.Hour

	// 每渠道每天的抽检轮数范围（读取时夹取）。1 是默认：少量 token 抽个检。
	scheduledAuditMinRoundsPerDay = 1
	scheduledAuditMaxRoundsPerDay = 4

	// scheduledAuditDay 是"轮数"的换算基准：一天分几轮 = 冷却多长。
	scheduledAuditDay = 24 * time.Hour

	// scheduledAuditMaxTargets 一轮最多处理几个渠道/模型。
	scheduledAuditMaxTargets = 8

	// scheduledAuditProbeBudget 就地跑单目标时的单目标超时。
	// 只给 runScheduledAuditTarget（仅测试与历史调用面在用）——生产抽检走队列，
	// 超时按请求量放大，见 enqueueScheduledAuditTarget 的注释。
	scheduledAuditProbeBudget = 45 * time.Second

	// scheduledAuditTaskBudget 整轮定时抽检的预算（现在只包含读渠道/入队，不含探测）。
	scheduledAuditTaskBudget = 20 * time.Minute

	// scheduledAuditLookupBudget 跟查时读一次渠道的超时。
	scheduledAuditLookupBudget = 15 * time.Second

	// scheduledAuditManualBaseTimeoutSeconds 入队用的基础超时，与手动入队同一口径：
	// handlers.enqueueModelAudit 把请求里的 timeout_seconds（缺省 0）直接交给队列，
	// 队列再用 op.AuditDefaultTimeoutSeconds 补齐并按请求量放大。这里显式写同一个
	// 基础值，随后同样过一次 op.AuditTimeoutFor。
	scheduledAuditManualBaseTimeoutSeconds = op.AuditDefaultTimeoutSeconds
)

// scheduledAuditProbesDefault 认模型的抽检题目：所有渠道都问这两项。
//
// 替换掉旧的「连通 / 改写 / 截断」三项，是因为那三项只能证明"这个渠道会回话"，
// 证明不了"回话的是不是它声称的那个模型"——而抽检存在的理由就是后者。指纹探针
// 拿三道题看它把哪些整数记错过（家族/版本的弱指纹），分词器探针拿约 12 次计数
// 比对 tokenizer 口径。
var scheduledAuditProbesDefault = []behavior.ProbeID{
	behavior.ProbeFingerprint,
	behavior.ProbeTokenizerID,
}

// scheduledAuditProbesAnthropic Anthropic 渠道再加两项：thinking 签名（harvest + replay
// 两次请求，中转站伪造不了）与系统提示词复述（看它自报的身份从哪来）。
var scheduledAuditProbesAnthropic = []behavior.ProbeID{
	behavior.ProbeFingerprint,
	behavior.ProbeTokenizerID,
	behavior.ProbeSignature,
	behavior.ProbeSystemPrompt,
}

// enqueueAuditFunc 入队函数。生产是 op.ModelAuditEnqueue；测试可换成计数器，
// 用来证明"开关关着时零入队"——这是本文件唯一会花钱的动作，所以它必须可注入。
var enqueueAuditFunc = op.ModelAuditEnqueue

type scheduledAuditTarget struct {
	channel model.Channel
	model   string
	trigger string
	// logID 是触发这次检测的那条日志行(回显不一致/来源线索跟进才有; 整轮定时抽检为 0)。
	// 非零时队列 worker 跑完会把结论先到先得地写回该行(model_audit_worker.go 的 finish)。
	logID int64
}

// RegisterAuditFollowUp 把出错跟进挂到日志写入路径。Init 里调一次即可。
//
// 挂上不等于会跑：回调进来先看两道开关（抽检总开关 + 跟查开关），都明确开着才动手，
// 见 op.AuditFollowUpTrigger。
func RegisterAuditFollowUp() {
	op.SetScheduledAuditFollowUpTriggered(func(channelID int, modelName, trigger string, logID int64) {
		runAuditFollowUp(channelID, modelName, trigger, logID)
	})
}

// auditEnabled 抽检总开关：只有明确是 true 才算开（含读失败 → 关）。
//
// 这是所有会花钱路径的第一道闸门。默认 false，且读不到配置一律按关——
// 一次设置读抖动不该变成"悄悄开始打上游"。
func auditEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyModelAuditEnabled)
	return err == nil && enabled
}

// auditChannelDailyRounds 每渠道每天的抽检轮数，夹到 1..4（读失败按 1）。
func auditChannelDailyRounds() int {
	rounds, err := op.SettingGetInt(model.SettingKeyModelAuditChannelDailyRounds)
	if err != nil {
		return scheduledAuditMinRoundsPerDay
	}
	if rounds < scheduledAuditMinRoundsPerDay {
		return scheduledAuditMinRoundsPerDay
	}
	if rounds > scheduledAuditMaxRoundsPerDay {
		return scheduledAuditMaxRoundsPerDay
	}
	return rounds
}

// auditCooldown 同一个渠道+模型两次抽检之间的最小间隔 = 24h / 轮数。
func auditCooldown() time.Duration {
	return scheduledAuditDay / time.Duration(auditChannelDailyRounds())
}

// runAuditFollowUp 可疑日志跟查：与定时抽检同一套题目、冷却、预算、入队，
// 只是目标来自单条日志而不是整轮扫描。
func runAuditFollowUp(channelID int, modelName string, trigger string, logID int64) {
	// 开关再挡一次：日志路径已经挡过，但这里是真正会入队的地方，闸门要站在钱前面。
	if !auditEnabled() {
		return
	}
	modelName = strings.TrimSpace(modelName)
	if channelID <= 0 || modelName == "" {
		return
	}
	if trigger == "" {
		trigger = "echo_mismatch"
	}
	now := time.Now()
	cooldown := auditCooldown()
	if op.ScheduledAuditOnCooldown(channelID, modelName, now, cooldown) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), scheduledAuditLookupBudget)
	defer cancel()
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil || channel == nil || !channel.Enabled {
		return
	}
	item, _ := enqueueScheduledAuditTarget(ctx, scheduledAuditTarget{
		channel: *channel,
		model:   modelName,
		trigger: trigger,
		logID:   logID,
	}, now)
	op.ScheduledAuditAppend(item, now)
}

// ModelAuditQuickTask 定时抽检：每个渠道每天 rounds 轮，全站每天最多 limit 次上游请求。
//
// 第一件事是读 model_audit_enabled：不是明确的 true（含读失败）就立即返回，
// 不碰任何渠道、不发任何请求、不写任何快照。
func ModelAuditQuickTask() {
	if !auditEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), scheduledAuditTaskBudget)
	defer cancel()
	now := time.Now()
	cooldown := auditCooldown()
	targets, err := collectScheduledAuditTargets(ctx, now, cooldown)
	if err != nil {
		log.Warnf("scheduled model audit skipped: %v", err)
		return
	}
	results := make([]model.ScheduledAuditResult, 0, len(targets))
	ran := 0
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			results = append(results, skippedAuditResult(target, "任务超时，本轮未跑完"))
			continue
		}
		item, budgetExhausted := enqueueScheduledAuditTarget(ctx, target, now)
		if !item.Skipped {
			ran++
			// 入队即记命中：否则同一个渠道会在下一次节拍被再挑中、再扣一次额度。
			op.ScheduledAuditMarkHit(target.channel.ID, target.model, now)
		}
		results = append(results, item)
		if budgetExhausted {
			// 额度用完就结束本轮：后面的目标一律不试，避免"每个目标都扣一次失败日志"
			// 把真正的结论挤出快照。
			break
		}
	}
	op.ScheduledAuditStore(model.ScheduledAuditSnapshot{
		LastRunUnix: now.Unix(),
		TargetCount: len(targets),
		RanCount:    ran,
		Results:     results,
	})
}

// collectScheduledAuditTargets 挑出这一轮要抽检的渠道+模型。
//
// 口径：每个启用渠道取它的第一个已选模型，24h/轮数 的冷却期内跳过。
//
// 可疑日志不在这一轮里跟查：跟查是"抽检开着 + 跟查开着"才发生的事
// （op.AuditFollowUpTrigger 的两道闸门）。以前这里顺带把日志扫描命中项也塞进
// 定时抽检，等于绕过跟查开关替用户花钱，所以那条路径去掉了。
func collectScheduledAuditTargets(ctx context.Context, now time.Time, cooldown time.Duration) ([]scheduledAuditTarget, error) {
	channels, err := op.ChannelList(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	targets := make([]scheduledAuditTarget, 0, scheduledAuditMaxTargets)
	add := func(ch model.Channel, modelName, trigger string) {
		modelName = strings.TrimSpace(modelName)
		if !ch.Enabled || modelName == "" {
			return
		}
		key := scheduledTargetKey(ch.ID, modelName)
		if _, ok := seen[key]; ok {
			return
		}
		if len(targets) >= scheduledAuditMaxTargets {
			return
		}
		if op.ScheduledAuditOnCooldown(ch.ID, modelName, now, cooldown) {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, scheduledAuditTarget{channel: ch, model: modelName, trigger: trigger})
	}

	for _, channel := range channels {
		if !channel.Enabled {
			continue
		}
		models := model.ChannelSelectedModelNames(channel)
		if len(models) == 0 {
			continue
		}
		add(channel, models[0], "schedule")
	}
	return targets, nil
}

// enqueueScheduledAuditTarget 扣额度 + 入队一个目标。
//
// 第二个返回值表示"今日额度已用完"，调用方据此结束本轮（而不是逐个目标各记一条跳过）。
//
// 为什么入队而不是就地跑：旧的就地快检给单目标 45 秒，而认模型的检查跑不完——
// 指纹三项是三次大输出，分词器约 12 次请求，两项加起来在慢上游上远超 45 秒，
// 就地跑只会得到一堆"网络超时"，那不是渠道的问题，是我们的预算给错了。检测队列
// 按计划请求量放大整单超时（op.AuditTimeoutFor），结果进检测历史，并由 worker
// （model_audit_worker.go 的 finish）把结论先到先得地写回触发的日志行。
func enqueueScheduledAuditTarget(ctx context.Context, target scheduledAuditTarget, now time.Time) (model.ScheduledAuditResult, bool) {
	probes, planned := auditPlanForChannel(target.channel, target.model)
	if planned <= 0 {
		return skippedAuditResult(target, "没有可用的抽检题目"), false
	}
	ok, used, limit := op.ModelAuditBudgetReserve(planned, now)
	if !ok {
		return skippedAuditResult(target, fmt.Sprintf("今日抽检额度已用完（已用 %d / 上限 %d）", used, limit)), true
	}
	probeNames := make([]string, len(probes))
	for i, id := range probes {
		probeNames[i] = string(id)
	}
	_, err := enqueueAuditFunc(ctx, op.ModelAuditEnqueueInput{
		ChannelID: target.channel.ID,
		// Models 用**请求模型名**（渠道已选模型），不是映射后的上游名：队列落库后由
		// worker 自己按 channel.ModelMapping 映射（model_audit_worker.go 的 upstreamModel），
		// 手动入队同样只传请求模型名。
		Models:         []string{target.model},
		Probes:         probeNames,
		TimeoutSeconds: op.AuditTimeoutFor(probeNames, scheduledAuditManualBaseTimeoutSeconds),
		LogID:          target.logID,
		Trigger:        auditTriggerOrSchedule(target.trigger),
	})
	if err != nil && !errors.Is(err, op.ErrAuditAlreadyQueued) {
		return skippedAuditResult(target, err.Error()), false
	}
	// ErrAuditAlreadyQueued 也算成功：这些模型已经在队列里等着跑了，
	// 重复入队只会二次计费，而我们想要的结果已经成立。
	return model.ScheduledAuditResult{
		ChannelID:   target.channel.ID,
		ChannelName: target.channel.Name,
		Model:       target.model,
		Trigger:     auditTriggerOrSchedule(target.trigger),
		LogID:       target.logID,
	}, false
}

func auditTriggerOrSchedule(trigger string) string {
	if strings.TrimSpace(trigger) == "" {
		return "schedule"
	}
	return trigger
}

func skippedAuditResult(target scheduledAuditTarget, reason string) model.ScheduledAuditResult {
	return model.ScheduledAuditResult{
		ChannelID:   target.channel.ID,
		ChannelName: target.channel.Name,
		Model:       target.model,
		Trigger:     auditTriggerOrSchedule(target.trigger),
		LogID:       target.logID,
		Skipped:     true,
		SkipReason:  reason,
	}
}

// auditPlanForChannel 给出这个渠道该跑哪些题、以及这些题一共打算发几次请求。
//
// 题目按渠道协议挑（Anthropic 才加签名/系统提示词），再用 behavior.PlanFor 的
// 适用性过滤一遍：签名探针在"型号不是单一明确的 anthropic 家族"时自己就不发题，
// 计划量也该是 0——预算按真实计划量扣，不是按勾选框扣。
func auditPlanForChannel(channel model.Channel, modelName string) ([]behavior.ProbeID, int) {
	protocol := auditProbeProtocol(channel.Type)
	base := scheduledAuditProbesDefault
	if protocol == "anthropic-messages" {
		base = scheduledAuditProbesAnthropic
	}
	plan := behavior.PlanFor(protocol, modelName)
	applicable := make(map[behavior.ProbeID]bool, len(plan))
	for _, item := range plan {
		applicable[item.ID] = item.Applicable
	}
	probes := make([]behavior.ProbeID, 0, len(base))
	for _, id := range base {
		if ok, known := applicable[id]; known && !ok {
			continue
		}
		probes = append(probes, id)
	}
	return probes, behavior.SumPlannedRequests(plan, probes)
}

// auditProbeProtocol 返回渠道实际使用的出站协议 id。
//
// 口径与手动检测一致（handlers.probeProtocolName / modeltest.ProbeSender.Protocol
// 同一套映射）：取渠道类型，不取入站端点。
func auditProbeProtocol(t outbound.OutboundType) string {
	switch t {
	case outbound.OutboundTypeAnthropic:
		return "anthropic-messages"
	case outbound.OutboundTypeOpenAIResponse:
		return "openai-responses"
	case outbound.OutboundTypeGemini:
		return "gemini-generate-content"
	default:
		return "openai-chat"
	}
}

// runScheduledAuditTarget 就地跑一个目标并返回结论。
//
// 生产路径已经不再调它（抽检改走队列，见 enqueueScheduledAuditTarget）：45 秒的单目标
// 预算装不下认模型的题目。保留它是因为 audit_probe_wiring_test.go 用它证明
// 「题目→发送器」这段接线是通的，而那段断言与本函数共用同一套题目和同一个发送器。
func runScheduledAuditTarget(ctx context.Context, target scheduledAuditTarget) model.ScheduledAuditResult {
	item := model.ScheduledAuditResult{
		ChannelID:   target.channel.ID,
		ChannelName: target.channel.Name,
		Model:       target.model,
		Trigger:     target.trigger,
		LogID:       target.logID,
	}
	upstreamModel := target.model
	if mapped, ok := target.channel.ModelMapping[upstreamModel]; ok && mapped != "" {
		upstreamModel = mapped
	}
	sender, err := modeltest.NewProbeSender(target.channel, upstreamModel, op.AuditEndpointForChannel(target.channel))
	if err != nil {
		item.Skipped = true
		item.SkipReason = err.Error()
		return item
	}
	probes, _ := auditPlanForChannel(target.channel, target.model)
	if len(probes) == 0 {
		item.Skipped = true
		item.SkipReason = "没有可用的抽检题目"
		return item
	}
	probeCtx, cancel := context.WithTimeout(ctx, scheduledAuditProbeBudget)
	defer cancel()
	started := time.Now()
	results := behavior.Run(probeCtx, probes, sender, upstreamModel)
	report := behavior.ToView(behavior.BuildReport(results, target.model))
	item.DurationMs = int(time.Since(started).Milliseconds())
	item.Verdict = string(report.Verdict)
	item.Score = report.Score
	item.FindingN = behavior.CountScoredViewFindings(report.Findings)
	item.ErrorN = len(report.Errors)

	// 结论落库: 钉回触发它的那行日志, 先到先得(空 verdict 才写), 重启不再丢。
	if target.logID > 0 {
		trigger := target.trigger
		if trigger == "" {
			trigger = "schedule"
		}
		if reportJSON, err := json.Marshal(report); err == nil {
			if err := op.RelayLogMarkAudited(ctx, target.logID, string(report.Verdict), report.Score, behavior.CountScoredViewFindings(report.Findings), trigger, string(reportJSON)); err != nil {
				log.Warnf("model audit persist for log %d failed: %v", target.logID, err)
			}
		}
	}
	return item
}

func scheduledTargetKey(channelID int, modelName string) string {
	return strconv.Itoa(channelID) + "\x1f" + strings.TrimSpace(modelName)
}
