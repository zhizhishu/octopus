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
// 维持 10（对齐参考实现），**刻意没有改成 30**。
//
// 曾经想按 PAMELA 的实测把它提到 30（N≥30 才能让 JSD 的 EER 阈值 0.073 可靠；
// N 偏小时单次 JSD 标准误 0.03–0.05 已大于判定间距 0.0402）。改完发现这是
// **在给一段还没上线的代码调参**：Judge / CollectProbe / PairSamples 三个函数
// 除 collect_test.go 外没有任何生产调用，整条探针层没接进检测链，改这个数字
// 既不改变任何真实结论，又会误导后来人以为它已经在生效。
//
// 真要接线时再一起定，届时必须连带扩题库（现只有 3 题，N=30 需要 8–12 题）
// 并配基线存储。在那之前，这个常量就是个摆设，别把它当成"已提升精度"。
//
// 实际生效值以采集计划的 min_n 为准（Compare 参数传入），这里只是缺省值。
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
