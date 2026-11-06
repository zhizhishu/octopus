package op

import (
	"reflect"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 规则表逐条命中：每条用例只放一个信号源，断言产出的 Hints 与期望完全相同
// （platform/strength/code/evidence 四项都比，避免"命中了但字段写错"混过去）。
func TestPassiveSourceHintRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		relayLog model.RelayLog
		want     []model.RelayLogSourceHint
	}{
		{
			// 规则 1：模型名 kiro- 前缀。刻意走 OpenAI 协议路径（chat），钉住
			// 「线索对所有协议生效」——不是只有 messages 才有来源线索。
			name: "rule1 model prefix kiro on non-anthropic protocol",
			relayLog: model.RelayLog{
				RequestEndpoint:       "chat",
				RequestModelName:      "claude-sonnet-4-6",
				UpstreamResponseModel: "kiro-claude-sonnet-4",
			},
			want: []model.RelayLogSourceHint{{
				Platform: "aws_kiro",
				Strength: "strong",
				Code:     "model_prefix_kiro",
				Evidence: "kiro-claude-sonnet-4",
			}},
		},
		{
			// 规则 2：Bedrock 的 anthropic.* / *.anthropic.claude 模型名形态。
			name: "rule2 model prefix bedrock",
			relayLog: model.RelayLog{
				RequestEndpoint:       "messages",
				RequestModelName:      "claude-sonnet-4-6",
				UpstreamResponseModel: "us.anthropic.claude-sonnet-4-20250514-v1:0",
			},
			want: []model.RelayLogSourceHint{{
				Platform: "aws_bedrock",
				Strength: "medium",
				Code:     "model_prefix_bedrock",
				Evidence: "us.anthropic.claude-sonnet-4-20250514-v1:0",
			}},
		},
		{
			// 规则 3：Vertex 的响应 id 前缀。evidence 只存前缀，不存完整编号。
			name: "rule3 response id vertex",
			relayLog: model.RelayLog{
				RequestEndpoint:  "messages",
				RequestModelName: "claude-sonnet-4-6",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					ResponseID: "req_vrtx_011CcXyzABCDEFG",
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "gcp_vertex",
				Strength: "strong",
				Code:     "response_id_vertex",
				Evidence: "req_vrtx_",
			}},
		},
		{
			// 规则 4：Vertex 的 tool id 形态 tool_<数字>。
			name: "rule4 tool id vertex",
			relayLog: model.RelayLog{
				RequestEndpoint:  "messages",
				RequestModelName: "claude-sonnet-4-6",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					FirstToolCallID: "tool_1234",
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "gcp_vertex",
				Strength: "strong",
				Code:     "tool_id_vertex",
				Evidence: "tool_<n>",
			}},
		},
		{
			// 规则 5：Bedrock 的 tool id 形态 tooluse_*。
			name: "rule5 tool id bedrock",
			relayLog: model.RelayLog{
				RequestEndpoint:  "messages",
				RequestModelName: "claude-sonnet-4-6",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					FirstToolCallID: "tooluse_01xyzABC",
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "aws",
				Strength: "medium",
				Code:     "tool_id_bedrock",
				Evidence: "tooluse_",
			}},
		},
		{
			// 规则 6：thinking 签名以 claude# 开头（Vertex 侧形态）。只记形态。
			name: "rule6 signature vertex",
			relayLog: model.RelayLog{
				RequestEndpoint:  "messages",
				RequestModelName: "claude-sonnet-4-6",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					SignatureClaudeHash: true,
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "gcp_vertex",
				Strength: "strong",
				Code:     "signature_vertex",
				Evidence: "claude#",
			}},
		},
		{
			// 规则 7：任意 AWS 家族响应头。同样走非 Anthropic 协议路径。
			name: "rule7 header aws on non-anthropic protocol",
			relayLog: model.RelayLog{
				RequestEndpoint:  "chat",
				RequestModelName: "gpt-5.6",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					HeaderFamilies: []string{"aws", "cloudflare"},
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "aws",
				Strength: "medium",
				Code:     "header_aws",
				Evidence: "x-amz*",
			}},
		},
		{
			// 规则 8：openai 家族头 + 请求的是别的家族 → 跨厂商。
			name: "rule8 openai header cross vendor",
			relayLog: model.RelayLog{
				RequestEndpoint:  "messages",
				RequestModelName: "claude-opus-4-8",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					HeaderFamilies: []string{"openai"},
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "openai",
				Strength: "medium",
				Code:     "header_openai_cross_vendor",
				Evidence: "openai-*",
			}},
		},
		{
			// 规则 9：anthropic-ratelimit-* 头 + 请求的不是 anthropic 家族。
			name: "rule9 anthropic ratelimit header cross vendor",
			relayLog: model.RelayLog{
				RequestEndpoint:  "chat",
				RequestModelName: "gpt-5.6",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					HeaderFamilies: []string{"anthropic_ratelimit"},
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "anthropic",
				Strength: "medium",
				Code:     "header_anthropic_cross_vendor",
				Evidence: "anthropic-ratelimit-*",
			}},
		},
		{
			// 规则 10：同一条流里换过自报模型名。platform 留空（不指向任何一家）。
			name: "rule10 model switch mid stream",
			relayLog: model.RelayLog{
				RequestEndpoint:       "messages",
				RequestModelName:      "claude-opus-4-8",
				UpstreamResponseModel: "claude-opus-4-8",
				UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
					ModelSwitchedTo: "claude-haiku-4-5",
				},
			},
			want: []model.RelayLogSourceHint{{
				Platform: "",
				Strength: "medium",
				Code:     "model_switch_mid_stream",
				Evidence: "claude-opus-4-8 → claude-haiku-4-5",
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := computePassiveSourceEvidence(tc.relayLog)
			if !reflect.DeepEqual(ev.Hints, tc.want) {
				t.Fatalf("hints = %+v, want %+v", ev.Hints, tc.want)
			}
			// 有线索才记响应头类别；这里顺带钉住它确实被带出来了。
			if len(tc.relayLog.UpstreamEnvelope.HeaderFamilies) > 0 &&
				!reflect.DeepEqual(ev.HeaderFamilies, tc.relayLog.UpstreamEnvelope.HeaderFamilies) {
				t.Fatalf("header families = %v, want %v", ev.HeaderFamilies, tc.relayLog.UpstreamEnvelope.HeaderFamilies)
			}
			if ev.Empty() {
				t.Fatalf("evidence must not be empty when a hint fired: %+v", ev)
			}
		})
	}
}

