package op

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/tokenizer"
)

// 被动探针的测试重点不是"能不能报出问题"，而是**该报的报、不该报的不报**。
//
// 这类检测最大的风险不是漏报，是噪声：一个天天误报的检查，用户三天就学会无视它，
// 之后真出问题也没人看。所以下面每条"不报"的用例和在"报"的用例一样重要。

// findCode 在结论集里找某一条码，找不到返回 nil。
func findCode(res RelayProbeResult, code string) *model.RelayProbeFinding {
	for i := range res.Findings {
		if res.Findings[i].Code == code {
			return &res.Findings[i]
		}
	}
	return nil
}

// chatBody 造一个最小的 OpenAI chat 响应体。
func chatBody(text string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": text},
		}},
	})
	return string(b)
}

func reqBody(prompt string) string {
	b, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-4-6",
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	})
	return string(b)
}

// TestRelayProbeTokenArithmeticFlagsMismatch 守最基本的一条账：
// total 不等于 prompt+completion 时必须报，且把三个数字都摆出来让人自己核。
func TestRelayProbeTokenArithmeticFlagsMismatch(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    50,
		TotalTokens:     200, // 应该是 150
		ResponseContent: chatBody("hello there"),
	})
	f := findCode(res, model.ProbeCodeTokenArithmetic)
	if f == nil {
		t.Fatal("total 对不上账时必须报出来")
	}
	if f.Severity != model.RelayProbeSeverityMedium {
		t.Fatalf("账目不符应为 medium，实际 %s", f.Severity)
	}
	// 数字必须原样进证据，用户要能自己复核而不是只能信结论。
	if f.Evidence["sum"] != 150 {
		t.Fatalf("证据里的和应为 150，实际 %v", f.Evidence["sum"])
	}
	if f.Evidence["diff"] != 50 {
		t.Fatalf("证据里的差额应为 50，实际 %v", f.Evidence["diff"])
	}
}

// TestRelayProbeTokenArithmeticSilentWhenConsistent 账算得平就不该出声。
func TestRelayProbeTokenArithmeticSilentWhenConsistent(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    50,
		TotalTokens:     150,
		ResponseContent: chatBody("hello there"),
	})
	if f := findCode(res, model.ProbeCodeTokenArithmetic); f != nil {
		t.Fatalf("账算得平不该报警，实际 %s", f.Title)
	}
}

// TestRelayProbeSkipsMissingUsage 三个 token 数字缺任何一个都不判账。
//
// 缺字段说明上游没报 usage，不是"账错了"。把"没数据"当"账不符"是最典型的噪声源。
func TestRelayProbeSkipsMissingUsage(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    0, // 上游没报输出
		TotalTokens:     0,
		ResponseContent: chatBody("hello"),
	})
	if f := findCode(res, model.ProbeCodeTokenArithmetic); f != nil {
		t.Fatalf("缺 usage 时不该判账，实际 %s", f.Title)
	}
}

// TestRelayProbeCacheExceedsPrompt 缓存命中不该超过总输入。
func TestRelayProbeCacheExceedsPrompt(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    20,
		TotalTokens:     120,
		CacheHitTokens:  150, // 比总输入还多，物理上不可能
		ResponseContent: chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeCacheExceedsPrompt); f == nil {
		t.Fatal("缓存超过总输入必须报出来")
	}
}

// TestRelayProbeCacheWithinPromptIsFine 缓存是输入的一部分，没超过就不报。
func TestRelayProbeCacheWithinPromptIsFine(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:   "claude-opus-4-6",
		InputTokens:      1000,
		OutputTokens:     50,
		TotalTokens:      1050,
		CacheHitTokens:   800,
		CacheWriteTokens: 100,
		ResponseContent:  chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeCacheExceedsPrompt); f != nil {
		t.Fatalf("缓存未超过输入不该报，实际 %s", f.Title)
	}
}

// TestRelayProbeEmptyOutputWithTokens 正文空却计了输出 token。
func TestRelayProbeEmptyOutputWithTokens(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    80,
		TotalTokens:     180,
		ResponseContent: chatBody(""),
	})
	if f := findCode(res, model.ProbeCodeEmptyOutputWithTokens); f == nil {
		t.Fatal("正文为空却有输出 token 必须报出来")
	}
}

// TestRelayProbeCharTokenRatioFlagsInflation 字符/token 比值离谱时报出来。
func TestRelayProbeCharTokenRatioFlagsInflation(t *testing.T) {
	// 10 个字符却记 100 个输出 token → 0.1 字/token，远低于下限。
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     10,
		OutputTokens:    100,
		TotalTokens:     110,
		ResponseContent: chatBody("0123456789"),
	})
	f := findCode(res, model.ProbeCodeCharTokenRatio)
	if f == nil {
		t.Fatal("字符/token 比值远低于下限时必须报出来")
	}
	if f.Severity != model.RelayProbeSeverityMedium {
		t.Fatalf("严重偏离应为 medium，实际 %s", f.Severity)
	}
}

