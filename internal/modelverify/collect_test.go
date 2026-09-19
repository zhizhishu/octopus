package modelverify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/modelverify/suite"
)

const testSuiteYAML = `suite_version: v1
output_contract:
  field: answer
  system_prompt: '请只回答一个合法的 JSON 对象。'
normalize:
  maps: {}
probes:
  onetoken:
    normalize: number
    thinking_effort: null
    items:
      - id: ot.test.001
        prompt: "说一个 1 到 100 之间的随机整数，只输出这个数字。"
      - id: ot.test.002
        prompt: "Pick a random integer between 1 and 100."
  tokenizer:
    normalize: none
    thinking_effort: null
    items:
      - id: tk.test.001
        prompt: "重复以下内容一次：こんにちは、🎉"
`

// fakeSender 按序返回预置回复；用完循环，err 非空时统一报错。
type fakeSender struct {
	replies []string
	err     error
	calls   []string
}

func (f *fakeSender) Ask(_ context.Context, req Ask) (Response, error) {
	f.calls = append(f.calls, req.Prompt)
	if f.err != nil {
		return Response{}, f.err
	}
	if len(f.replies) == 0 {
		return Response{}, errors.New("fakeSender 没有预置回复")
	}
	r := f.replies[(len(f.calls)-1)%len(f.replies)]
	return Response{
		Content: r,
		Usage:   Usage{Prompt: 42, Completion: 3},
	}, nil
}

func loadTestSuite(t *testing.T) *suite.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suite.yaml")
	if err := os.WriteFile(path, []byte(testSuiteYAML), 0o600); err != nil {
		t.Fatalf("写测试题库失败: %v", err)
	}
	f, err := suite.Load(path, "")
	if err != nil {
		t.Fatalf("加载测试题库失败: %v", err)
	}
	return f
}

func TestCollectProbeExtractsObservations(t *testing.T) {
	f := loadTestSuite(t)
	sender := &fakeSender{replies: []string{`{"answer": "7"}`}}
	c := &Collector{Suite: f, Sender: sender}

	samples, err := c.CollectProbe(context.Background(), "onetoken", 3)
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	// 2 道题 × 3 轮
	if len(samples) != 6 {
		t.Fatalf("样本数 = %d，期望 6", len(samples))
	}
	for _, s := range samples {
		if s.Err != nil {
			t.Fatalf("样本 %s#%d 采集报错: %v", s.ItemID, s.RepeatIdx, s.Err)
		}
		if string(s.Obs) != `{"value":"7"}` {
			t.Fatalf("观测 = %s，期望 {\"value\":\"7\"}", s.Obs)
		}
		if s.Protocol != "openai-chat" {
			t.Fatalf("协议缺省值 = %q，期望 openai-chat", s.Protocol)
		}
	}
	// 题目文本必须原样送出，不许被探针层改写。
	for _, call := range sender.calls {
		if call == "" {
			t.Fatal("送出了空题目")
		}
	}
}

func TestCollectProbeRecordsUpstreamErrors(t *testing.T) {
	f := loadTestSuite(t)
	sender := &fakeSender{err: errors.New("上游挂了")}
	c := &Collector{Suite: f, Sender: sender}

	samples, err := c.CollectProbe(context.Background(), "onetoken", 2)
	if err != nil {
		t.Fatalf("采集本身不该失败: %v", err)
	}
	for _, s := range samples {
		if s.Err == nil {
			t.Fatalf("上游报错时样本 %s 应带 Err", s.ItemID)
		}
		if s.Obs != nil {
			t.Fatal("报错的样本不该带观测，否则会污染分布")
		}
	}
}

func TestCollectProbeTokenizerUsesReportedUsage(t *testing.T) {
	f := loadTestSuite(t)
	sender := &fakeSender{replies: []string{"任意文本"}}
	c := &Collector{Suite: f, Sender: sender}

	samples, err := c.CollectProbe(context.Background(), "tokenizer", 2)
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("样本数 = %d，期望 2", len(samples))
	}
	for _, s := range samples {
		if s.Err != nil {
			t.Fatalf("tokenizer 采集报错: %v", s.Err)
		}
		if string(s.Obs) != `{"prompt_tokens":42}` {
			t.Fatalf("观测 = %s，期望使用上游上报的输入 token 数", s.Obs)
		}
	}
}

