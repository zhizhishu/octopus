package op

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/tokenizer"
)

// 本文件的测试钉死 2026-10-27 生产误报修复（工具调用/推理/媒体/折叠分母）。
// 每条「不报」的用例都对应一类真实误报形态；变异自检（删掉对应修复）
// 必须能让这些用例红出来。

// toolCallBody 造一个"短正文 + 一条工具调用"的响应——智能体流量的典型形状。
func toolCallBody(content, toolArgs string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": toolArgs,
					},
				}},
			},
		}},
	})
	return string(b)
}

// TestRelayProbeCharTokenRatioCountsToolCallArguments 守生产那条误报：
// 13 字正文 + 一条 bash 命令 = 120 output token。只数正文会算出 0.11 字/token
// 报"灌水"；把工具参数也数进来，比值落回正常区间，不该报。
func TestRelayProbeCharTokenRatioCountsToolCallArguments(t *testing.T) {
	// 工具参数约 300 字符：(13+300)/120 ≈ 2.6，落在 1.2–10 区间。
	args := `{"command": "cd /some/project && agent-browser-cli exec --tab 1184438179 ` +
		`--file _eval/poll_task.js 2>&1 | python -c \"import json,sys; d=json.load(sys.stdin); ` +
		`print(d.get('result',{}).get('js_return') or json.dumps(d)[:600])\"", "timeoutMs": 150000}`
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "kimi-k3",
		InputTokens:     1000,
		OutputTokens:    120,
		TotalTokens:     1120,
		ResponseContent: toolCallBody("全绿。查基线任务出分没有。", args),
	})
	if f := findCode(res, model.ProbeCodeCharTokenRatio); f != nil {
		t.Fatalf("工具调用回复不该触发字符/token 报警，实际 %s", f.Title)
	}
}

// TestRelayProbeCharTokenRatioSkipsLowSideWithReasoning 守方向感知：
// 推理型请求（reasoning_effort）下，不可见推理的质量没有上界，低位比值
// （疑似灌水）不可判——整条跳过。
func TestRelayProbeCharTokenRatioSkipsLowSideWithReasoning(t *testing.T) {
	req := `{"model":"kimi-k3","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "kimi-k3",
		InputTokens:     100,
		OutputTokens:    120,
		TotalTokens:     220,
		RequestContent:  req,
		ResponseContent: chatBody("短回复"),
	})
	if f := findCode(res, model.ProbeCodeCharTokenRatio); f != nil {
		t.Fatalf("推理型请求的低位比值不该报，实际 %s", f.Title)
	}
}

// TestRelayProbeCharTokenRatioStillFlagsHighSideWithReasoning 高位越界
// （字符远超 token）推理解释不了——推理只会让分母变大、比值变小。
// 所以即使有 reasoning_effort 也必须照报。
func TestRelayProbeCharTokenRatioStillFlagsHighSideWithReasoning(t *testing.T) {
	req := `{"model":"kimi-k3","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	longText := strings.Repeat("这是一段相当长的输出内容，用来把字符/token 比值顶到上界之外。", 30)
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "kimi-k3",
		InputTokens:     100,
		OutputTokens:    30, // 近 2000 字 / 30 token ≈ 60+，远超上限 10
		TotalTokens:     130,
		RequestContent:  req,
		ResponseContent: chatBody(longText),
	})
	f := findCode(res, model.ProbeCodeCharTokenRatio)
	if f == nil {
		t.Fatal("高位越界即使有推理也必须报出来")
	}
}

// TestRelayProbeCharTokenRatioCountsAllChoices 守多 choice 求和：
// output_tokens 是全部 choice 之和，分子只数第一个会让比值必然偏低。
func TestRelayProbeCharTokenRatioCountsAllChoices(t *testing.T) {
	body := `{"choices":[` +
		`{"finish_reason":"stop","message":{"role":"assistant","content":"` + strings.Repeat("甲", 800) + `"}},` +
		`{"finish_reason":"stop","message":{"role":"assistant","content":"` + strings.Repeat("乙", 800) + `"}}]}`
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "gpt-5.6",
		InputTokens:     100,
		OutputTokens:    100, // 全量 1600/100=16 > 10 触发；只数第一个=8 落区间内
		TotalTokens:     200,
		ResponseContent: body,
	})
	f := findCode(res, model.ProbeCodeCharTokenRatio)
	if f == nil {
		t.Fatal("多 choice 时分子必须求和，否则高位越界被漏掉")
	}
	if f.Confidence != model.RelayProbeConfidenceMedium {
		t.Fatalf("多 choice 应降 medium 置信，实际 %q", f.Confidence)
	}
}

