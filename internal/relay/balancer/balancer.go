package balancer

import (
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bestruirui/octopus/internal/model"
)

var roundRobinCounters sync.Map // key: candidate signature -> *uint64
var weightedCounters sync.Map   // key: candidate signature -> *uint64

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

// RoundRobin 轮询：从上次位置开始轮转排列
type RoundRobin struct{}

func (b *RoundRobin) Candidates(items []model.GroupItem) []model.GroupItem {
	if len(items) == 0 {
		return nil
	}
	buckets := priorityBuckets(items)
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

// Random 随机：随机打乱所有 items
type Random struct{}

func (b *Random) Candidates(items []model.GroupItem) []model.GroupItem {
	if len(items) == 0 {
		return nil
	}
	buckets := priorityBuckets(items)
	result := make([]model.GroupItem, 0, len(items))
	for _, bucket := range buckets {
		shuffled := make([]model.GroupItem, len(bucket))
		copy(shuffled, bucket)
		rand.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		result = append(result, shuffled...)
	}
	return result
}

// Failover 故障转移：按优先级排序
type Failover struct{}

func (b *Failover) Candidates(items []model.GroupItem) []model.GroupItem {
	if len(items) == 0 {
		return nil
	}
	return sortByPriority(items)
}

// Spread 轮询：同一优先级内 round-robin 均摊，并按“容量/健康硬分层 + 容量感知评分”
// 把不健康、过载或明显更慢的候选稳定地排到同优先级桶的后面。
//
// 设计借鉴 axonhub 的 composite 评分，但刻意“内敛”，避免它最怕的 collapse：
//   - priority 永远是硬边界；
//   - spreadTier 仍是粗粒度硬分层（无可用 key / 熔断 / 冷却 / 忙 / 闲），等价于
//     axonhub 用大额负分把饱和渠道“踢出但保留为末位 failover”；
//   - 在同一 (priority, tier) 桶内再用 spreadRank 这个“粗粒度评分”按近期负载、连续
//     失败、延迟/首 token 做二次排序——但只用粗分档，所以延迟相近的候选仍维持
//     round-robin 顺序而真正轮转。这正是 axonhub「轮转预算 > 延迟预算，最快单点不会
//     永远赢」的精神：延迟只在“明显更慢”时把某个渠道降档，绝不让单点 collapse。
type Spread struct{}

func (b *Spread) Candidates(items []model.GroupItem) []model.GroupItem {
	if len(items) == 0 {
		return nil
	}
	// round-robin 预排：priority 分桶 + 桶内轮转，既是均摊基线，也是同档候选的轮转来源。
	rotated := (&RoundRobin{}).Candidates(items)
	// 稳定排序：priority（硬边界）> 健康/容量分层 > 容量感知评分。同优先级、同健康层、
	// 同评分档的候选保持 round-robin 顺序，从而真正轮转、不 collapse。
	sort.SliceStable(rotated, func(i, j int) bool {
		left, right := rotated[i], rotated[j]
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		if lt, rtier := spreadTier(left), spreadTier(right); lt != rtier {
			return lt < rtier
		}
		return spreadRank(left) < spreadRank(right)
	})
	return rotated
}

// spreadTier buckets a candidate by health/capacity for the spread strategy:
// idle-healthy first, then busy, then soft-cooldown, then circuit-unavailable,
// then channels with no usable key at all. It deliberately ignores
// latency/throughput/success so equally healthy channels keep their round-robin
// turn instead of all collapsing onto one.
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
	case rt.MaxConcurrent > 0 && rt.InFlight+rt.PendingSelections >= int64(rt.MaxConcurrent):
		// At its configured concurrency cap: demote (like a soft cooldown) so a burst
		// spreads to peers with spare capacity, but keep it usable as a last resort —
		// consistent with never hard-blacking-out a route that still has a usable key.
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

// Weighted 加权分配：按权重概率排序
type Weighted struct{}

func (b *Weighted) Candidates(items []model.GroupItem) []model.GroupItem {
	if len(items) == 0 {
		return nil
	}

	buckets := priorityBuckets(items)
	result := make([]model.GroupItem, 0, len(items))
	for _, bucket := range buckets {
		n := len(bucket)
		if n == 0 {
			continue
		}

		totalWeight := 0
		for _, item := range bucket {
			w := item.Weight
			if w <= 0 {
				w = 1
			}
			totalWeight += w
		}
		if totalWeight <= 0 {
			totalWeight = n
		}

		slot := int((atomic.AddUint64(weightedCounterForItems(bucket), 1) - 1) % uint64(totalWeight))
		selected := 0
		acc := 0
		for i, item := range bucket {
			w := item.Weight
			if w <= 0 {
				w = 1
			}
			acc += w
			if slot < acc {
				selected = i
				break
			}
		}

		for i := 0; i < n; i++ {
			result = append(result, bucket[(selected+i)%n])
		}
	}
	return result
}

// weightedSignature keys the Weighted strategy's counter. Unlike the rotation
// counter (membership-only, so weight edits do not reset it), the weighted
// distribution genuinely depends on each item's weight, so weight must be part of
// the key — otherwise two buckets with identical membership but different weights
// would share one counter and bleed into each other's distribution.
func weightedSignature(items []model.GroupItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(strconv.Itoa(item.ChannelID))
		b.WriteByte(':')
		b.WriteString(item.ModelName)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(item.Weight))
		b.WriteByte('|')
	}
	return b.String()
}

