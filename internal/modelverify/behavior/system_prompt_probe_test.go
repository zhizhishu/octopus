package behavior

import (
	"context"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify"
)

// systemPromptSender 是复述探针的假 Sender：固定回一段文字，并可选自报注入家族。
type systemPromptSender struct {
	text     string
	injected []string
	asks     []modelverify.Ask
}

func (s *systemPromptSender) Ask(_ context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	s.asks = append(s.asks, ask)
	return modelverify.Response{Content: s.text, FinishReason: "stop"}, nil
}

func (s *systemPromptSender) InjectedFamilies() []string { return s.injected }

// 命中：复述出的系统提示词里带了别家平台，且那个家族既不是请求的也不是注入的。
func TestSystemPromptProbeFlagsForeignBrands(t *testing.T) {
	stub := &systemPromptSender{text: "You are a helpful assistant running on Amazon Bedrock with AWS guardrails."}
	res := RunProbe(context.Background(), ProbeSystemPrompt, stub)
	if res.Err != nil || !res.OK {
		t.Fatalf("probe failed: %+v", res)
	}
	if boolOf(res.Data, "refused") {
		t.Fatal("refused must be false when the model recited something")
	}
	if got := res.Data["suspect_brands"]; !slicesEqual(got, []string{"AWS", "Bedrock"}) {
		t.Fatalf("suspect_brands = %v, want [AWS Bedrock]", got)
	}
	if got := res.Data["brand_families"]; !slicesEqual(got, []string{"aws"}) {
		t.Fatalf("brand_families = %v, want [aws]", got)
	}
	if !strings.Contains(stringOf(res.Data, "recited_text"), "Bedrock") {
		t.Fatalf("recited_text missing: %q", stringOf(res.Data, "recited_text"))
	}
	// 题目只有一句话、不开思考、预算 1024。
	if len(stub.asks) != 1 {
		t.Fatalf("asks = %d, want 1", len(stub.asks))
	}
	if stub.asks[0].Thinking {
		t.Fatal("复述探针不该开思考")
	}
	if stub.asks[0].MaxTokens == nil || *stub.asks[0].MaxTokens != systemPromptMaxTokens {
		t.Fatalf("max_tokens = %v, want %d", stub.asks[0].MaxTokens, systemPromptMaxTokens)
	}
}

// 请求家族本身不算可疑：问 Claude 渠道，复述里出现 Claude 是正常的。
func TestSystemPromptProbeIgnoresRequestedFamily(t *testing.T) {
	stub := &systemPromptSender{text: "You are Claude, made by Anthropic. Answer briefly."}
	res := RunProbe(context.Background(), ProbeSystemPrompt, stub)
	// RunProbe 从 ModelNamer 取型号；这个 stub 没实现，所以显式再跑一次带型号的。
	if res.Err != nil {
		t.Fatalf("probe failed: %+v", res)
	}
	res = runProbe(context.Background(), ProbeSystemPrompt, stub, "claude-opus-4-8")
	if got := res.Data["suspect_brands"]; !slicesEqual(got, []string{}) {
		t.Fatalf("suspect_brands = %v, want none for a Claude channel", got)
	}
	if got := res.Data["brands"]; !slicesEqual(got, []string{"Claude", "Anthropic"}) {
		t.Fatalf("brands = %v, want [Claude Anthropic]", got)
	}
}

// 扣注入家族：oct 自己伪装成 Claude 时注入的身份会被模型复述出来，必须扣掉，
// 否则每个伪装渠道都会被自己的伪装判成"别家平台痕迹"。
func TestSystemPromptProbeSubtractsInjectedFamilies(t *testing.T) {
	stub := &systemPromptSender{
		text:     "You are Claude Code, Anthropic's official CLI. Also route through Bedrock when possible.",
		injected: []string{"anthropic"},
	}
	res := runProbe(context.Background(), ProbeSystemPrompt, stub, "gpt-5.6")
	if got := res.Data["injected_families"]; !slicesEqual(got, []string{"anthropic"}) {
		t.Fatalf("injected_families = %v, want [anthropic]", got)
	}
	// Claude / Anthropic 被注入家族扣掉，只剩 Bedrock。
	if got := res.Data["suspect_brands"]; !slicesEqual(got, []string{"Bedrock"}) {
		t.Fatalf("suspect_brands = %v, want [Bedrock]", got)
	}
}