// 失败日志一条线索都不加：错误页的头和正文反映的是网关/前置层，不是模型后端。
func TestPassiveSourceHintsSkipFailedLogs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		relayLog model.RelayLog
	}{
		{
			name: "error text",
			relayLog: model.RelayLog{
				RequestEndpoint:       "messages",
				UpstreamResponseModel: "kiro-claude-sonnet-4",
				Error:                 "upstream boom",
			},
		},
		{
			name: "error code",
			relayLog: model.RelayLog{
				RequestEndpoint:       "messages",
				UpstreamResponseModel: "kiro-claude-sonnet-4",
				ErrorCode:             "octopus_upstream_auth_failed",
			},
		},
		{
			name: "error status",
			relayLog: model.RelayLog{
				RequestEndpoint:       "messages",
				UpstreamResponseModel: "kiro-claude-sonnet-4",
				ErrorStatus:           503,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 即便响应头里有 aws 家族，也不许变成线索。
			tc.relayLog.UpstreamEnvelope.HeaderFamilies = []string{"aws"}
			ev := computePassiveSourceEvidence(tc.relayLog)
			if len(ev.Hints) != 0 {
				t.Fatalf("failed log must not produce hints, got %+v", ev.Hints)
			}
			if len(ev.HeaderFamilies) != 0 {
				t.Fatalf("failed log must not record header families, got %v", ev.HeaderFamilies)
			}
			if !ev.Empty() {
				t.Fatalf("failed log must stay empty, got %+v", ev)
			}
		})
	}
}

