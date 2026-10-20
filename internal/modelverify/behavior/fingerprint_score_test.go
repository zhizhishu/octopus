package behavior

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/fingerprint"
)

// 指纹判定层的测试。
//
// 重点不在"能不能算出一个排名"（那是 fingerprint 包的黄金测试管的），
// 而在**判定的克制**：证据不足时不下结论、对得上时不吹成正品证明、
// 只有"连家族都不对且分差明确"才计风险分。

// fingerprintResult 造一个带已发布结论的探针结果。
func fingerprintResult(decision, prediction, family, familyName string, probability *float64, candidates ...fingerprint.Candidate) Result {
	top := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		entry := map[string]any{
			"model":        c.Model,
			"display_name": c.DisplayName,
			"family":       c.Family,
			"family_name":  c.FamilyName,
			"score":        c.Score,
		}
		if c.Probability != nil {
			entry["probability"] = *c.Probability
		}
		top = append(top, entry)
	}
	data := map[string]any{
		"fingerprint_decision":        decision,
		"fingerprint_prediction":      prediction,
		"fingerprint_prediction_name": "display-" + prediction,
		"fingerprint_family":          family,
		"fingerprint_family_name":     familyName,
		"fingerprint_candidates":      top,
	}
	if probability != nil {
		data["fingerprint_probability"] = *probability
	}
	return Result{ProbeID: ProbeFingerprint, OK: true, Data: data}
}

