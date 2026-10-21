package openai

import (
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// 这条 chat 路径是国产模型的主干道：DeepSeek / Qwen / Kimi / MiniMax /
// 百川 / 混元 / 阶跃全走这里。
//
// 改动前这里是一行 `request.ReasoningEffort = ""` 到底，理由是防严格上游
// 400。防 400 是真的，但代价是所有国产模型永远收不到强度——客户端要 max，
// 上游看到一个没有任何强度语义的裸请求。
func TestDeepSeekGetsBothThinkingAndEffort(t *testing.T) {
	req := &model.InternalLLMRequest{Model: "deepseek-v4-pro", ReasoningEffort: "max"}
	applyReasoningProjection(req)

	if string(req.Thinking) != `{"type":"enabled"}` {
		t.Errorf("DeepSeek 应收到 thinking=enabled，实际 %q", req.Thinking)
	}
	if req.ReasoningEffort != "max" {
		t.Errorf("DeepSeek 应收到原始档位 max，实际 %q", req.ReasoningEffort)
	}
}

// 出站必须发**归一后的原始档位**，不是换算值。
// new-api 的 deepSeekRunEffort 只写日志/计费（adaptor.go:142），不落 wire；
// 把换算值发上线会和计费口径打架。
func TestDeepSeekSendsOriginalLevelNotConverted(t *testing.T) {
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		req := &model.InternalLLMRequest{Model: "deepseek-v4-pro", ReasoningEffort: level}
		applyReasoningProjection(req)
		if req.ReasoningEffort != level {
			t.Errorf("档位 %q 应原样出站，实际被改成 %q", level, req.ReasoningEffort)
		}
	}
}

func TestDeepSeekOffMeansDisabled(t *testing.T) {
	for _, level := range []string{"none", "minimal", " NONE "} {
		req := &model.InternalLLMRequest{Model: "deepseek-v4-pro", ReasoningEffort: level}
		applyReasoningProjection(req)
		if string(req.Thinking) != `{"type":"disabled"}` {
			t.Errorf("档位 %q 应关闭思考，实际 thinking=%q", level, req.Thinking)
		}
		if req.ReasoningEffort != "none" {
			t.Errorf("档位 %q 应把 effort 置 none，实际 %q", level, req.ReasoningEffort)
		}
	}
}

// Qwen 用布尔开关 + 顶层预算，且不认 reasoning_effort。
func TestQwenGetsBooleanAndBudget(t *testing.T) {
	req := &model.InternalLLMRequest{Model: "qwen3.8-max", ReasoningEffort: "high"}
	applyReasoningProjection(req)

	if req.EnableThinking == nil || !*req.EnableThinking {
		t.Fatalf("Qwen 应收到 enable_thinking=true，实际 %v", req.EnableThinking)
	}
	if req.ThinkingBudget == nil {
		t.Fatal("Qwen 应收到 thinking_budget")
	}
	if got, want := *req.ThinkingBudget, model.EffortBudget["high"]; got != want {
		t.Errorf("thinking_budget 应为 %d，实际 %d", want, got)
	}
	if req.ReasoningEffort != "" {
		t.Errorf("Qwen 不认 reasoning_effort，应清空，实际 %q", req.ReasoningEffort)
	}

	off := &model.InternalLLMRequest{Model: "qwen3.8-max", ReasoningEffort: "none"}
	applyReasoningProjection(off)
	if off.EnableThinking == nil || *off.EnableThinking {
		t.Errorf("档位 none 时 Qwen 应收到 enable_thinking=false，实际 %v", off.EnableThinking)
	}
}

// thinking_budget 发给非 Qwen 上游是立即 400（new-api 按模型族剥离，
// relaykit/dto/openai_request.go:139-141）。这条锁住我们不会漏清。
func TestThinkingBudgetNeverLeaksToNonQwen(t *testing.T) {
	leak := int64(32000)
	for _, m := range []string{"deepseek-v4-pro", "kimi-k3", "glm-5.3", "some-random-model"} {
		req := &model.InternalLLMRequest{
			Model:           m,
			ReasoningEffort: "high",
			ThinkingBudget:  &leak,
		}
		applyReasoningProjection(req)
		if req.ThinkingBudget != nil {
			t.Errorf("%s 不该带 thinking_budget，实际 %d", m, *req.ThinkingBudget)
		}
	}
}

