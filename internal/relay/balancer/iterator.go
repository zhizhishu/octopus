package balancer

import (
	"fmt"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// Iterator 统一的负载均衡迭代器
// 内部编排：策略排序 + 粘性优先 + 决策追踪
type Iterator struct {
	candidates  []model.GroupItem
	index       int
	stickyIdx   int // 粘性通道在 candidates 中的位置，-1 表示无
	stickyKeyID int
	modelName   string // 请求模型名（用于熔断检查）

	// 内嵌追踪
	attempts []model.ChannelAttempt
	count    int
}

// NewIterator 创建负载均衡迭代器
// 自动处理：策略排序 + 粘性通道提前
func NewIterator(group model.Group, apiKeyID int, requestModel string) *Iterator {
	return NewIteratorWithSessionKey(group, apiKeyID, requestModel, "")
}

// NewIteratorWithSessionKey 创建带客户端会话作用域的负载均衡迭代器。
// 当 clientSessionKey 为空时保持原来的 apiKeyID+requestModel 粘性行为。
func NewIteratorWithSessionKey(group model.Group, apiKeyID int, requestModel, clientSessionKey string) *Iterator {
	b := GetBalancer(group.Mode)
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
	if keepTime > 0 {
		stickyTTL := time.Duration(keepTime) * time.Second
		if sticky := GetStickyWithSessionKey(apiKeyID, requestModel, clientSessionKey, stickyTTL); sticky != nil {
			for i, item := range candidates {
				if item.ChannelID == sticky.ChannelID {
					tripped, _ := IsTripped(sticky.ChannelID, sticky.ChannelKeyID, item.ModelName)
					keyRuntime := SnapshotKeyRuntime(sticky.ChannelID, sticky.ChannelKeyID, item.ModelName)
					if tripped || keyRuntime.CooldownRemainingMs > 0 {
						break
					}
					if i > 0 {
						// 将粘性通道移到最前面
						stickyItem := candidates[i]
						copy(candidates[1:i+1], candidates[0:i])
						candidates[0] = stickyItem
					}
					stickyIdx = 0
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
	}
	// Optional decision-trace hook (set by the relay layer when the debug setting is
	// on). Kept as an injected hook so this package needs no logger/settings import.
	if DecisionLogHook != nil {
		DecisionLogHook(requestModel, it)
	}
	return it
}

// PrioritizeChannels keeps the balancer order inside each bucket while moving
// preferred channels to the front. It is used for protocol-native-first routing.
func (it *Iterator) PrioritizeChannels(preferred map[int]bool) {
	if it == nil || len(preferred) == 0 || len(it.candidates) < 2 {
		return
	}

	stickyChannelID := 0
	hasSticky := it.stickyIdx >= 0 && it.stickyIdx < len(it.candidates)
	if hasSticky {
		stickyChannelID = it.candidates[it.stickyIdx].ChannelID
	}

	prioritized := make([]model.GroupItem, 0, len(it.candidates))
	others := make([]model.GroupItem, 0, len(it.candidates))
	for _, item := range it.candidates {
		if preferred[item.ChannelID] {
			prioritized = append(prioritized, item)
		} else {
			others = append(others, item)
		}
	}
	if len(prioritized) == 0 || len(others) == 0 {
		return
	}
	prioritized = append(prioritized, others...)
	it.candidates = prioritized
	it.stickyIdx = -1
	if hasSticky {
		for i, item := range it.candidates {
			if item.ChannelID == stickyChannelID {
				it.stickyIdx = i
				break
			}
		}
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
		Msg:          msg,
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
		Msg:          msg,
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

// End 结束尝试：设置状态，自动计算耗时，追加到 Iterator
func (s *AttemptSpan) End(status model.AttemptStatus, statusCode int, msg string) {
	if s.ended {
		return
	}
	s.ended = true
	s.attempt.Status = status
	s.attempt.Duration = int(time.Since(s.startTime).Milliseconds())
	s.attempt.Msg = msg
	s.iter.attempts = append(s.iter.attempts, s.attempt)
}

// SetUpstreamPath records the normalized upstream URL path for audit logs.
func (s *AttemptSpan) SetUpstreamPath(path string) {
	if s == nil || s.ended {
		return
	}
	s.attempt.UpstreamPath = path
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
