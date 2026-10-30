package tokenbank

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

func mustBuiltin(t *testing.T) *Bank {
	t.Helper()
	b, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBuiltinBankShape(t *testing.T) {
	b := mustBuiltin(t)
	if b.Schema != Schema || len(b.Probes) != 72 || len(b.Classes) != 62 || len(b.APIModels) != 8 {
		t.Fatalf("bank = %s probes=%d classes=%d api=%d", b.Schema, len(b.Probes), len(b.Classes), len(b.APIModels))
	}
	if b.GeneratedAt != "2026-09-30" || b.Source.Tokenizers != 290 {
		t.Fatalf("bank provenance = %s / %d", b.GeneratedAt, b.Source.Tokenizers)
	}
	// 前后缀必须以非空白开头/结尾：出站链路会 TrimSpace 题面，参考实现正是为此这样设计。
	w := b.Wrap("")
	if strings.TrimSpace(w) != w || strings.TrimSpace(b.Wrap("x")) != b.Wrap("x") {
		t.Fatalf("wrapper is not trim-stable: %q", w)
	}
}

func TestParseRejectsBrokenBanks(t *testing.T) {
	b := mustBuiltin(t)
	base := func() map[string]any {
		raw, _ := json.Marshal(b)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m
	}
	cases := map[string]func(m map[string]any){
		"schema": func(m map[string]any) { m["schema"] = "tokenizer-bank-v0" },
		"short counts": func(m map[string]any) {
			c := m["classes"].([]any)[0].(map[string]any)
			c["counts"] = c["counts"].([]any)[:3]
		},
		"dup class": func(m map[string]any) {
			cs := m["classes"].([]any)
			cs[1].(map[string]any)["id"] = cs[0].(map[string]any)["id"]
		},
		"dangling api class": func(m map[string]any) {
			m["api_models"].([]any)[2].(map[string]any)["class"] = "nope"
		},
		"bad regex": func(m map[string]any) {
			m["api_models"].([]any)[0].(map[string]any)["pattern"] = "^(claude"
		},
		"bad alt key": func(m map[string]any) {
			m["classes"].([]any)[0].(map[string]any)["alternatives"] = map[string]any{"999": []any{1}}
		},
	}
	for name, mutate := range cases {
		m := base()
		mutate(m)
		raw, _ := json.Marshal(m)
		if _, err := Parse(raw); err == nil {
			t.Errorf("%s: Parse accepted a broken bank", name)
		}
	}
	raw, _ := json.Marshal(base())
	if _, err := Parse(raw); err != nil {
		t.Fatalf("round-tripped bank rejected: %v", err)
	}
}

func TestExpectedClassesMapping(t *testing.T) {
	b := mustBuiltin(t)
	cases := []struct {
		model      string
		expected   string // 逗号分隔；"-" 表示 nil
		firstParty bool
	}{
		{"gpt-5.5", "o200k", true},
		{"openai/gpt-6.1-terra", "o200k", true},
		{"gpt-4-turbo", "cl100k", true},
		{"claude-opus-5", "", true}, // 厂商未公开：expected 为空
		{"Anthropic/Claude-Sonnet-5:beta", "", true},
		{"grok-5", "", true},
		{"gemini-3-pro", "gemma3", true},
		{"deepseek-v4-pro", "deepseek-v3", false},
		{"deepseek-v2-lite", "deepseek-v2", false},
		// (?!\d) 前瞻：v25 不是 v2，regexp2 ECMAScript 模式必须支持
		{"deepseek-v25", "-", false},
		{"glm-4.7-flash", "glm-5", false},
		{"glm-4.6", "glm-4", false},
		{"totally-unknown-model", "-", false},
		{"", "-", false},
	}
	for _, c := range cases {
		got := b.ExpectedClasses(c.model)
		if c.expected == "-" {
			if got != nil {
				t.Errorf("%q: expected nil, got %+v", c.model, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%q: got nil", c.model)
			continue
		}
		if strings.Join(got.Expected, ",") != c.expected || got.FirstParty != c.firstParty {
			t.Errorf("%q: got %v firstParty=%v, want %s firstParty=%v", c.model, got.Expected, got.FirstParty, c.expected, c.firstParty)
		}
	}
}

func TestNormalizeModel(t *testing.T) {
	for in, want := range map[string]string{
		"  OpenAI/GPT-5 ":            "gpt-5",
		"a/b/c/qwen3.5-plus:free":    "qwen3.5-plus",
		"claude-opus-5":              "claude-opus-5",
		"meta-llama/llama-3.1:8b:q4": "llama-3.1",
	} {
		if got := NormalizeModel(in); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// tableAsk 用一张"探测 id → 绝对计数"的表模拟上游。
func tableAsk(b *Bank, overhead int, counts func(j int) int) AskFunc {
	byPrompt := map[string]int{}
	for j, p := range b.Probes {
		byPrompt[b.Wrap(p.Text)] = j
	}
	return func(_ context.Context, prompt string) (int, string, error) {
		if prompt == b.Wrap("") {
			return overhead, "m", nil
		}
		return overhead + counts(byPrompt[prompt]), "m", nil
	}
}

func TestProbeNoUsageIsAnErrorNotAVerdict(t *testing.T) {
	b := mustBuiltin(t)
	run := b.Probe(context.Background(), func(context.Context, string) (int, string, error) { return 0, "", nil }, "gpt-5", 0)
	if !errors.Is(run.Err, ErrNoUsage) {
		t.Fatalf("err = %v, want ErrNoUsage", run.Err)
	}
	if run.Verdict != nil || len(run.Observations) != 0 {
		t.Fatalf("no-usage run produced a verdict/observations: %+v", run)
	}
}

func TestProbeStopsAtFirstFailure(t *testing.T) {
	b := mustBuiltin(t)
	calls := 0
	boom := errors.New("upstream 502")
	run := b.Probe(context.Background(), func(_ context.Context, prompt string) (int, string, error) {
		calls++
		if calls == 3 {
			return 0, "", boom
		}
		return 20 + len(prompt)%7, "m", nil
	}, "gpt-5", 0)
	if !errors.Is(run.Err, boom) || calls != 3 || len(run.Steps) != 3 || run.Steps[2].State != "failed" {
		t.Fatalf("err=%v calls=%d steps=%+v", run.Err, calls, run.Steps)
	}
	// 参考实现：已答复的探测仍给出判定（标记了错误，由上层决定是否计分）。
	if run.Verdict == nil || run.Posterior.Answered != 1 {
		t.Fatalf("partial run should still carry a verdict over 1 answered probe")
	}
}

func TestProbeRespectsMaximum(t *testing.T) {
	b := mustBuiltin(t)
	rng := rand.New(rand.NewPCG(1, 2))
	alien := make([]int, len(b.Probes))
	for j := range alien {
		alien[j] = 3 + rng.IntN(60)
	}
	run := b.Probe(context.Background(), tableAsk(b, 10, func(j int) int { return alien[j] }), "", 5)
	if run.Err != nil {
		t.Fatal(run.Err)
	}
	if run.Posterior.Answered > 5 || len(run.Steps) > 5+2 {
		t.Fatalf("answered=%d steps=%d, maximum 5", run.Posterior.Answered, len(run.Steps))
	}
}

func TestSweepAndLocalBaselineRoundTrip(t *testing.T) {
	b := mustBuiltin(t)
	// 测试夹具：一个"闭源分词器"的计数——随机生成，只存在于本测试，不是任何真实型号的数据。
	rng := rand.New(rand.NewPCG(7, 9))
	closed := make([]int, len(b.Probes))
	for j := range closed {
		closed[j] = 2 + rng.IntN(70)
	}
	sweep := b.Sweep(context.Background(), tableAsk(b, 37, func(j int) int { return closed[j] }))
	if sweep.Err != nil || sweep.BaselineDrift || len(sweep.Steps) != len(b.Probes)+2 {
		t.Fatalf("sweep err=%v drift=%v steps=%d", sweep.Err, sweep.BaselineDrift, len(sweep.Steps))
	}
	counts, err := b.DifferentialCounts(sweep)
	if err != nil {
		t.Fatal(err)
	}
	for j, p := range b.Probes {
		if counts[p.ID] != closed[j] {
			t.Fatalf("differential %s = %d, want %d (overhead must cancel)", p.ID, counts[p.ID], closed[j])
		}
	}

	// 没有本地基线：claude 声明只能按"库外 = 相容"核对（参考实现行为）。
	plain := b.Probe(context.Background(), tableAsk(b, 51, func(j int) int { return closed[j] }), "claude-opus-5", 0)
	if plain.Verdict.Claim == nil || len(plain.Verdict.Claim.Expected) != 0 {
		t.Fatalf("without local baseline claude claim should map to null class: %+v", plain.Verdict.Claim)
	}

	merged, err := b.WithLocal([]LocalBaseline{{Model: "claude-opus-5", Label: "Claude Opus 5（可信渠道）", Counts: counts, BankGeneratedAt: b.GeneratedAt}})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Classes) != 62 {
		t.Fatalf("WithLocal mutated the builtin bank")
	}
	// 同一闭源分词器、不同隐藏开销：本地类精确吻合，声明一致。
	same := merged.Probe(context.Background(), tableAsk(merged, 51, func(j int) int { return closed[j] }), "anthropic/claude-opus-5", 0)
	if same.Verdict.Kind != KindExact || same.Verdict.Top.ID != LocalClassID("claude-opus-5") || same.Verdict.Claim.Status != ClaimConsistent {
		t.Fatalf("same tokenizer: verdict=%+v claim=%+v", same.Verdict, same.Verdict.Claim)
	}
	// 冒充：声明 claude-opus-5，实际是库里的 qwen3.5 分词器 → 不一致。
	qwen, _ := merged.Class("qwen3.5")
	fake := merged.Probe(context.Background(), tableAsk(merged, 51, func(j int) int { return qwen.Counts[j] }), "claude-opus-5", 0)
	if fake.Verdict.Claim.Status != ClaimInconsistent {
		t.Fatalf("substituted tokenizer should be inconsistent, got %+v", fake.Verdict.Claim)
	}
}

func TestWithLocalRejectsUnsafeBaselines(t *testing.T) {
	b := mustBuiltin(t)
	full := map[string]int{}
	for _, p := range b.Probes {
		full[p.ID] = 5
	}
	partial := map[string]int{b.Probes[0].ID: 5}
	cases := map[string][]LocalBaseline{
		"stale bank":   {{Model: "claude-x", Counts: full, BankGeneratedAt: "2020-01-01"}},
		"partial":      {{Model: "claude-x", Counts: partial, BankGeneratedAt: b.GeneratedAt}},
		"empty model":  {{Model: "  ", Counts: full, BankGeneratedAt: b.GeneratedAt}},
		"duplicate id": {{Model: "claude-x", Counts: full, BankGeneratedAt: b.GeneratedAt}, {Model: "Vendor/claude-x", Counts: full, BankGeneratedAt: b.GeneratedAt}},
	}
	for name, locals := range cases {
		if _, err := b.WithLocal(locals); err == nil {
			t.Errorf("%s: WithLocal accepted an unsafe baseline", name)
		}
	}
	same, err := b.WithLocal(nil)
	if err != nil || same != b {
		t.Fatalf("WithLocal(nil) should return the bank itself")
	}
}

func TestDifferentialCountsRejectsIncompleteRuns(t *testing.T) {
	b := mustBuiltin(t)
	ok := b.Sweep(context.Background(), tableAsk(b, 10, func(j int) int { return j + 1 }))
	if _, err := b.DifferentialCounts(ok); err != nil {
		t.Fatalf("complete sweep rejected: %v", err)
	}
	drift := ok
	drift.BaselineDrift = true
	if _, err := b.DifferentialCounts(drift); err == nil {
		t.Error("drifting sweep accepted")
	}
	short := ok
	short.Observations = ok.Observations[:len(ok.Observations)-3]
	if _, err := b.DifferentialCounts(short); err == nil {
		t.Error("incomplete sweep accepted")
	}
	failed := ok
	failed.Err = errors.New("x")
	if _, err := b.DifferentialCounts(failed); err == nil {
		t.Error("failed sweep accepted")
	}
	calls := 0
	drifting := b.Sweep(context.Background(), func(_ context.Context, prompt string) (int, string, error) {
		calls++
		if prompt == b.Wrap("") {
			return 10 + calls, "m", nil
		}
		return 30, "m", nil
	})
	if !drifting.BaselineDrift {
		t.Error("Sweep did not flag differing baselines")
	}
}