// 老一代"自带思考"的模型不接受强度参数，也无所谓开关——它们永远思考。
// 清空是正确行为，不是缺陷。参考项目同样只对 deepseek-v4-* / deepseek-flash
// 做转换（new-api suffix.go:283-285 把 base 限定在这两族）。
func TestLegacyDeepSeekReasonerStaysUntouched(t *testing.T) {
	for _, m := range []string{"deepseek-reasoner", "deepseek-r1", "deepseek-r2", "deepseek-v4-thinking"} {
		req := &model.InternalLLMRequest{Model: m, ReasoningEffort: "high"}
		applyReasoningProjection(req)
		if req.ReasoningEffort != "" {
			t.Errorf("%s 是自带思考的老模型，不该收到 effort，实际 %q", m, req.ReasoningEffort)
		}
		if req.Thinking != nil {
			t.Errorf("%s 不该收到 thinking 开关，实际 %q", m, req.Thinking)
		}
	}
}

// Kimi 的 thinking 自带 effort 成员。
func TestKimiEmbedsEffortInThinking(t *testing.T) {
	req := &model.InternalLLMRequest{Model: "kimi-k3", ReasoningEffort: "xhigh"}
	applyReasoningProjection(req)

	var got map[string]string
	if err := json.Unmarshal(req.Thinking, &got); err != nil {
		t.Fatalf("Kimi thinking 应是合法 JSON，实际 %q", req.Thinking)
	}
	if got["type"] != "enabled" || got["effort"] != "xhigh" {
		t.Errorf("Kimi thinking 应为 enabled+xhigh，实际 %v", got)
	}
	if req.ReasoningEffort != "" {
		t.Errorf("Kimi 不用顶层 reasoning_effort，应清空，实际 %q", req.ReasoningEffort)
	}
}

// 未知上游一律剥离：不许发出去让上游 400。
// 这是当初那行无条件清空想解决的问题，现在只对真正未知的生效。
func TestUnknownUpstreamStripsEverything(t *testing.T) {
	b := int64(8000)
	req := &model.InternalLLMRequest{
		Model:           "totally-unknown-model",
		ReasoningEffort: "max",
		ThinkingBudget:  &b,
	}
	applyReasoningProjection(req)

	if req.ReasoningEffort != "" {
		t.Errorf("未知上游应清空 reasoning_effort，实际 %q", req.ReasoningEffort)
	}
	if req.EnableThinking != nil {
		t.Errorf("未知上游应清空 enable_thinking，实际 %v", req.EnableThinking)
	}
	if req.ThinkingBudget != nil {
		t.Errorf("未知上游应清空 thinking_budget，实际 %d", *req.ThinkingBudget)
	}
}

// GLM 走自己的通道，这里只负责清掉双字段。
func TestGLMOnlyClearsEffortHere(t *testing.T) {
	req := &model.InternalLLMRequest{Model: "glm-5.3", ReasoningEffort: "max"}
	applyReasoningProjection(req)
	if req.ReasoningEffort != "" {
		t.Errorf("GLM 的双字段应被清掉，实际 %q", req.ReasoningEffort)
	}
}

// 🔴 端到端：必须验证投影真的落到了**最终出站报文**上，而不是只验证
// applyReasoningProjection 这个函数本身。
//
// 这是被变异测试抓出来的真实教训：把 TransformRequest 里的
// applyReasoningProjection(request) 换成 request.ReasoningEffort = ""，
// 上面那些直接调函数的测试**全部照常通过**——因为它们测的是函数，
// 不是管线。只测函数等于没测接线。
func TestReasoningProjectionReachesOutboundBody(t *testing.T) {
	cases := []struct {
		model       string
		mustHave    []string
		mustNotHave []string
	}{
		{"deepseek-v4-pro", []string{"thinking", "reasoning_effort"}, []string{"thinking_budget", "enable_thinking"}},
		{"qwen3.8-max", []string{"enable_thinking", "thinking_budget"}, []string{"reasoning_effort"}},
		{"kimi-k3", []string{"thinking"}, []string{"reasoning_effort", "thinking_budget"}},
		{"glm-5.3", []string{"thinking"}, []string{"reasoning_effort", "thinking_budget"}},
	}
	for _, c := range cases {
		body := chatRequestBody(t, &model.InternalLLMRequest{
			Model:           c.model,
			ReasoningEffort: "max",
			Messages:        userMessages(),
		})
		for _, k := range c.mustHave {
			if _, ok := body[k]; !ok {
				t.Errorf("%s 的出站报文应含 %q，实际 %v", c.model, k, keysOf(body))
			}
		}
		for _, k := range c.mustNotHave {
			if _, ok := body[k]; ok {
				t.Errorf("%s 的出站报文不该含 %q，实际 %v", c.model, k, keysOf(body))
			}
		}
	}
}