func TestPairSamplesSkipsFailedAndEmpty(t *testing.T) {
	a := []Sample{
		{ItemID: "q1", Bucket: 0, Obs: []byte(`{"value":"1"}`)},
		{ItemID: "q1", Bucket: 0, Err: errors.New("boom")},
		{ItemID: "q1", Bucket: 0}, // 无观测
		{ItemID: "q2", Bucket: 0, Obs: []byte(`{"value":"2"}`)},
	}
	b := []Sample{
		{ItemID: "q1", Bucket: 0, Obs: []byte(`{"value":"1"}`)},
		{ItemID: "q2", Bucket: 0, Obs: []byte(`{"value":"3"}`)},
	}

	pairs := PairSamples(a, b)
	if len(pairs) != 2 {
		t.Fatalf("cell 数 = %d，期望 2", len(pairs))
	}
	for _, p := range pairs {
		if len(p.A) != 1 {
			t.Fatalf("cell %v 的 A 侧样本 = %d，期望失败/空样本已被剔除", p.Key, len(p.A))
		}
		if len(p.B) != 1 {
			t.Fatalf("cell %v 的 B 侧样本 = %d", p.Key, len(p.B))
		}
	}
}

func TestJudgeOnetokenIdenticalDistributionsPass(t *testing.T) {
	same := []string{`{"value":"7"}`, `{"value":"7"}`, `{"value":"3"}`, `{"value":"9"}`}
	var a, b []Sample
	for _, v := range same {
		a = append(a, Sample{ItemID: "q1", Bucket: 0, Obs: []byte(v)})
		b = append(b, Sample{ItemID: "q1", Bucket: 0, Obs: []byte(v)})
	}

	verdict, err := Judge("onetoken", PairSamples(a, b), 0.15, 2)
	if err != nil {
		t.Fatalf("判定失败: %v", err)
	}
	if verdict.Verdict != "pass" {
		t.Fatalf("同一分布应判 pass，实际 %q（%s）", verdict.Verdict, verdict.Note)
	}
	if verdict.Statistic == nil || *verdict.Statistic != 0 {
		t.Fatalf("同一分布的 JSD 应为 0，实际 %v", verdict.Statistic)
	}
}

func TestJudgeOnetokenDivergentDistributionsFail(t *testing.T) {
	var a, b []Sample
	for range 12 {
		a = append(a, Sample{ItemID: "q1", Bucket: 0, Obs: []byte(`{"value":"7"}`)})
		b = append(b, Sample{ItemID: "q1", Bucket: 0, Obs: []byte(`{"value":"3"}`)})
	}

	verdict, err := Judge("onetoken", PairSamples(a, b), 0.15, 2)
	if err != nil {
		t.Fatalf("判定失败: %v", err)
	}
	if verdict.Verdict != "fail" {
		t.Fatalf("完全不同分布应判 fail，实际 %q", verdict.Verdict)
	}
}

func TestJudgeInsufficientSamplesIsInconclusive(t *testing.T) {
	var a, b []Sample
	a = append(a, Sample{ItemID: "q1", Bucket: 0, Obs: []byte(`{"value":"7"}`)})
	b = append(b, Sample{ItemID: "q1", Bucket: 0, Obs: []byte(`{"value":"3"}`)})

	verdict, err := Judge("onetoken", PairSamples(a, b), 0.15, 10)
	if err != nil {
		t.Fatalf("判定失败: %v", err)
	}
	if verdict.Verdict != "inconclusive" {
		t.Fatalf("样本不足应判 inconclusive（不能伪装成通过），实际 %q", verdict.Verdict)
	}
}

func TestCollectProbeUnknownProbeFails(t *testing.T) {
	f := loadTestSuite(t)
	c := &Collector{Suite: f, Sender: &fakeSender{replies: []string{"x"}}}
	if _, err := c.CollectProbe(context.Background(), "nope", 1); err == nil {
		t.Fatal("未声明的探针应报错")
	}
}
