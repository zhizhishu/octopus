package relay

import (
	"net/http"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// classifyFamilies 是测试侧的小包装：把结果拼成可比较的一行，空结果给 "<none>"。
func classifyFamilies(t *testing.T, h http.Header) string {
	t.Helper()
	families := dbmodel.ClassifyUpstreamHeaderFamilies(h)
	if len(families) == 0 {
		return "<none>"
	}
	joined := ""
	for i, f := range families {
		if i > 0 {
			joined += ","
		}
		joined += f
	}
	return joined
}

func TestClassifyUpstreamHeaderFamiliesMatchesEveryFamily(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amzn-RequestId", "abc")
	h.Set("X-Envoy-Upstream-Service-Time", "120")
	h.Set("Openai-Processing-Ms", "88")
	h.Set("Anthropic-Ratelimit-Requests-Limit", "50")
	h.Set("Cf-Ray", "8d0f-SJC")

	if got, want := classifyFamilies(t, h), "anthropic_ratelimit,aws,cloudflare,envoy,openai"; got != want {
		t.Fatalf("families = %q, want %q", got, want)
	}
}

// 名字大小写不该影响判定：http.Header 的键通常已规范化，但也可能被直接写进 map
// （非规范大小写），这里两种都钉住。
func TestClassifyUpstreamHeaderFamiliesIsCaseInsensitive(t *testing.T) {
	h := http.Header{}
	h["x-amz-meta-foo"] = []string{"v"}
	h["OPENAI-VERSION"] = []string{"v"}

	if got, want := classifyFamilies(t, h), "aws,openai"; got != want {
		t.Fatalf("families = %q, want %q", got, want)
	}
}

// 同一个家族命中多个头名时只记一次。
func TestClassifyUpstreamHeaderFamiliesDedupes(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amzn-RequestId", "a")
	h.Set("X-Amzn-Trace-Id", "b")
	h.Set("X-Amz-Cf-Id", "c")

	if got, want := classifyFamilies(t, h), "aws"; got != want {
		t.Fatalf("families = %q, want %q (aws must appear once)", got, want)
	}
}

// 输出必须是字典序（模型/聚合层会逐字节比对这个列表）。
func TestClassifyUpstreamHeaderFamiliesIsSorted(t *testing.T) {
	h := http.Header{}
	h.Set("Cf-Ray", "1")
	h.Set("Openai-Version", "2")
	h.Set("X-Amz-Meta-1", "3")

	if got, want := classifyFamilies(t, h), "aws,cloudflare,openai"; got != want {
		t.Fatalf("families = %q, want %q", got, want)
	}
}

// 只看头名不看值：值里出现家族名不算命中——否则任何转发一段文本头都会被误判。
func TestClassifyUpstreamHeaderFamiliesIgnoresValues(t *testing.T) {
	h := http.Header{}
	h.Set("X-Custom-Note", "x-amzn-trace-id=1; openai-version=2; cf-ray=3")

	if got := classifyFamilies(t, h); got != "<none>" {
		t.Fatalf("families = %q, want <none> (values must not be inspected)", got)
	}
}

// 非 Anthropic 渠道不采编号：其它出站适配器可能自己重写 id / tool call id，
// 拿它们当云端线索就是凭空造证据。
func TestCaptureUpstreamEnvelopeSkipsIDsForNonAnthropicChannel(t *testing.T) {
	ra := &relayAttempt{channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat}}
	signature := "claude#opaque"
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{
		ID: "req_vrtx_01",
		Choices: []model.Choice{{
			Delta: &model.Message{
				ReasoningSignature: &signature,
				ToolCalls:          []model.ToolCall{{ID: "tool_1"}},
			},
		}},
	})

	env := ra.upstreamEnvelope
	if env.ResponseID != "" || env.FirstToolCallID != "" || env.SignatureClaudeHash {
		t.Fatalf("non-anthropic channel must not capture ids/signature, got %+v", env)
	}
}

