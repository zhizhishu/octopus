package task

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modeltest"
	"github.com/bestruirui/octopus/internal/modelverify/behavior"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const (
	scheduledAuditInterval    = 6 * time.Hour
	scheduledAuditCooldown    = 6 * time.Hour
	scheduledAuditMaxTargets  = 8
	scheduledAuditProbeBudget = 45 * time.Second
	scheduledAuditTaskBudget  = 20 * time.Minute
)

var scheduledAuditQuickProbes = []behavior.ProbeID{
	behavior.ProbeLiveness,
	behavior.ProbeEchoRewrite,
	behavior.ProbeContextCanary,
}

type scheduledAuditTarget struct {
	channel model.Channel
	model   string
	trigger string
	// logID 是触发这次检测的那条日志行(回显不一致跟进才有; 整轮定时快检为 0)。
	// 非零时检测结论会先到先得地写回该行, 不再只留在内存快照里。
	logID int64
}

// RegisterAuditFollowUp 把出错跟进挂到日志写入路径。Init 里调一次即可。
func RegisterAuditFollowUp() {
	op.SetScheduledAuditFollowUp(func(channelID int, modelName string, logID int64) {
		runAuditFollowUp(channelID, modelName, logID)
	})
}

func runAuditFollowUp(channelID int, modelName string, logID int64) {
	now := time.Now()
	if op.ScheduledAuditOnCooldown(channelID, modelName, now, scheduledAuditCooldown) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), scheduledAuditProbeBudget+5*time.Second)
	defer cancel()
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil || channel == nil || !channel.Enabled {
		return
	}
	item := runScheduledAuditTarget(ctx, scheduledAuditTarget{
		channel: *channel,
		model:   modelName,
		trigger: "echo_mismatch",
		logID:   logID,
	})
	if !item.Skipped {
		op.ScheduledAuditMarkHit(channelID, modelName, now)
	}
	op.ScheduledAuditAppend(item, now)
}

// ModelAuditQuickTask 对启用渠道做轻量快检。启动时不跑，避免一开机就打上游。
func ModelAuditQuickTask() {
	ctx, cancel := context.WithTimeout(context.Background(), scheduledAuditTaskBudget)
	defer cancel()
	now := time.Now()
	targets, err := collectScheduledAuditTargets(ctx, now)
	if err != nil {
		log.Warnf("scheduled model audit skipped: %v", err)
		return
	}
	results := make([]model.ScheduledAuditResult, 0, len(targets))
	ran := 0
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			results = append(results, model.ScheduledAuditResult{
				ChannelID:   target.channel.ID,
				ChannelName: target.channel.Name,
				Model:       target.model,
				Trigger:     target.trigger,
				Skipped:     true,
				SkipReason:  "任务超时，本轮未跑完",
			})
			continue
		}
		item := runScheduledAuditTarget(ctx, target)
		if !item.Skipped {
			ran++
			op.ScheduledAuditMarkHit(target.channel.ID, target.model, now)
		}
		results = append(results, item)
	}
	op.ScheduledAuditStore(model.ScheduledAuditSnapshot{
		LastRunUnix: now.Unix(),
		TargetCount: len(targets),
		RanCount:    ran,
		Results:     results,
	})
}

func collectScheduledAuditTargets(ctx context.Context, now time.Time) ([]scheduledAuditTarget, error) {
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
		if op.ScheduledAuditOnCooldown(ch.ID, modelName, now, scheduledAuditCooldown) {
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

	report, err := op.LogAnomalyScanGet(ctx)
	if err != nil {
		log.Warnf("scheduled model audit log scan failed: %v", err)
		return targets, nil
	}
	byID := map[int]model.Channel{}
	for _, channel := range channels {
		byID[channel.ID] = channel
	}
	for _, finding := range report.Findings {
		if finding.Code != "echo_mismatch_rate" {
			continue
		}
		channel, ok := byID[finding.ChannelID]
		if !ok {
			continue
		}
		add(channel, finding.Model, "echo_mismatch")
	}
	return targets, nil
}

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
	sender, err := modeltest.NewProbeSender(target.channel, upstreamModel, scheduledAuditEndpoint(target.channel))
	if err != nil {
		item.Skipped = true
		item.SkipReason = err.Error()
		return item
	}
	probeCtx, cancel := context.WithTimeout(ctx, scheduledAuditProbeBudget)
	defer cancel()
	started := time.Now()
	results := behavior.Run(probeCtx, scheduledAuditQuickProbes, sender)
	report := behavior.ToView(behavior.BuildReport(results, target.model))
	item.DurationMs = int(time.Since(started).Milliseconds())
	item.Verdict = string(report.Verdict)
	item.Score = report.Score
	item.FindingN = len(report.Findings)
	item.ErrorN = len(report.Errors)

	// 结论落库: 钉回触发它的那行日志, 先到先得(空 verdict 才写), 重启不再丢。
	if target.logID > 0 {
		trigger := target.trigger
		if trigger == "" {
			trigger = "schedule"
		}
		if reportJSON, err := json.Marshal(report); err == nil {
			if err := op.RelayLogMarkAudited(ctx, target.logID, string(report.Verdict), report.Score, len(report.Findings), trigger, string(reportJSON)); err != nil {
				log.Warnf("model audit persist for log %d failed: %v", target.logID, err)
			}
		}
	}
	return item
}

func scheduledAuditEndpoint(ch model.Channel) string {
	switch ch.Type {
	case outbound.OutboundTypeAnthropic:
		return "anthropic_messages"
	case outbound.OutboundTypeOpenAIResponse:
		return "openai_responses"
	case outbound.OutboundTypeGemini:
		return "gemini_generate_content"
	default:
		return "openai_chat"
	}
}

func scheduledTargetKey(channelID int, modelName string) string {
	return strconv.Itoa(channelID) + "\x1f" + strings.TrimSpace(modelName)
}
