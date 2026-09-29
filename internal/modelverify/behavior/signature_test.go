package behavior

import (
	"context"
	"encoding/base64"
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/modelverify"
)

// ---------------------------------------------------------- protobuf 构造工具
//
// 测试要能造出「结构上像真签名」的字节，否则 ParseSignature 的判定根本没被验证。
// 这里按上游记录的结构手工编码，不引 protobuf 依赖。

func pbVarintValue(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func pbLenField(field int, payload []byte) []byte {
	out := pbVarintValue(uint64(field)<<3 | 2)
	out = append(out, pbVarintValue(uint64(len(payload)))...)
	return append(out, payload...)
}

func pbVarintField(field int, val uint64) []byte {
	out := pbVarintValue(uint64(field)<<3 | 0)
	return append(out, pbVarintValue(val)...)
}

// filler 造一段给定长度的高熵字节。
//
// 用固定种子的 math/rand，任意长度下分布都接近均匀。早期版本用线性同余自己滚
// （byte(x>>24)），连续输出的高位彼此相关，实测熵只有 7.2，会被熵判据当成
// "假密文"——测试的填充器本身必须足够像真密文，否则判据没被正确验证。
func filler(n int, seed int) []byte {
	out := make([]byte, n)
	r := rand.New(rand.NewSource(int64(seed)*7919 + 1))
	for i := range out {
		out[i] = byte(r.Intn(256))
	}
	return out
}

// makeSignature 按上游记录的结构拼一个签名。
// model 为空时省略绑定模型名那段；nonceSegs 控制 nonce 段数；
// zeroCiphertext 为真时用全零密文（低熵，模拟伪造）。
func makeSignature(model string, nonceSegs, ciphertextLen int, zeroCiphertext bool) string {
	header := []byte{}
	if model != "" {
		header = append(header, pbLenField(sigFieldModelName, []byte(model))...)
	}
	header = append(header, pbVarintField(sigFieldBlockType, 1)...)

	inner := pbLenField(sigInnerHeaderFld, header)
	for i := 0; i < nonceSegs; i++ {
		inner = append(inner, pbLenField(2+i, filler(12, i))...)
	}
	ct := filler(ciphertextLen, 1)
	if zeroCiphertext {
		ct = make([]byte, ciphertextLen)
	}
	inner = append(inner, pbLenField(sigCiphertextField, ct)...)

	return base64.StdEncoding.EncodeToString(pbLenField(sigOuterInnerField, inner))
}

// ------------------------------------------------------------------ 结构解析

func TestParseSignatureAcceptsWellFormed(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	info := ParseSignature(sig)
	if !info.OK {
		t.Fatalf("结构良好的签名应通过, 原因: %s", info.Reason)
	}
	if info.BoundModel != "claude-sonnet-4-5" {
		t.Fatalf("绑定模型名 = %q", info.BoundModel)
	}
	if info.BlockType != 1 {
		t.Fatalf("块类型 = %d, 期望 1", info.BlockType)
	}
	if len(info.NonceLengths) != 2 {
		t.Fatalf("nonce 段数 = %d, 期望 2", len(info.NonceLengths))
	}
	if info.CiphertextLen != 256 {
		t.Fatalf("密文长度 = %d, 期望 256", info.CiphertextLen)
	}
	// 熵判据是"随样本量缩放"的：真随机值应紧贴该样本量下的理论期望上限。
	// 直接对 expectedMaxEntropy 做比较，而不是写死 7.9——写死的数在
	// 样本量变化时会变成一条与判据无关的断言。
	ceiling := expectedMaxEntropy(256)
	if info.CiphertextEntropy < ceiling-sigEntropyTolerance {
		t.Fatalf("随机填充的熵 = %.3f, 该样本量下的期望上限 %.3f, 不该被判为低熵",
			info.CiphertextEntropy, ceiling)
	}
}

func TestParseSignatureRejectsMissingModel(t *testing.T) {
	info := ParseSignature(makeSignature("", 2, 128, false))
	if info.OK {
		t.Fatal("未绑定模型名不应通过")
	}
	if !strings.Contains(info.Reason, "模型名") {
		t.Fatalf("原因应提到模型名, 得到 %q", info.Reason)
	}
}

func TestParseSignatureRejectsTooFewNonceSegments(t *testing.T) {
	info := ParseSignature(makeSignature("claude-sonnet-4-5", 1, 128, false))
	if info.OK {
		t.Fatal("nonce 段数不足不应通过")
	}
	if !strings.Contains(info.Reason, "nonce") {
		t.Fatalf("原因应提到 nonce, 得到 %q", info.Reason)
	}
}

// 低熵密文是伪造的典型特征：真 AEAD 密文不可能是一段可预测的字节。
func TestParseSignatureRejectsLowEntropyCiphertext(t *testing.T) {
	info := ParseSignature(makeSignature("claude-sonnet-4-5", 2, 256, true))
	if info.OK {
		t.Fatal("全零密文不应通过")
	}
	if !strings.Contains(info.Reason, "熵") {
		t.Fatalf("原因应提到熵, 得到 %q", info.Reason)
	}
	if info.CiphertextEntropy != 0 {
		t.Fatalf("全零密文熵应为 0, 得到 %.3f", info.CiphertextEntropy)
	}
}

func TestParseSignatureRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"not-base64-at-all!!!",
		base64.StdEncoding.EncodeToString([]byte("short")),
	}
	for _, c := range cases {
		if info := ParseSignature(c); info.OK {
			t.Fatalf("垃圾输入 %q 不应通过, 得到 %+v", c, info)
		}
	}
}

