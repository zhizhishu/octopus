package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/tokenbank"
)

// tokenizerSender 用库里某个类的计数模拟上游：基线 = overhead，探测 = overhead + counts[j]。
// 计数走 Usage.Input（与 ProbeSender 的口径一致）；proto 为空时不实现 ProtocolNamer。
type tokenizerSender struct {
	t        *testing.T
	bank     *tokenbank.Bank
	overhead int
	counts   []int
	proto    string
	mu       sync.Mutex
	asks     []modelverify.Ask
	failAt   int // 第几次请求失败（1 起）；0 = 不失败
	onlyOld  bool
	baseCall int
	drift    int
}

func (s *tokenizerSender) Ask(_ context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks = append(s.asks, ask)
	if s.failAt > 0 && len(s.asks) == s.failAt {
		return modelverify.Response{}, errors.New("upstream 502")
	}
	tokens := -1
	if ask.Prompt == s.bank.Wrap("") {
		tokens = s.overhead + s.baseCall*s.drift
		s.baseCall++
	} else {
		for j, p := range s.bank.Probes {
			if ask.Prompt == s.bank.Wrap(p.Text) {
				tokens = s.overhead + s.counts[j]
			}
		}
	}
	if tokens < 0 {
		s.t.Fatalf("unexpected prompt %q", ask.Prompt)
	}
	if s.onlyOld {
		return modelverify.Response{Usage: modelverify.Usage{Prompt: tokens}}, nil
	}
	return modelverify.Response{Usage: modelverify.Usage{Input: tokens, Prompt: 1}}, nil
}

type protoTokenizerSender struct{ *tokenizerSender }

func (s protoTokenizerSender) Protocol() string { return s.proto }

func classCounts(t *testing.T, id string) []int {
	t.Helper()
	b, err := tokenbank.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	c, ok := b.Class(id)
	if !ok {
		t.Fatalf("class %s missing", id)
	}
	return c.Counts
}

func newTokenizerSender(t *testing.T, class string) *tokenizerSender {
	b, _ := tokenbank.Builtin()
	return &tokenizerSender{t: t, bank: b, overhead: 23, counts: classCounts(t, class)}
}

func TestTokenizerProbesAreOptIn(t *testing.T) {
	for _, id := range DefaultProbes() {
		if id == ProbeTokenizerID || id == ProbeTokenizerBaseline {
			t.Fatalf("%s must not be a default probe (reference: off by default)", id)
		}
	}
	if !IsKnownProbe(ProbeTokenizerID) || !IsKnownProbe(ProbeTokenizerBaseline) {
		t.Fatal("tokenizer probes are not registered")
	}
}

func TestTokenizerIDMaxTokensOnlyForMessages(t *testing.T) {
	for _, tc := range []struct {
		proto string
		want  bool
	}{{"anthropic-messages", true}, {"openai-chat", false}, {"openai-responses", false}, {"", false}} {
		s := newTokenizerSender(t, "o200k")
		var sender modelverify.Sender = s
		if tc.proto != "" {
			s.proto = tc.proto
			sender = protoTokenizerSender{s}
		}
		res := RunProbe(context.Background(), ProbeTokenizerID, sender)
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		for _, ask := range s.asks {
			got := ask.MaxTokens != nil
			if got != tc.want || (got && *ask.MaxTokens != tokenbank.ProbeOutputTokens) {
				t.Fatalf("proto %q: MaxTokens=%v, want set=%v", tc.proto, ask.MaxTokens, tc.want)
			}
			if ask.Thinking || ask.Temperature != nil || len(ask.Turns) > 0 {
				t.Fatalf("tokenizer probe must not alter other request fields: %+v", ask)
			}
		}
	}
}

