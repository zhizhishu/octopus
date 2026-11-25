package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
)

// 任务页小方块：跟着事件走，跑完就丢，不留旧进度。
func TestAuditLiveTrackerFollowsEvents(t *testing.T) {
	ids := []behavior.ProbeID{behavior.ProbeLiveness, behavior.ProbeTokenDelta}
	tr := AuditLiveStart(9001, 77, "gpt-test", "openai-chat", ids)
	snap := AuditLiveSnapshot(9001)
	if snap == nil || snap.Planned != 3 || len(snap.Probes) != 2 || snap.Probes[0].Status != AuditLiveWaiting {
		t.Fatalf("initial snapshot = %+v", snap)
	}
	tr.ProbeStarted(behavior.ProbeLiveness)
	tr.RequestSent()
	tr.ProbeFinished(behavior.ProbeLiveness, true)
	tr.ProbeStarted(behavior.ProbeTokenDelta)
	tr.RequestSent()
	snap = AuditLiveSnapshot(9001)
	if snap.Sent != 2 || snap.Probes[0].Status != AuditLiveDone || snap.Probes[1].Status != AuditLiveRunning || snap.Probes[1].Sent != 1 {
		t.Fatalf("mid snapshot = %+v", snap)
	}
	tr.ProbeFinished(behavior.ProbeTokenDelta, false)
	if AuditLiveSnapshot(9001).Probes[1].Status != AuditLiveFailed {
		t.Fatalf("failed probe should read failed")
	}
	// 下一个模型登记后，上一个的 End 不能把它删掉。
	next := AuditLiveStart(9001, 78, "gpt-test-2", "openai-chat", ids)
	AuditLiveEnd(tr)
	if s := AuditLiveSnapshot(9001); s == nil || s.ResultID != 78 {
		t.Fatalf("ending the old tracker removed the new one: %+v", s)
	}
	AuditLiveEnd(next)
	if AuditLiveSnapshot(9001) != nil {
		t.Fatalf("progress should be gone after the run")
	}
}
