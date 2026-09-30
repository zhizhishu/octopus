package balancer

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// 分层轮询（画布「大数字优先、同级轮询」）：
// 数字大的层整体排在前面，同一个数字层内仍然轮转 —— 这是用户 2026-09-30 定的语义。
func TestSpreadTieredHigherPriorityTierComesFirst(t *testing.T) {
	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1},
		{ChannelID: 2, ModelName: "m", Priority: 3},
		{ChannelID: 3, ModelName: "m", Priority: 1},
	}
	got := (&SpreadTiered{}).Candidates(items)
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(got))
	}
	if got[0].ChannelID != 2 {
		t.Fatalf("数字最大的层必须排第一, got channel %d", got[0].ChannelID)
	}
	if got[1].Priority != 1 || got[2].Priority != 1 {
		t.Fatalf("低层必须整体排在高层之后, got %d/%d", got[1].Priority, got[2].Priority)
	}
}

// 同一个数字层内必须是轮询，不能因为序号唯一就退化成固定顺序（纯轮询的老毛病）。
func TestSpreadTieredSameTierStillRotates(t *testing.T) {
	items := []model.GroupItem{
		{ChannelID: 11, ModelName: "m", Priority: 2},
		{ChannelID: 12, ModelName: "m", Priority: 2},
		{ChannelID: 13, ModelName: "m", Priority: 1},
	}
	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		got := (&SpreadTiered{}).Candidates(items)
		if got[0].Priority != 2 {
			t.Fatalf("高层必须始终最先, got priority %d", got[0].Priority)
		}
		seen[got[0].ChannelID] = true
	}
	if !seen[11] || !seen[12] {
		t.Fatalf("同层两个候选都要被轮到, seen=%v", seen)
	}
}

// 数字全等时分层轮询等同纯轮询（旧规则就地升级不改行为）。
func TestSpreadTieredAllEqualMatchesSpreadTiering(t *testing.T) {
	items := []model.GroupItem{
		{ChannelID: 21, ModelName: "m", Priority: 1},
		{ChannelID: 22, ModelName: "m", Priority: 1},
	}
	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		seen[(&SpreadTiered{}).Candidates(items)[0].ChannelID] = true
	}
	if !seen[21] || !seen[22] {
		t.Fatalf("全等数字必须轮转, seen=%v", seen)
	}
}

func TestSpreadTieredEmpty(t *testing.T) {
	if got := (&SpreadTiered{}).Candidates(nil); got != nil {
		t.Fatalf("empty input must return nil, got %v", got)
	}
}

// GetBalancer 必须把新模式接到分层轮询上，而不是回落到纯轮询。
func TestGetBalancerMapsTieredMode(t *testing.T) {
	if _, ok := GetBalancer(model.GroupModeSpreadTiered).(*SpreadTiered); !ok {
		t.Fatalf("GroupModeSpreadTiered 必须映射到 SpreadTiered")
	}
	if _, ok := GetBalancer(model.GroupModeSpread).(*Spread); !ok {
		t.Fatalf("GroupModeSpread 必须保持纯轮询")
	}
	if _, ok := GetBalancer(model.GroupModeFillFirst).(*Failover); !ok {
		t.Fatalf("GroupModeFillFirst 必须保持故障转移")
	}
}
