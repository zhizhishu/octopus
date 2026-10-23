package authropic

import (
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// 出站 `output_config.effort` 是 Anthropic 的**枚举**字段。
//
// 【为什么这条测试存在】adaptive 分支（messages.go 的 `req.AdaptiveThinking`）
// 里是这么写的：
//
//	result.OutputConfig = &anthropicModel.OutputConfig{Effort: req.ReasoningEffort}
//
// 也就是把客户端给的值**原样**写进去，一个字符都不校验。而 effort 在 Anthropic
// 侧是枚举，收到不认识的值整条请求 400。于是任何自造词（`ultra`、`turbo`、
// `EXTREME`）都会把一次本来能成的请求打成 400 —— 而且报错来自上游，
// 排查时看着像"上游抽风"，不像我们的 bug。
//
// 更麻烦的是大小写：`HIGH` / `High` 语义上完全合法，原样发出去却可能被枚举挡掉。
//
// 这条测试钉三件事：
//  1. 合法档位（含 xhigh/max 与各种拼法）必须原样或规范化后发出正确值；
//  2. 不认识的值**不许**原样发给上游（否则等于拿用户的请求去撞 400）；
//  3. format/Extra 不能被 effort 的清洗顺手丢掉（结构化输出是独立意图）。
func adaptiveWireEffort(t *testing.T, effort string) (string, *anthropicOutputWire) {
	t.Helper()
	maxTok := int64(200000)
	out := convertToAnthropicRequest(&model.InternalLLMRequest{
		Model:            "claude-sonnet-4-5",
		ReasoningEffort:  effort,
		AdaptiveThinking: true,
		MaxTokens:        &maxTok,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("hi")}},
		},
	})
	if out.OutputConfig == nil {
		return "", nil
	}
	raw, err := json.Marshal(out.OutputConfig)
	if err != nil {
		t.Fatalf("effort %q: marshal output_config: %v", effort, err)
	}
	var wire anthropicOutputWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("effort %q: unmarshal output_config: %v", effort, err)
	}
	return wire.Effort, &wire
}

type anthropicOutputWire struct {
	Effort string          `json:"effort"`
	Format json.RawMessage `json:"format"`
}

// 合法档位必须正确落到线上。xhigh 与 max 是 Anthropic ≥2.1.28x 认的强档。
func TestAdaptiveEffortLegalValuesOnTheWire(t *testing.T) {
	cases := map[string]string{
		"low":        "low",
		"medium":     "medium",
		"high":       "high",
		"xhigh":      "xhigh",
		"max":        "max",
		"LOW":        "low",
		" High ":     "high",
		"XHIGH":      "xhigh",
		"MAX":        "max",
		"x-high":     "xhigh",
		"x_high":     "xhigh",
		"extra-high": "xhigh",
	}
	for in, want := range cases {
		got, _ := adaptiveWireEffort(t, in)
		if got != want {
			t.Errorf("effort %q: 线上 effort = %q, want %q", in, got, want)
		}
	}
}

// 不认识的值不许原样发给上游。发出去就是拿用户的请求去撞 400。
func TestAdaptiveEffortUnknownValueIsNotForwardedVerbatim(t *testing.T) {
	for _, in := range []string{"ultra", "turbo", "EXTREME", "ultra-max", "highish", "1"} {
		got, wire := adaptiveWireEffort(t, in)
		if wire == nil {
			continue // 整段没发也算安全
		}
		if got == in {
			t.Errorf("effort %q: 被原样发给了上游。Anthropic 的 output_config.effort 是枚举，"+
				"不认识的值会让整条请求 400，而且报错来自上游、排查时看不出是我们的锅", in)
		}
	}
}

// effort 的清洗不能把结构化输出的 format 丢掉（两件事住在同一个对象里）。
func TestAdaptiveEffortCleanupKeepsFormat(t *testing.T) {
	maxTok := int64(200000)
	out := convertToAnthropicRequest(&model.InternalLLMRequest{
		Model:            "claude-sonnet-4-5",
		ReasoningEffort:  "ultra", // 不认识的值
		AdaptiveThinking: true,
		MaxTokens:        &maxTok,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("hi")}},
		},
		ResponseFormat: &model.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: json.RawMessage(`{"name":"t","schema":{"type":"object"}}`),
		},
	})
	if out.OutputConfig == nil {
		t.Fatal("结构化输出被整段丢掉了：format 与 effort 住在同一个对象里，清 effort 不能连 format 一起清")
	}
	if len(out.OutputConfig.Format) == 0 {
		t.Fatalf("format 被清掉了（effort=%q）：结构化输出是独立意图，不该受思考档位影响",
			out.OutputConfig.Effort)
	}
}
