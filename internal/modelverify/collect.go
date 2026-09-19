package modelverify

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bestruirui/octopus/internal/modelverify/probe"
	"github.com/bestruirui/octopus/internal/modelverify/suite"
)

// Sender 把一条探针题目经**既有出站链路**发出去。
//
// 实现方（oct 侧的渠道检测桥）负责一切出站契约：客户端身份、指纹、cloak 门控、
// wire headers、协议渲染与响应解析。本包只消费归一后的 Response——这是移植参考
// 项目时刻意保留的边界：它的 adapter/transport 用标准 net/http 自拼请求，会丢弃
// 我们模拟的客户端身份，因此那两层一律不搬。
type Sender interface {
	Ask(ctx context.Context, req Ask) (Response, error)
}

// Sample 一条采集到的观测。与参考项目 rawData 里的 record 同义。
type Sample struct {
	ProbeID   string
	ItemID    string
	Bucket    int
	Protocol  string
	RepeatIdx int
	Obs       probe.Observation // 观测 JSON，供 probe.Compare 消费
	Err       error             // 采集失败的该条不进统计，但保留原因便于呈现
}

// Collector 按题库发题、提取观测。不持有任何渠道或网络细节。
type Collector struct {
	Suite       *suite.File
	Sender      Sender
	Protocol    string // 该渠道实际使用的出站协议，决定 think-effort 的观测单位
	Buckets     []int  // 上下文档位；空 = 仅 [0]（不做长上下文填充）
	PaddingSeed int64
}

// CollectProbe 对指定探针采集 repeats 轮，覆盖全部题目与档位。
//
// 返回的 Sample 里 Err 非空表示该条采集失败（上游报错、响应无可统计内容等），
// 调用方应把它计入可用率，而不是塞进分布——把错误观测当成一个取值会污染统计。
func (c *Collector) CollectProbe(ctx context.Context, probeID string, repeats int) ([]Sample, error) {
	if c.Suite == nil {
		return nil, fmt.Errorf("题库未加载")
	}
	if c.Sender == nil {
		return nil, fmt.Errorf("未配置出站发送器")
	}
	if repeats < 1 {
		repeats = 1
	}
	items := c.Suite.ItemsOf(probeID)
	if len(items) == 0 {
		return nil, fmt.Errorf("题库中没有探针 %q 的题目", probeID)
	}
	set, ok := c.Suite.ProbeSetOf(probeID)
	if !ok {
		return nil, fmt.Errorf("题库未声明探针 %q", probeID)
	}

	buckets := c.Buckets
	if len(buckets) == 0 {
		buckets = []int{0}
	}
	protocol := c.Protocol
	if protocol == "" {
		protocol = "openai-chat"
	}
	norm := c.Suite.Normalizer()
	unit := ReasoningUnit(protocol)

	var out []Sample
	for _, it := range items {
		rule := set.NormalizeOf(it)
		contract := c.Suite.ContractFor(rule)
		jsonField := ""
		if contract != nil {
			jsonField = contract.FieldOf()
		}
		toolNames := toolNamesOf(set.ToolsOf(it))

		for _, bucket := range buckets {
			// needle 的题面必须按档位现场组装（埋点来自题 ID 派生，不在题库里明文出现）。
			prompt, markers := probePrompt(probeID, it, set, bucket, c.PaddingSeed)

			for i := 0; i < repeats; i++ {
				s := Sample{
					ProbeID:   probeID,
					ItemID:    it.ID,
					Bucket:    bucket,
					Protocol:  protocol,
					RepeatIdx: i,
				}
				resp, err := c.Sender.Ask(ctx, Ask{Prompt: prompt})
				if err != nil {
					s.Err = err
					out = append(out, s)
					continue
				}
				obs, err := observeFor(probeID, resp, norm, rule, jsonField, markers, toolNames, unit)
				if err != nil {
					s.Err = err
				} else {
					s.Obs = obs
				}
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// probePrompt 组装某一题的最终题面。needle 探针需要按档位拼接长上下文；
// 其余探针直接使用题库里的 prompt。
func probePrompt(probeID string, it suite.Item, set suite.ProbeSet, bucket int, seed int64) (string, []string) {
	if probeID != "needle" {
		return it.Prompt, nil
	}
	positions := set.PositionsOf(it)
	markers := suite.NeedleMarkers(it.ID, positions)
	corpus := it.Prompt
	if bucket > 0 {
		corpus = suite.GeneratePadding(bucket, seed)
	}
	return suite.AssembleNeedleContext(corpus, it.ID, positions), markers
}

func toolNamesOf(tools []suite.Tool) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

// PairSamples 把两侧的观测按 cell 配成对，交给 probe.Compare。
//
// cell 键与参考项目一致：question_id + context_bucket（needle/tokenizer 等探针的
// Meta().CellKey 由各自声明）。错误观测一律剔除——分布里塞错误样本会把"上游挂了"
// 判成"模型换了"。
func PairSamples(a, b []Sample) []probe.CellPair {
	type cell struct {
		item   string
		bucket int
	}
	index := map[cell]*probe.CellPair{}
	order := []cell{}

	add := func(samples []Sample, sideB bool) {
		for _, s := range samples {
			if s.Err != nil || len(s.Obs) == 0 {
				continue
			}
			k := cell{item: s.ItemID, bucket: s.Bucket}
			p, ok := index[k]
			if !ok {
				p = &probe.CellPair{Key: map[string]string{
					"question_id":    s.ItemID,
					"context_bucket": fmt.Sprintf("%d", s.Bucket),
				}}
				index[k] = p
				order = append(order, k)
			}
			if sideB {
				p.B = append(p.B, s.Obs)
			} else {
				p.A = append(p.A, s.Obs)
			}
		}
	}
	add(a, false)
	add(b, true)

	pairs := make([]probe.CellPair, 0, len(order))
	for _, k := range order {
		pairs = append(pairs, *index[k])
	}
	return pairs
}

// Judge 对两侧采集结果做逐探针判定。threshold 与 minN 由调用方给出——
// 参考项目刻意不提供默认阈值，因为"看起来合理的默认值会替操作者做出
// 他不知道正在做出的判断"。
func Judge(probeID string, pairs []probe.CellPair, threshold float64, minN int) (probe.Verdict, error) {
	p, ok := probe.Get(probeID)
	if !ok {
		return probe.Verdict{}, fmt.Errorf("未实现的探针 %q", probeID)
	}
	return p.Compare(pairs, threshold, minN), nil
}

// MarshalSample 序列化观测，供持久化。
func MarshalSample(s Sample) json.RawMessage {
	if len(s.Obs) == 0 {
		return nil
	}
	return s.Obs
}
