package op

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/safe"
)

const scheduledAuditDisclaimer = "定时快检结果提供参考，不等同模型真实性证明。网络失败不记为造假。"

var (
	scheduledAuditMu       sync.RWMutex
	scheduledAuditSnapshot model.ScheduledAuditSnapshot
	scheduledAuditLastHit  = map[string]int64{}
	scheduledAuditFollowUp func(channelID int, modelName string)
)

const scheduledAuditSnapshotKeep = 20

// ScheduledAuditGet 返回最近一次定时快检快照。从未跑过则空结果。
func ScheduledAuditGet() model.ScheduledAuditSnapshot {
	scheduledAuditMu.RLock()
	defer scheduledAuditMu.RUnlock()
	out := scheduledAuditSnapshot
	if out.Results == nil {
		out.Results = []model.ScheduledAuditResult{}
	}
	if out.Disclaimer == "" {
		out.Disclaimer = scheduledAuditDisclaimer
	}
	return out
}

// ScheduledAuditStore 覆盖最近一次快检快照。
func ScheduledAuditStore(snapshot model.ScheduledAuditSnapshot) {
	if snapshot.Disclaimer == "" {
		snapshot.Disclaimer = scheduledAuditDisclaimer
	}
	if snapshot.Results == nil {
		snapshot.Results = []model.ScheduledAuditResult{}
	}
	scheduledAuditMu.Lock()
	scheduledAuditSnapshot = snapshot
	scheduledAuditMu.Unlock()
}

// ScheduledAuditMarkHit 记录这个渠道+模型刚被快检过。
func ScheduledAuditMarkHit(channelID int, modelName string, at time.Time) {
	scheduledAuditMu.Lock()
	scheduledAuditLastHit[scheduledAuditCooldownKey(channelID, modelName)] = at.Unix()
	scheduledAuditMu.Unlock()
}

// ScheduledAuditOnCooldown 判断冷却期内是否刚检过，避免出错跟进把上游打爆。
func ScheduledAuditOnCooldown(channelID int, modelName string, now time.Time, cooldown time.Duration) bool {
	scheduledAuditMu.RLock()
	defer scheduledAuditMu.RUnlock()
	last, ok := scheduledAuditLastHit[scheduledAuditCooldownKey(channelID, modelName)]
	if !ok {
		return false
	}
	return now.Unix()-last < int64(cooldown.Seconds())
}

func scheduledAuditCooldownKey(channelID int, modelName string) string {
	return strconv.Itoa(channelID) + "\x1f" + modelName
}

// SetScheduledAuditFollowUp 由定时任务在启动时挂上。日志层不直接打上游。
func SetScheduledAuditFollowUp(fn func(channelID int, modelName string)) {
	scheduledAuditMu.Lock()
	scheduledAuditFollowUp = fn
	scheduledAuditMu.Unlock()
}

// ShouldTriggerAuditFollowUp 只有上游回显对不上才跟进。网络失败、空请求、本地校验都不打。
func ShouldTriggerAuditFollowUp(relayLog model.RelayLog) bool {
	if relayLog.ChannelId <= 0 {
		return false
	}
	if relayLogExcludedFromModelTelemetry(relayLog) {
		return false
	}
	if relayLog.UpstreamModelMismatch == nil || !*relayLog.UpstreamModelMismatch {
		return false
	}
	if strings.TrimSpace(relayLog.RequestModelName) == "" && strings.TrimSpace(relayLog.ActualModelName) == "" {
		return false
	}
	return true
}

func maybeTriggerAuditFollowUp(relayLog model.RelayLog) {
	if !ShouldTriggerAuditFollowUp(relayLog) {
		return
	}
	modelName := strings.TrimSpace(relayLog.RequestModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(relayLog.ActualModelName)
	}
	scheduledAuditMu.RLock()
	fn := scheduledAuditFollowUp
	scheduledAuditMu.RUnlock()
	if fn == nil {
		return
	}
	channelID := relayLog.ChannelId
	safe.SafeGo("scheduled-audit-followup", func() { fn(channelID, modelName) })
}

// ScheduledAuditAppend 把单次跟进结果接到快照前面，不覆盖整轮定时结果。
func ScheduledAuditAppend(result model.ScheduledAuditResult, now time.Time) {
	scheduledAuditMu.Lock()
	defer scheduledAuditMu.Unlock()
	out := scheduledAuditSnapshot
	if out.Disclaimer == "" {
		out.Disclaimer = scheduledAuditDisclaimer
	}
	out.LastRunUnix = now.Unix()
	out.TargetCount++
	if !result.Skipped {
		out.RanCount++
	}
	next := make([]model.ScheduledAuditResult, 0, len(out.Results)+1)
	next = append(next, result)
	next = append(next, out.Results...)
	if len(next) > scheduledAuditSnapshotKeep {
		next = next[:scheduledAuditSnapshotKeep]
	}
	out.Results = next
	scheduledAuditSnapshot = out
}