// Anthropic 出站路径：ID / tool call id 是上游原文，签名以 claude# 开头时只置布尔位。
func TestCaptureUpstreamEnvelopeCapturesAnthropicIDs(t *testing.T) {
	ra := &relayAttempt{channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}}
	signature := "claude#abc"
	// 首个非空胜出：第二片重复回显不该覆盖。
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{
		ID: "  ",
		Choices: []model.Choice{{
			Delta: &model.Message{ToolCalls: []model.ToolCall{{ID: ""}}},
		}},
	})
	if ra.upstreamEnvelope.ResponseID != "" || ra.upstreamEnvelope.FirstToolCallID != "" {
		t.Fatalf("empty ids must not become values, got %+v", ra.upstreamEnvelope)
	}

	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{
		ID: "req_vrtx_01abc",
		Choices: []model.Choice{{
			Message: &model.Message{
				ReasoningSignature: &signature,
				ToolCalls:          []model.ToolCall{{ID: "tooluse_01xyz"}},
			},
		}},
	})
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{
		ID: "req_vrtx_second",
		Choices: []model.Choice{{
			Message: &model.Message{ToolCalls: []model.ToolCall{{ID: "tool_second"}}},
		}},
	})

	env := ra.upstreamEnvelope
	if env.ResponseID != "req_vrtx_01abc" {
		t.Fatalf("ResponseID = %q, want first non-empty", env.ResponseID)
	}
	if env.FirstToolCallID != "tooluse_01xyz" {
		t.Fatalf("FirstToolCallID = %q, want first non-empty", env.FirstToolCallID)
	}
	if !env.SignatureClaudeHash {
		t.Fatal("SignatureClaudeHash = false, want true for a claude# signature")
	}
}

// 签名不是 claude# 前缀时不能置位（只认形态，不认"有签名"这件事本身）。
func TestCaptureUpstreamEnvelopeSignatureNeedsClaudeHashPrefix(t *testing.T) {
	ra := &relayAttempt{channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}}
	other := "EqQBCkgIAxABGAIiQL"
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{
		Choices: []model.Choice{{Message: &model.Message{ReasoningSignature: &other}}},
	})
	if ra.upstreamEnvelope.SignatureClaudeHash {
		t.Fatal("SignatureClaudeHash = true for a non-claude# signature")
	}
}

// 流内换模型：同名重复不算换；不同名只写一次；每条新流从零开始判。
func TestCaptureUpstreamEnvelopeModelSwitchIsPerStream(t *testing.T) {
	ra := &relayAttempt{channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}}

	// 同一模型名重复回显（流式分片常态）：不算换。
	for i := 0; i < 3; i++ {
		ra.captureUpstreamEnvelope(&model.InternalLLMResponse{Model: "claude-opus-4-8"})
	}
	if ra.upstreamEnvelope.ModelSwitchedTo != "" {
		t.Fatalf("ModelSwitchedTo = %q, want empty for a stable stream", ra.upstreamEnvelope.ModelSwitchedTo)
	}

	// 大小写/空白差异也不算换（上游分片偶尔在名字里塞空格）。
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{Model: "Claude-Opus-4-8 "})
	if ra.upstreamEnvelope.ModelSwitchedTo != "" {
		t.Fatalf("ModelSwitchedTo = %q, want empty for a case/space-only difference", ra.upstreamEnvelope.ModelSwitchedTo)
	}

	// 真换了：记下第二个名字。
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{Model: "claude-haiku-4-5"})
	if ra.upstreamEnvelope.ModelSwitchedTo != "claude-haiku-4-5" {
		t.Fatalf("ModelSwitchedTo = %q, want the second name", ra.upstreamEnvelope.ModelSwitchedTo)
	}

	// 只写一次：第三个名字不覆盖。
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{Model: "claude-sonnet-4-6"})
	if ra.upstreamEnvelope.ModelSwitchedTo != "claude-haiku-4-5" {
		t.Fatalf("ModelSwitchedTo = %q, want the first switch only", ra.upstreamEnvelope.ModelSwitchedTo)
	}

	// 新的一条流（四个响应入口开头都调这个）：同一个"不同名字"不再算换。
	ra.resetResponseObservationForNewResponse()
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{Model: "claude-opus-4-8"})
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{Model: "claude-opus-4-8"})
	if ra.upstreamEnvelope.ModelSwitchedTo != "" {
		t.Fatalf("ModelSwitchedTo = %q, want empty after a per-stream reset", ra.upstreamEnvelope.ModelSwitchedTo)
	}
}