func TestTokenizerIDConsistentClaimProducesNoFinding(t *testing.T) {
	s := newTokenizerSender(t, "o200k")
	res := runProbe(context.Background(), ProbeTokenizerID, s, "gpt-5.5")
	if res.Err != nil || !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if res.Data["kind"] != "exact" || res.Data["top_class"] != "o200k" || res.Data["claim_status"] != "consistent" {
		t.Fatalf("data = %+v", res.Data)
	}
	if !probeCountsAsUsable(res) {
		t.Fatal("complete run should count as usable")
	}
	if got := findingsFor(res, "gpt-5.5", nil); len(got) != 0 {
		t.Fatalf("consistent claim must not produce findings: %+v", got)
	}
	n := res.Data["requests"].(int)
	if n < tokenbank.Model.MinimumProbes+2 || n > tokenbank.Model.MaximumProbes+2 {
		t.Fatalf("requests = %d outside [%d,%d]", n, tokenbank.Model.MinimumProbes+2, tokenbank.Model.MaximumProbes+2)
	}
}

func TestTokenizerIDInconsistentClaimScores25(t *testing.T) {
	s := newTokenizerSender(t, "qwen3.5")
	res := runProbe(context.Background(), ProbeTokenizerID, s, "gpt-5.5")
	if res.Data["claim_status"] != "inconsistent" {
		t.Fatalf("data = %+v", res.Data)
	}
	got := findingsFor(res, "gpt-5.5", nil)
	// 25 写死：与它取代的 token_delta 分词器不符项同分（tokenizer_score.go 的取值理由）。
	if len(got) != 1 || got[0].Score != 25 || got[0].Severity != SeverityMedium {
		t.Fatalf("findings = %+v", got)
	}
	if !strings.Contains(got[0].Title, "Qwen3.5") {
		t.Fatalf("title should name the measured tokenizer: %q", got[0].Title)
	}
	if strings.Contains(got[0].Recommendation, "tiktoken") {
		t.Fatalf("relay-estimate hint only applies to o200k/cl100k: %q", got[0].Recommendation)
	}
}

func TestTokenizerIDRelayEstimateHint(t *testing.T) {
	// 声明 claude（厂商未公开分词器），测得 o200k：参考实现提示可能是中转站本地估算。
	s := newTokenizerSender(t, "o200k")
	res := runProbe(context.Background(), ProbeTokenizerID, s, "claude-opus-5")
	if res.Data["claim_status"] != "inconsistent" || res.Data["relay_estimate_possible"] != true {
		t.Fatalf("data = %+v", res.Data)
	}
	got := findingsFor(res, "claude-opus-5", nil)
	if len(got) != 1 || got[0].Score != tokenizerMismatchScore || !strings.Contains(got[0].Recommendation, "tiktoken") {
		t.Fatalf("findings = %+v", got)
	}
}

func TestTokenizerIDDriftIsNotScored(t *testing.T) {
	s := newTokenizerSender(t, "qwen3.5")
	s.drift = 2
	res := runProbe(context.Background(), ProbeTokenizerID, s, "gpt-5.5")
	if res.Data["baseline_drift"] != true || res.Data["usable"] != false {
		t.Fatalf("data = %+v", res.Data)
	}
	if probeCountsAsUsable(res) {
		t.Fatal("drifting run must not count as usable")
	}
	got := findingsFor(res, "gpt-5.5", nil)
	if len(got) != 1 || got[0].Score != 0 || !strings.Contains(got[0].Title, "基线") {
		t.Fatalf("drift should only produce a 0-score note: %+v", got)
	}
}

func TestTokenizerIDPartialRunIsNotScored(t *testing.T) {
	s := newTokenizerSender(t, "qwen3.5")
	s.failAt = 4
	res := runProbe(context.Background(), ProbeTokenizerID, s, "gpt-5.5")
	if res.Err != nil || !res.OK || res.Data["complete"] != false || res.Data["usable"] != false {
		t.Fatalf("res = %+v", res)
	}
	for _, f := range findingsFor(res, "gpt-5.5", nil) {
		if f.Score != 0 {
			t.Fatalf("partial run must not score: %+v", f)
		}
	}
}