func weightedCounterForItems(items []model.GroupItem) *uint64 {
	key := weightedSignature(items)
	counter := new(uint64)
	actual, _ := weightedCounters.LoadOrStore(key, counter)
	return actual.(*uint64)
}

// Smart 智能路由：参考成熟代理项目的优先级分桶思路。
//
// CCH/CLIProxyAPI 都不是把所有指标揉成一个魔法总分，而是先按可解释的
// 优先级层级收窄候选，再在同层里处理权重/健康信号。Octopus 的候选列表
// 需要保留后续 fallback，所以这里输出全量排序：优先级永远是硬边界，近期
// 首 token/吞吐/熔断/并发与权重只在同一优先级桶内排序。
type Smart struct{}

func (b *Smart) Candidates(items []model.GroupItem) []model.GroupItem {
	n := len(items)
	if n == 0 {
		return nil
	}

	// Round-robin pre-order so candidates that end up fully tied on every
	// health/load signal (e.g. a cold start with no telemetry yet) still rotate
	// turn by turn instead of always picking the lowest channel ID. The stable
	// sort below preserves this order whenever ranking cannot separate two
	// candidates, which keeps spread/round-robin mode actually spreading.
	items = (&RoundRobin{}).Candidates(items)

	ranked := make([]smartRankedItem, n)
	for i, item := range items {
		ranked[i] = rankSmartItem(item)
	}
	boostUnobservedSmartCandidates(ranked)

	sort.SliceStable(ranked, func(i, j int) bool {
		left := ranked[i]
		right := ranked[j]

		if left.itemPriority != right.itemPriority {
			return left.itemPriority < right.itemPriority
		}
		if left.channelPriority != right.channelPriority {
			return left.channelPriority < right.channelPriority
		}
		if left.unavailable != right.unavailable {
			return !left.unavailable
		}
		if left.softCooldown != right.softCooldown {
			return !left.softCooldown
		}
		if left.consecutiveFailures != right.consecutiveFailures {
			return left.consecutiveFailures < right.consecutiveFailures
		}
		if left.failureRate != right.failureRate {
			return left.failureRate < right.failureRate
		}
		if left.inFlight != right.inFlight {
			return left.inFlight < right.inFlight
		}
		if left.score != right.score {
			return left.score > right.score
		}
		if left.hasStats != right.hasStats {
			return left.hasStats
		}
		if left.streamFirstTokenMs != right.streamFirstTokenMs && left.streamFirstTokenMs > 0 && right.streamFirstTokenMs > 0 {
			return left.streamFirstTokenMs < right.streamFirstTokenMs
		}
		if left.avgWaitMs != right.avgWaitMs && left.avgWaitMs > 0 && right.avgWaitMs > 0 {
			return left.avgWaitMs < right.avgWaitMs
		}
		if left.loadRatio != right.loadRatio {
			return left.loadRatio < right.loadRatio
		}
		if left.weight != right.weight {
			return left.weight > right.weight
		}
		if left.successCount != right.successCount {
			return left.successCount > right.successCount
		}
		if left.requestCount != right.requestCount {
			return left.requestCount > right.requestCount
		}
		return left.item.ChannelID < right.item.ChannelID
	})

	result := make([]model.GroupItem, n)
	for i := range ranked {
		result[i] = ranked[i].item
	}
	return result
}

type smartRankedItem struct {
	item                model.GroupItem
	itemPriority        int
	channelPriority     int
	failureRate         float64
	hasStats            bool
	avgWaitMs           float64
	streamFirstTokenMs  float64
	throughput          float64
	inFlight            int64
	consecutiveFailures int64
	unavailable         bool
	softCooldown        bool
	loadRatio           float64
	score               float64
	weight              int
	successCount        int64
	requestCount        int64
}

