package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// 选路失败留下的审计行绝不能进模型健康统计/排行榜：那是一次配置问题，
// 不是"这个模型挂了"。抓的是 model.RelayLogErrorStrategyRouteSelectionPart
// 这个标记——relay 侧写日志时用它，这里用它排除，两边必须对得上。
func TestRouteUnresolvedLogExcludedFromModelTelemetry(t *testing.T) {
	for _, reason := range []string{"no_candidates", "route_unconfigured"} {
		entry := model.RelayLog{
			ErrorCode: model.RelayLogErrorCodeRouteUnresolved,
			ErrorStrategy: model.RelayLogErrorStrategyRouteSelectionPart +
				";reason=" + reason + ";upstream_forwarded=false",
		}
		if !relayLogExcludedFromModelTelemetry(entry) {
			t.Fatalf("route selection failure (reason=%s) must stay out of model telemetry", reason)
		}
	}
}

// 反向边界：正常的成功日志不能被顺手排除掉，否则统计会静默缺数据。
func TestNormalRelayLogStillCountsTowardModelTelemetry(t *testing.T) {
	entry := model.RelayLog{}
	if relayLogExcludedFromModelTelemetry(entry) {
		t.Fatal("a plain successful log must still count toward model telemetry")
	}
}