// TestRelayProbeTokenizerResidualCountsToolCallsAndTools 守输入复算同源：
// 智能体请求的大头在历史 tool_calls 参数与顶层 tools——只数 message 文本
// 会系统性低估（生产实证 -43%），数全后上游报值落在容差内，不该报。
func TestRelayProbeTokenizerResidualCountsToolCallsAndTools(t *testing.T) {
	var messages []any
	// 造 200 条 assistant 工具调用历史，参数是智能体流量的大头。
	for i := 0; i < 200; i++ {
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": fmt.Sprintf("第 %d 步", i),
			"tool_calls": []any{map[string]any{
				"function": map[string]any{
					"name":      "bash",
					"arguments": fmt.Sprintf(`{"command": "echo step-%d && ls -la /some/dir && cat README.md | head -50"}`, i),
				},
			}},
		})
	}
	tools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "bash",
			"description": strings.Repeat("Run a bash command in the workspace and return its output. ", 40),
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string", "description": strings.Repeat("the command to run ", 30)}}},
		},
	}}
	req, _ := json.Marshal(map[string]any{"model": "kimi-k3", "messages": messages, "tools": tools})
	full, meta := requestBillableText(string(req))
	if !meta.hasTools || !meta.toolArgsCounted {
		t.Fatalf("复算应数到 tools 与工具参数，meta=%+v", meta)
	}
	local := localCountFromText(t, full)
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "kimi-k3",
		InputTokens:     local + local/10, // 上游报值偏 10%，在 35% 容差内
		OutputTokens:    20,
		TotalTokens:     local + local/10 + 20,
		RequestContent:  string(req),
		ResponseContent: chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeTokenizerResidual); f != nil {
		t.Fatalf("数全后偏差在容差内不该报，实际 %s（diff=%v）", f.Title, f.Evidence["relative_diff"])
	}
}

// localCountFromText 在测试里复算本地分词器结果，避免把魔法数字写进测试。
func localCountFromText(t *testing.T, text string) int {
	t.Helper()
	n := tokenizer.CountTokens(text, "kimi-k3")
	if n <= 0 {
		t.Fatalf("本地分词器应能算出正数，得到 %d", n)
	}
	return n
}

// TestRelayProbeTokenizerResidualSkipsLocalEstimate 守同源门槛：
// UsageSource=local_estimate 的数字本来就是我们用同一个分词器估的，
// 再复算比对等于自己核对自己——整条跳过，偏差多大都不报。
func TestRelayProbeTokenizerResidualSkipsLocalEstimate(t *testing.T) {
	prompt := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "claude-opus-4-6",
		UsageSource:     model.RelayLogUsageSourceLocalEstimate,
		InputTokens:     6000, // 故意 3 倍偏差
		OutputTokens:    20,
		TotalTokens:     6020,
		RequestContent:  reqBody(prompt),
		ResponseContent: chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeTokenizerResidual); f != nil {
		t.Fatalf("本地估算的数字不该参与残差判定，实际 %s", f.Title)
	}
}

