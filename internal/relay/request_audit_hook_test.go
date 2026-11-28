package relay

import (
	"context"
	"errors"
	"testing"

	"github.com/zhizhishu/octopus/internal/relay/requestaudit"
)

// hook 拼装层的判官逻辑：caller 注入 mock，绝不发真实请求（工单红线）。
// 包内 requestaudit_test.go 已覆盖缓存/阈值/24KB/1h 过期，这里补「nil caller
// fail-open」「命中 block」「错误 fail-open」三条端到端判官路径。

func testCfg() *requestaudit.Config {
	return &requestaudit.Config{
		Enabled:   true,
		ChannelID: 68,
		Rules: []requestaudit.Rule{
			{ID: "r1", Name: "n1", Enabled: true, Instructions: "q", Action: requestaudit.ActionBlock, Threshold: 0.8},
		},
	}
}

func TestNilCallerFailsOpen(t *testing.T) {
	out := requestaudit.NewAuditor(nil).Audit(testCfg(), 68, "gpt-6-luna", []byte("x"))
	if out.Audited || out.Blocked {
		t.Fatalf("nil caller must fail open: %+v", out)
	}
}

func TestHighProbabilityBlocks(t *testing.T) {
	a := requestaudit.NewAuditor(func(_ context.Context, sc requestaudit.SampleContext, qs []requestaudit.Question) (map[string]float64, error) {
		if sc.ChannelID != 68 || len(qs) != 1 || qs[0].RuleID != "r1" {
			t.Fatalf("caller got sc=%+v qs=%+v", sc, qs)
		}
		if len(sc.Sample) == 0 || len(sc.Sample) > requestaudit.MaxSampleBytes {
			t.Fatalf("bad sample len %d", len(sc.Sample))
		}
		return map[string]float64{"r1": 0.95}, nil
	})
	out := a.Audit(testCfg(), 68, "gpt-6-luna", []byte("bad content"))
	if !out.Blocked || len(out.Findings) != 1 || out.Findings[0].RuleID != "r1" {
		t.Fatalf("expected block: %+v", out)
	}
}

func TestCallerErrorFailsOpen(t *testing.T) {
	a := requestaudit.NewAuditor(func(_ context.Context, sc requestaudit.SampleContext, qs []requestaudit.Question) (map[string]float64, error) {
		return nil, errors.New("boom")
	})
	out := a.Audit(testCfg(), 68, "gpt-6-luna", []byte("other"))
	if out.Audited || out.Blocked {
		t.Fatalf("fail-open expected: %+v", out)
	}
}