// 缺 padding 的 base64 在真实流量里常见（中转站会做截断），必须能补齐。
func TestParseSignatureToleratesMissingBase64Padding(t *testing.T) {
	// 先找一个确实带 padding 的样本长度：base64 长度是否含 '=' 取决于字节数，
	// 写死一个长度可能恰好落在 4 的倍数上，那样这条用例就什么都没测到。
	var padded string
	for n := 100; n < 400; n++ {
		if s := makeSignature("claude-opus-4-8", 2, n, false); strings.HasSuffix(s, "=") {
			padded = s
			break
		}
	}
	if padded == "" {
		t.Fatal("找不到带 padding 的样本，这条用例失去意义")
	}
	unpadded := strings.TrimRight(padded, "=")
	if info := ParseSignature(unpadded); !info.OK {
		t.Fatalf("缺 padding 的合法签名应能解析, 原因: %s", info.Reason)
	}
}

// 未知 wire type 之后的位置不可信，遍历必须停下而不是继续错位读。
func TestWalkProtoStopsOnUnknownWireType(t *testing.T) {
	buf := []byte{}
	buf = append(buf, pbVarintValue(1<<3|0)...) // field 1, varint
	buf = append(buf, pbVarintValue(42)...)
	buf = append(buf, 0x0f) // field 1, wire type 7（非法）
	buf = append(buf, 0xff, 0xff, 0xff)

	var seen []int
	walkProto(buf, func(num, wire int, _ uint64, _ []byte) bool {
		seen = append(seen, num)
		return true
	})
	if len(seen) != 1 || seen[0] != 1 {
		t.Fatalf("非法 wire type 之后不应继续解析, 得到 %v", seen)
	}
}

// ------------------------------------------------------------------ 模型名匹配

func TestMatchSignatureModel(t *testing.T) {
	cases := []struct {
		bound, requested string
		want             bool
	}{
		{"claude-sonnet-4-5", "claude-sonnet-4-5", true},
		{"Claude-Sonnet-4-5", "claude-sonnet-4-5", true},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5", true},
		{"claude-sonnet-4-5", "claude-opus-4-8", false},
		{"", "claude-sonnet-4-5", false},
		{"claude-sonnet-4-5", "", false},
	}
	for _, c := range cases {
		if got := MatchSignatureModel(c.bound, c.requested); got != c.want {
			t.Fatalf("MatchSignatureModel(%q, %q) = %v, 期望 %v", c.bound, c.requested, got, c.want)
		}
	}
}

