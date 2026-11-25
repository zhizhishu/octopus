package op

import (
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 任务页「每一项一个小方块」的进度，只活在内存里。
//
// 为什么不落库：进度一秒变好几次，写库只为了给页面看一眼不值得；进程一重启，
// 正在跑的那一行本来就会被租约回收收尾（见 AuditReclaimExpired），进度跟着消失
// 正好。任务页拿不到进度时退回「这一项在等」，不会编一个数字出来。
//
// 只观察、不干预：观察者挂在 behavior.Run 的 context 上，拿到的只是「开始/发了一次/
// 结束」事件，不改请求、不改结论（见 behavior.ProgressObserver）。

// 小方块的状态。
const (
	AuditLiveWaiting = "waiting"
	AuditLiveRunning = "running"
	AuditLiveDone    = "done"
	AuditLiveFailed  = "failed"
)

// AuditLiveProbe 一项检查的进度。
type AuditLiveProbe struct {
	ID string `json:"id"`
	// Status 见 AuditLive* 常量。
	Status string `json:"status"`
	// Sent 这一项已经发出的上游请求数（成功失败都算）。
	Sent int `json:"sent"`
	// Planned 这一项计划发几次（来自 behavior.PlanFor，不适用的为 0）。
	Planned int `json:"planned"`
}

// AuditLiveProgress 一个任务当前在跑的那一个模型的进度快照。
type AuditLiveProgress struct {
	ResultID  int64            `json:"result_id"`
	Model     string           `json:"model"`
	Sent      int              `json:"sent"`
	Planned   int              `json:"planned"`
	Probes    []AuditLiveProbe `json:"probes"`
	StartedAt time.Time        `json:"started_at"`
}

// AuditLiveTracker 是挂在一次 behavior.Run 上的观察者。并发安全。
type AuditLiveTracker struct {
	jobID int64

	mu       sync.Mutex
	progress AuditLiveProgress
	current  int // 当前项在 Probes 里的下标；-1 = 还没开始
}

var auditLive sync.Map // jobID(int64) -> *AuditLiveTracker

// AuditProbeProtocol 把渠道类型翻成探针计划用的协议名（与 ProbeSender.Protocol 同口径）。
func AuditProbeProtocol(t outbound.OutboundType) string {
	switch t {
	case outbound.OutboundTypeAnthropic:
		return "anthropic-messages"
	case outbound.OutboundTypeOpenAIResponse:
		return "openai-responses"
	case outbound.OutboundTypeGemini:
		return "gemini-generate-content"
	default:
		return "openai-chat"
	}
}

// AuditPlannedRequestsFor 按真实计划算一组探针会发几次请求（不适用的按 0）。
func AuditPlannedRequestsFor(protocol, model string, ids []behavior.ProbeID) int {
	return behavior.SumPlannedRequests(behavior.PlanFor(protocol, model), ids)
}

// AuditLiveStart 为一个任务开始记录当前模型的进度，返回观察者。
// 同一任务再次开始（下一个模型）会覆盖上一份。
func AuditLiveStart(jobID, resultID int64, model, protocol string, ids []behavior.ProbeID) *AuditLiveTracker {
	plan := behavior.PlanFor(protocol, model)
	planned := make(map[behavior.ProbeID]int, len(plan))
	for _, p := range plan {
		if p.Applicable {
			planned[p.ID] = p.Requests
		}
	}
	probes := make([]AuditLiveProbe, 0, len(ids))
	total := 0
	for _, id := range ids {
		n := planned[id]
		total += n
		probes = append(probes, AuditLiveProbe{ID: string(id), Status: AuditLiveWaiting, Planned: n})
	}
	tr := &AuditLiveTracker{
		jobID:   jobID,
		current: -1,
		progress: AuditLiveProgress{
			ResultID:  resultID,
			Model:     model,
			Planned:   total,
			Probes:    probes,
			StartedAt: time.Now(),
		},
	}
	if jobID > 0 {
		auditLive.Store(jobID, tr)
	}
	return tr
}

// AuditLiveEnd 丢掉一个任务的进度（这个模型跑完了）。只删自己那一份，
// 不会误删下一个模型刚登记的进度。
func AuditLiveEnd(tr *AuditLiveTracker) {
	if tr == nil || tr.jobID <= 0 {
		return
	}
	auditLive.CompareAndDelete(tr.jobID, tr)
}

// AuditLiveSnapshot 取某个任务当前的进度副本；没有在跑就返回 nil。
func AuditLiveSnapshot(jobID int64) *AuditLiveProgress {
	v, ok := auditLive.Load(jobID)
	if !ok {
		return nil
	}
	tr := v.(*AuditLiveTracker)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	cp := tr.progress
	cp.Probes = append([]AuditLiveProbe(nil), tr.progress.Probes...)
	return &cp
}

func (t *AuditLiveTracker) indexOf(id behavior.ProbeID) int {
	for i := range t.progress.Probes {
		if t.progress.Probes[i].ID == string(id) {
			return i
		}
	}
	return -1
}

// ProbeStarted 见 behavior.ProgressObserver。
func (t *AuditLiveTracker) ProbeStarted(id behavior.ProbeID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i := t.indexOf(id); i >= 0 {
		t.current = i
		t.progress.Probes[i].Status = AuditLiveRunning
	}
}

// RequestSent 见 behavior.ProgressObserver。
func (t *AuditLiveTracker) RequestSent() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.progress.Sent++
	if t.current >= 0 && t.current < len(t.progress.Probes) {
		t.progress.Probes[t.current].Sent++
	}
}

// ProbeFinished 见 behavior.ProgressObserver。
func (t *AuditLiveTracker) ProbeFinished(id behavior.ProbeID, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i := t.indexOf(id); i >= 0 {
		if ok {
			t.progress.Probes[i].Status = AuditLiveDone
		} else {
			t.progress.Probes[i].Status = AuditLiveFailed
		}
		if t.current == i {
			t.current = -1
		}
	}
}
