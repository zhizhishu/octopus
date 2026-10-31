package balancer

import (
	"fmt"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
)

// Iterator 统一的负载均衡迭代器
// 内部编排：策略排序 + 粘性优先 + 决策追踪
type Iterator struct {
	candidates  []model.GroupItem
	index       int
	stickyIdx   int // 粘性通道在 candidates 中的位置，-1 表示无
	stickyKeyID int
	modelName   string // 请求模型名（用于熔断检查）
	mode        model.GroupMode
	// planRoute：方案路由。顺序完全由方案页决定，PrioritizeChannels（原生协议优先）不再重排。
	planRoute bool

	// 内嵌追踪
	attempts []model.ChannelAttempt
	count    int
}

// NewIterator 创建负载均衡迭代器
// 自动处理：策略排序 + 粘性通道提前
func NewIterator(group model.Group, apiKeyID int, requestModel string) *Iterator {
	return NewIteratorWithSessionKey(group, apiKeyID, requestModel, "")
}

// NewIteratorWithSessionKey 创建带客户端会话作用域的负载均衡迭代器（默认启用会话粘性）。
// 当 clientSessionKey 为空时保持原来的 apiKeyID+requestModel 粘性行为。
func NewIteratorWithSessionKey(group model.Group, apiKeyID int, requestModel, clientSessionKey string) *Iterator {
	return NewIteratorWithSession(group, apiKeyID, requestModel, clientSessionKey, true)
}

// NewIteratorWithSession 同上，但可显式控制会话粘性。stickyEnabled=false 时完全跳过粘性
// （既不查 sticky，也不把任何渠道提到队首），让候选保持负载均衡器排好的轮转顺序。
// 轮询/负载均衡模式下，对"换渠道只丢 prompt-cache 命中、不破坏正确性"的会话来源
// （prompt_cache_key / oct 自造指纹 / user / safety-identifier）应传 false，否则 sticky 会
// 把同一会话永远钉在最初那个渠道、使轮询形同虚设（真机日志实测同会话 100% 粘同渠道）。
// 需要跨轮正确性的来源（previous_response_id / thread / trace / codex session）仍传 true。
// clientSessionKey 始终照传（指纹合成与日志复用它），stickyEnabled 只决定它是否参与选路。
func NewIteratorWithSession(group model.Group, apiKeyID int, requestModel, clientSessionKey string, stickyEnabled bool) *Iterator {
	b := GetBalancerForGroup(group)
	candidates := b.Candidates(group.Items)

	stickyIdx := -1
	stickyKeyID := 0
	// Effective sticky window: a group's own SessionKeepTime wins; when unset (<=0)
	// fall back to the global session_keep_time_default so new/unconfigured groups can
	// still stick without per-group setup. 0 on both = sticky off (legacy behavior).
	keepTime := group.SessionKeepTime
	if keepTime <= 0 {
		keepTime = sessionKeepTimeDefault()
	}
	if stickyEnabled && keepTime > 0 {
		stickyTTL := time.Duration(keepTime) * time.Second
		if sticky := GetStickyWithSessionKey(apiKeyID, requestModel, clientSessionKey, stickyTTL); sticky != nil {
			for i, item := range candidates {
				if item.ChannelID == sticky.ChannelID {
					tripped, _ := IsTripped(sticky.ChannelID, sticky.ChannelKeyID, item.ModelName)
					keyRuntime := SnapshotKeyRuntime(sticky.ChannelID, sticky.ChannelKeyID, item.ModelName)
					if tripped || keyRuntime.CooldownRemainingMs > 0 {
						break
					}
					// 只在粘性通道自己所属的硬路由层内部提前，绝不越过更高的硬层：
					// 低层粘性通道不得插到高层候选前面（与 PrioritizeChannels 同规则）。
					tierStart := i
					for tierStart > 0 && SameRoutingTier(group.Mode, candidates[i], candidates[tierStart-1]) {
						tierStart--
					}
					if tierStart < i {
						stickyItem := candidates[i]
						copy(candidates[tierStart+1:i+1], candidates[tierStart:i])
						candidates[tierStart] = stickyItem
					}
					stickyIdx = tierStart
					stickyKeyID = sticky.ChannelKeyID
					break
				}
			}
		}
	}

	// Selection-time reservation: tell telemetry the top candidate was just picked,
	// before the real attempt starts, so a burst of concurrent requests spreads to
	// other channels instead of all stampeding the same idle one. Fill-first
	// strategies ignore this; only the load-aware path reads it back when ranking.
	if len(candidates) > 0 {
		MarkRuntimeSelection(candidates[0].ChannelID, candidates[0].ModelName)
	}

	it := &Iterator{
		candidates:  candidates,
		index:       -1,
		stickyIdx:   stickyIdx,
		stickyKeyID: stickyKeyID,
		modelName:   requestModel,
		mode:        group.Mode,
		planRoute:   group.PlanRoute,
	}
	// Optional decision-trace hook (set by the relay layer when the debug setting is
	// on). Kept as an injected hook so this package needs no logger/settings import.
	if DecisionLogHook != nil {
		DecisionLogHook(requestModel, it)
	}
	return it
}