// TestRelayProbeTokenizerResidualSkipsMediaParts 守不可计算即跳过：
// 图片占多少 token 无法从存储正文恢复，不猜——整条跳过，不用低置信度硬报。
func TestRelayProbeTokenizerResidualSkipsMediaParts(t *testing.T) {
	req := `{"model":"gpt-5.6","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"` + strings.Repeat("describe this image in detail ", 100) + `"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "gpt-5.6",
		InputTokens:     90000, // 故意造 3 倍以上偏差
		OutputTokens:    20,
		TotalTokens:     90020,
		RequestContent:  req,
		ResponseContent: chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeTokenizerResidual); f != nil {
		t.Fatalf("含图片部件的请求应整条跳过，实际 %s", f.Title)
	}
}

// TestRelayProbeTokenizerResidualUsesFoldedDenominator 守 Anthropic 折叠分母：
// 独占口径 input_tokens=12 只是非缓存部分，真实输入是折叠后的总量。
// 拿 12 当分母会把每条带缓存的 Anthropic 请求报成 +300000% 假残差。
func TestRelayProbeTokenizerResidualUsesFoldedDenominator(t *testing.T) {
	prompt := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	local := localCount(t, reqBody(prompt))
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:         "claude-opus-4-6",
		InputTokens:            1000, // Anthropic 独占口径：非缓存部分（缓存才是大头）
		CacheFoldedInputTokens: local + local/10,
		OutputTokens:           20,
		TotalTokens:            local + local/10 + 20,
		RequestContent:         reqBody(prompt),
		ResponseContent:        chatBody("ok"),
	})
	if f := findCode(res, model.ProbeCodeTokenizerResidual); f != nil {
		t.Fatalf("折叠分母下偏差在容差内不该报，实际 %s（diff=%v）", f.Title, f.Evidence["relative_diff"])
	}
}

// TestRelayProbeTokenizerResidualConfidenceFactors 守置信度：
// 有 tools 已计入（服务商模板包装不可算）+ 非 o200k 家族（本地分词器不同族）
// → medium，且因子机器可读。
func TestRelayProbeTokenizerResidualConfidenceFactors(t *testing.T) {
	var messages []any
	messages = append(messages, map[string]any{"role": "user", "content": strings.Repeat("check the workspace state ", 100)})
	tools := []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "bash", "description": strings.Repeat("Run commands. ", 20),
	}}}
	req, _ := json.Marshal(map[string]any{"model": "kimi-k3", "messages": messages, "tools": tools})
	full, _ := requestBillableText(string(req))
	local := localCountFromText(t, full)
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "kimi-k3", // 非 o200k 家族
		InputTokens:     local * 3, // 200% 偏差，必须报
		OutputTokens:    20,
		TotalTokens:     local*3 + 20,
		RequestContent:  string(req),
		ResponseContent: chatBody("ok"),
	})
	f := findCode(res, model.ProbeCodeTokenizerResidual)
	if f == nil {
		t.Fatal("3 倍偏差必须报出来")
	}
	if f.Confidence != model.RelayProbeConfidenceMedium {
		t.Fatalf("有界干扰下应为 medium 置信，实际 %q", f.Confidence)
	}
	factors, _ := f.Evidence["confidence_factors"].([]string)
	joined := fmt.Sprint(factors)
	for _, want := range []string{"upstream_templated_tools", "tokenizer_family_mismatch"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("置信因子缺 %s，实际 %v", want, factors)
		}
	}
	if f.Evidence["upstream_input_folded"] == nil || f.Evidence["counted_parts"] == nil {
		t.Fatalf("证据缺 folded 分母或 counted_parts：%v", f.Evidence)
	}
}

// TestRelayProbeEmptyOutputReasoningFieldNotEmpty 守第二个推理字段：
// OpenRouter/Ollama 风格把推理放在 message.reasoning，只认 reasoning_content
// 会把纯推理回复误报成"空回复计了费"。
func TestRelayProbeEmptyOutputReasoningFieldNotEmpty(t *testing.T) {
	body := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning":"让我想想这个问题……"}}]}`
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "gpt-5.6",
		InputTokens:     100,
		OutputTokens:    80,
		TotalTokens:     180,
		ResponseContent: body,
	})
	if f := findCode(res, model.ProbeCodeEmptyOutputWithTokens); f != nil {
		t.Fatalf("reasoning 字段是真实产出，不该报空回复，实际 %s", f.Title)
	}
}

// TestRelayProbeEmptyOutputRefusalNotEmpty 守拒答：拒答文本是模型逐字吐出的
// 真实产出，content 为空不等于没有输出。
func TestRelayProbeEmptyOutputRefusalNotEmpty(t *testing.T) {
	body := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null,"refusal":"抱歉，这个忙我帮不了。"}}]}`
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "gpt-5.6",
		InputTokens:     100,
		OutputTokens:    40,
		TotalTokens:     140,
		ResponseContent: body,
	})
	if f := findCode(res, model.ProbeCodeEmptyOutputWithTokens); f != nil {
		t.Fatalf("拒答是真实产出，不该报空回复，实际 %s", f.Title)
	}
}

// TestRelayProbeEmptyOutputStrippedMediaNotEmpty 守落库剥媒体：日志里媒体数据
// 被换成占位符，占位符在 = 当时确有媒体产出，不能报"空回复计了费"。
func TestRelayProbeEmptyOutputStrippedMediaNotEmpty(t *testing.T) {
	body := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"[image data omitted for storage]"}}]}}]}`
	res := ComputeRelayLogProbes(RelayProbeInput{
		RequestedModel:  "gpt-image-1",
		InputTokens:     100,
		OutputTokens:    900,
		TotalTokens:     1000,
		ResponseContent: body,
	})
	if f := findCode(res, model.ProbeCodeEmptyOutputWithTokens); f != nil {
		t.Fatalf("被剥离的媒体是真实产出，不该报空回复，实际 %s", f.Title)
	}
}