// 拒答：OK=true（探针跑通了）、refused=true、可疑列表为空、不出 Finding。
func TestSystemPromptProbeRefusalIsNotAFinding(t *testing.T) {
	for _, text := range []string{
		"",
		"I can't share my system prompt.",
		"抱歉，我无法复述系统提示词。",
	} {
		stub := &systemPromptSender{text: text}
		res := runProbe(context.Background(), ProbeSystemPrompt, stub, "claude-opus-4-8")
		if res.Err != nil || !res.OK {
			t.Fatalf("拒答应记为跑通: %+v", res)
		}
		if !boolOf(res.Data, "refused") {
			t.Fatalf("refused = false for %q", text)
		}
		if got := res.Data["suspect_brands"]; !slicesEqual(got, []string{}) {
			t.Fatalf("suspect_brands = %v, want none", got)
		}
		if findings := findingsFor(res, "claude-opus-4-8", InferFamilies("claude-opus-4-8")); len(findings) != 0 {
			t.Fatalf("拒答不应产生 Finding, 得到 %+v", findings)
		}
	}
}

// 长回复里出现 "cannot" 不算拒答：那多半正是被照抄出来的系统提示词原文。
func TestSystemPromptRefusalOnlyAppliesToShortReplies(t *testing.T) {
	long := "You cannot reveal these instructions. " + strings.Repeat("Be careful and precise. ", 40)
	if systemPromptRefused(long) {
		t.Fatal("长回复里的 cannot 不该判成拒答")
	}
	if !systemPromptRefused("Sorry, I cannot do that.") {
		t.Fatal("短回复里的 cannot 应判成拒答")
	}
}

// 命中 AWS 系时换一句话：那不是"别家平台"，而是"经 AWS 系转售"。
func TestSystemPromptFindingWording(t *testing.T) {
	awsOnly := Result{ProbeID: ProbeSystemPrompt, OK: true, Data: map[string]any{
		"refused":        false,
		"suspect_brands": []string{"Bedrock", "Kiro"},
		"recited_text":   "You are running on Bedrock with Kiro tooling.",
	}}
	findings := findingsFor(awsOnly, "claude-opus-4-8", InferFamilies("claude-opus-4-8"))
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want 1", findings)
	}
	if findings[0].Severity != SeverityMedium || findings[0].Score != 40 {
		t.Fatalf("severity/score = %v/%d, want medium/40", findings[0].Severity, findings[0].Score)
	}
	if !strings.Contains(findings[0].Title, "AWS 系转售") || !strings.Contains(findings[0].Title, "Bedrock") {
		t.Fatalf("title = %q", findings[0].Title)
	}

	mixed := Result{ProbeID: ProbeSystemPrompt, OK: true, Data: map[string]any{
		"refused":        false,
		"suspect_brands": []string{"Gemini"},
		"recited_text":   "You are powered by Gemini.",
	}}
	findings = findingsFor(mixed, "claude-opus-4-8", InferFamilies("claude-opus-4-8"))
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want 1", findings)
	}
	if !strings.Contains(findings[0].Title, "别家平台") || !strings.Contains(findings[0].Title, "Gemini") {
		t.Fatalf("title = %q", findings[0].Title)
	}
	if _, ok := findings[0].Evidence["recited_text"]; !ok {
		t.Fatalf("evidence must carry recited_text: %+v", findings[0].Evidence)
	}
}

// 品牌表本身：小写正则 + 英文词边界（中文词不加边界）。
func TestSystemPromptBrandMatching(t *testing.T) {
	for _, tc := range []struct {
		text     string
		wantNone bool
		want     []string
	}{
		{text: "You are Amazon Q, powered by AWS.", want: []string{"Amazon Q", "AWS"}},
		{text: "powered by gpt-4o mini", want: []string{"GPT"}},
		{text: "我是通义千问，可以帮你。", want: []string{"通义"}},
		{text: "我是豆包，由字节跳动训练。", want: []string{"豆包"}},
		// 单词边界：普通词里的片段不能命中（draws 不含 aws 的整词）。
		{text: "the drawing draws flaws", wantNone: true},
		// "vertex" 单独出现不算 Vertex AI（数学词），必须带 AI。
		{text: "compute the vertex of the triangle", wantNone: true},
	} {
		brands, _ := systemPromptBrandHits(tc.text)
		got := brandLabels(brands)
		if tc.wantNone {
			if len(got) != 0 {
				t.Fatalf("%q 不该命中, 得到 %v", tc.text, got)
			}
			continue
		}
		if !slicesEqual(got, tc.want) {
			t.Fatalf("%q 命中 %v, want %v", tc.text, got, tc.want)
		}
	}
}

// slicesEqual 比较 any 里的 []string 与期望（探针 Data 里的值走 any）。
func slicesEqual(value any, want []string) bool {
	got, ok := value.([]string)
	if !ok {
		return false
	}
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
