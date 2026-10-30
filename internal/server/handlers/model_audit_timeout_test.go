package handlers

import (
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/op"
)

// worker 以前把任务里存的超时一律截到 300 秒——入队放大过的预算在这里会被悄悄砍回去，
// 74 次请求的基线录制照样跑到一半被掐。这里守：放大的值原样生效、只按同口径封顶。
func TestAuditRunTimeoutHonoursScaledBudget(t *testing.T) {
	record := []behavior.ProbeID{behavior.ProbeTokenizerBaseline}
	scaled := op.AuditTimeoutFor([]string{string(behavior.ProbeTokenizerBaseline)}, op.AuditMaxTimeoutSeconds)
	if scaled <= op.AuditMaxTimeoutSeconds {
		t.Fatalf("precondition: recording budget %d must exceed %d", scaled, op.AuditMaxTimeoutSeconds)
	}
	if got := auditRunTimeout(scaled, record); got != time.Duration(scaled)*time.Second {
		t.Fatalf("stored scaled timeout was cut: %v", got)
	}
	if got := auditRunTimeout(0, record); got != time.Duration(scaled)*time.Second {
		t.Fatalf("missing stored timeout should default to the scaled budget: %v", got)
	}
	if got := auditRunTimeout(999999, record); got != time.Duration(scaled)*time.Second {
		t.Fatalf("dirty stored value must be capped at the scaled limit: %v", got)
	}
	standard := behavior.DefaultProbes()
	if got := auditRunTimeout(999999, standard); got != time.Duration(op.AuditMaxTimeoutSeconds)*time.Second {
		t.Fatalf("standard probes keep the 300s cap: %v", got)
	}
	if got := auditRunTimeout(120, standard); got != 120*time.Second {
		t.Fatalf("stored value under the cap is used as-is: %v", got)
	}
	// 预算至少覆盖每次请求 10 秒：74 次请求 ≥ 740 秒。
	if scaled < 74*10 {
		t.Fatalf("budget %d too small for 74 requests", scaled)
	}
}