// 没有任何线索时 HeaderFamilies 不写：否则每条带 cf-ray 的日志都会多一段 JSON，
// 库被撑大、聚合也失去意义。
func TestPassiveSourceHeaderFamiliesOnlyWithHints(t *testing.T) {
	l := model.RelayLog{
		RequestEndpoint:  "messages",
		RequestModelName: "claude-sonnet-4-6",
		UpstreamEnvelope: model.RelayLogUpstreamEnvelope{
			HeaderFamilies: []string{"cloudflare", "envoy"},
		},
	}
	ev := computePassiveSourceEvidence(l)
	if len(ev.Hints) != 0 {
		t.Fatalf("hints = %+v, want none", ev.Hints)
	}
	if len(ev.HeaderFamilies) != 0 {
		t.Fatalf("header families = %v, want none without a hint", ev.HeaderFamilies)
	}
	if !ev.Empty() {
		t.Fatalf("evidence must be empty, got %+v", ev)
	}
}

// 原有 Anthropic 打分口径不变：输入与 TestSourceEvidenceFullAnthropicSignals 相同，
// 分数必须仍是 6（1+2 TTL 拆分 + 3 service_tier），且不该凭空多出线索。
func TestPassiveSourceAnthropicScoreUnchanged(t *testing.T) {
	l := model.RelayLog{
		RequestEndpoint:     "messages",
		RequestModelName:    "claude-sonnet-4-6",
		UpstreamServiceTier: "standard",
		CacheWriteTokens:    1000,
		CacheWrite5mTokens:  600,
		CacheWrite1hTokens:  400,
	}
	ev := computePassiveSourceEvidence(l)
	if ev.AnthropicScore != 6 {
		t.Fatalf("score = %d, want 6 (%+v)", ev.AnthropicScore, ev)
	}
	if len(ev.CounterEvidence) != 0 {
		t.Fatalf("expected no counter evidence, got %v", ev.CounterEvidence)
	}
	if len(ev.Hints) != 0 || len(ev.HeaderFamilies) != 0 {
		t.Fatalf("no envelope signals present, want no hints, got %+v", ev)
	}
}

// 非 Anthropic 协议路径不再 early return：打分块被跳过，但线索照采。
// 这条守的是「改造 early return 时别把下面的规则一起挡掉」。
func TestPassiveSourceNonAnthropicPathStillScoresHints(t *testing.T) {
	l := model.RelayLog{
		RequestEndpoint:     "chat",
		RequestModelName:    "claude-sonnet-4-6",
		UpstreamServiceTier: "standard",
		CacheWriteTokens:    1000,
	}
	ev := computePassiveSourceEvidence(l)
	if ev.AnthropicScore != 0 || len(ev.Signals) != 0 || len(ev.CounterEvidence) != 0 {
		t.Fatalf("anthropic scoring must be skipped off the messages path, got %+v", ev)
	}

	l.UpstreamResponseModel = "kiro-claude-sonnet-4"
	ev = computePassiveSourceEvidence(l)
	if len(ev.Hints) != 1 || ev.Hints[0].Code != "model_prefix_kiro" {
		t.Fatalf("hints = %+v, want the kiro model prefix hint", ev.Hints)
	}
}

// Evidence 只放前缀/模型名，最多 64 字符：上游给一个超长模型名也不能撑爆这一列。
func TestPassiveSourceHintEvidenceIsTruncated(t *testing.T) {
	longName := ""
	for i := 0; i < 100; i++ {
		longName += "k"
	}
	l := model.RelayLog{
		RequestEndpoint:       "chat",
		UpstreamResponseModel: longName,
	}
	ev := computePassiveSourceEvidence(l)
	if len(ev.Hints) != 0 {
		t.Fatalf("hints = %+v, want none (no prefix hit)", ev.Hints)
	}

	l.UpstreamResponseModel = "kiro-" + longName
	ev = computePassiveSourceEvidence(l)
	if len(ev.Hints) != 1 {
		t.Fatalf("hints = %+v, want one", ev.Hints)
	}
	if got := len([]rune(ev.Hints[0].Evidence)); got != 64 {
		t.Fatalf("evidence length = %d runes, want 64", got)
	}
}