// TestRelayProbeCharTokenRatioSilentForNormalText 正常文本不该报。
//
// 这条专门守中文：中文一个字大约一个 token 甚至不到，如果阈值定得太松，
// 中文用户会天天看到假警报。
func TestRelayProbeCharTokenRatioSilentForNormalText(t *testing.T) {
	text := strings.Repeat("这是一段正常的中文回复，用来验证比值检查不会误报。", 20)
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     50,
		OutputTokens:    len([]rune(text)) / 2, // 约 2 字/token
		TotalTokens:     50 + len([]rune(text))/2,
		ResponseContent: chatBody(text),
	})
	if f := findCode(res, model.ProbeCodeCharTokenRatio); f != nil {
		t.Fatalf("正常中文不该报，实际 %s", f.Title)
	}
}

// TestRelayProbeTokenizerResidualFlagsForeignVocab 守最有分量的一条：
// 上游报的输入 token 与本地复算差得离谱时，说明分词器根本不是同一家。
func TestRelayProbeTokenizerResidualFlagsForeignVocab(t *testing.T) {
	// 造一段足够长的英文，本地 o200k 会算出可观的 token 数。
	prompt := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	// 本地实际会算出约 1900+，这里故意把上游报的值抬到 3 倍。
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     6000,
		OutputTokens:    20,
		TotalTokens:     6020,
		RequestContent:  reqBody(prompt),
		ResponseContent: chatBody("ok"),
	})
	f := findCode(res, model.ProbeCodeTokenizerResidual)
	if f == nil {
		t.Fatal("分词器口径差 3 倍时必须报出来")
	}
	if f.Severity != model.RelayProbeSeverityMedium {
		t.Fatalf("应为 medium，实际 %s", f.Severity)
	}
}

// TestRelayProbeTokenizerResidualSilentWhenClose 接近时不该报。
func TestRelayProbeTokenizerResidualSilentWhenClose(t *testing.T) {
	prompt := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	// 先算出本地真值，再让上游报一个落在容忍范围内的数。
	local := localCount(t, reqBody(prompt))
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     local + local/10, // 偏 10%，在 35% 容忍内
		OutputTokens:    20,
		TotalTokens:     local + local/10 + 20,
		RequestContent:  reqBody(prompt),
		ResponseContent: chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeTokenizerResidual); f != nil {
		t.Fatalf("偏差在容忍范围内不该报，实际 %s", f.Title)
	}
}

// TestRelayProbeTokenizerResidualSkipsSmallSamples 小样本不判。
//
// 几十个 token 上一点点计数差异就会放大成很大比例，这个量级下结论纯属噪声。
func TestRelayProbeTokenizerResidualSkipsSmallSamples(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     50, // 低于 ProbeTokenizerMinTokens
		OutputTokens:    10,
		TotalTokens:     60,
		RequestContent:  reqBody("hi"),
		ResponseContent: chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeTokenizerResidual); f != nil {
		t.Fatalf("小样本不该判分词器，实际 %s", f.Title)
	}
}

// TestRelayProbeSkipsErroredRequests 失败请求一律不判。
//
// 错误回复的 token 数字通常是空的或残缺的，拿来算账只会制造假警报。
func TestRelayProbeSkipsErroredRequests(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    50,
		TotalTokens:     999, // 故意错
		ResponseContent: chatBody("x"),
		HasError:        true,
	})
	if len(res.Findings) != 0 {
		t.Fatalf("失败请求不该产出任何结论，实际 %d 条", len(res.Findings))
	}
}

// TestRelayProbeNeverClaimsAuthenticity 是一条类型层面的约束：
// 结论里绝不能出现"正品/已验真/passed"这类断言。
//
// 这个包只能证伪。把"没发现问题"读成"确认是正品"是本项目最不能犯的错。
func TestRelayProbeNeverClaimsAuthenticity(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    50,
		TotalTokens:     150,
		ResponseContent: chatBody("hello"),
	})
	for _, f := range res.Findings {
		for _, banned := range []string{"正品", "已确认", "验真通过", "passed", "verified"} {
			if strings.Contains(f.Title, banned) {
				t.Fatalf("结论里出现了认证性断言 %q：%s", banned, f.Title)
			}
		}
	}
}

// localCount 在测试里复算本地分词器结果，避免把魔法数字写进测试。
func localCount(t *testing.T, body string) int {
	t.Helper()
	text, _ := requestBillableText(body)
	n := tokenizer.CountTokens(text, "claude-opus-4-6")
	if n <= 0 {
		t.Fatalf("本地分词器应能算出正数，得到 %d", n)
	}
	return n
}

