package balancer

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bestruirui/octopus/internal/model"
)

var roundRobinCounters sync.Map // key: candidate signature -> *uint64

// Balancer 根据负载均衡模式选择通道
type Balancer interface {
	// Candidates 返回按策略排序的候选列表
	// 调用方在遍历候选列表时自行检查熔断状态
	Candidates(items []model.GroupItem) []model.GroupItem
}

// GetBalancer 根据模式返回对应的负载均衡器
// GetBalancer maps a group mode to a strategy. The product now exposes only two
// modes, both capacity-aware:
//
//   - Fill-first (GroupModeFillFirst): a stable priority order keeps traffic
//     concentrated on the top healthy channel for the best upstream prompt-cache
//     hit rate, sinking to the next only on trip/cooldown/rate-limit.
//   - Spread / round-robin (everything else): load-aware distribution across
//     same-priority channels using recent health, in-flight + selection
//     reservations, latency and throughput, with priority still a hard boundary.
//
// Legacy stored values (random / weighted / smart) fold into the spread strategy
// so existing groups keep working without a data migration.
func GetBalancer(mode model.GroupMode) Balancer {
	switch mode {
	case model.GroupModeFillFirst:
		return &Failover{}
	default:
		return &Spread{}
	}
}

func roundRobinCounterForItems(items []model.GroupItem) *uint64 {
	key := roundRobinSignature(items)
	counter := new(uint64)
	actual, _ := roundRobinCounters.LoadOrStore(key, counter)
	return actual.(*uint64)
}

// roundRobinSignature identifies a candidate set by its channel/model membership
// only — deliberately NOT by priority/weight. Priority already scopes the counter
// because the lookup happens per priority bucket, and a (channelID, modelName) pair
// lives in exactly one bucket, so membership alone is a unique key. Folding
// weight/priority into the signature would mint a fresh counter (resetting rotation
// to the first channel and re-hammering it) on every admin weight/priority tweak.
// Keying on membership keeps the rotation cursor stable across such edits — the
// cursor-stability idea borrowed from CLIProxyAPI's selector.
func roundRobinSignature(items []model.GroupItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(strconv.Itoa(item.ChannelID))
		b.WriteByte(':')
		b.WriteString(item.ModelName)
		b.WriteByte('|')
	}
	return b.String()
}

// Failover 故障转移：按优先级排序（渠道优先级为主键，条目优先级为次级）
type Failover struct{}

func (b *Failover) Candidates(items []model.GroupItem) []model.GroupItem {
	if len(items) == 0 {
		return nil
	}
	return sortByRoutingPriority(items)
}

// Spread 轮询/负载均衡：同一「渠道优先级」(ChannelPriority) 内 round-robin 均摊，并按
// “容量/健康硬分层 + 容量感知评分”把不健康、过载或明显更慢的候选稳定地排到同优先级桶后面。
//
// 关键：轮询模式里「条目优先级」(Priority，实为 UI 拖拽序号，每条目递增唯一) 不是硬边界，
// 只有 ChannelPriority 是。否则条目序号一唯一(拖拽 / 自动加渠道后必然如此)就把下面的
// spreadTier/spreadRank 整段短路，退化成纯拖拽顺序、永不轮转、永不避慢——这正是“轮询不
// 轮询、慢渠道不降档”的根因。要按固定顺序堆叠请改用 fill-first(Failover) 模式。
//
// 设计借鉴 axonhub 的 composite 评分，但刻意“内敛”，避免它最怕的 collapse：
//   - ChannelPriority 永远是硬边界；
//   - spreadTier 仍是粗粒度硬分层（无可用 key / 熔断 / 冷却 / 忙 / 闲），等价于
//     axonhub 用大额负分把饱和渠道“踢出但保留为末位 failover”；
//   - 在同一 (ChannelPriority, tier) 桶内再用 spreadRank 这个“粗粒度评分”按近期负载、连续
//     失败、延迟/首 token 做二次排序——但只用粗分档，所以延迟相近的候选仍维持 round-robin
//     顺序而真正轮转。这正是 axonhub「轮转预算 > 延迟预算，最快单点不会永远赢」的精神：
//     延迟只在“明显更慢”时把某个渠道降档，绝不让单点 collapse。
type Spread struct{}

func (b *Spread) Candidates(items []model.GroupItem) []model.GroupItem {
	if len(items) == 0 {
		return nil
	}
	// round-robin 预排：按 ChannelPriority 分桶 + 桶内轮转(而非按条目 Priority)，既是均摊基线，
	// 也是同档候选的轮转来源。轮询模式下条目拖拽序不是路由边界，只有 ChannelPriority 分层。
	rotated := rotateByChannelPriority(items)
	// 稳定排序：ChannelPriority(唯一硬边界) > 健康/容量分层。同 ChannelPriority、同健康层的
	// 候选保持 round-robin 预排序，从而真正轮转——轮询的语义就是“同优先级渠道都能用上”。
	// ⚠️刻意不再按 spreadRank(延迟)细排：延迟略低的渠道会每轮抢到队首、把轮询 collapse 成
	// “只打最快那一个”，这正是“优先级一样不切渠道 / 渠道用不上”的真因。延迟只是快慢、不是
	// “不行”；只有 spreadTier 里真正不行的(无 key/熔断/冷却/连续失败/满载)才降级、被轮转跳过。
	// spreadRank/延迟感知仅保留给 DecisionTrace 调试输出，不再驱动选路。
	sort.SliceStable(rotated, func(i, j int) bool {
		left, right := rotated[i], rotated[j]
		if left.ChannelPriority != right.ChannelPriority {
			return left.ChannelPriority < right.ChannelPriority
		}
		return spreadTier(left) < spreadTier(right)
	})
	return rotated
}

