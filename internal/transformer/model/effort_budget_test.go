package model

import "testing"

// 这套映射以前在四个地方各有一份互不相同的数字，且入站那套产不出 max。
// 这里锁死两条不变式，防止再次漂移。
func TestEffortLadderIsMonotonic(t *testing.T) {
	// 档位必须单调递增。改动前 xhigh 落进 default(8192)，比 high(32768) 小四倍,
	// 「第二强的档位反而最省」—— 这条断言就是为了钉死这个坑。
	ladder := []string{"low", "medium", "high", "xhigh", "max"}
	prev := int64(0)
	for _, e := range ladder {
		b, ok := EffortBudget[e]
		if !ok {
			t.Fatalf("missing ladder entry %q", e)
		}
		if b <= prev {
			t.Fatalf("ladder not monotonic at %q: %d <= %d", e, b, prev)
		}
		prev = b
	}
}

// 预算 → 档位 → 预算必须往返不漂移，否则跨协议一趟强度就变了。
func TestEffortRoundTripThroughBudget(t *testing.T) {
	for _, want := range []string{"low", "medium", "high", "xhigh", "max"} {
		budget := EffortBudgetFor(want)
		got := EffortFromBudget(budget)
		if got != want {
			t.Fatalf("round trip %q -> %d -> %q", want, budget, got)
		}
	}
}

// 未知档位不得被静默降成低档。改动前默认是 8192(medium)，
// 客户端明确要重推理却被降级，且日志里看不出来。
func TestUnknownEffortFallsBackToHigh(t *testing.T) {
	if got := EffortBudgetFor("ultra"); got != EffortBudgetDefault {
		t.Fatalf("unknown effort budget = %d, want %d", got, EffortBudgetDefault)
	}
	if EffortBudgetDefault < EffortBudget["high"] {
		t.Fatalf("default must not be cheaper than high")
	}
}

// 大小写与空白不该绕过映射表，否则 "MAX" 会当成未知档降级。
func TestEffortBudgetNormalizesCaseAndSpace(t *testing.T) {
	for _, in := range []string{"MAX", " max ", "Max"} {
		if got := EffortBudgetFor(in); got != EffortBudget["max"] {
			t.Fatalf("EffortBudgetFor(%q) = %d, want %d", in, got, EffortBudget["max"])
		}
	}
}

// 预算反解要能认出大额预算属于 max —— 这正是改动前做不到的事
// （type=enabled + budget_tokens=100000 也只会得到 high）。
func TestEffortFromBudgetRecognizesMax(t *testing.T) {
	cases := []struct {
		budget int64
		want   string
	}{
		{0, ""},
		{1024, "low"},
		{5000, "medium"},
		{8192, "medium"},
		{20000, "high"},
		{32768, "high"},
		{40000, "xhigh"},
		{64000, "max"},
		{100000, "max"},
	}
	for _, c := range cases {
		if got := EffortFromBudget(c.budget); got != c.want {
			t.Fatalf("EffortFromBudget(%d) = %q, want %q", c.budget, got, c.want)
		}
	}
}
