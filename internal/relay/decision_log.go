package relay

import (
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// init wires the balancer's decision-trace hook to the relay logger, gated by the
// debug_load_balancer setting. The balancer stays import-cycle-free; the per-request
// cost when the setting is off is a single (cached) settings read.
func init() {
	balancer.DecisionLogHook = func(requestModel string, it *balancer.Iterator) {
		legacy, err := op.SettingGetBool(dbmodel.SettingKeyDebugLoadBalancer)
		legacy = err == nil && legacy
		debugMode := op.LogModeDebugActive()
		if !legacy && !debugMode {
			return
		}
		diagLogf(legacy && !debugMode, "[lb-decision] model=%s %s", requestModel, it.DecisionTrace())
	}
}

// diagLogf 打印排障行（[diag] / [lb-decision]）。
//
// 日志模式 = 调试时运行日志已是 debug 级，按 Info 打即可。只单独开了旧的
// diagnostic_mode / debug_load_balancer、而日志模式仍是普通（warn 级）时，按 Warn 打——
// 否则这两个旧开关在新的默认级别下会「开了也看不到」。
func diagLogf(onlyLegacyToggle bool, template string, args ...interface{}) {
	if onlyLegacyToggle {
		log.Warnf(template, args...)
		return
	}
	log.Infof(template, args...)
}