func TestTokenizerIDNoAnswerIsAnError(t *testing.T) {
	s := newTokenizerSender(t, "qwen3.5")
	s.failAt = 1
	res := runProbe(context.Background(), ProbeTokenizerID, s, "gpt-5.5")
	if res.Err == nil || res.OK {
		t.Fatalf("first-request failure must be a probe error, got %+v", res)
	}
	rep := BuildReport([]Result{res}, "gpt-5.5")
	if rep.Score != 0 || len(rep.Errors) != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestTokenizerIDFallsBackToPromptUsage(t *testing.T) {
	s := newTokenizerSender(t, "o200k")
	s.onlyOld = true
	res := runProbe(context.Background(), ProbeTokenizerID, s, "gpt-5.5")
	if res.Data["claim_status"] != "consistent" {
		t.Fatalf("Prompt-only sender should still work: %+v", res.Data)
	}
}

func TestTokenizerIDSupersedesTokenDelta(t *testing.T) {
	tokenizer := runProbe(context.Background(), ProbeTokenizerID, newTokenizerSender(t, "qwen3.5"), "gpt-5.5")
	delta := Result{ProbeID: ProbeTokenDelta, OK: true, Data: map[string]any{
		"method": "differential", "usable": true, "delta": 400,
	}}
	if len(findingsFor(delta, "gpt-5.5", nil)) == 0 {
		t.Fatal("precondition: this token_delta result scores on its own")
	}
	alone := BuildReport([]Result{delta}, "gpt-5.5")
	both := BuildReport([]Result{tokenizer, delta}, "gpt-5.5")
	for _, f := range both.Findings {
		if f.Probe == ProbeTokenDelta {
			t.Fatalf("token_delta must not score when tokenizer_id is usable: %+v", both.Findings)
		}
	}
	if both.Score != tokenizerMismatchScore || alone.Score == 0 {
		t.Fatalf("scores alone=%d both=%d", alone.Score, both.Score)
	}
	// tokenizer_id 不可用（漂移）时，token_delta 照常计分。
	s := newTokenizerSender(t, "qwen3.5")
	s.drift = 1
	drifted := runProbe(context.Background(), ProbeTokenizerID, s, "gpt-5.5")
	mixed := BuildReport([]Result{drifted, delta}, "gpt-5.5")
	if mixed.Score != alone.Score {
		t.Fatalf("unusable tokenizer_id must not suppress token_delta: %d vs %d", mixed.Score, alone.Score)
	}
}

func TestTokenizerBaselineRecordsDifferentialCounts(t *testing.T) {
	s := newTokenizerSender(t, "glm-5")
	res := runProbe(context.Background(), ProbeTokenizerBaseline, s, "claude-opus-5")
	if res.Err != nil || !res.OK || res.Data["recordable"] != true {
		t.Fatalf("res = %+v", res)
	}
	counts := res.Data["counts"].(map[string]int)
	b, _ := tokenbank.Builtin()
	want := classCounts(t, "glm-5")
	for j, p := range b.Probes {
		if counts[p.ID] != want[j] {
			t.Fatalf("count %s = %d, want %d", p.ID, counts[p.ID], want[j])
		}
	}
	if len(s.asks) != len(b.Probes)+2 {
		t.Fatalf("sweep sent %d requests, want %d", len(s.asks), len(b.Probes)+2)
	}
	if probeCountsAsUsable(res) || len(findingsFor(res, "claude-opus-5", nil)) != 0 {
		t.Fatal("recording is not a check: no findings, not usable")
	}
	drift := newTokenizerSender(t, "glm-5")
	drift.drift = 1
	bad := runProbe(context.Background(), ProbeTokenizerBaseline, drift, "claude-opus-5")
	if bad.Data["recordable"] != false || bad.Data["reason"] == "" {
		t.Fatalf("drifting recording must be refused: %+v", bad.Data)
	}
}

func TestTokenizerIDUsesLocalBaselines(t *testing.T) {
	b, _ := tokenbank.Builtin()
	// 测试专用的合成计数（不是任何真实模型的数据）：在 glm-5 上逐题 +3，保证与库内所有类都不同。
	synthetic := append([]int(nil), classCounts(t, "glm-5")...)
	for j := range synthetic {
		synthetic[j] += 3
	}
	rec := newTokenizerSender(t, "glm-5")
	rec.counts = synthetic
	recorded := runProbe(context.Background(), ProbeTokenizerBaseline, rec, "claude-x")
	counts := recorded.Data["counts"].(map[string]int)
	SetTokenizerBaselineSource(func(context.Context) ([]tokenbank.LocalBaseline, error) {
		return []tokenbank.LocalBaseline{
			{Model: "claude-x", Counts: counts, BankGeneratedAt: b.GeneratedAt},
			{Model: "claude-stale", Counts: counts, BankGeneratedAt: "1999-01-01"},
		}, nil
	})
	t.Cleanup(func() { SetTokenizerBaselineSource(nil) })

	same := newTokenizerSender(t, "glm-5")
	same.counts = synthetic
	res := runProbe(context.Background(), ProbeTokenizerID, same, "claude-x")
	if res.Data["claim_status"] != "consistent" || res.Data["top_local"] != true || res.Data["claim_first_party"] != true {
		t.Fatalf("local baseline not used: %+v", res.Data)
	}
	warnings, _ := res.Data["warnings"].([]string)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "claude-stale") {
		t.Fatalf("stale baseline should be skipped with a warning: %v", warnings)
	}
	if res.Data["bank_classes"] != len(b.Classes)+1 {
		t.Fatalf("bank_classes = %v", res.Data["bank_classes"])
	}
	// 冒充：声明 claude-x，实际是 qwen3.5 → 不一致并计分。
	fake := runProbe(context.Background(), ProbeTokenizerID, newTokenizerSender(t, "qwen3.5"), "claude-x")
	if fake.Data["claim_status"] != "inconsistent" {
		t.Fatalf("substitution against local baseline should be inconsistent: %+v", fake.Data)
	}
	if got := findingsFor(fake, "claude-x", nil); len(got) != 1 || got[0].Score != tokenizerMismatchScore {
		t.Fatalf("findings = %+v", got)
	}
	// 没有本地基线时照参考实现：claude 映射到"未公开分词器"（class null），核对概率取
	// unknown 后验；测得库内已知的开源分词器 → 不一致。
	SetTokenizerBaselineSource(nil)
	plain := runProbe(context.Background(), ProbeTokenizerID, newTokenizerSender(t, "qwen3.5"), "claude-x")
	if plain.Data["claim_status"] != "inconsistent" {
		t.Fatalf("reference semantics: null-class claim vs known tokenizer: %+v", plain.Data)
	}
	// 库里完全没有映射的型号：无从核对，只给 0 分提示。
	unmapped := runProbe(context.Background(), ProbeTokenizerID, newTokenizerSender(t, "qwen3.5"), "mystery-model-7")
	got := findingsFor(unmapped, "mystery-model-7", nil)
	if unmapped.Data["claim_status"] != nil && unmapped.Data["claim_status"] != "" {
		t.Fatalf("unmapped model should have no claim: %+v", unmapped.Data)
	}
	for _, f := range got {
		if f.Score != 0 {
			t.Fatalf("unmapped claim must not score: %+v", f)
		}
	}
}

