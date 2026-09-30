package balancer

import (
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// 分层轮询的候选顺序 + 迭代推进必须合起来满足「高层整体不可用才下沉」。
// 这两个测试专门锁住用户点名的失败路径——此前只有快乐路径，跳过高层再落到低层没人考试。
func TestSpreadTieredIteratorKeepsLowerTierLast(t *testing.T) {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode: model.GroupModeSpreadTiered,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 5, Weight: 1},
			{ChannelID: 2, ModelName: "m", Priority: 5, Weight: 1},
			{ChannelID: 3, ModelName: "m", Priority: 1, Weight: 1},
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)

	order := make([]int, 0, 3)
	for it.Next() {
		order = append(order, it.Item().ChannelID)
	}
	if len(order) != 3 {
		t.Fatalf("want 3 candidates, got %v", order)
	}
	if order[2] != 3 {
		t.Fatalf("低层候选必须最后才被尝试, got %v", order)
	}
	if !((order[0] == 1 && order[1] == 2) || (order[0] == 2 && order[1] == 1)) {
		t.Fatalf("高层的两个同层候选必须都排在低层之前, got %v", order)
	}
}

// 高层整层不可用（熔断 / 无可用 key / 禁用 —— relay.go 里都是逐条 Skip 再 continue）
// 时必须继续走到低层，而不是把请求打黑。
func TestSpreadTieredIteratorFallsThroughToLowerTierAfterHighTierSkips(t *testing.T) {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode: model.GroupModeSpreadTiered,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 3, Weight: 1},
			{ChannelID: 2, ModelName: "m", Priority: 3, Weight: 1},
			{ChannelID: 3, ModelName: "m", Priority: 1, Weight: 1},
			{ChannelID: 4, ModelName: "m", Priority: 1, Weight: 1},
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)

	visited := make([]int, 0, 4)
	skippedHighTier := 0
	hit := 0
	for it.Next() {
		item := it.Item()
		visited = append(visited, item.ChannelID)
		if item.Priority == 3 {
			// 复刻 relay.go 的跳过分支：熔断 / 没有可用 key。
			it.Skip(item.ChannelID, 0, "c", "simulated high-tier unavailable")
			skippedHighTier++
			continue
		}
		hit = item.ChannelID
		break
	}
	if skippedHighTier != 2 {
		t.Fatalf("高层两个候选都应先被尝试并跳过, got %d (visited=%v)", skippedHighTier, visited)
	}
	if hit != 3 && hit != 4 {
		t.Fatalf("高层整体不可用时必须下沉到低层, got channel %d (visited=%v)", hit, visited)
	}
}

// 同层里第一个失败后，不能直接跳到低层：同数字的第二个仍要先试（同数字内继续轮询，
// 失败时同样成立）。
func TestSpreadTieredIteratorTriesSameTierPeerBeforeLowering(t *testing.T) {
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode: model.GroupModeSpreadTiered,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 4, Weight: 1},
			{ChannelID: 2, ModelName: "m", Priority: 4, Weight: 1},
			{ChannelID: 3, ModelName: "m", Priority: 2, Weight: 1},
		},
	}
	it := NewIteratorWithSession(group, 0, "m", "", false)

	visited := make([]int, 0, 3)
	hit := 0
	for it.Next() {
		item := it.Item()
		visited = append(visited, item.ChannelID)
		if item.Priority == 4 && len(visited) == 1 {
			it.Skip(item.ChannelID, 0, "c", "simulated first high-tier failure")
			continue
		}
		hit = item.ChannelID
		break
	}
	if hit != 1 && hit != 2 {
		t.Fatalf("同层还有候选时不能直接下沉到低层, got channel %d (visited=%v)", hit, visited)
	}
	if hit == visited[0] {
		t.Fatalf("第二次尝试必须是同层的另一个候选, visited=%v", visited)
	}
}

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
