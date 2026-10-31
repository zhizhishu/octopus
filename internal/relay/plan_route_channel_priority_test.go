package relay

import (
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 方案「优先」：A 排第 1、渠道页优先级 5；B 排第 2、渠道页优先级 0。方案路由必须先试 A。
// 同样两条渠道放进非方案（旧分组）路由，渠道优先级照旧是主键，先试 B。
func TestPlanRouteIgnoresChannelPagePriority(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	a := createPathTestChannel(t, ctx, "plan-a", outbound.OutboundTypeOpenAIChat, "http://127.0.0.1:1", "m", func(ch *dbmodel.Channel) { ch.Priority = 5 })
	b := createPathTestChannel(t, ctx, "plan-b", outbound.OutboundTypeOpenAIChat, "http://127.0.0.1:1", "m", func(ch *dbmodel.Channel) { ch.Priority = 0 })
	items := []dbmodel.GroupItem{
		{ChannelID: a.ID, ModelName: "m", Priority: 1},
		{ChannelID: b.ID, ModelName: "m", Priority: 2},
	}

	plan := enrichGroupForSmartRouting(ctx, dbmodel.Group{Mode: dbmodel.GroupModeFillFirst, ModeLocked: true, PlanRoute: true, Items: items})
	it := balancer.NewIteratorWithSession(plan, 0, "m", "", false)
	it.Next()
	if it.Item().ChannelID != a.ID {
		t.Fatalf("plan 优先 must try A (plan #1) first, got channel %d", it.Item().ChannelID)
	}

	legacy := enrichGroupForSmartRouting(ctx, dbmodel.Group{Mode: dbmodel.GroupModeFillFirst, ModeLocked: true, Items: items})
	it = balancer.NewIteratorWithSession(legacy, 0, "m", "", false)
	it.Next()
	if it.Item().ChannelID != b.ID {
		t.Fatalf("legacy group keeps channel priority, want B first, got %d", it.Item().ChannelID)
	}
}
