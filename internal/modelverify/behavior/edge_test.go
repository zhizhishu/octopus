package behavior

import (
	"context"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify"
)

// ---------------------------------------------------------------- xai 家族边界

func TestInferFamiliesGrokAndXai(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"I'm Grok by xAI.", []string{"xai"}},
		{"x-ai grok-4.7", []string{"xai"}},
		{"xai/grok-4.7", []string{"xai"}},
		{"grok-4.7", []string{"xai"}},
		// 中文包围的品牌自称同样按边界命中（设计内的正向样本）。
		{"我是Grok，由xAI训练。", []string{"xai"}},
		// 边界反例：普通词/词内片段不得命中。
		{"myxaiword is not the brand", nil},
		{"still grokking the problem", nil},
	}
	for _, tc := range cases {
		got := InferFamilies(tc.text)
		if len(got) != len(tc.want) {
			t.Fatalf("InferFamilies(%q) = %v, want %v", tc.text, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("InferFamilies(%q) = %v, want %v", tc.text, got, tc.want)
			}
		}
	}
}

// 其它家族保持既有裸子串语义：不受 xai 边界化影响（既有测试已覆盖正向，
// 这里只钉死一个裸子串语义未被波及的样本）。
func TestInferFamiliesOtherFamiliesUnchanged(t *testing.T) {
	// "chatgpt" 里的 "gpt" 命中 openai 是既有裸子串语义，应保持。
	got := InferFamilies("chatgpt")
	if len(got) != 1 || got[0] != "openai" {
		t.Fatalf("InferFamilies(%q) = %v, want [openai]", "chatgpt", got)
	}
}

// ---------------------------------------------------------------- glitch 适用性

// glitchSender 计数 stub：记录 Ask 次数，默认返回一份完整的复述。
type glitchSender struct {
	calls int
}

func (g *glitchSender) Ask(_ context.Context, _ modelverify.Ask) (modelverify.Response, error) {
	g.calls++
	return modelverify.Response{Content: glitchReply(nil), FinishReason: "stop"}, nil
}

// 请求家族有本族签名（deepseek）：正常发题；完整复述无失败项 → 不告警（既有行为）。
func TestGlitchRunsAndFlagsWrongFamilyForDeepSeek(t *testing.T) {
	s := &glitchSender{}
	res := Run(context.Background(), []ProbeID{ProbeGlitch}, s, "deepseek-v3")
	if res[0].Err != nil {
		t.Fatalf("不应报错: %v", res[0].Err)
	}
	if s.calls != 1 {
		t.Fatalf("适用家族应发题一次, 实际 %d", s.calls)
	}
	if findings := BuildReport(res, "deepseek-v3").Findings; len(findings) != 0 {
		t.Fatalf("完整复述不应告警, 得到 %+v", findings)
	}
}

// 请求家族无本族签名（anthropic）：发送前判定不适用，零请求。
func TestGlitchSkipsFamilyWithoutSignature(t *testing.T) {
	s := &glitchSender{}
	res := Run(context.Background(), []ProbeID{ProbeGlitch}, s, "claude-sonnet-4-5")
	if res[0].Err != nil {
		t.Fatalf("不应报错: %v", res[0].Err)
	}
	if s.calls != 0 {
		t.Fatalf("无签名家族不应发题, 实际 %d 次", s.calls)
	}
	if applicable, _ := res[0].Data["applicable"].(bool); applicable {
		t.Fatal("应标记不适用")
	}
	if findings := BuildReport(res, "claude-sonnet-4-5").Findings; len(findings) != 0 {
		t.Fatalf("不适用不应产生风险项, 得到 %+v", findings)
	}
}

// 型号元数据未知：零请求。
func TestGlitchSkipsUnknownModel(t *testing.T) {
	s := &glitchSender{}
	res := Run(context.Background(), []ProbeID{ProbeGlitch}, s, "totally-unknown-model")
	if s.calls != 0 {
		t.Fatalf("未知型号不应发题, 实际 %d 次", s.calls)
	}
	if applicable, _ := res[0].Data["applicable"].(bool); applicable {
		t.Fatal("应标记不适用")
	}
}

// 型号元数据缺失（含 Sender 未实现 ModelNamer）：同样零请求，reason=missing_model。
func TestGlitchSkipsMissingModelMetadata(t *testing.T) {
	s := &glitchSender{} // 未实现 ModelNamer
	res := Run(context.Background(), []ProbeID{ProbeGlitch}, s)
	if s.calls != 0 {
		t.Fatalf("缺型号元数据不应发题, 实际 %d 次", s.calls)
	}
	if applicable, _ := res[0].Data["applicable"].(bool); applicable {
		t.Fatal("应标记不适用")
	}
	if reason := stringOf(res[0].Data, "not_applicable_reason"); reason != "missing_model" {
		t.Fatalf("reason = %q, 应为 missing_model", reason)
	}
}