// TestComputeRelayProbesForLogSkipsSelfArithmetic 守一个逻辑陷阱：
// RelayLog 上只有输入/输出两栏, 没有独立的总数字段。如果拿"自己加出来的总数"
// 去核对它自己, 这条检查永远成立, 等于白跑还占着一个"检查过"的名额。
func TestComputeRelayProbesForLogSkipsSelfArithmetic(t *testing.T) {
	_ = model.RelayLog{}
	findings := computeRelayProbesForLog(model.RelayLog{})
	for _, f := range findings {
		if f.Code == model.ProbeCodeTokenArithmetic {
			t.Fatal("日志路径上没有独立的总数字段, 不该产出账目检查结论")
		}
	}
}

// TestComputeRelayProbesForLogCarriesTokenNumbers 确认映射层把字段接对了。
//
// 用一条缓存超输入的日志验证: 只有在 CacheHitTokens 真的被传进去时这条才会触发。
func TestComputeRelayProbesForLogCarriesTokenNumbers(t *testing.T) {
	findings := computeRelayProbesForLog(model.RelayLog{
		InputTokens:     100,
		OutputTokens:    20,
		CacheHitTokens:  500,
		ResponseContent: chatBody("ok"),
		ActualModelName: "claude-opus-4-6",
	})
	var hit bool
	for _, f := range findings {
		if f.Code == model.ProbeCodeCacheExceedsPrompt {
			hit = true
		}
	}
	if !hit {
		t.Fatal("映射层应把 CacheHitTokens 传进探针, 否则这条检查永远不会触发")
	}
}

// TestComputeRelayProbesForLogSkipsErrors 失败请求不产出结论。
func TestComputeRelayProbesForLogSkipsErrors(t *testing.T) {
	findings := computeRelayProbesForLog(model.RelayLog{
		InputTokens:     100,
		OutputTokens:    20,
		CacheHitTokens:  500,
		ResponseContent: chatBody("ok"),
		Error:           "upstream 500",
	})
	if len(findings) != 0 {
		t.Fatalf("失败请求不该产出结论, 实际 %d 条", len(findings))
	}
}

// 纯工具调用 / 纯推理 / 结构化输出都不是"空回复"。
//
// 这条守的是一个会让用户白跑一趟的假警报：一轮正常的工具调用，正文本来就是
// 空的（内容全在 tool_calls 里），但 output token 是真的花了。旧判据只看
// visibleReplyText，于是把它判成"回复正文是空的，却记了 N 个输出 token"，
// 提示用户去查上游被吃内容——实际什么都没错。
func TestRelayProbePureToolCallIsNotEmptyOutput(t *testing.T) {
	cases := map[string]string{
		"openai tool_calls": `{"choices":[{"message":{"role":"assistant","content":null,` +
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`,
		"openai reasoning only": `{"choices":[{"message":{"role":"assistant","content":"",` +
			`"reasoning_content":"先想一下这个问题该怎么拆"}}]}`,
		"openai function_call": `{"choices":[{"message":{"role":"assistant","content":null,` +
			`"function_call":{"name":"lookup","arguments":"{}"}}}]}`,
		"anthropic tool_use":   `{"content":[{"type":"tool_use","id":"tu_1","name":"search","input":{"q":"x"}}]}`,
		"anthropic thinking":   `{"content":[{"type":"thinking","thinking":"推理内容","signature":"sig"}]}`,
		"top-level tool_calls": `{"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
	}
	for name, body := range cases {
		res := ComputeRelayLogProbes(RelayProbeInput{
			RequestedModel:  "claude-opus-4-6",
			InputTokens:     100,
			OutputTokens:    80,
			TotalTokens:     180,
			ResponseContent: body,
		})
		if f := findCode(res, model.ProbeCodeEmptyOutputWithTokens); f != nil {
			t.Errorf("%s: 有真实产出却被判成空回复：%s", name, f.Title)
		}
	}
}

// 反过来：真的什么都没有（正文空、无工具、无推理）时仍必须报。
// 上面放宽判据之后，这条防止把探针改成永不触发的摆设。
func TestRelayProbeTrulyEmptyOutputStillFlagged(t *testing.T) {
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		InputTokens:     100,
		OutputTokens:    80,
		TotalTokens:     180,
		ResponseContent: `{"choices":[{"message":{"role":"assistant","content":""}}]}`,
	})
	if f := findCode(res, model.ProbeCodeEmptyOutputWithTokens); f == nil {
		t.Fatal("正文空、无工具、无推理，花了 80 个输出 token，必须报出来")
	}
}