// SameRoutingTier reports whether two candidates occupy the SAME hard routing tier for
// the given group mode — i.e. the balancer treats them as equally preferred on every hard
// boundary, so they may be reordered relative to one another or raced together WITHOUT a
// lower tier leapfrogging a higher one.
//
// The rule mirrors each strategy's real hard boundaries (see balancer.go):
//   - SpreadTiered: item Priority is the outer hard boundary and ChannelPriority the
//     inner one, so a tier is a (Priority, ChannelPriority) pair.
//   - FillFirst (Failover): sortByRoutingPriority orders by ChannelPriority primary and
//     item Priority secondary, so a tier is likewise a (ChannelPriority, Priority) pair.
//     The ChannelID deterministic tie-break is a serial-order detail, not a boundary —
//     two channels tied on both keys are the same tier and may race.
//   - Spread (round-robin) and any unknown/legacy mode: item Priority is deliberately NOT
//     a boundary (a unique drag order would freeze the rotation), so a tier is just a
//     ChannelPriority value.
//
// Exported so the relay can group cross-channel racers from the same tier.
func SameRoutingTier(mode model.GroupMode, a, b model.GroupItem) bool {
	switch mode {
	case model.GroupModeSpreadTiered, model.GroupModeFillFirst:
		return a.Priority == b.Priority && a.ChannelPriority == b.ChannelPriority
	default:
		return a.ChannelPriority == b.ChannelPriority
	}
}

// PrioritizeChannels moves preferred (protocol-native) channels to the front of their
// OWN hard-tier block, preserving relative order inside each block. It is used for
// protocol-native-first routing.
//
// It is deliberately a PER-TIER reorder, not a global two-way split: a lower-tier native
// channel must never jump ahead of a higher-tier converted one, so the canvas tier number
// stays a hard boundary even on rules that mix protocols. A preferred channel can only
// win its own tier. The global owner exception (which IS allowed to cross tiers) lives in
// PrioritizeOwnerChannel instead.
func (it *Iterator) PrioritizeChannels(preferred map[int]bool) {
	// 方案路由：方案页写的顺序 / 「数字一样的轮流」就是实际顺序，不按协议再排。
	if it == nil || it.planRoute || len(preferred) == 0 || len(it.candidates) < 2 {
		return
	}

	stickyChannelID := 0
	hasSticky := it.stickyIdx >= 0 && it.stickyIdx < len(it.candidates)
	if hasSticky {
		stickyChannelID = it.candidates[it.stickyIdx].ChannelID
	}

	reordered := make([]model.GroupItem, 0, len(it.candidates))
	for blockStart := 0; blockStart < len(it.candidates); {
		blockEnd := blockStart + 1
		for blockEnd < len(it.candidates) &&
			SameRoutingTier(it.mode, it.candidates[blockStart], it.candidates[blockEnd]) {
			blockEnd++
		}
		chosen := make([]model.GroupItem, 0, blockEnd-blockStart)
		others := make([]model.GroupItem, 0, blockEnd-blockStart)
		for _, item := range it.candidates[blockStart:blockEnd] {
			if preferred[item.ChannelID] {
				chosen = append(chosen, item)
			} else {
				others = append(others, item)
			}
		}
		reordered = append(reordered, chosen...)
		reordered = append(reordered, others...)
		blockStart = blockEnd
	}
	it.candidates = reordered
	it.refindStickyChannel(stickyChannelID, hasSticky)
}