// ------------------------------------------------------------------ 探针流程

// errTest 供"探针跑不动"的用例复用。
var errTest = errors.New("upstream unavailable")

// signatureStub 用闭包应答，同时记录每次的请求参数以便断言回放形态。
type signatureStub struct {
	respond func(call int, ask modelverify.Ask) (modelverify.Response, error)
	calls   int
	asks    []modelverify.Ask
}

func (s *signatureStub) Ask(_ context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	s.asks = append(s.asks, ask)
	s.calls++
	return s.respond(s.calls, ask)
}

// protocolStub 在内层 Sender 之上自报一个出站协议，用来验证「不适用」分支。
type protocolStub struct {
	inner modelverify.Sender
	proto string
}

func (s *protocolStub) Ask(ctx context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	return s.inner.Ask(ctx, ask)
}

func (s *protocolStub) Protocol() string { return s.proto }

func TestRunSignatureHarvestThenReplay(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, ask modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// harvest 必须带思考，否则取不到签名。
			if !ask.Thinking {
				t.Error("harvest 阶段应请求 extended thinking")
			}
			return modelverify.Response{Signature: sig, ReasoningContent: "17 * 23 = 391", FinishReason: "end_turn",
				Usage: modelverify.Usage{Reasoning: 120, ReasoningReported: true}}, nil
		}
		return modelverify.Response{
			Content:      "<cot>17 * 23 = 391</cot>",
			FinishReason: "end_turn",
		}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	if res.Err != nil {
		t.Fatalf("不应报错: %v", res.Err)
	}
	if !res.OK {
		t.Fatalf("应判定通过: %+v", res.Data)
	}
	if stub.calls != 2 {
		t.Fatalf("应发两次请求（采集 + 回放），实际 %d", stub.calls)
	}
	// 回放请求必须把签名塞进 assistant 轮。
	replayTurns := stub.asks[1].Turns
	if len(replayTurns) != 2 {
		t.Fatalf("回放应有 2 轮, 得到 %d", len(replayTurns))
	}
	if replayTurns[0].Role != "assistant" || replayTurns[0].Signature != sig {
		t.Fatalf("第一轮应是带签名的 assistant 轮: %+v", replayTurns[0])
	}
	// 回放必须正文+签名成对发回：出站护栏会丢弃无正文的 thinking 块，
	// 只回签名不回正文等于没回放（F11）。
	if replayTurns[0].Thinking == "" {
		t.Fatalf("回放的 assistant 轮应携带同块思考正文: %+v", replayTurns[0])
	}
	if !stub.asks[1].Thinking {
		t.Error("回放轮应保持思考开启（会话里带着 thinking 块）")
	}
	if replayTurns[1].Role != "user" {
		t.Fatalf("第二轮应是 user 轮: %+v", replayTurns[1])
	}
	if unsealed, _ := res.Data["unsealed"].(bool); !unsealed {
		t.Fatal("回放解出 <cot> 内容应记为已解封")
	}
	if got := intOf(res.Data, "cot_len"); got != len("17 * 23 = 391") {
		t.Fatalf("cot 长度 = %d", got)
	}
	if suspects := stringSliceOf(res.Data, "suspects"); len(suspects) != 0 {
		t.Fatalf("干净内容不应有痕迹词, 得到 %v", suspects)
	}
}

