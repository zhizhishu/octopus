package op

import (
	"strconv"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

const scheduledAuditDisclaimer = "定时快检结果提供参考，不等同模型真实性证明。网络失败不记为造假。"

var (
	scheduledAuditMu       sync.RWMutex
	scheduledAuditSnapshot model.ScheduledAuditSnapshot
	scheduledAuditLastHit  = map[string]int64{}
)

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