// PrioritizeOwnerChannel moves ONLY the given owner channel's candidate to the very front,
// keeping every other candidate's relative order intact. Unlike PrioritizeChannels this is
// allowed to cross hard tiers on purpose: a Responses cursor can only be replayed on the
// exact channel+key that minted it, so the owner channel must be tried first even when it
// sits in a lower tier. It never drags the owner's tier-mates up with it. The sticky marker
// is re-found afterwards, exactly like every other reorder.
func (it *Iterator) PrioritizeOwnerChannel(channelID int) {
	if it == nil || channelID == 0 || len(it.candidates) < 2 {
		return
	}
	ownerIndex := -1
	for i, item := range it.candidates {
		if item.ChannelID == channelID {
			ownerIndex = i
			break
		}
	}
	if ownerIndex <= 0 {
		return
	}

	stickyChannelID := 0
	hasSticky := it.stickyIdx >= 0 && it.stickyIdx < len(it.candidates)
	if hasSticky {
		stickyChannelID = it.candidates[it.stickyIdx].ChannelID
	}

	ownerItem := it.candidates[ownerIndex]
	copy(it.candidates[1:ownerIndex+1], it.candidates[0:ownerIndex])
	it.candidates[0] = ownerItem
	it.refindStickyChannel(stickyChannelID, hasSticky)
}

// refindStickyChannel restores stickyIdx after a reorder by looking the sticky channel
// back up. Callers compute stickyChannelID/hasSticky BEFORE mutating candidates. When
// several candidates share the sticky channel id the first match wins, matching the
// historical behaviour of PrioritizeChannels.
func (it *Iterator) refindStickyChannel(stickyChannelID int, hasSticky bool) {
	if it == nil {
		return
	}
	it.stickyIdx = -1
	if !hasSticky {
		return
	}
	for i, item := range it.candidates {
		if item.ChannelID == stickyChannelID {
			it.stickyIdx = i
			break
		}
	}
}

// RemainingTierItems returns value copies of the candidates that follow the current index
// and share the current candidate's hard routing tier (SameRoutingTier), stopping at the
// first candidate that does not (the tier is contiguous, so this is the whole remaining
// tail of the current tier). The current item itself is EXCLUDED and the index is not
// moved. The relay uses it to discover which later candidates may race alongside the
// current one. nil-safe and boundary-safe.
func (it *Iterator) RemainingTierItems() []model.GroupItem {
	if it == nil || it.index < 0 || it.index >= len(it.candidates) {
		return nil
	}
	current := it.candidates[it.index]
	var result []model.GroupItem
	for j := it.index + 1; j < len(it.candidates); j++ {
		if !SameRoutingTier(it.mode, current, it.candidates[j]) {
			break
		}
		result = append(result, it.candidates[j])
	}
	return result
}

// ConsumeTierItems advances the index past n candidates that were actually raced as
// contiguous same-tier contenders alongside the current item. n counts only the
// immediately-following candidates (the current item is NOT included), so a relay that
// races the current item plus the first k of RemainingTierItems calls ConsumeTierItems(k).
//
// It never skips a candidate that was not raced, and it is clamped to the END of the
// current hard tier — not the end of the whole candidate list — so an over-large n can
// never jump across a lower tier. index < 0 and n <= 0 are no-ops. The index lands on the
// last consumed candidate so the next Next() resumes right after it. nil-safe.
func (it *Iterator) ConsumeTierItems(n int) {
	if it == nil || n <= 0 || it.index < 0 || it.index >= len(it.candidates) {
		return
	}
	current := it.candidates[it.index]
	tierTail := it.index
	for tierTail+1 < len(it.candidates) && SameRoutingTier(it.mode, current, it.candidates[tierTail+1]) {
		tierTail++
	}
	it.index += n
	if it.index > tierTail {
		it.index = tierTail
	}
}

