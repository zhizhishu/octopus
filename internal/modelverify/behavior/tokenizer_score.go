package behavior

import "fmt"

// tokenizerMismatchScore 是"上游分词器与声明型号不符"的风险分。
//
// 参考实现（lm-detector）把分词器结果当作排名旁的参考信息、不改排名；我们的报告是
// 风险分制，只能给它一个分值。取 25（medium 一档），与它取代的 token_delta 分词器
// 不符项同分：这是行为证据（看的是上游计费口径，不看模型说了什么），但一个分词器
// 常被多个模型共用、中转站本地估算用量也会触发，单凭它不足以判高危。
const tokenizerMismatchScore = 25

// tokenizerIDFindings 判读分词器差分探针。
//
// 只在"完整跑完 + 两次基线一致 + 声明可核对且后验 ≤ 0.1"时计分；其余情形要么不出
// 结论，要么出 0 分的参考提示——测不准不能报成测出问题。
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
		rec := "分词器只看上游计费的输入 token 口径，不看回答内容；同一分词器常被多个模型共用，" +
			"需结合指纹/签名等探针佐证。"
		if boolOf(res.Data, "relay_estimate_possible") {
			rec = "测成 o200k/cl100k 时，也可能是中转站用 tiktoken 在本地估算用量（用量数字并非上游给出）。" + rec
		}
		return []Finding{{
			Probe: ProbeTokenizerID, Severity: SeverityMedium, Score: tokenizerMismatchScore,
			Title:          fmt.Sprintf("上游分词器与声明型号不符：测得 %s", measured),
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