func TestRunSignatureDetectsAWSTraces(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// 回放需要同块思考正文（F11：只回签名会被出站护栏丢弃）。
			return modelverify.Response{Signature: sig, ReasoningContent: "17 * 23 = 391", FinishReason: "end_turn"}, nil
		}
		return modelverify.Response{
			Content:      "<cot>I am running on Bedrock via Kiro, the guardrails apply.</cot>",
			FinishReason: "end_turn",
		}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	suspects := stringSliceOf(res.Data, "suspects")
	if len(suspects) == 0 {
		t.Fatal("应检出 AWS 系痕迹词")
	}
	// 词表顺序保留，且大小写不敏感（原文是 Bedrock / Kiro）。
	joined := strings.Join(suspects, ",")
	for _, want := range []string{"kiro", "bedrock", "guardrails"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("应命中 %q, 得到 %v", want, suspects)
		}
	}

	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	found := false
	for _, f := range report.Findings {
		if f.Score == 60 {
			found = true
		}
	}
	if !found {
		t.Fatalf("应产出 60 分的高危项, 得到 %+v", report.Findings)
	}
}

func TestRunSignatureReplayRejectedByUpstream(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// harvest 正常拿到签名（同渠道刚成功，反衬回放被拒的异常性）。
			return modelverify.Response{Signature: sig, ReasoningContent: "17 * 23 = 391", FinishReason: "end_turn"}, nil
		}
		// 回放被上游 4xx 拒收：真 Anthropic 后端不会拒收自己签发的签名。
		return modelverify.Response{}, &modelverify.UpstreamStatusError{Status: 400, Err: errors.New("invalid_request_error: signature")}
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	// 4xx 拒收是判定级观测，不是探针故障——必须走 finding 通道而非 Err 通道。
	if res.Err != nil {
		t.Fatalf("4xx 拒收不应走 Err 通道: %v", res.Err)
	}
	if rejected, _ := res.Data["replay_rejected"].(bool); !rejected {
		t.Fatalf("应记 replay_rejected: %+v", res.Data)
	}
	if got := intOf(res.Data, "replay_reject_status"); got != 400 {
		t.Fatalf("拒收状态码 = %d, 应为 400", got)
	}

	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	found := false
	for _, f := range report.Findings {
		if f.Title == "签名回放被上游拒绝" {
			found = true
			if f.Score != 50 || f.Severity != SeverityHigh {
				t.Fatalf("应为 50 分高危, 得到 %+v", f)
			}
		}
		// 拒收已含「未解封」，不应再叠加证据不足项（避免同一信号双计）。
		if f.Title == "签名回放未解封（证据不足）" {
			t.Fatalf("拒收时不应叠加未解封项: %+v", f)
		}
	}
	if !found {
		t.Fatalf("应有「签名回放被上游拒绝」finding: %+v", report.Findings)
	}
	// 50 分 ≥ 40 中危线：单此一条即中危。此前该信号走 Err 通道零分，
	// 伪造签名的替身仅凭最硬证据最多得 none/unknown。
	if report.Verdict != VerdictMedium {
		t.Fatalf("单条拒收应判中危, 得到 %s", report.Verdict)
	}
}

func TestRunSignatureReplayServerErrorStaysErr(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			return modelverify.Response{Signature: sig, ReasoningContent: "17 * 23 = 391", FinishReason: "end_turn"}, nil
		}
		// 5xx = 上游容量/网关问题，不算任何一方的证据：仍走 Err 通道。
		return modelverify.Response{}, &modelverify.UpstreamStatusError{Status: 503, Err: errors.New("upstream overloaded")}
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	if res.Err == nil {
		t.Fatal("5xx 应保持 Err 通道（探针没跑成，不是判定证据）")
	}
	if rejected, _ := res.Data["replay_rejected"].(bool); rejected {
		t.Fatal("5xx 不应记 replay_rejected")
	}
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	if len(report.Findings) != 0 {
		t.Fatalf("5xx 不应产生任何 finding: %+v", report.Findings)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("5xx 应记一条探针错误: %+v", report.Errors)
	}
}

