package op

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

const scheduledAuditDisclaimer = "定时快检结果提供参考，不等同模型真实性证明。网络失败不记为造假。"

var (
	scheduledAuditMu       sync.RWMutex
	scheduledAuditSnapshot model.ScheduledAuditSnapshot
	scheduledAuditLastHit  = map[string]int64{}
	// scheduledAuditFollowUp 是旧的三参数注册口（channelID, modelName, logID），
	// 保留它是因为同包既有用例还在用它证明"零回调"。生产用下面那个带 trigger 的。
	scheduledAuditFollowUp func(channelID int, modelName string, logID int64)
	// scheduledAuditFollowUpTriggered 是带触发原因的注册口。
	//
	// 为什么需要 trigger：跟查要落库/入队时得说清“为什么查这一次”——回显对不上是
	// echo_mismatch，被动来源线索可疑是 passive_hint，两者在检测历史里必须可区分。
	// 而回调签名一变，同包里已经按三参数注册的用例就编不过，所以单独开一个口子，
	// 两个口子共存：带 trigger 的优先，没注册时退回旧口。
	scheduledAuditFollowUpTriggered func(channelID int, modelName, trigger string, logID int64)
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
// logID 是触发跟进的那条日志行 ID: 跟进跑完把结论写回该行(先到先得), 不再只留在内存快照里。
func SetScheduledAuditFollowUp(fn func(channelID int, modelName string, logID int64)) {
	scheduledAuditMu.Lock()
	scheduledAuditFollowUp = fn
	scheduledAuditMu.Unlock()
}

// SetScheduledAuditFollowUpTriggered 由定时任务在启动时挂上（带触发原因）。
// 与 SetScheduledAuditFollowUp 二选一，带 trigger 的优先。
func SetScheduledAuditFollowUpTriggered(fn func(channelID int, modelName, trigger string, logID int64)) {
	scheduledAuditMu.Lock()
	scheduledAuditFollowUpTriggered = fn
	scheduledAuditMu.Unlock()
}

// AuditFollowUpTrigger 返回这条日志该不该跟查、以及触发原因（"" = 不跟查）。
//
// 两道开关都必须**明确**为 true：
//   - model_audit_enabled：抽检总开关。关着时跟查一并停——跟查就是抽检的一个来源；
//   - model_audit_followup_enabled：抽检开着时，可疑日志要不要自动跟查。
//
// 读失败一律按关。这一点与被动探针那种零成本路径正好相反：跟查会真的把检测题
// 发到上游、真的计费，所以“读不到配置”绝不能解读成“那就开着吧”。
//
// 触发条件（任一即跟查，其余约束相同）：
//  1. 回显对不上（UpstreamModelMismatch）→ "echo_mismatch"；
//  2. 被动来源证据里出现了 strong/medium 的线索（依赖日志已有的 PassiveSource，
//     零额外请求、零 token）→ "passive_hint"。
//
// 网络失败、空请求、本地校验、弱线索都不打。
func AuditFollowUpTrigger(relayLog model.RelayLog) string {
	if relayLog.ChannelId <= 0 {
		return ""
	}
	if !auditSwitchExplicitlyOn(model.SettingKeyModelAuditEnabled) {
		return ""
	}
	if !auditSwitchExplicitlyOn(model.SettingKeyModelAuditFollowUpEnabled) {
		return ""
	}
	if relayLogExcludedFromModelTelemetry(relayLog) {
		return ""
	}
	if relayLog.UpstreamModelMismatch != nil && *relayLog.UpstreamModelMismatch {
		if strings.TrimSpace(relayLog.RequestModelName) != "" || strings.TrimSpace(relayLog.ActualModelName) != "" {
			return "echo_mismatch"
		}
	}
	if relayLogHasStrongPassiveHint(relayLog) {
		if strings.TrimSpace(relayLog.RequestModelName) != "" || strings.TrimSpace(relayLog.ActualModelName) != "" {
			return "passive_hint"
		}
	}
	return ""
}

// ShouldTriggerAuditFollowUp 只有开关都明确开着、且日志给出明确理由时才跟进。
// 保留这个布尔口是为了既有调用面与用例；判定逻辑都在 AuditFollowUpTrigger 里。
func ShouldTriggerAuditFollowUp(relayLog model.RelayLog) bool {
	return AuditFollowUpTrigger(relayLog) != ""
}

// auditSwitchExplicitlyOn 读一个布尔开关，只有明确是 true 才算开着（读失败=关）。
func auditSwitchExplicitlyOn(key model.SettingKey) bool {
	on, err := SettingGetBool(key)
	return err == nil && on
}

// relayLogHasStrongPassiveHint 判断这条日志的被动来源证据里有没有 strong/medium 线索。
//
// 只认 strong/medium：weak 档（如果以后出现）只说明“有点像”，拿它去触发一次
// 计费检测就是把证据门槛降到噪音水平。nil（没证据）当然不算。
func relayLogHasStrongPassiveHint(relayLog model.RelayLog) bool {
	if relayLog.PassiveSource == nil {
		return false
	}
	for _, hint := range relayLog.PassiveSource.Hints {
		switch hint.Strength {
		case behavior.ProvenanceStrengthStrong, behavior.ProvenanceStrengthMedium:
			return true
		}
	}
	return false
}

func maybeTriggerAuditFollowUp(relayLog model.RelayLog) {
	trigger := AuditFollowUpTrigger(relayLog)
	if trigger == "" {
		return
	}
	modelName := strings.TrimSpace(relayLog.RequestModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(relayLog.ActualModelName)
	}
	if modelName == "" {
		return
	}
	scheduledAuditMu.RLock()
	triggered := scheduledAuditFollowUpTriggered
	legacy := scheduledAuditFollowUp
	scheduledAuditMu.RUnlock()
	channelID := relayLog.ChannelId
	logID := relayLog.ID
	switch {
	case triggered != nil:
		safe.SafeGo("scheduled-audit-followup", func() { triggered(channelID, modelName, trigger, logID) })
	case legacy != nil:
		safe.SafeGo("scheduled-audit-followup", func() { legacy(channelID, modelName, logID) })
	}
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