// rotateByChannelPriority 与 RoundRobin.Candidates 同构，但按 ChannelPriority 分桶而非
// Priority——轮询(Spread)模式下 Priority(拖拽序)不是路由边界，只有 ChannelPriority 分层。
// 复用 roundRobinCounterForItems：其 signature 只看 (channelID, modelName) 成员，一个成员
// 恒落在唯一一个桶，所以计数器在按 ChannelPriority 分桶时同样稳定、不会被优先级调整重置。
func rotateByChannelPriority(items []model.GroupItem) []model.GroupItem {
	buckets := channelPriorityBuckets(items)
	result := make([]model.GroupItem, 0, len(items))
	for _, bucket := range buckets {
		n := len(bucket)
		if n == 0 {
			continue
		}
		idx := int((atomic.AddUint64(roundRobinCounterForItems(bucket), 1) - 1) % uint64(n))
		for i := 0; i < n; i++ {
			result = append(result, bucket[(idx+i)%n])
		}
	}
	return result
}

// channelPriorityBuckets 按 ChannelPriority 升序分桶(与 priorityBuckets 同构，键换成
// ChannelPriority)。同一 ChannelPriority 的候选归入同一桶，交给上层轮转/评分。
func channelPriorityBuckets(items []model.GroupItem) [][]model.GroupItem {
	if len(items) == 0 {
		return nil
	}
	sorted := make([]model.GroupItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ChannelPriority < sorted[j].ChannelPriority
	})
	buckets := make([][]model.GroupItem, 0, len(sorted))
	for _, item := range sorted {
		if len(buckets) == 0 {
			buckets = append(buckets, []model.GroupItem{item})
			continue
		}
		last := len(buckets) - 1
		if buckets[last][0].ChannelPriority == item.ChannelPriority {
			buckets[last] = append(buckets[last], item)
			continue
		}
		buckets = append(buckets, []model.GroupItem{item})
	}
	return buckets
}

// spreadFailureDemoteThreshold: 连续失败达到此数就把渠道降级(等同软冷却)，让轮询跳过它、
// 优先同级健康渠道，直到它恢复。以前这条降级藏在 spreadRank(延迟评分)里，去掉延迟细排后
// 移进 spreadTier，保证“当前渠道不行(在失败)就切走”仍然成立——这正是用户要的语义。
const spreadFailureDemoteThreshold = 3

// spreadTier buckets a candidate by health/capacity for the spread strategy:
// idle/busy-but-healthy first (they share a tier so round-robin rotates over ALL
// usable channels), then soft-cooldown / recently-failing / capped, then
// circuit-unavailable, then channels with no usable key at all. It deliberately
// ignores latency/throughput so equally-servable channels keep their round-robin
// turn instead of collapsing onto the fastest one — “slow” is not “broken”. A
// channel only sinks when it genuinely can’t serve well: no key, tripped circuit,
// cooling down, at a hard cap, or on a run of consecutive failures.
//
// A channel whose snapshot reports zero available (enabled) keys cannot serve the
// request — the iterator will skip it for lack of a key. Sinking it behind every
// channel that can still serve keeps spread rotating over usable channels instead
// of wasting a turn at the front on an empty one. When snapshots are not hydrated
// every candidate reports zero available keys, so they share this tier and the
// round-robin pre-order still rotates them.
func spreadTier(item model.GroupItem) int {
	rt := item.RoutingStats
	switch {
	case rt.AvailableKeyCount == 0:
		return 4
	case rt.HealthyKeyCount == 0 &&
		(rt.CircuitOpenKeys > 0 || rt.KeyCooldownOpenCount > 0 || rt.CooldownRemainingMs > 0):
		return 3
	case rt.CircuitTripped:
		return 3
	case rt.CooldownRemainingMs > 0:
		return 2
	case rt.ConsecutiveFailures >= spreadFailureDemoteThreshold:
		// 近期连续失败但还没触发冷却/熔断：正在“不行”，降级(与软冷却同档)，让轮询把它
		// 跳过、优先同级健康渠道，直到它恢复。慢≠不行，但连续失败=不行。
		return 2
	case rt.MaxConcurrent > 0 && rt.InFlight+rt.PendingSelections >= int64(rt.MaxConcurrent):
		// At its configured concurrency cap: demote (like a soft cooldown) so a burst
		// spreads to peers with spare capacity, but keep it usable as a last resort —
		// consistent with never hard-blacking-out a route that still has a usable key.
		return 2
	case rt.RPMLimit > 0 && rt.RecentRequestCount >= int64(rt.RPMLimit):
		// At its configured requests-per-minute cap: demote like the concurrency cap so
		// a burst spreads to peers with spare RPM budget, but keep it usable as a last
		// resort. Same never-blackout principle — the hard cap lives at the group level.
		return 2
	case rt.InFlight+rt.PendingSelections > 0:
		return 1
	default:
		return 0
	}
}