// 同一个 attempt 里 goto 重试换适配器会收到第二份回复：上一份的编号/签名不能串进来，
// 但这一份回复 2xx 时已写好的响应头类别要保留。
func TestResetUpstreamEnvelopeDropsPreviousResponseButKeepsHeaders(t *testing.T) {
	ra := &relayAttempt{channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}}
	sig := "claude#abc"
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{
		ID:    "req_vrtx_first",
		Model: "claude-opus-4-8",
		Choices: []model.Choice{{Message: &model.Message{
			ReasoningSignature: &sig,
			ToolCalls:          []model.ToolCall{{ID: "tool_1"}},
		}}},
	})
	ra.upstreamEnvelope.HeaderFamilies = []string{"aws"}

	ra.resetResponseObservationForNewResponse()
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{ID: "msg_second", Model: "claude-opus-4-8"})

	env := ra.upstreamEnvelope
	if env.ResponseID != "msg_second" {
		t.Fatalf("ResponseID = %q, want the second response's id (no carry-over)", env.ResponseID)
	}
	if env.FirstToolCallID != "" || env.SignatureClaudeHash {
		t.Fatalf("tool id / signature carried over from the previous response: %+v", env)
	}
	if len(env.HeaderFamilies) != 1 || env.HeaderFamilies[0] != "aws" {
		t.Fatalf("HeaderFamilies = %v, want kept (written at 2xx before the handler runs)", env.HeaderFamilies)
	}
}

// Gemini 出站只把自报名写进审计字段 UpstreamDeclaredModel、不写 Model：换模型判断要用同一口径。
func TestCaptureUpstreamEnvelopeUsesAuditDeclaredModel(t *testing.T) {
	ra := &relayAttempt{channel: &dbmodel.Channel{Type: outbound.OutboundTypeGemini}}
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{UpstreamDeclaredModel: "gemini-3-pro", Model: "client-name"})
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{UpstreamDeclaredModel: "gemini-3-pro", Model: "client-name"})
	if ra.upstreamEnvelope.ModelSwitchedTo != "" {
		t.Fatalf("ModelSwitchedTo = %q, want empty when the audit name is stable", ra.upstreamEnvelope.ModelSwitchedTo)
	}
	ra.captureUpstreamEnvelope(&model.InternalLLMResponse{UpstreamDeclaredModel: "gemini-2-flash", Model: "client-name"})
	if ra.upstreamEnvelope.ModelSwitchedTo != "gemini-2-flash" {
		t.Fatalf("ModelSwitchedTo = %q, want gemini-2-flash", ra.upstreamEnvelope.ModelSwitchedTo)
	}
}

// 同一个 attempt 里 goto 重试会收到第二份回复：上一份回复盖的首字时间必须清掉，
// 否则后一份回复的 IsZero 守卫永远不成立，ftut 从输的那一次借来、output_tokens
// 算赢的这一份——“生成窗口”横跨两次尝试（2026-11-06 生产实证的污染源）。
func TestResetResponseObservationClearsStaleFirstTokenTime(t *testing.T) {
	metrics := NewRelayMetrics(1, 1, "127.0.0.1", "m", nil)
	first := time.Now().Add(-3 * time.Second)
	metrics.SetFirstTokenTime(first)
	ra := &relayAttempt{
		relayRequest: &relayRequest{metrics: metrics},
		channel:      &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}
	ra.upstreamEnvelope.HeaderFamilies = []string{"anthropic"}

	ra.resetResponseObservationForNewResponse()

	if !metrics.FirstTokenTime.IsZero() {
		t.Fatalf("FirstTokenTime = %v, want cleared so the new response stamps its own", metrics.FirstTokenTime)
	}
	if len(ra.upstreamEnvelope.HeaderFamilies) != 1 || ra.upstreamEnvelope.HeaderFamilies[0] != "anthropic" {
		t.Fatalf("HeaderFamilies = %v, want kept (written at 2xx before the handler runs)", ra.upstreamEnvelope.HeaderFamilies)
	}
	// 盖新章不能被旧值拦住：新回复的写入直接生效。
	second := time.Now()
	ra.setFirstTokenTime(second)
	if !metrics.FirstTokenTime.Equal(second) {
		t.Fatalf("FirstTokenTime = %v, want the second response's stamp %v", metrics.FirstTokenTime, second)
	}
}

// 没有 metrics（纯观测关掉的路径）时清场不得 panic。
func TestResetResponseObservationToleratesNilMetrics(t *testing.T) {
	ra := &relayAttempt{relayRequest: &relayRequest{}}
	ra.upstreamEnvelope.HeaderFamilies = []string{"anthropic"}
	ra.streamFirstDeclaredModel = "leftover"
	ra.resetResponseObservationForNewResponse()
	if ra.streamFirstDeclaredModel != "" {
		t.Fatalf("streamFirstDeclaredModel = %q, want empty", ra.streamFirstDeclaredModel)
	}
	if len(ra.upstreamEnvelope.HeaderFamilies) != 1 {
		t.Fatalf("HeaderFamilies = %v, want kept", ra.upstreamEnvelope.HeaderFamilies)
	}
}