func TestRunSignatureDetectsUnsealed(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// 回放需要同块思考正文（F11：只回签名会被出站护栏丢弃）。
			return modelverify.Response{Signature: sig, ReasoningContent: "17 * 23 = 391", FinishReason: "end_turn"}, nil
		}
		// 模型没按格式输出 <cot> 段。
		return modelverify.Response{Content: "I cannot access that.", FinishReason: "end_turn"}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	if unsealed, _ := res.Data["unsealed"].(bool); unsealed {
		t.Fatal("没有 <cot> 段不应记为已解封")
	}
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	found := false
	for _, f := range report.Findings {
		// <cot> 有无不是密码学证据（C05）：签名已发回且未被拒，只是模型没按
		// 指令复述——降为「证据不足」的中危提示，不再当已证实的解封失败。
		if f.Title == "签名回放未解封（证据不足）" {
			found = true
			if f.Score != 25 || f.Severity != SeverityMedium {
				t.Fatalf("应为 25 分中危, 得到 %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("应产出「未解封（证据不足）」风险项, 得到 %+v", report.Findings)
	}
}

// <cot> 缺失时仍要在整段回复里找痕迹词：格式没守不代表证据不成立。
func TestRunSignatureFallsBackToWholeReplyForSuspects(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// 回放需要同块思考正文（F11：只回签名会被出站护栏丢弃）。
			return modelverify.Response{Signature: sig, ReasoningContent: "17 * 23 = 391", FinishReason: "end_turn"}, nil
		}
		return modelverify.Response{Content: "Sure, running on bedrock here.", FinishReason: "end_turn"}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	if suspects := stringSliceOf(res.Data, "suspects"); len(suspects) == 0 {
		t.Fatal("无 <cot> 时仍应扫描整段回复")
	}
}

// 非 Anthropic 出站没有签名这回事，必须自报不适用且不产生成本。
func TestRunSignatureNotApplicableOnOtherProtocols(t *testing.T) {
	stub := &signatureStub{respond: func(int, modelverify.Ask) (modelverify.Response, error) {
		t.Fatal("不适用的协议不应发出任何请求")
		return modelverify.Response{}, nil
	}}
	sender := &protocolStub{inner: stub, proto: "openai-chat"}
	res := RunProbe(context.Background(), ProbeSignature, sender)
	if res.Err != nil {
		t.Fatalf("不适用不应记为错误: %v", res.Err)
	}
	if applicable, _ := res.Data["applicable"].(bool); applicable {
		t.Fatal("应标记不适用")
	}
	if findings := BuildReport([]Result{res}, "gpt-5").Findings; len(findings) != 0 {
		t.Fatalf("不适用不应产生风险项, 得到 %+v", findings)
	}
}

// 采集失败（上游错误）走 Err 通道，绝不产生风险项。
func TestRunSignatureHarvestErrorIsNotEvidence(t *testing.T) {
	stub := &signatureStub{respond: func(int, modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{}, errTest
	}}
	res := RunProbe(context.Background(), ProbeSignature, stub)
	if res.Err == nil {
		t.Fatal("应记录采集失败")
	}
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	if len(report.Findings) != 0 {
		t.Fatalf("采集失败不应产生风险项, 得到 %+v", report.Findings)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("应记录 1 条探针错误, 得到 %d", len(report.Errors))
	}
}

// 完全没有签名时如实记录，判定层给高危并要求复核。
func TestRunSignatureNoSignatureAtAll(t *testing.T) {
	stub := &signatureStub{respond: func(int, modelverify.Ask) (modelverify.Response, error) {
		return modelverify.Response{Content: "391", FinishReason: "end_turn"}, nil
	}}
	res := RunProbe(context.Background(), ProbeSignature, stub)
	if res.Err != nil {
		t.Fatalf("不应报错: %v", res.Err)
	}
	if present, _ := res.Data["signature_present"].(bool); present {
		t.Fatal("不应记为有签名")
	}
	// 只发了一次（采集），拿不到签名就没有回放的必要。
	if stub.calls != 1 {
		t.Fatalf("拿不到签名时不应继续回放, 实际请求 %d 次", stub.calls)
	}
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	if len(report.Findings) != 1 || report.Findings[0].Score != 50 {
		t.Fatalf("应产出 50 分高危项, 得到 %+v", report.Findings)
	}
}

// redacted 签名解封不出可读内容属预期，不该判成"回放失败"。
func TestRunSignatureRedactedIsNotReplayFailure(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, ask modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			return modelverify.Response{RedactedSignature: sig, FinishReason: "end_turn"}, nil
		}
		// 确认回放时带的是 redacted 形态。
		if len(ask.Turns) > 0 && !ask.Turns[0].Redacted {
			t.Error("redacted 签名回放时应标记 Redacted")
		}
		return modelverify.Response{Content: "", FinishReason: "end_turn"}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	if applicable, _ := res.Data["replay_applicable"].(bool); applicable {
		t.Fatal("redacted 签名不应走可解封判定")
	}
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	for _, f := range report.Findings {
		if f.Title == "签名回放无法解封" {
			t.Fatalf("redacted 签名不该判为无法解封: %+v", f)
		}
	}
}