func TestTokenizerIDFindingsByStatus(t *testing.T) {
	base := func(status string) Result {
		return Result{ProbeID: ProbeTokenizerID, OK: true, Data: map[string]any{
			"usable": true, "claim_status": status, "top_series": "Qwen3.5 and later", "top_lab": "Alibaba Qwen",
		}}
	}
	if got := tokenizerIDFindings(base("consistent")); got != nil {
		t.Fatalf("consistent: %+v", got)
	}
	for _, status := range []string{"uncertain", ""} {
		got := tokenizerIDFindings(base(status))
		if len(got) != 1 || got[0].Score != 0 || got[0].Severity != SeverityLow {
			t.Fatalf("%q: %+v", status, got)
		}
	}
	got := tokenizerIDFindings(base("inconsistent"))
	if len(got) != 1 || got[0].Score != tokenizerMismatchScore || !strings.Contains(got[0].Title, "Qwen3.5 and later（Alibaba Qwen）") {
		t.Fatalf("inconsistent: %+v", got)
	}
	notOK := base("inconsistent")
	notOK.OK = false
	if tokenizerIDFindings(notOK) != nil {
		t.Fatal("failed probe must not produce findings")
	}
	unusable := base("inconsistent")
	unusable.Data["usable"] = false
	if got := tokenizerIDFindings(unusable); len(got) != 1 || got[0].Score != 0 {
		t.Fatalf("unusable: %+v", got)
	}
}
