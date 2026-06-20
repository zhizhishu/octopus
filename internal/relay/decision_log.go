package relay

import (
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/utils/log"
)

// init wires the balancer's decision-trace hook to the relay logger, gated by the
// debug_load_balancer setting. The balancer stays import-cycle-free; the per-request
// cost when the setting is off is a single (cached) settings read.
func init() {
	balancer.DecisionLogHook = func(requestModel string, it *balancer.Iterator) {
		enabled, err := op.SettingGetBool(dbmodel.SettingKeyDebugLoadBalancer)
		if err != nil || !enabled {
			return
		}
		log.Infof("[lb-decision] model=%s %s", requestModel, it.DecisionTrace())
	}
}