// 模型名不符只作旁证，判低分。
func TestSignatureModelMismatchIsWeakSignal(t *testing.T) {
	sig := makeSignature("claude-haiku-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// 回放需要同块思考正文（F11：只回签名会被出站护栏丢弃）。
			return modelverify.Response{Signature: sig, ReasoningContent: "arithmetic", FinishReason: "end_turn",
				Usage: modelverify.Usage{Reasoning: 50, ReasoningReported: true}}, nil
		}
		return modelverify.Response{Content: "<cot>arithmetic</cot>", FinishReason: "end_turn"}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	report := BuildReport([]Result{res}, "claude-opus-4-8")
	var mismatch *Finding
	for i := range report.Findings {
		if report.Findings[i].Title == "签名绑定的模型名与请求模型不符" {
			mismatch = &report.Findings[i]
		}
	}
	if mismatch == nil {
		t.Fatalf("应产出模型名不符风险项, 得到 %+v", report.Findings)
	}
	if mismatch.Score != 15 || mismatch.Severity != SeverityLow {
		t.Fatalf("应为 15 分低危, 得到 %+v", mismatch)
	}
}

// 请求了思考、上游明确上报用量、但思考产出为 0，记轻微异常。
// （未上报该字段的渠道不产生这条——缺失是「没有数据」，不是异常。）
func TestSignatureZeroThinkingTokens(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// 回放需要同块思考正文（F11：只回签名会被出站护栏丢弃）。
			return modelverify.Response{Signature: sig, ReasoningContent: "x", FinishReason: "end_turn",
				Usage: modelverify.Usage{Reasoning: 0, ReasoningReported: true}}, nil
		}
		return modelverify.Response{Content: "<cot>x</cot>", FinishReason: "end_turn"}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	found := false
	for _, f := range report.Findings {
		if f.Title == "请求了扩展思考但思考 token 为 0" && f.Score == 10 {
			found = true
		}
	}
	if !found {
		t.Fatalf("应产出 10 分轻微异常, 得到 %+v", report.Findings)
	}
}