// spreadRank refines ordering WITHIN one (priority, spreadTier) bucket using a
// coarse, capacity-aware score (lower = preferred). It is deliberately quantized:
// candidates that land in the same rank keep their round-robin order and therefore
// keep rotating, so equally-good channels never collapse onto one. Only a
// meaningfully worse candidate — heavier recent load, recent consecutive failures,
// or distinctly higher latency/first-token — drops a rank and sinks behind its
// healthier peers. This mirrors axonhub's composite scoring while keeping the
// round-robin spread dominant: the bands are coarse, so latency/load only bias the
// order at the margin instead of letting one "fastest" channel win every turn.
//
// spreadTier already separates idle from busy and ejects unhealthy candidates, so
// spreadRank mostly differentiates peers that share a tier: faster idle channels
// ahead of slow idle ones, and lighter-loaded busy channels ahead of heavier ones.
func spreadRank(item model.GroupItem) int {
	rt := item.RoutingStats
	rank := 0

	// Recent load (in-flight + selection reservations). spreadTier already gates
	// idle vs busy; this gives a finer order among busy peers so a burst keeps
	// spreading toward the least-loaded one.
	switch load := rt.InFlight + rt.PendingSelections; {
	case load >= 6:
		rank += 3
	case load >= 3:
		rank += 2
	case load >= 1:
		rank += 1
	}

	// Recent reliability: a channel that is failing but has not yet tripped a
	// cooldown/circuit should still yield to a clean peer in the same tier.
	switch {
	case rt.ConsecutiveFailures >= 3:
		rank += 2
	case rt.ConsecutiveFailures >= 1:
		rank += 1
	}

	// Responsiveness: only when we have a recent sample. Unobserved (or stale)
	// channels stay rank 0 (neutral) so they get explored and cold starts keep
	// rotating; bands are coarse so only a distinctly slower channel is demoted.
	// NOTE: latency bands use exclusive '>' (exactly 1500ms is still band +1) while
	// the load and failure bands above use inclusive '>='.
	if lat := effectiveSpreadLatencyMs(rt); lat > 0 {
		switch {
		case lat > 6000:
			rank += 4
		case lat > 3000:
			rank += 3
		case lat > 1500:
			rank += 2
		case lat > 750:
			rank += 1
		}
	}

	return rank
}

// effectiveSpreadLatencyMs prefers first-token latency for streaming turns (what a
// CLI actually feels) and falls back to overall latency otherwise. A stale sample
// (no fresh attempt within latencyStalenessWindow) is treated as unobserved so a
// demoted channel periodically re-enters rotation and re-probes instead of being
// pinned behind a fast peer forever (self-heal — see latencyStalenessWindow).
func effectiveSpreadLatencyMs(rt model.RoutingRuntimeStats) float64 {
	if rt.LatencyStale {
		return 0
	}
	if rt.PreferStream && rt.FirstTokenEWMAms > 0 {
		return rt.FirstTokenEWMAms
	}
	return rt.LatencyEWMAms
}

// comparePriority is the production routing-priority ordering shared by Failover and
// Spread: the per-channel「渠道优先级」(ChannelPriority) is the PRIMARY hard boundary
// (smaller = selected first), and the per-item pool priority (Priority) is the
// SECONDARY tie-break. Returns -1 if a ranks ahead of b, +1 if behind, 0 if tied on
// both. ChannelPriority defaults to 0, so a deployment that never set a channel
// priority leaves every candidate tied on the primary key and falls through to the
// pre-existing per-item priority order — zero behaviour change until an operator sets
// a channel priority, at which point it overrides the item priority.
func comparePriority(a, b model.GroupItem) int {
	if a.ChannelPriority != b.ChannelPriority {
		if a.ChannelPriority < b.ChannelPriority {
			return -1
		}
		return 1
	}
	if a.Priority != b.Priority {
		if a.Priority < b.Priority {
			return -1
		}
		return 1
	}
	return 0
}

// sortByRoutingPriority orders candidates for the fill-first (Failover) strategy by
// comparePriority (channel priority primary, item priority secondary), with ChannelID
// as a final deterministic tie-break so fill-first concentrates on the SAME channel
// across restarts when several share the same priority — instead of a nondeterministic
// sort.Slice order.
func sortByRoutingPriority(items []model.GroupItem) []model.GroupItem {
	sorted := make([]model.GroupItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		if c := comparePriority(sorted[i], sorted[j]); c != 0 {
			return c < 0
		}
		return sorted[i].ChannelID < sorted[j].ChannelID
	})
	return sorted
}