// Next 移动到下一个候选，返回 false 表示遍历完成
func (it *Iterator) Next() bool {
	it.index++
	return it.index < len(it.candidates)
}

// Item 返回当前候选的 GroupItem
func (it *Iterator) Item() model.GroupItem {
	return it.candidates[it.index]
}

// RemapCurrentModel rewrites the current candidate's model name in place. The relay
// layer calls this when a channel's model_mapping rewrites the client-facing alias to
// the upstream model, so the circuit-breaker check (which reads the candidate's model
// name) agrees with the attempt records (which use the mapped upstream name). Without
// it the breaker is checked under the alias but recorded under the upstream name and
// therefore never trips.
func (it *Iterator) RemapCurrentModel(modelName string) {
	if it == nil || modelName == "" {
		return
	}
	if it.index < 0 || it.index >= len(it.candidates) {
		return
	}
	it.candidates[it.index].ModelName = modelName
}

// IsSticky 当前候选是否为粘性通道
func (it *Iterator) IsSticky() bool {
	return it.stickyIdx >= 0 && it.index == it.stickyIdx
}

func (it *Iterator) IsStickyChannel(channelID int) bool {
	if it == nil || !it.IsSticky() {
		return false
	}
	if it.stickyIdx < 0 || it.stickyIdx >= len(it.candidates) {
		return false
	}
	return it.candidates[it.stickyIdx].ChannelID == channelID
}

func (it *Iterator) StickyKeyIDForCurrentChannel(channelID int) int {
	if it == nil || !it.IsStickyChannel(channelID) || it.stickyKeyID == 0 {
		return 0
	}
	return it.stickyKeyID
}

func (it *Iterator) IsStickyChannelKey(channelID, channelKeyID int) bool {
	if it == nil || !it.IsSticky() || it.stickyKeyID == 0 || channelKeyID == 0 {
		return false
	}
	if it.stickyIdx < 0 || it.stickyIdx >= len(it.candidates) {
		return false
	}
	return it.candidates[it.stickyIdx].ChannelID == channelID && it.stickyKeyID == channelKeyID
}

// Len 返回候选列表长度
func (it *Iterator) Len() int {
	return len(it.candidates)
}

// Index 返回当前迭代位置（0-based）
func (it *Iterator) Index() int {
	return it.index
}

// Skip 记录当前通道被跳过（通道禁用、无Key、类型不兼容等）
func (it *Iterator) Skip(channelID, channelKeyID int, channelName, msg string) {
	it.count++
	it.attempts = append(it.attempts, model.ChannelAttempt{
		ChannelID:    channelID,
		ChannelKeyID: channelKeyID,
		ChannelName:  channelName,
		ModelName:    it.candidates[it.index].ModelName,
		AttemptNum:   it.count,
		Status:       model.AttemptSkipped,
		Sticky:       it.IsSticky(),
		Msg:          xredact.Secrets(msg),
	})
}

// SkipCircuitBreak 检查熔断状态，若已熔断自动记录（含剩余冷却时间）并返回 true
func (it *Iterator) SkipCircuitBreak(channelID, channelKeyID int, channelName string) bool {
	return it.SkipCircuitBreakScoped(channelID, channelKeyID, channelName, "", "")
}