// 未知上游的报文里不许出现任何思考字段——这是当初那行无条件清空要防的 400。
func TestUnknownUpstreamBodyHasNoReasoningFields(t *testing.T) {
	body := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "totally-unknown-model",
		ReasoningEffort: "max",
		Messages:        userMessages(),
	})
	for _, k := range []string{"reasoning_effort", "thinking", "enable_thinking", "thinking_budget"} {
		if _, ok := body[k]; ok {
			t.Errorf("未知上游报文不该含 %q，实际 %v", k, keysOf(body))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 渠道前缀不能把模型族判定漏掉——同一个模型经不同中转，模型名前面会挂
// 不同的 provider/路径前缀。判定方式与参考项目对齐
// （new-api relaykit/dto/openai_request.go:389-395 用 Contains("/qwen")）。
func TestModelFamilyDetectionSurvivesChannelPrefix(t *testing.T) {
	qwen := []string{"qwen3.8-max", "Qwen3-Max", "qwq-32b", "ali/qwen3-max",
		"dashscope/qwen-plus", "openrouter/qwen/qwen3"}
	for _, m := range qwen {
		if !isQwenModel(m) {
			t.Errorf("%q 应识别为 Qwen 族", m)
		}
	}
	kimi := []string{"kimi-k3", "Kimi-K3", "moonshot-v1-8k", "pro/kimi-k3"}
	for _, m := range kimi {
		if !isKimiModel(m) {
			t.Errorf("%q 应识别为 Kimi 族", m)
		}
	}
	deepseek := []string{"deepseek-v4-pro", "openrouter/deepseek-v4-flash", "x/deepseek-flash"}
	for _, m := range deepseek {
		if !isDeepSeekThinkingCapable(m) {
			t.Errorf("%q 应识别为支持思考开关的 DeepSeek", m)
		}
	}
	// 带上渠道前缀后，投影仍要落到出站报文上，而不只是识别函数为真。
	body := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "openrouter/qwen/qwen3-max",
		ReasoningEffort: "high",
		Messages:        userMessages(),
	})
	if _, ok := body["enable_thinking"]; !ok {
		t.Errorf("带前缀的 Qwen 仍应发出 enable_thinking，实际 %v", keysOf(body))
	}
}

// 客户端直接发 enable_thinking 时必须尊重它，不能用自己的推导覆盖。
//
// 实测过这个坑：客户端只发 {"enable_thinking":true} 而不发档位时，早先的
// 实现会算出 enabled=false 把它按关掉处理——客户端明确要开思考，被我们
// 自作聪明关掉了。这和"静默降级"是同一件事的两面。
func TestClientProvidedEnableThinkingIsRespected(t *testing.T) {
	yes := true
	body := chatRequestBody(t, &model.InternalLLMRequest{
		Model:          "qwen3-max",
		EnableThinking: &yes,
		Messages:       userMessages(),
	})
	if got, ok := body["enable_thinking"].(bool); !ok || !got {
		t.Fatalf("客户端明确要求开思考，出站必须仍是 true，实际 %v", body["enable_thinking"])
	}
	// 没给预算时应补一个，而不是留空让上游按默认走。
	if _, ok := body["thinking_budget"]; !ok {
		t.Errorf("客户端要开思考但没给预算时，应补上默认预算，实际 %v", keysOf(body))
	}

	no := false
	body2 := chatRequestBody(t, &model.InternalLLMRequest{
		Model:          "qwen3-max",
		EnableThinking: &no,
		Messages:       userMessages(),
	})
	if got, ok := body2["enable_thinking"].(bool); !ok || got {
		t.Fatalf("客户端明确要求关思考，出站必须仍是 false，实际 %v", body2["enable_thinking"])
	}
	if _, ok := body2["thinking_budget"]; ok {
		t.Errorf("关思考时不该带预算，实际 %v", keysOf(body2))
	}
}

// 客户端没表态时，才允许由档位推导开关。
func TestEnableThinkingDerivedOnlyWhenClientSilent(t *testing.T) {
	on := chatRequestBody(t, &model.InternalLLMRequest{
		Model: "qwen3-max", ReasoningEffort: "high", Messages: userMessages(),
	})
	if got, ok := on["enable_thinking"].(bool); !ok || !got {
		t.Errorf("档位 high 且客户端未表态时，应推导为开启，实际 %v", on["enable_thinking"])
	}
	off := chatRequestBody(t, &model.InternalLLMRequest{
		Model: "qwen3-max", ReasoningEffort: "none", Messages: userMessages(),
	})
	if got, ok := off["enable_thinking"].(bool); !ok || got {
		t.Errorf("档位 none 且客户端未表态时，应推导为关闭，实际 %v", off["enable_thinking"])
	}
}
