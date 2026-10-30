package probe

import (
	"encoding/json"
	"fmt"

	"github.com/zhizhishu/octopus/internal/modelverify/stats"
)

// onetokenProbe 极短答案取值分布探针：逐 cell 算两侧分布的 JSD，
// 探针级统计量取达标 cell 的均值，与阈值（距离上限）比较。
type onetokenProbe struct{}

// MinN 是双侧各至少多少个有效样本才参与 JSD。
//
// 定为 10，**照抄参考实现的官方配置，不改成 30**（2026-10-30 复核后的决定）：
//   - Token-Verifier config.example.yaml:119 缺省 `min_n: 10`（repeats 10）；
//   - 官方基线发布（chaterm/Token-Verifier-Data，suite v4）的两份采集配置
//     config.claudeopus5.aws.yaml:40 与 config.gpt56sol.azure.yaml:46 都是
//     `repeats: 12, min_n: 10`。基线是按这个口径采的，比较侧改成 30 就和基线不可比。
//
// "30" 来自 PAMELA（pamela-model-id README：N≥30 时 JSD 的 EER 阈值 0.073 才可靠），
// 那是另一个项目、另一套题与阈值，不能嫁接到 Token-Verifier 的阈值体系上。
//
// 另外，这一层（Judge / CollectProbe / PairSamples）至今**没有接进检测链**：
// Token-Verifier 的比较需要同一出站形态下采的两侧原始 prompt_tokens / 回答分布，
// 而本项目出站带 CLI 形态的隐藏前缀，官方基线（直连 API 采的）与之不可比。
// 分词器识别改走 lm-detector 的差分计数（behavior/tokenizer_probe.go，前缀在差分里
// 相消）。这里的常量只是缺省值，实际生效值以采集计划的 min_n 为准（Compare 参数传入）。
const onetokenMinN = 10

func (onetokenProbe) Meta() ProbeMeta {
	return ProbeMeta{
		ID:                "onetoken",
		Version:           "2",
		CellKey:           []string{"question_id", "context_bucket"},
		MinN:              onetokenMinN,
		ObservationSchema: "onetoken/v2",
		StatKind:          StatDistance,
	}
}

// onetokenObs 观测结构：{"value": "7"}，value 为采集期归一化后的答案。
type onetokenObs struct {
	Value string `json:"value"`
}

func (p onetokenProbe) Compare(pairs []CellPair, threshold float64, minN int) Verdict {
	if minN < 1 {
		minN = p.Meta().MinN // 旧 rawData 无 min_n 字段 → 回退缺省
	}
	v := Verdict{
		ProbeID:   p.Meta().ID,
		Threshold: threshold,
	}

	var jsds []float64
	for _, pair := range pairs {
		a, badA := parseOnetokenValues(pair.A)
		b, badB := parseOnetokenValues(pair.B)

		detail := CellDetail{
			Key: pair.Key,
			NA:  len(a),
			NB:  len(b),
		}
		if badA+badB > 0 {
			detail.Extra = map[string]any{"parse_failed": badA + badB}
		}

		// 样本量不足的 cell 剔除而非降权
		if len(a) < minN || len(b) < minN {
			detail.Insufficient = true
			v.Cells = append(v.Cells, detail)
			continue
		}

		distA, distB := stats.FromSamples(a), stats.FromSamples(b)
		jsd := stats.JSD(distA, distB)
		jsds = append(jsds, jsd)
		detail.Stat = &jsd
		// 证据数据：两侧取值分布进报告（verbose 渲染直方图），不参与判定
		detail.Extra = mergeExtra(detail.Extra, map[string]any{
			"a_dist": map[string]int(distA),
			"b_dist": map[string]int(distB),
		})
		v.Cells = append(v.Cells, detail)
	}

	if len(jsds) == 0 {
		v.Verdict = "inconclusive"
		if len(pairs) == 0 {
			v.Note = "两侧没有共同 cell"
		} else {
			v.Note = fmt.Sprintf("所有配对 cell 的有效样本量均不足（n < %d）", minN)
		}
		return v
	}

	mean := stats.Mean(jsds)
	v.Statistic = &mean
	r := ratio(StatDistance, mean, threshold)
	v.Ratio = &r
	if mean > threshold {
		v.Verdict = "fail"
	} else {
		v.Verdict = "pass"
	}
	return v
}

// parseOnetokenValues 提取可解析的观测值，返回（值列表，解析失败数）。
func parseOnetokenValues(obs []Observation) ([]string, int) {
	values := make([]string, 0, len(obs))
	bad := 0
	for _, raw := range obs {
		var o onetokenObs
		if err := json.Unmarshal(raw, &o); err != nil {
			bad++
			continue
		}
		values = append(values, o.Value)
	}
	return values, bad
}