func (it *Iterator) SkipCircuitBreakScoped(channelID, channelKeyID int, channelName, endpoint, capability string) bool {
	modelName := it.candidates[it.index].ModelName
	tripped, remaining := IsTrippedScoped(channelID, channelKeyID, modelName, endpoint, capability)
	if !tripped {
		return false
	}
	msg := "circuit breaker tripped"
	if remaining > 0 {
		msg = fmt.Sprintf("circuit breaker tripped, remaining cooldown: %ds", int(remaining.Seconds()))
	}
	it.count++
	it.attempts = append(it.attempts, model.ChannelAttempt{
		ChannelID:    channelID,
		ChannelKeyID: channelKeyID,
		ChannelName:  channelName,
		ModelName:    modelName,
		Endpoint:     endpoint,
		Capability:   capability,
		AttemptNum:   it.count,
		Status:       model.AttemptCircuitBreak,
		Sticky:       it.IsSticky(),
		Msg:          xredact.Secrets(msg),
	})
	return true
}

// StartAttempt 开始一次真实转发尝试，返回 Span 用于记录结果
func (it *Iterator) StartAttempt(channelID, channelKeyID int, channelName string) *AttemptSpan {
	it.count++
	return &AttemptSpan{
		attempt: model.ChannelAttempt{
			ChannelID:    channelID,
			ChannelKeyID: channelKeyID,
			ChannelName:  channelName,
			ModelName:    it.candidates[it.index].ModelName,
			AttemptNum:   it.count,
			Sticky:       it.IsSticky(),
		},
		startTime: time.Now(),
		iter:      it,
	}
}

// Attempts 返回所有决策记录（交给日志模块持久化）
func (it *Iterator) Attempts() []model.ChannelAttempt {
	return it.attempts
}

// AttemptSpan 管理单次通道尝试的生命周期（计时、状态、结果）
type AttemptSpan struct {
	attempt   model.ChannelAttempt
	startTime time.Time
	iter      *Iterator
	ended     bool
}

// SetProxy records the egress route this forwarding attempt took: whether a proxy
// was used and, if so, its source (channel-specific vs system), scheme (http/socks5)
// and target host. The balancer stays free of settings/op imports, so the relay
// layer — which holds the channel's proxy config — computes these and sets them
// here. Only real forward attempts (StartAttempt) carry a route; skipped/circuit-
// broken attempts never leave the box, so they keep an empty (direct) route.
func (s *AttemptSpan) SetProxy(used bool, source, scheme, target string) {
	if s == nil {
		return
	}
	s.attempt.ProxyUsed = used
	s.attempt.ProxySource = source
	s.attempt.ProxyScheme = scheme
	s.attempt.ProxyTarget = target
}

// End 结束尝试：设置状态，自动计算耗时，追加到 Iterator
func (s *AttemptSpan) End(status model.AttemptStatus, statusCode int, msg string) {
	if s.ended {
		return
	}
	s.ended = true
	s.attempt.Status = status
	s.attempt.Duration = int(time.Since(s.startTime).Milliseconds())
	s.attempt.Msg = xredact.Secrets(msg)
	s.iter.attempts = append(s.iter.attempts, s.attempt)
}

// SetUpstreamPath records the normalized upstream URL path for audit logs.
func (s *AttemptSpan) SetUpstreamPath(path string) {
	if s == nil || s.ended {
		return
	}
	s.attempt.UpstreamPath = path
}

// SetModelName rewrites the attempt's recorded model name. The relay race core uses it
// so a cross-channel/generic-contender winner or loser is audited under the model it
// actually attempted (the channel's mapped/runtime model) rather than the iterator's
// current candidate model — preventing a race winner on another channel from being
// mis-attributed in the audit log. No-op after the span has ended or for a nil span.
func (s *AttemptSpan) SetModelName(modelName string) {
	if s == nil || s.ended {
		return
	}
	s.attempt.ModelName = modelName
}

func (s *AttemptSpan) SetRouteScope(endpoint, capability string) {
	if s == nil || s.ended {
		return
	}
	s.attempt.Endpoint = endpoint
	s.attempt.Capability = capability
}

// Duration 返回从开始到现在的耗时
func (s *AttemptSpan) Duration() time.Duration {
	return time.Since(s.startTime)
}

// StartedAt returns the attempt start time for per-channel runtime telemetry.
func (s *AttemptSpan) StartedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.startTime
}
