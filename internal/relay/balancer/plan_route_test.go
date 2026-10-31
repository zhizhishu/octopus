package balancer

import (
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

func resetPlanRouteState() {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()
}

func planOrder(t *testing.T, g model.Group) []int {
	t.Helper()
	it := NewIteratorWithSession(g, 0, "m", "", false)
	out := make([]int, 0, it.Len())
	for it.Next() {
		out = append(out, it.Item().ChannelID)
	}
	return out
}

func healthyStats() model.RoutingRuntimeStats {
	return model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1}
}

// 方案「优先」：A 排第 1（渠道页优先级 5），B 排第 2（渠道页优先级 0）。
// 方案路由不看渠道优先级（relay 层清零后传进来），A 必须先试。
func TestPlanFillFirstFollowsPlanOrderOnly(t *testing.T) {
	resetPlanRouteState()
	g := model.Group{Mode: model.GroupModeFillFirst, PlanRoute: true, Items: []model.GroupItem{
		{ChannelID: 2, ModelName: "m", Priority: 2},
		{ChannelID: 1, ModelName: "m", Priority: 1},
		{ChannelID: 3, ModelName: "m", Priority: 3},
	}}
	for round := 0; round < 4; round++ {
		if got := planOrder(t, g); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
			t.Fatalf("round %d: 优先 must follow plan order 1,2,3, got %v", round, got)
		}
	}
}

// 轮询（mode 6）：数字大的先试；同一个数字严格轮流，不受在途数 / 延迟影响。
func TestPlanTieredRotatesStrictlyWithinNumber(t *testing.T) {
	resetPlanRouteState()
	busy := healthyStats()
	busy.InFlight = 5
	busy.LatencyEWMAms = 9000
	g := model.Group{Mode: model.GroupModeSpreadTiered, PlanRoute: true, Items: []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 3, RoutingStats: healthyStats()},
		{ChannelID: 2, ModelName: "m", Priority: 1, RoutingStats: busy},
		{ChannelID: 3, ModelName: "m", Priority: 1, RoutingStats: healthyStats()},
		{ChannelID: 4, ModelName: "m", Priority: 1, RoutingStats: healthyStats()},
	}}
	seconds := make([]int, 0, 6)
	for round := 0; round < 6; round++ {
		got := planOrder(t, g)
		if len(got) != 4 || got[0] != 1 {
			t.Fatalf("round %d: 数字最大的 A(1) 必须最先试, got %v", round, got)
		}
		seconds = append(seconds, got[1])
	}
	want := []int{2, 3, 4, 2, 3, 4}
	for i := range want {
		if seconds[i] != want[i] {
			t.Fatalf("同一个数字必须严格轮流（忙 / 慢的渠道也照样轮到）: got %v want %v", seconds, want)
		}
	}
}

// 轮询（mode 1）：全部一级，严格轮流。
func TestPlanSpreadRotatesStrictly(t *testing.T) {
	resetPlanRouteState()
	g := model.Group{Mode: model.GroupModeSpread, PlanRoute: true, Items: []model.GroupItem{
		{ChannelID: 7, ModelName: "m", Priority: 1, RoutingStats: healthyStats()},
		{ChannelID: 5, ModelName: "m", Priority: 1, RoutingStats: healthyStats()},
		{ChannelID: 6, ModelName: "m", Priority: 1, RoutingStats: healthyStats()},
	}}
	firsts := []int{}
	for round := 0; round < 6; round++ {
		firsts = append(firsts, planOrder(t, g)[0])
	}
	want := []int{5, 6, 7, 5, 6, 7}
	for i := range want {
		if firsts[i] != want[i] {
			t.Fatalf("got %v want %v", firsts, want)
		}
	}
}

// 不能用的渠道（熔断 / 冷却 / 没 Key）只被跳到这一级末尾，不越过这一级；
// 这一级都排完才轮到数字小的一级（失败往下掉）。
func TestPlanTieredSkipsUnhealthyButKeepsLevels(t *testing.T) {
	resetPlanRouteState()
	tripped := healthyStats()
	tripped.CircuitTripped = true
	g := model.Group{Mode: model.GroupModeSpreadTiered, PlanRoute: true, Items: []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 2, RoutingStats: tripped},
		{ChannelID: 2, ModelName: "m", Priority: 2, RoutingStats: healthyStats()},
		{ChannelID: 3, ModelName: "m", Priority: 1, RoutingStats: healthyStats()},
	}}
	for round := 0; round < 4; round++ {
		got := planOrder(t, g)
		if len(got) != 3 || got[0] != 2 || got[1] != 1 || got[2] != 3 {
			t.Fatalf("round %d: want [2 1 3] (健康的先，坏的排本级末尾，再往下一级), got %v", round, got)
		}
	}
}

// 渠道优先级只对非方案路由生效：旧的分组路由行为不变。
func TestNonPlanGroupStillUsesChannelPriority(t *testing.T) {
	resetPlanRouteState()
	g := model.Group{Mode: model.GroupModeFillFirst, Items: []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, ChannelPriority: 5},
		{ChannelID: 2, ModelName: "m", Priority: 2, ChannelPriority: 0},
	}}
	if got := planOrder(t, g); got[0] != 2 {
		t.Fatalf("legacy group keeps channel priority as primary key, got %v", got)
	}
}

// 方案路由不按「原生协议优先」重排。
func TestPlanRouteIgnoresNativeProtocolReorder(t *testing.T) {
	resetPlanRouteState()
	g := model.Group{Mode: model.GroupModeFillFirst, PlanRoute: true, Items: []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1},
		{ChannelID: 2, ModelName: "m", Priority: 1},
	}}
	it := NewIteratorWithSession(g, 0, "m", "", false)
	it.PrioritizeChannels(map[int]bool{2: true})
	it.Next()
	if it.Item().ChannelID != 1 {
		t.Fatalf("plan route must keep plan order, got %d first", it.Item().ChannelID)
	}
}
