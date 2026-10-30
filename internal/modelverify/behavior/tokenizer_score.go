package behavior

import "fmt"

// 分词器结论**只作参考，不计分**——照抄参考实现。
//
// lm-detector docs/content/docs/principles/tokenizer.en.mdx:83：分词器结果是排名旁的
// 参考信息，不改变候选排名和置信度；同一分词器常被多个模型共用，中转站也可能在本地
// 估算用量，所以计数只回答"上游分词器与输入的型号是否相符"这一个问题。
// 因此这里所有分支都是 0 分、low（本评分体系里最低、且不计分的一档），不改变 verdict；
// 探针本身也不进 verdict 的"跑通"分母（见 score.go probeCountsTowardVerdict）。

// tokenizerIDFindings 判读分词器差分探针。
//
// 一律 0 分：不一致时给带边界说明的参考提示；测不准（中途失败/基线漂移）时提示仅供参考。
func tokenizerIDFindings(res Result) []Finding {
	if !res.OK {
		return nil
	}
	series := stringOf(res.Data, "top_series")
	if series == "" {
		series = stringOf(res.Data, "top_class")
	}
	lab := stringOf(res.Data, "top_lab")
	measured := series
	if lab != "" {
		measured = fmt.Sprintf("%s（%s）", series, lab)
	}
	if !boolOf(res.Data, "usable") {
		title := "分词器探测中途失败，结果仅供参考"
		if boolOf(res.Data, "baseline_drift") {
			title = "两次基线计数不同（上游附加的隐藏输入长度在变），分词器结果可能不准"
		}
		return []Finding{{
			Probe: ProbeTokenizerID, Severity: SeverityLow, Score: 0,
			Title:    title,
			Evidence: res.Data,
		}}
	}
	status := stringOf(res.Data, "claim_status")
	switch status {
	case "consistent":
		// 没有矛盾 ≠ 正品证明：同一分词器常被多个模型共用。不产风险项。
		return nil
	case "inconsistent":
		rec := "仅供参考，不计入风险分。分词器只看上游计费的输入 token 口径，不看回答内容；" +
			"同一分词器常被多个模型共用，需结合指纹/签名等探针佐证。"
		if boolOf(res.Data, "relay_estimate_possible") {
			rec = "测成 o200k/cl100k 时，也可能是中转站用 tiktoken 在本地估算用量（用量数字并非上游给出）。" + rec
		}
		return []Finding{{
			Probe: ProbeTokenizerID, Severity: SeverityLow, Score: 0,
			Title:          fmt.Sprintf("上游分词器与声明型号不符（仅供参考）：测得 %s", measured),
			Evidence:       res.Data,
			Recommendation: rec,
		}}
	case "uncertain":
		return []Finding{{
			Probe: ProbeTokenizerID, Severity: SeverityLow, Score: 0,
			Title:    fmt.Sprintf("分词器与声明型号的相容性不确定（最接近 %s）", measured),
			Evidence: res.Data,
		}}
	default:
		return []Finding{{
			Probe: ProbeTokenizerID, Severity: SeverityLow, Score: 0,
			Title:    fmt.Sprintf("声明型号不在分词器映射里，无法核对；测得最接近 %s", measured),
			Evidence: res.Data,
		}}
	}
}
