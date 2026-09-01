package relay

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// RequestState 保存单个请求的实时状态，用于 SSE 推送"调用中"日志。
// 仅在内存维护，不写数据库（终态仍由 op.RelayLogAdd 写 DB 审计日志）。
type RequestState struct {
	ID        uint64    `json:"id"`
	Status    string    `json:"status"` // "running" | "success" | "failed" | "canceled"
	StartedAt time.Time `json:"started_at"`
	Model     string    `json:"model"`
	Endpoint  string    `json:"endpoint"`

	// 当前轮次信息
	Round         int    `json:"round"`
	TargetChannel string `json:"target_channel,omitempty"`
	TargetModel   string `json:"target_model,omitempty"`
	Sending       bool   `json:"sending"` // 当前轮次是否正在发送请求
	Error         string `json:"error,omitempty"`

	// 终态信息
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	TotalLatency int64      `json:"total_latency_ms,omitempty"` // 毫秒

	// 全部尝试记录（供前端详情展开）
	Attempts []AttemptSnapshot `json:"attempts,omitempty"`
}

// AttemptSnapshot 单次渠道尝试的快照（轻量级，不含完整 body）
type AttemptSnapshot struct {
	Round         int    `json:"round"`
	ChannelName   string `json:"channel_name"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	Status        string `json:"status"` // "trying" | "success" | "error"
	ErrorMsg      string `json:"error_msg,omitempty"`
	LatencyMs     int64  `json:"latency_ms,omitempty"`
}

var (
	stateIDSeq    atomic.Uint64                          // 进程内严格递增的请求状态 ID
	stateMu       sync.RWMutex                           // 全部共享状态的互斥锁
	stateRequests = make(map[uint64]*RequestState)      // 按 ID 保存的全部请求状态
	stateWatchers = make(map[chan *RequestState]struct{}) // 全部 SSE 订阅者（用双向 chan 作 key）
	maxFinished   = 100                                 // 内存中保留的已完成请求数上限
	finishedQueue []uint64                              // FIFO 队列，记录已完成请求的 ID
)

// newRequestState 分配请求 ID 并登记初始 running 状态，立即广播。
func newRequestState(model, endpoint string) *RequestState {
	stateMu.Lock()
	defer stateMu.Unlock()

	state := &RequestState{
		ID:        stateIDSeq.Add(1),
		Status:    "running",
		StartedAt: time.Now(),
		Model:     model,
		Endpoint:  endpoint,
		Attempts:  make([]AttemptSnapshot, 0, 4),
	}
	stateRequests[state.ID] = state
	publishStateLocked(state)
	return state
}

// startRound 记录本轮选中的目标渠道和模型，进入发送状态，广播更新。
func (s *RequestState) startRound(channelName, upstreamModel string) int {
	stateMu.Lock()
	defer stateMu.Unlock()

	s.Round++
	s.TargetChannel = channelName
	s.TargetModel = upstreamModel
	s.Sending = true
	s.Error = ""

	// 记录到 attempts
	s.Attempts = append(s.Attempts, AttemptSnapshot{
		Round:         s.Round,
		ChannelName:   channelName,
		UpstreamModel: upstreamModel,
		Status:        "trying",
	})

	publishStateLocked(s)
	return s.Round
}

// finishRound 记录本轮上游结果，errText 为空表示成功。
func (s *RequestState) finishRound(errText string, latencyMs int64) {
	stateMu.Lock()
	defer stateMu.Unlock()

	s.Sending = false
	s.Error = errText

	// 更新最后一次 attempt 的状态
	if len(s.Attempts) > 0 {
		last := &s.Attempts[len(s.Attempts)-1]
		if errText == "" {
			last.Status = "success"
		} else {
			last.Status = "error"
			last.ErrorMsg = errText
		}
		last.LatencyMs = latencyMs
	}

	publishStateLocked(s)
}

// markSuccess 标记请求成功完成。
func (s *RequestState) markSuccess() {
	stateMu.Lock()
	defer stateMu.Unlock()

	s.Status = "success"
	s.Error = ""
	now := time.Now()
	s.FinishedAt = &now
	s.TotalLatency = now.Sub(s.StartedAt).Milliseconds()

	publishStateLocked(s)
	enqueueFinishedLocked(s.ID)
}

// markFailed 标记请求失败。
func (s *RequestState) markFailed(err string) {
	stateMu.Lock()
	defer stateMu.Unlock()

	s.Status = "failed"
	s.Error = err
	now := time.Now()
	s.FinishedAt = &now
	s.TotalLatency = now.Sub(s.StartedAt).Milliseconds()

	publishStateLocked(s)
	enqueueFinishedLocked(s.ID)
}

// markCanceled 标记请求被取消。
func (s *RequestState) markCanceled() {
	stateMu.Lock()
	defer stateMu.Unlock()

	s.Status = "canceled"
	now := time.Now()
	s.FinishedAt = &now
	s.TotalLatency = now.Sub(s.StartedAt).Milliseconds()

	publishStateLocked(s)
	enqueueFinishedLocked(s.ID)
}

// publishStateLocked 向所有 SSE 订阅者广播状态更新（调用前必须已持有 stateMu 锁）。
func publishStateLocked(state *RequestState) {
	// 复制一份，避免订阅者持有指针后被后续修改
	snapshot := *state
	snapshot.Attempts = append([]AttemptSnapshot(nil), state.Attempts...)

	for ch := range stateWatchers {
		select {
		case ch <- &snapshot:
		default:
			// 订阅者消费慢或断开，丢弃本次推送（不阻塞其他订阅者）
		}
	}
}

// enqueueFinishedLocked 将已完成请求加入 FIFO 队列，超过上限时淘汰最旧的。
func enqueueFinishedLocked(id uint64) {
	finishedQueue = append(finishedQueue, id)
	if len(finishedQueue) > maxFinished {
		// 淘汰最旧的
		oldestID := finishedQueue[0]
		finishedQueue = finishedQueue[1:]
		delete(stateRequests, oldestID)
	}
}

// SubscribeRequestState 注册一个 SSE 订阅者，返回接收通道。
func SubscribeRequestState(ctx context.Context) <-chan *RequestState {
	ch := make(chan *RequestState, 16) // 缓冲避免慢消费者阻塞广播

	stateMu.Lock()
	stateWatchers[ch] = struct{}{}
	stateMu.Unlock()

	// 发送当前所有状态的快照
	stateMu.RLock()
	for _, state := range stateRequests {
		snapshot := *state
		snapshot.Attempts = append([]AttemptSnapshot(nil), state.Attempts...)
		select {
		case ch <- &snapshot:
		case <-ctx.Done():
			stateMu.RUnlock()
			UnsubscribeRequestState(ch)
			return ch
		}
	}
	stateMu.RUnlock()

	return ch
}

// UnsubscribeRequestState 取消订阅。
func UnsubscribeRequestState(ch <-chan *RequestState) {
	stateMu.Lock()
	defer stateMu.Unlock()
	// 遍历删除（因为存储的是双向 chan，而传入的是单向只读 chan）
	for key := range stateWatchers {
		if (<-chan *RequestState)(key) == ch {
			delete(stateWatchers, key)
			break
		}
	}
}

// GetRequestStateSnapshot 获取当前所有请求状态的快照（供 HTTP 轮询接口）。
func GetRequestStateSnapshot() []*RequestState {
	stateMu.RLock()
	defer stateMu.RUnlock()

	snapshot := make([]*RequestState, 0, len(stateRequests))
	for _, state := range stateRequests {
		stateCopy := *state
		stateCopy.Attempts = append([]AttemptSnapshot(nil), state.Attempts...)
		snapshot = append(snapshot, &stateCopy)
	}
	return snapshot
}