// TestFingerprintMismatchIsFlagged 是核心用例：声明与指纹不同族且分差明确 → 计风险分。
func TestFingerprintMismatchIsFlagged(t *testing.T) {
	p := 0.94
	res := fingerprintResult("scorable", "deepseek-v4-pro", "deepseek", "DeepSeek", &p,
		fingerprint.Candidate{Model: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", Family: "deepseek", FamilyName: "DeepSeek", Score: 4.8},
		fingerprint.Candidate{Model: "glm-5.3", DisplayName: "GLM 5.3", Family: "glm", FamilyName: "GLM", Score: 3.1},
	)
	findings := fingerprintFindings(res, "claude-opus-4-6")
	if len(findings) != 1 {
		t.Fatalf("应有 1 条判定，实际 %d", len(findings))
	}
	if findings[0].Score != fingerprintMismatchScore {
		t.Fatalf("不同族且分差明确应计 %d 分，实际 %d", fingerprintMismatchScore, findings[0].Score)
	}
	if !strings.Contains(findings[0].Title, "不符") {
		t.Fatalf("提示语应说明不符，实际: %s", findings[0].Title)
	}
}

// TestFingerprintMatchIsNotACertificate 是最重要的一条约束：
// 对上了只能说"没有矛盾"，绝不能写成正品证明，分数必须是 0。
func TestFingerprintMatchIsNotACertificate(t *testing.T) {
	p := 0.97
	res := fingerprintResult("scorable", "claude-opus-4.6", "anthropic", "Claude", &p,
		fingerprint.Candidate{Model: "claude-opus-4.6", DisplayName: "Claude Opus 4.6", Family: "anthropic", FamilyName: "Claude", Score: 4.9},
		fingerprint.Candidate{Model: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", Family: "anthropic", FamilyName: "Claude", Score: 3.2},
	)
	findings := fingerprintFindings(res, "claude-opus-4-6")
	if len(findings) != 1 {
		t.Fatalf("应有 1 条判定，实际 %d", len(findings))
	}
	if findings[0].Score != 0 {
		t.Fatalf("指纹吻合不应计风险分，实际 %d", findings[0].Score)
	}
	title := findings[0].Title
	if !strings.Contains(title, "不能作为正品证明") {
		t.Fatalf("提示语必须写明不能作为正品证明，实际: %s", title)
	}
	// 反向检查：不能出现"确认/证明是正品"这类过度断言。
	for _, banned := range []string{"确认是", "证明是正品", "确定为正品"} {
		if strings.Contains(title, banned) {
			t.Fatalf("提示语出现过度断言 %q: %s", banned, title)
		}
	}
}

// TestFingerprintSmallGapDoesNotConviction：分差小说明榜首不稳，不能定罪。
func TestFingerprintSmallGapDoesNotConviction(t *testing.T) {
	p := 0.4
	res := fingerprintResult("scorable", "deepseek-v4-pro", "deepseek", "DeepSeek", &p,
		fingerprint.Candidate{Model: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", Family: "deepseek", FamilyName: "DeepSeek", Score: 4.00},
		fingerprint.Candidate{Model: "glm-5.3", DisplayName: "GLM 5.3", Family: "glm", FamilyName: "GLM", Score: 3.95},
	)
	findings := fingerprintFindings(res, "claude-opus-4-6")
	if len(findings) != 1 {
		t.Fatalf("应有 1 条判定，实际 %d", len(findings))
	}
	if findings[0].Score != 0 {
		t.Fatalf("分差过小不应计风险分，实际 %d", findings[0].Score)
	}
	if !strings.Contains(findings[0].Title, "难以区分") {
		t.Fatalf("应说明难以区分，实际: %s", findings[0].Title)
	}
}

// TestFingerprintUnknownDeclaredModelSaysIncomparable：
// 声明型号认不出家族时没有可比对象：既不能说"一致"，也不能说"不符"，
// 必须如实说"无法比较"。曾经这里默认走"一致"分支，等于给一个根本没比较过的
// 结论盖了合格章。
func TestFingerprintUnknownDeclaredModelSaysIncomparable(t *testing.T) {
	p := 0.9
	res := fingerprintResult("scorable", "deepseek-v4-pro", "deepseek", "DeepSeek", &p,
		fingerprint.Candidate{Model: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", Family: "deepseek", FamilyName: "DeepSeek", Score: 4.8},
		fingerprint.Candidate{Model: "glm-5.3", DisplayName: "GLM 5.3", Family: "glm", FamilyName: "GLM", Score: 3.0},
	)
	findings := fingerprintFindings(res, "my-custom-alias")
	if len(findings) != 1 {
		t.Fatalf("应有 1 条判定，实际 %d", len(findings))
	}
	if findings[0].Score != 0 {
		t.Fatalf("认不出声明家族时不应计风险分，实际 %d", findings[0].Score)
	}
	if !strings.Contains(findings[0].Title, "无法比较") {
		t.Fatalf("应明确说无法比较，实际: %s", findings[0].Title)
	}
	if strings.Contains(findings[0].Title, "一致") {
		t.Fatalf("没比较过就不能说一致，实际: %s", findings[0].Title)
	}
}

// TestFingerprintInsufficientEvidenceIsNotEvidence：
// 证据不足时必须 0 分，且不能被算成"跑通"。
func TestFingerprintInsufficientEvidenceIsNotEvidence(t *testing.T) {
	// unscorable / partial 要产一条"不可判"提示并明确说明原因；
	// 空 decision 表示探针压根没给出结论，此时连提示都不该编。
	for _, decision := range []string{"unscorable", "partial", ""} {
		res := fingerprintResult(decision, "", "", "", nil)
		findings := fingerprintFindings(res, "claude-opus-4-6")
		if decision == "" {
			if findings != nil {
				t.Fatalf("没有结论时不应产判定，实际 %+v", findings)
			}
		} else {
			if len(findings) != 1 {
				t.Fatalf("%q: 应有 1 条判定，实际 %d", decision, len(findings))
			}
			if findings[0].Score != 0 {
				t.Fatalf("%q: 证据不足不应计风险分，实际 %d", decision, findings[0].Score)
			}
		}
		report := BuildReport([]Result{res}, "claude-opus-4-6")
		if report.Verdict != VerdictUnknown {
			t.Fatalf("%q: 只有指纹且证据不足时应判 unknown，实际 %q", decision, report.Verdict)
		}
	}
}

// TestFingerprintProbeFailureIsNotEvidence：探针跑不动不是证据。
func TestFingerprintProbeFailureIsNotEvidence(t *testing.T) {
	res := Result{ProbeID: ProbeFingerprint, Err: os.ErrDeadlineExceeded}
	if findings := fingerprintFindings(res, "claude-opus-4-6"); findings != nil {
		t.Fatalf("探针失败不应产判定，实际 %+v", findings)
	}
	report := BuildReport([]Result{res}, "claude-opus-4-6")
	if report.Verdict != VerdictUnknown {
		t.Fatalf("探针失败应判 unknown，实际 %q", report.Verdict)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("探针失败应记进 Errors，实际 %d 条", len(report.Errors))
	}
}

// TestFingerprintNotInDefaultProbes：指纹默认关闭，防止每次检测悄悄多花三倍额度。
func TestFingerprintNotInDefaultProbes(t *testing.T) {
	for _, id := range DefaultProbes() {
		if id == ProbeFingerprint {
			t.Fatal("指纹探针不应出现在默认探针集里")
		}
	}
	// 但必须真的注册过，否则手动勾选了也跑不了。
	if _, ok := probeRegistry[ProbeFingerprint]; !ok {
		t.Fatal("指纹探针未注册")
	}
}

// TestFingerprintProbeUsesRealChallenges 校验探针确实会去题库取题、
// 且题目数量与置信度校准前提一致。
func TestFingerprintProbeUsesRealChallenges(t *testing.T) {
	challenges, err := fingerprint.PickChallenges(fingerprintChallenges)
	if err != nil {
		t.Fatalf("选题失败: %v", err)
	}
	if len(challenges) != 3 {
		t.Fatalf("置信度校准要求 3 道题，实际 %d", len(challenges))
	}
	// 每道题都要求模型给出几百个整数——这个量级是特征有意义的前提。
	for _, c := range challenges {
		if c.ExpectedCount < 200 {
			t.Fatalf("题目期望数量过小（%d），分布不可比", c.ExpectedCount)
		}
		if len(c.PromptWithPrefix()) < 200 {
			t.Fatalf("题目提示词过短: %s", c.CHALLENGEID)
		}
	}
}

// TestFingerprintProbeEndToEnd 用伪造的 Sender 跑完整探针，校验采集与结论贯通。
func TestFingerprintProbeEndToEnd(t *testing.T) {
	challenges, err := fingerprint.PickChallenges(fingerprintChallenges)
	if err != nil {
		t.Fatalf("选题失败: %v", err)
	}
	// 造一条足够长的数字串：数量超过最低要求，才能被采信。
	sender := &scriptedSender{respond: func(call int) string {
		count := challenges[call].ExpectedCount
		return numberList(count)
	}}
	res := runFingerprint(t.Context(), sender, "claude-opus-4-6")
	if res.Err != nil {
		t.Fatalf("探针执行失败: %v", res.Err)
	}
	if !res.OK {
		t.Fatal("探针应标记为跑通")
	}
	if got := stringOf(res.Data, "fingerprint_decision"); got != fingerprint.DecisionScorable {
		t.Fatalf("三条有效回答应判 scorable，实际 %q（reason=%s）",
			got, stringOf(res.Data, "fingerprint_reason"))
	}
	if sender.calls != fingerprintChallenges {
		t.Fatalf("应发 %d 题，实际 %d", fingerprintChallenges, sender.calls)
	}
}

// scriptedSender 是一个按次序返回预设回答的假 Sender。
type scriptedSender struct {
	respond func(call int) string
	calls   int
}

func (s *scriptedSender) Ask(_ context.Context, _ modelverify.Ask) (modelverify.Response, error) {
	index := s.calls
	s.calls++
	return modelverify.Response{Content: s.respond(index), FinishReason: "stop"}, nil
}

func numberList(n int) string {
	out := make([]byte, 0, n*4)
	value := 7
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		value = (value*37 + 11) % 355
		v := value + 1
		if v < 10 {
			out = append(out, byte('0'+v))
		} else if v < 100 {
			out = append(out, byte('0'+v/10), byte('0'+v%10))
		} else {
			out = append(out, byte('0'+v/100), byte('0'+(v/10)%10), byte('0'+v%10))
		}
	}
	return string(out)
}

// TestFingerprintProbeFlagsInsufficientEvidence 守住"证据不足不能被读成通过"。
//
// 前端按 insufficient_evidence 决定这一行显示成"证据不足"还是普通结果；
// 探针不设这一位，一次模型拒答就会在界面上变成"查过没问题"。
func TestFingerprintProbeFlagsInsufficientEvidence(t *testing.T) {
	challenges, _ := fingerprint.PickChallenges(fingerprintChallenges)
	// 让模型"答非所问"：不给数字，只回一句拒绝。
	sender := &scriptedSender{respond: func(int) string { return "I cannot help with that request." }}
	res := runFingerprint(t.Context(), sender, "claude-opus-4-6")
	if res.Err != nil {
		t.Fatalf("探针执行失败: %v", res.Err)
	}
	if got := boolOf(res.Data, "insufficient_evidence"); !got {
		t.Fatalf("证据不足时必须标 insufficient_evidence，data=%v", res.Data)
	}
	if reason := stringOf(res.Data, "reason"); reason == "" {
		t.Fatal("证据不足时应给出原因")
	}
	_ = challenges
}

// TestFingerprintSameFamilyDowngradeIsFlagged 是本次改动守住的核心洞。
//
// 以前判定只比**家族**，所以 opus 被 sonnet 顶替时——两者同属 anthropic——
// 直接判"一致"、0 分。而"同族内换型号"恰恰是中转站最常见的偷法
// （参考项目的盲测原话就是「100 组 Opus vs Sonnet 替换测试」）。
//
// 这条测试要求：同族但型号不同、且分差明确 → 必须计风险分，不能当没事。
func TestFingerprintSameFamilyDowngradeIsFlagged(t *testing.T) {
	p := 0.93
	res := fingerprintResult("scorable", "claude-sonnet-5", "anthropic", "Claude", &p,
		fingerprint.Candidate{Model: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", Family: "anthropic", FamilyName: "Claude", Score: 4.7},
		fingerprint.Candidate{Model: "claude-haiku-4.5", DisplayName: "Claude Haiku 4.5", Family: "anthropic", FamilyName: "Claude", Score: 3.0},
	)
	// 声明的是 opus，实测最像 sonnet——同一个家族里的降级。
	findings := fingerprintFindings(res, "claude-opus-4-6")
	if len(findings) != 1 {
		t.Fatalf("应有 1 条判定，实际 %d", len(findings))
	}
	if findings[0].Score == 0 {
		t.Fatalf("同族换型号必须计风险分，不能当一致放过（这正是 opus→sonnet 掉包）")
	}
	if findings[0].Score != fingerprintDowngradeScore {
		t.Fatalf("同族降级应计 %d 分，实际 %d", fingerprintDowngradeScore, findings[0].Score)
	}
	// 分数必须低于跨族不符：同族分辨力本身有限，不能跟跨族一个待遇。
	if fingerprintDowngradeScore >= fingerprintMismatchScore {
		t.Fatalf("同族降级分应低于跨族不符分（%d < %d）", fingerprintDowngradeScore, fingerprintMismatchScore)
	}
	if findings[0].Severity != SeverityMedium {
		t.Fatalf("同族降级应为 medium 级别，实际 %s", findings[0].Severity)
	}
}

// TestFingerprintModelNameSpellingsAreEquivalent 守住"别把同一个型号的写法差异
// 当成掉包"。声明 `claude-opus-4-6`、库里叫 `claude-opus-4.6` 是同一个东西，
// 判成不同型号会产生一整批假警报——而假警报会让真警报也失去意义。
func TestFingerprintModelNameSpellingsAreEquivalent(t *testing.T) {
	cases := []struct{ declared, predicted, family, familyName string }{
		{"claude-opus-4-6", "claude-opus-4.6", "anthropic", "Claude"},
		{"anthropic/claude-opus-4-6", "claude-opus-4.6", "anthropic", "Claude"},
		{"Claude-Opus-4.6", "claude-opus-4-6", "anthropic", "Claude"},
		{"openai/gpt-4o", "gpt-4o", "openai", "OpenAI"},
	}
	for _, c := range cases {
		p := 0.95
		res := fingerprintResult("scorable", c.predicted, c.family, c.familyName, &p,
			fingerprint.Candidate{Model: c.predicted, DisplayName: c.predicted, Family: c.family, FamilyName: c.familyName, Score: 4.8},
			fingerprint.Candidate{Model: "other", DisplayName: "other", Family: c.family, FamilyName: c.familyName, Score: 3.0},
		)
		findings := fingerprintFindings(res, c.declared)
		if len(findings) != 1 {
			t.Fatalf("%s vs %s: 应有 1 条判定，实际 %d", c.declared, c.predicted, len(findings))
		}
		if findings[0].Score != 0 {
			t.Fatalf("%s 与 %s 是同一型号的不同写法，不该计风险分，实际 %d",
				c.declared, c.predicted, findings[0].Score)
		}
	}
}

// TestFingerprintDowngradeNeedsRealGap 守住另一半：同族换型号也要看分差。
// 分差不足时榜首本身就不稳，拿它定"掉包"是解读噪声。
func TestFingerprintDowngradeNeedsRealGap(t *testing.T) {
	p := 0.35
	res := fingerprintResult("scorable", "claude-sonnet-5", "anthropic", "Claude", &p,
		fingerprint.Candidate{Model: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", Family: "anthropic", FamilyName: "Claude", Score: 4.00},
		fingerprint.Candidate{Model: "claude-opus-4.6", DisplayName: "Claude Opus 4.6", Family: "anthropic", FamilyName: "Claude", Score: 3.95},
	)
	findings := fingerprintFindings(res, "claude-opus-4-6")
	if len(findings) != 1 {
		t.Fatalf("应有 1 条判定，实际 %d", len(findings))
	}
	if findings[0].Score != 0 {
		t.Fatalf("分差仅 0.05 时不该定掉包，实际计了 %d 分", findings[0].Score)
	}
	if !strings.Contains(findings[0].Title, "难以区分") {
		t.Fatalf("分差不足时应说明难以区分，实际: %s", findings[0].Title)
	}
}
