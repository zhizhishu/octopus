package requestaudit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := ParseConfig(`{"enabled":true,"channel_id":68,"rules":[{"id":"r1","name":"nsfw","instructions":"是否含违规内容"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.ChannelID != 68 {
		t.Fatalf("cfg = %+v", cfg)
	}
	r := cfg.Rules[0]
	if !r.Enabled || r.Action != ActionBlock || r.Threshold != 0.8 {
		t.Fatalf("defaults wrong: %+v", r)
	}
	for _, bad := range []string{
		`{"rules":[{"id":"","instructions":"x"}]}`,
		`{"rules":[{"id":"a","instructions":""}]}`,
		`{"rules":[{"id":"a","instructions":"x","action":"yeet"}]}`,
		`{"rules":[{"id":"a","instructions":"x","threshold":1.5}]}`,
		`{"rules":[{"id":"a","instructions":"x"},{"id":"a","instructions":"y"}]}`,
	} {
		if _, err := ParseConfig(bad); err == nil {
			t.Fatalf("expected error for %s", bad)
		} else if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("want ErrInvalidConfig, got %v", err)
		}
	}
	if cfg, err := ParseConfig(""); err != nil || cfg == nil || cfg.Enabled {
		t.Fatal("empty config should parse to zero config")
	}
}

func TestSampleCapsAt24KB(t *testing.T) {
	if got := Sample(make([]byte, MaxSampleBytes+100)); len(got) != MaxSampleBytes {
		t.Fatalf("len = %d", len(got))
	}
	if got := Sample([]byte("short")); string(got) != "short" {
		t.Fatalf("got %q", got)
	}
}

func TestAuditDisabledOrNoRules(t *testing.T) {
	a := NewAuditor(nil)
	out := a.Audit(context.Background(), &Config{Enabled: false}, 68, "gpt-6-luna", []byte("body"))
	if !out.Audited || out.Blocked {
		t.Fatalf("disabled cfg should pass: %+v", out)
	}
	out = a.Audit(context.Background(), &Config{Enabled: true, Rules: []Rule{{ID: "x", Enabled: false, Instructions: "i", Action: "block", Threshold: 0.8}}}, 68, "", []byte("b"))
	if !out.Audited {
		t.Fatalf("all-disabled rules should pass: %+v", out)
	}
}

func testRules() []Rule {
	return []Rule{
		{ID: "block-rule", Name: "b", Enabled: true, Instructions: "q1", Action: ActionBlock, Threshold: 0.8},
		{ID: "warn-rule", Name: "w", Enabled: true, Instructions: "q2", Action: ActionWarn, Threshold: 0.5},
	}
}

func TestAuditBlockWarnAndBelowThreshold(t *testing.T) {
	caller := func(_ context.Context, sc SampleContext, qs []Question) (map[string]float64, error) {
		if len(qs) != 2 {
			t.Fatalf("questions = %d", len(qs))
		}
		if len(sc.Sample) == 0 {
			t.Fatal("empty sample")
		}
		return map[string]float64{"block-rule": 0.9, "warn-rule": 0.6}, nil
	}
	a := NewAuditor(caller)
	cfg := &Config{Enabled: true, Rules: testRules()}
	out := a.Audit(context.Background(), cfg, 68, "", []byte("payload"))
	if !out.Audited || !out.Blocked || len(out.Findings) != 2 {
		t.Fatalf("out = %+v", out)
	}

	caller2 := func(_ context.Context, sc SampleContext, qs []Question) (map[string]float64, error) {
		return map[string]float64{"block-rule": 0.1}, nil
	}
	a2 := NewAuditor(caller2)
	out2 := a2.Audit(context.Background(), cfg, 68, "", []byte("other-body"))
	if out2.Blocked || len(out2.Findings) != 0 {
		t.Fatalf("below threshold should pass clean: %+v", out2)
	}
}

func TestAuditFailOpenOnError(t *testing.T) {
	a := NewAuditor(func(_ context.Context, sc SampleContext, qs []Question) (map[string]float64, error) {
		return nil, errors.New("timeout")
	})
	out := a.Audit(context.Background(), &Config{Enabled: true, Rules: testRules()}, 68, "", []byte("x"))
	if out.Audited || out.Blocked {
		t.Fatalf("fail-open expected: %+v", out)
	}
	if !strings.Contains(out.Reason, "timeout") {
		t.Fatalf("reason = %q", out.Reason)
	}
}

func TestFingerprintCacheHitSkipsCaller(t *testing.T) {
	calls := 0
	a := NewAuditor(func(_ context.Context, sc SampleContext, qs []Question) (map[string]float64, error) {
		calls++
		return map[string]float64{"block-rule": 0.1}, nil
	})
	cfg := &Config{Enabled: true, Rules: testRules()}
	body := []byte("same body")
	a.Audit(context.Background(), cfg, 68, "", body)
	out := a.Audit(context.Background(), cfg, 68, "", body)
	if calls != 1 {
		t.Fatalf("cache miss: caller ran %d times", calls)
	}
	if !out.CacheHit {
		t.Fatalf("expected cache hit: %+v", out)
	}
}

func TestFingerprintCacheExpiresAfter1h(t *testing.T) {
	calls := 0
	a := NewAuditor(func(_ context.Context, sc SampleContext, qs []Question) (map[string]float64, error) {
		calls++
		return map[string]float64{"block-rule": 0.1}, nil
	})
	now := time.Now()
	a.now = func() time.Time { return now }
	cfg := &Config{Enabled: true, Rules: testRules()}
	body := []byte("b")
	a.Audit(context.Background(), cfg, 68, "", body)
	now = now.Add(61 * time.Minute)
	a.Audit(context.Background(), cfg, 68, "", body)
	if calls != 2 {
		t.Fatalf("cache should expire after 1h: caller ran %d times", calls)
	}
}

func TestJoinDecisionsURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.openai.com":    "https://api.openai.com/v1/decisions",
		"https://api.openai.com/":   "https://api.openai.com/v1/decisions",
		"https://api.openai.com/v1": "https://api.openai.com/v1/decisions",
	} {
		if got := joinDecisionsURL(in); got != want {
			t.Fatalf("%s -> %s want %s", in, got, want)
		}
	}
}
