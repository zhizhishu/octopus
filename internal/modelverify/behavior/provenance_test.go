package behavior

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify"
)

// 规则表逐条命中：每条用例只放一个信号源，断言产出的 Hints 与期望完全相同
// （platform/strength/code/evidence 四项都比，避免"命中了但字段写错"混过去）。
//
// 这份表必须与 internal/op/log_passive_provenance_test.go 保持同一批取值：
// 两个消费者跑同一份规则，任何一侧改了取值域，另一侧的契约就断了。
func TestProvenanceHintRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ProvenanceInput
		want []ProvenanceHint
	}{
		{
			name: "rule1 model prefix kiro",
			in: ProvenanceInput{
				RequestModel:  "claude-sonnet-4-6",
				UpstreamModel: "kiro-claude-sonnet-4",
			},
			want: []ProvenanceHint{{
				Platform: "aws_kiro",
				Strength: "strong",
				Code:     "model_prefix_kiro",
				Evidence: "kiro-claude-sonnet-4",
			}},
		},
		{
			name: "rule2 model prefix bedrock",
			in: ProvenanceInput{
				RequestModel:  "claude-sonnet-4-6",
				UpstreamModel: "us.anthropic.claude-sonnet-4-20250514-v1:0",
			},
			want: []ProvenanceHint{{
				Platform: "aws_bedrock",
				Strength: "medium",
				Code:     "model_prefix_bedrock",
				Evidence: "us.anthropic.claude-sonnet-4-20250514-v1:0",
			}},
		},
		{
			name: "rule3 response id vertex",
			in: ProvenanceInput{
				RequestModel: "claude-sonnet-4-6",
				ResponseID:   "req_vrtx_011CcXyzABCDEFG",
			},
			want: []ProvenanceHint{{
				Platform: "gcp_vertex",
				Strength: "strong",
				Code:     "response_id_vertex",
				Evidence: "req_vrtx_",
			}},
		},
		{
			name: "rule3b response id bedrock",
			in: ProvenanceInput{
				RequestModel: "claude-opus-4-8",
				ResponseID:   "msg_bdrk_01ABCdef",
			},
			want: []ProvenanceHint{{
				Platform: "aws_bedrock",
				Strength: "strong",
				Code:     "response_id_bedrock",
				Evidence: "msg_bdrk_",
			}},
		},
		{
			name: "rule4 tool id vertex",
			in: ProvenanceInput{
				RequestModel:    "claude-sonnet-4-6",
				FirstToolCallID: "tool_1234",
			},
			want: []ProvenanceHint{{
				Platform: "gcp_vertex",
				Strength: "strong",
				Code:     "tool_id_vertex",
				Evidence: "tool_<n>",
			}},
		},
		{
			name: "rule5 tool id bedrock converse",
			in: ProvenanceInput{
				RequestModel:    "claude-sonnet-4-6",
				FirstToolCallID: "tooluse_01xyzABC",
			},
			want: []ProvenanceHint{{
				Platform: "aws",
				Strength: "medium",
				Code:     "tool_id_bedrock",
				Evidence: "tooluse_",
			}},
		},
		{
			name: "rule5b tool id bedrock messages format",
			in: ProvenanceInput{
				RequestModel:    "claude-opus-4-8",
				FirstToolCallID: "toolu_bdrk_01DXLNojQXabpu5QtCXdKDGw",
			},
			want: []ProvenanceHint{{
				Platform: "aws_bedrock",
				Strength: "strong",
				Code:     "tool_id_bedrock_messages",
				Evidence: "toolu_bdrk_",
			}},
		},
		{
			name: "rule6 signature vertex",
			in: ProvenanceInput{
				RequestModel:        "claude-sonnet-4-6",
				SignatureClaudeHash: true,
			},
			want: []ProvenanceHint{{
				Platform: "gcp_vertex",
				Strength: "strong",
				Code:     "signature_vertex",
				Evidence: "claude#",
			}},
		},
		{
			name: "rule7 header aws",
			in: ProvenanceInput{
				RequestModel:   "gpt-5.6",
				HeaderFamilies: []string{"aws", "cloudflare"},
			},
			want: []ProvenanceHint{{
				Platform: "aws",
				Strength: "medium",
				Code:     "header_aws",
				Evidence: "x-amz*",
			}},
		},
		{
			name: "rule8 openai header cross vendor",
			in: ProvenanceInput{
				RequestModel:   "claude-opus-4-8",
				HeaderFamilies: []string{"openai"},
			},
			want: []ProvenanceHint{{
				Platform: "openai",
				Strength: "medium",
				Code:     "header_openai_cross_vendor",
				Evidence: "openai-*",
			}},
		},
		{
			name: "rule9 anthropic ratelimit header cross vendor",
			in: ProvenanceInput{
				RequestModel:   "gpt-5.6",
				HeaderFamilies: []string{"anthropic_ratelimit"},
			},
			want: []ProvenanceHint{{
				Platform: "anthropic",
				Strength: "medium",
				Code:     "header_anthropic_cross_vendor",
				Evidence: "anthropic-ratelimit-*",
			}},
		},
		{
			// 换模型这条不指向任何一家：platform 留空是刻意的。
			name: "rule10 model switch mid stream",
			in: ProvenanceInput{
				RequestModel:    "claude-opus-4-8",
				UpstreamModel:   "claude-opus-4-8",
				ModelSwitchedTo: "claude-haiku-4-5",
			},
			want: []ProvenanceHint{{
				Platform: "",
				Strength: "medium",
				Code:     "model_switch_mid_stream",
				Evidence: "claude-opus-4-8 → claude-haiku-4-5",
			}},
		},
		{
			// 官方形态不给任何线索：普通 msg_ / toolu_ 编号不能当可疑。
			name: "plain ids give no hint",
			in: ProvenanceInput{
				RequestModel:    "claude-opus-4-8",
				ResponseID:      "msg_01ABCdef",
				FirstToolCallID: "toolu_01DXLNojQXabpu5QtCXdKDGw",
			},
			want: nil,
		},
		{
			// 规则 8 的跨厂商前提：请求的就是 openai 家族时报了也不算信号。
			name: "openai header same vendor is not a hint",
			in: ProvenanceInput{
				RequestModel:   "gpt-5.6",
				HeaderFamilies: []string{"openai"},
			},
			want: nil,
		},
		{
			// 请求家族识别不出时不提跨厂商：缺一边不是信号。
			name: "cross vendor needs a recognized request family",
			in: ProvenanceInput{
				HeaderFamilies: []string{"openai", "anthropic_ratelimit"},
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ProvenanceHints(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("hints = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Evidence 只放前缀/模型名，最多 64 字符：上游给一个超长模型名也不能撑爆这一列。
func TestProvenanceHintEvidenceIsTruncated(t *testing.T) {
	longName := strings.Repeat("k", 100)
	hints := ProvenanceHints(ProvenanceInput{UpstreamModel: "kiro-" + longName})
	if len(hints) != 1 {
		t.Fatalf("hints = %+v, want one", hints)
	}
	if got := len([]rune(hints[0].Evidence)); got != 64 {
		t.Fatalf("evidence length = %d runes, want 64", got)
	}
}

// 汇总口径：按 Code 去重、头家族取并集、Observed 记全部信封。
func TestSummarizeProvenanceDedupesByCodeAndUnionsHeaders(t *testing.T) {
	summary := summarizeProvenance([]ProvenanceInput{
		{
			RequestModel:   "gpt-5.6",
			ResponseID:     "req_vrtx_01",
			HeaderFamilies: []string{"cloudflare", "aws"},
		},
		{
			// 同一份 id 前缀再来一次：只能报一条。
			RequestModel:   "gpt-5.6",
			ResponseID:     "req_vrtx_02",
			HeaderFamilies: []string{"aws"},
		},
		{
			// 另一条规则的线索，不该被上面的去重吞掉。
			RequestModel:  "gpt-5.6",
			UpstreamModel: "kiro-claude-sonnet-4",
		},
	})
	if summary.Observed != 3 {
		t.Fatalf("observed = %d, want 3", summary.Observed)
	}
	if len(summary.Hints) != 3 {
		t.Fatalf("hints = %+v, want 3 (rule3 once + rule7 + rule1)", summary.Hints)
	}
	codes := []string{summary.Hints[0].Code, summary.Hints[1].Code, summary.Hints[2].Code}
	if !reflect.DeepEqual(codes, []string{"response_id_vertex", "header_aws", "model_prefix_kiro"}) {
		t.Fatalf("hint codes = %v, want rule order preserved with rule3 deduped", codes)
	}
	if !reflect.DeepEqual(summary.HeaderFamilies, []string{"aws", "cloudflare"}) {
		t.Fatalf("header families = %v, want sorted union", summary.HeaderFamilies)
	}
	// 展示用的那条要是强证据，不是随手第一条。
	if hint, ok := summary.topHint(); !ok || hint.Strength != ProvenanceStrengthStrong {
		t.Fatalf("topHint = %+v, want a strong one", hint)
	}
}

// 一条信封都没有时 Observed=0：这是「没看到回复」，不是「没发现线索」。
func TestSummarizeProvenanceEmpty(t *testing.T) {
	summary := summarizeProvenance(nil)
	if summary.Observed != 0 || len(summary.Hints) != 0 || len(summary.HeaderFamilies) != 0 {
		t.Fatalf("empty summary = %+v", summary)
	}
	if _, ok := summary.topHint(); ok {
		t.Fatal("empty summary must not yield a top hint")
	}
}

// envelopeStub 是带全部可选接口的假 Sender：用来验包装器有没有转发它们。
type envelopeStub struct {
	calls int
	mu    sync.Mutex
}

func (s *envelopeStub) Ask(_ context.Context, _ modelverify.Ask) (modelverify.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return modelverify.Response{
		Content:         "ok",
		DeclaredModel:   "claude-opus-4-8",
		ResponseID:      "msg_bdrk_01",
		HeaderFamilies:  []string{"aws"},
		FirstToolCallID: "tooluse_01",
	}, nil
}

func (s *envelopeStub) TargetModel() string { return "claude-opus-4-8" }
func (s *envelopeStub) Protocol() string    { return "anthropic-messages" }
func (s *envelopeStub) InjectedFamilies() []string {
	return []string{"anthropic"}
}

// bareStub 只实现 Sender 本身：包装器对没实现的接口必须安静地退化成零值。
type bareStub struct{}

func (bareStub) Ask(_ context.Context, _ modelverify.Ask) (modelverify.Response, error) {
	return modelverify.Response{Content: "ok"}, nil
}

// 记录必须并发安全：手动检测与后台队列会同时跑各自的审计。
// 这条用例在 -race 下跑才有意义（多个 goroutine 同时 Ask）。
func TestRecordingSenderIsConcurrencySafe(t *testing.T) {
	rec := &recordingSender{inner: bareStub{}, requestModel: "gpt-5.6"}
	const goroutines = 32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := rec.Ask(context.Background(), modelverify.Ask{Prompt: "hi"}); err != nil {
				t.Errorf("ask: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := rec.summary().Observed; got != goroutines {
		t.Fatalf("observed = %d, want %d (records lost or torn)", got, goroutines)
	}
}

// 包装器必须原样转发内层 sender 的可选接口，否则探针里的类型断言会全部失效。
func TestRecordingSenderForwardsOptionalInterfaces(t *testing.T) {
	inner := &envelopeStub{}
	rec := &recordingSender{inner: inner, requestModel: "claude-opus-4-8"}

	var _ ModelNamer = rec
	var _ ProtocolNamer = rec
	var _ InjectedFamiliesNamer = rec

	if got := rec.TargetModel(); got != "claude-opus-4-8" {
		t.Fatalf("TargetModel = %q, want forwarded", got)
	}
	if got := rec.Protocol(); got != "anthropic-messages" {
		t.Fatalf("Protocol = %q, want forwarded", got)
	}
	if got := rec.InjectedFamilies(); !reflect.DeepEqual(got, []string{"anthropic"}) {
		t.Fatalf("InjectedFamilies = %v, want forwarded", got)
	}

	// 内层没实现时退化成零值，而不是 panic 或返回垃圾。
	bare := &recordingSender{inner: bareStub{}}
	if bare.TargetModel() != "" || bare.Protocol() != "" || bare.InjectedFamilies() != nil {
		t.Fatalf("bare wrapper must degrade to zero values: %q %q %v",
			bare.TargetModel(), bare.Protocol(), bare.InjectedFamilies())
	}
}

// 端到端：Run 抄信封 → BuildReport 把汇总升格成 report.Provenance，
// 并且原始结果里不再残留那份内部数据（它不该出现在展示用的 results 里）。
func TestRunCollectsProvenanceIntoReport(t *testing.T) {
	inner := &envelopeStub{}
	// 用一个只需一次请求、且对 stub 内容无强判定的探针。
	results := Run(context.Background(), []ProbeID{ProbeLiveness}, inner, "claude-opus-4-8")
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if _, ok := results[0].Data[provenanceDataKey]; !ok {
		t.Fatal("Run 应把信封汇总挂在首条结果里交给 BuildReport")
	}

	report := BuildReport(results, "claude-opus-4-8")
	if report.Provenance.Observed != 1 {
		t.Fatalf("observed = %d, want 1", report.Provenance.Observed)
	}
	codes := make([]string, 0, len(report.Provenance.Hints))
	for _, hint := range report.Provenance.Hints {
		codes = append(codes, hint.Code)
	}
	// msg_bdrk_ 响应 id + tooluse_ 工具编号 + x-amz 头：三条都是来源线索，
	// 且按规则表顺序（规则 3 → 5 → 7）出现。
	if !reflect.DeepEqual(codes, []string{"response_id_bedrock", "tool_id_bedrock", "header_aws"}) {
		t.Fatalf("hint codes = %v", codes)
	}
	if len(report.Results) != 1 {
		t.Fatalf("report results = %d, want 1", len(report.Results))
	}
	if _, ok := report.Results[0].Data[provenanceDataKey]; ok {
		t.Fatal("汇总升格后，展示用结果里不该再留内部通道键")
	}
	// 调用方手里那份 results 不能被就地改掉。
	if _, ok := results[0].Data[provenanceDataKey]; !ok {
		t.Fatal("BuildReport 就地删了调用方的数据")
	}
}

// 失败请求不产生信封：错误响应的头/正文来自前置层，不是模型后端。
func TestRunSkipsEnvelopesForFailedAsks(t *testing.T) {
	results := Run(context.Background(), []ProbeID{ProbeLiveness}, failingSender{}, "gpt-5.6")
	report := BuildReport(results, "gpt-5.6")
	if report.Provenance.Observed != 0 || len(report.Provenance.Hints) != 0 {
		t.Fatalf("failed asks must not produce provenance: %+v", report.Provenance)
	}
}

type failingSender struct{}

func (failingSender) Ask(_ context.Context, _ modelverify.Ask) (modelverify.Response, error) {
	return modelverify.Response{}, context.DeadlineExceeded
}