// 已有签名家族错族仍告警：请求 deepseek，错法特征指向 moonshot → 15 分弱信号。
func TestGlitchWrongFamilyWithSignatureStillFlags(t *testing.T) {
	// moonshot 签名是 {5, 11, 12}；全部复述错这三位。
	text := glitchReply(map[int]string{5: "???", 11: "???", 12: "???"})
	obs := observeGlitch(text, "stop")
	res := Result{ProbeID: ProbeGlitch, OK: obs.Analyzable, Data: map[string]any{
		"candidates": MatchGlitchFamilies(obs.FailedIdx),
	}}
	findings := BuildReport([]Result{res}, "deepseek-v3").Findings
	if len(findings) != 1 || findings[0].Score != 15 {
		t.Fatalf("已有签名家族的错族仍应告警 15 分, 得到 %+v", findings)
	}
}

// score 层防御：请求 anthropic（无 glitch 签名）时，即便结果里带了别的家族
// 候选也不告警。
func TestGlitchScoreDefendsNoSignatureFamily(t *testing.T) {
	res := Result{ProbeID: ProbeGlitch, OK: true, Data: map[string]any{
		"candidates": MatchGlitchFamilies([]int{5, 11, 12}),
	}}
	if findings := BuildReport([]Result{res}, "claude-sonnet-4-5").Findings; len(findings) != 0 {
		t.Fatalf("无签名家族不得告警, 得到 %+v", findings)
	}
}

// ---------------------------------------------------------------- signature 覆盖率

// 带型号元数据的不适用不计 usable：单独一条时结论必须是 unknown 不是 none。
func TestSignatureNotApplicableDoesNotFakeNone(t *testing.T) {
	notApplicable := Result{ProbeID: ProbeSignature, OK: true, Data: map[string]any{
		"applicable": false, "protocol": "openai-chat",
	}}
	report := BuildReport([]Result{notApplicable}, "gpt-5")
	if report.Verdict != VerdictUnknown {
		t.Fatalf("全不适用时不得判 none, 得到 %q", report.Verdict)
	}
}

// 恰好阈值边界：minUsableProbes=3。两条干净 + 一条无签名（不计 usable）
// → 只有 2 条有效证据，必须 unknown；补一条干净凑到 3 条 → 按原规则判 none。
// 一条不足不得把多条有效证据全部否掉。
func TestInsufficientEvidenceThresholdBoundary(t *testing.T) {
	clean := func(id ProbeID) Result {
		return Result{ProbeID: id, OK: true, Data: map[string]any{}}
	}
	insufficient := Result{ProbeID: ProbeSignature, OK: true, Data: map[string]any{
		"applicable": true, "signature_present": false, "insufficient_evidence": true,
	}}

	below := BuildReport([]Result{clean(ProbeLiveness), clean(ProbeEchoRewrite), insufficient}, "claude-sonnet-4-5")
	if below.Verdict != VerdictUnknown {
		t.Fatalf("两条干净+一条不足 = 2 条有效证据, 应判 unknown, 得到 %q", below.Verdict)
	}

	atThreshold := BuildReport([]Result{
		clean(ProbeLiveness), clean(ProbeEchoRewrite), clean(ProbeContextCanary), insufficient,
	}, "claude-sonnet-4-5")
	if atThreshold.Verdict != VerdictNone {
		t.Fatalf("三条干净探针已到阈值, 应判 none（不足不否掉全部）, 得到 %q", atThreshold.Verdict)
	}
}

// 缺 signature_present 键的旧形态 Result 不新造证据：仍按原样计入 usable。
func TestLegacyResultWithoutSignaturePresentStillUsable(t *testing.T) {
	clean := func(id ProbeID) Result {
		return Result{ProbeID: id, OK: true, Data: map[string]any{}}
	}
	legacy := Result{ProbeID: ProbeSignature, OK: true, Data: map[string]any{}}
	report := BuildReport([]Result{clean(ProbeLiveness), clean(ProbeEchoRewrite), legacy}, "claude-sonnet-4-5")
	if report.Verdict != VerdictNone {
		t.Fatalf("旧形态空 Data 仍计 usable, 三条应判 none, 得到 %q", report.Verdict)
	}
}

// ---------------------------------------------------------------- Run 取消传播

// ctx 已取消时 Run 不应对任何探针发题（fake-count 验证零 Ask）。
func TestRunWithCanceledContextNeverAsks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &glitchSender{}
	results := Run(ctx, []ProbeID{ProbeGlitch, ProbeLiveness, ProbeIdentity}, s, "deepseek-v3")
	if s.calls != 0 {
		t.Fatalf("取消后不应发题, 实际 %d 次", s.calls)
	}
	if len(results) != 3 {
		t.Fatalf("应返回 3 项, 得到 %d", len(results))
	}
	for _, res := range results {
		if res.Err == nil {
			t.Fatalf("取消后每项应带 Err: %+v", res)
		}
	}
}