func TestExtractCOT(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<cot>hello</cot>", "hello"},
		{"prefix <cot> a\nb </cot> suffix", "a\nb"},
		{"<cot>first</cot> <cot>second</cot>", "first"},
		{"no tags here", ""},
		{"<cot>unclosed", ""},
	}
	for _, c := range cases {
		if got := extractCOT(c.in); got != c.want {
			t.Fatalf("extractCOT(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 普通签名但拿不到同块思考正文：无法构成合法无损回放（出站护栏会丢弃无
// 正文的 thinking 块）。按「证据不足」走 Err 通道：不发伪造回放、不扣解封
// 失败的分、也不计成有效验真。
func TestRunSignatureNoThinkingTextIsInsufficient(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// 有签名、无思考正文。
			return modelverify.Response{Signature: sig, FinishReason: "end_turn"}, nil
		}
		t.Error("证据不足时不应发出回放请求")
		return modelverify.Response{}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	if res.Err == nil {
		t.Fatal("应按证据不足记 Err")
	}
	if stub.calls != 1 {
		t.Fatalf("只应发一次采集请求, 实际 %d 次", stub.calls)
	}
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	if len(report.Findings) != 0 {
		t.Fatalf("证据不足不应产生风险项, 得到 %+v", report.Findings)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("应记录 1 条探针错误, 得到 %d", len(report.Errors))
	}
}

// 普通英文词不得命中平台痕迹词：裸子串匹配时 "draws"/"flaws" 含 "aws" 即
// 误报；整词边界下真实平台词仍命中（大小写不敏感、多词短语）。
func TestMatchSuspectWordBoundary(t *testing.T) {
	kws := []string{"kiro", "amazon q", "bedrock", "nova", "titan", "guardrails", "aws", "firewall"}

	if hits := matchSuspect(kws, "<cot>This draws on multiplication and points out its flaws.</cot>"); len(hits) != 0 {
		t.Fatalf("普通英文词不得命中痕迹词, 得到 %v", hits)
	}
	if hits := matchSuspect(kws, "The AWS firewall applies here."); len(hits) != 2 {
		t.Fatalf("真实平台词应命中 aws+firewall, 得到 %v", hits)
	}
	if hits := matchSuspect(kws, "Running on Bedrock via Amazon Q."); len(hits) != 2 {
		t.Fatalf("大小写不敏感与多词短语应命中, 得到 %v", hits)
	}
	if hits := matchSuspect(kws, "supernova and titanic are ordinary words"); len(hits) != 0 {
		t.Fatalf("词内片段不得命中, 得到 %v", hits)
	}
}

// 上游未上报思考用量字段：缺失是「没有数据」，不扣「思考 token 为 0」的分。
func TestSignatureMissingUsageNotScored(t *testing.T) {
	sig := makeSignature("claude-sonnet-4-5", 2, 256, false)
	stub := &signatureStub{respond: func(call int, _ modelverify.Ask) (modelverify.Response, error) {
		if call == 1 {
			// ReasoningReported=false：上游没报这个字段（Reasoning 的 0 只是默认值）。
			return modelverify.Response{Signature: sig, ReasoningContent: "17 * 23 = 391", FinishReason: "end_turn"}, nil
		}
		return modelverify.Response{Content: "<cot>17 * 23 = 391</cot>", FinishReason: "end_turn"}, nil
	}}

	res := RunProbe(context.Background(), ProbeSignature, stub)
	if _, present := res.Data["thinking_tokens"]; present {
		t.Fatal("未上报用量时不应记录 thinking_tokens 键")
	}
	report := BuildReport([]Result{res}, "claude-sonnet-4-5")
	for _, f := range report.Findings {
		if f.Title == "请求了扩展思考但思考 token 为 0" {
			t.Fatalf("缺失用量不得扣分, 得到 %+v", f)
		}
	}
}

// 自报不适用的探针不计 usable：只剩少数真正跑通的探针时，结论必须是
// 「没查清」而不是「全部查过没问题」。
func TestBuildReportNotApplicableNotUsable(t *testing.T) {
	notApplicable := Result{ProbeID: ProbeSignature, OK: true, Data: map[string]any{"applicable": false, "protocol": "openai-chat"}}
	usable := Result{ProbeID: ProbeLiveness, OK: true, Data: map[string]any{}}

	report := BuildReport([]Result{notApplicable, usable}, "gpt-5")
	if report.Verdict != VerdictUnknown {
		t.Fatalf("不适用探针不应计入 usable, 结论应为 unknown, 得到 %q", report.Verdict)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("不适用不应产生风险项, 得到 %+v", report.Findings)
	}
}