func rankSmartItem(item model.GroupItem) smartRankedItem {
	stats := item.ChannelStats
	weight := item.Weight
	if weight <= 0 {
		weight = 1
	}

	requests := stats.RequestSuccess + stats.RequestFailed
	runtime := item.RoutingStats
	runtimeRequests := runtime.RequestSuccess + runtime.RequestFailed
	totalRequests := requests + runtimeRequests
	ranked := smartRankedItem{
		item:            item,
		itemPriority:    item.Priority,
		channelPriority: item.ChannelPriority,
		weight:          weight,
		successCount:    stats.RequestSuccess + runtime.RequestSuccess,
		requestCount:    totalRequests,
		// inFlight folds in pending selection reservations so a burst of concurrent
		// picks spreads across channels instead of all stampeding the same idle one.
		inFlight:            runtime.InFlight + runtime.PendingSelections,
		consecutiveFailures: runtime.ConsecutiveFailures,
		throughput:          runtime.ThroughputEWMA,
		streamFirstTokenMs:  runtime.FirstTokenEWMAms,
	}

	if requests > 0 {
		ranked.hasStats = true
		ranked.failureRate = float64(stats.RequestFailed) / float64(requests)
		ranked.avgWaitMs = float64(stats.WaitTime) / float64(requests)
	}
	if runtime.HasRuntime {
		ranked.hasStats = true
		if runtime.LatencyEWMAms > 0 {
			ranked.avgWaitMs = runtime.LatencyEWMAms
		}
		if totalRequests > 0 {
			ranked.failureRate = float64(stats.RequestFailed+runtime.RequestFailed) / float64(totalRequests)
		}
	}
	if runtime.AvailableKeyCount > 0 && runtime.HealthyKeyCount == 0 && (runtime.CircuitOpenKeys > 0 || runtime.KeyCooldownOpenCount > 0 || runtime.CooldownRemainingMs > 0) {
		ranked.unavailable = true
	}
	if runtime.CooldownRemainingMs > 0 {
		ranked.softCooldown = true
	}
	ranked.loadRatio = float64(runtime.Attempts+runtime.InFlight+runtime.PendingSelections) / float64(weight)
	ranked.score = smartScore(ranked, runtime)

	return ranked
}

func smartScore(r smartRankedItem, runtime model.RoutingRuntimeStats) float64 {
	score := 100.0
	score += math.Log1p(float64(r.weight)) * 8
	score -= r.failureRate * 60
	score -= float64(r.consecutiveFailures) * 12
	score -= float64(r.inFlight) * 18
	score -= r.loadRatio * 3
	if r.unavailable {
		score -= 1000
	}
	if r.softCooldown {
		score -= 200
	}

	latency := r.avgWaitMs
	if latency > 0 {
		score += 40 / (1 + latency/1000)
	}
	if runtime.PreferStream {
		if r.streamFirstTokenMs > 0 {
			score += 50 / (1 + r.streamFirstTokenMs/500)
		}
		if r.throughput > 0 {
			score += math.Min(25, math.Log1p(r.throughput)*6)
		}
	}
	if r.hasStats && r.successCount > 0 {
		score += 5
	}
	if !r.hasStats {
		score += 20
	}
	return score
}

func boostUnobservedSmartCandidates(ranked []smartRankedItem) {
	for start := 0; start < len(ranked); {
		end := start + 1
		for end < len(ranked) &&
			ranked[end].itemPriority == ranked[start].itemPriority &&
			ranked[end].channelPriority == ranked[start].channelPriority {
			end++
		}

		shouldExplore := false
		for i := start; i < end; i++ {
			item := ranked[i]
			if !item.hasStats {
				continue
			}
			if item.loadRatio >= 1 || item.inFlight > 0 || item.avgWaitMs >= 1000 || item.streamFirstTokenMs >= 750 {
				shouldExplore = true
				break
			}
		}
		if shouldExplore {
			for i := start; i < end; i++ {
				if !ranked[i].hasStats {
					ranked[i].score += 45
				}
			}
		}
		start = end
	}
}

func sortByPriority(items []model.GroupItem) []model.GroupItem {
	sorted := make([]model.GroupItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Priority < sorted[j].Priority
	})
	return sorted
}

func priorityBuckets(items []model.GroupItem) [][]model.GroupItem {
	if len(items) == 0 {
		return nil
	}

	sorted := sortByPriority(items)
	buckets := make([][]model.GroupItem, 0, len(sorted))
	for _, item := range sorted {
		if len(buckets) == 0 {
			buckets = append(buckets, []model.GroupItem{item})
			continue
		}
		last := len(buckets) - 1
		if buckets[last][0].Priority == item.Priority {
			buckets[last] = append(buckets[last], item)
			continue
		}
		buckets = append(buckets, []model.GroupItem{item})
	}
	return buckets
}
