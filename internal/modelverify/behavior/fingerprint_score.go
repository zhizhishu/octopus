package behavior

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/modelverify/fingerprint"
)

// 指纹探针的判定。
//
// 判定原则与其它探针一致，但这里要额外守一条：**只证伪，不认证**。
//
//   - 指纹与声明的型号对得上 → 无风险，但**不能**写成"确认是正品"：
//     参考库只有 59 个候选，中转站用库里某个相近型号冒充也会对上。
//   - 指纹与声明的型号对不上 → 这才是真信号。声明是 A、怎么看都不像 A，
//     说明背后大概率不是 A。这与"问你是谁"得到的口供不同，是行为证据。
//   - 证据不足（样本不够、库外型号、结论本身不成立）→ 0 分「不可判」，
//     并明确不把它算成"查过没问题"。
const (
	// fingerprintMismatchScore 是"指纹与声明型号明显不符"的风险分。
	//
	// 给 35（中等偏上）：这确实是行为证据，不像口供那样可以一句话伪造；
	// 但也不能给满分——参考库的家族识别在相邻版本间本来就会混
	// （同代不同小版本几乎无法区分），给太高会把正常波动判成事故。
	fingerprintMismatchScore = 35

	// fingerprintGapThreshold 是"前两名分差小于此值"的判定阈值。
	//
	// 分差小意味着两个候选都说得通。此时若恰好第一名与声明型号不符，
	// 绝不能据此定罪——那是在解读噪声。
	fingerprintGapThreshold = 0.15
)

// fingerprintFindings 是随机数指纹探针的判定。
//
// res.Err 非空时返回 nil：与其它探针一样，"跑不动"不是证据。
func fingerprintFindings(res Result, requestedModel string) []Finding {
	if res.Err != nil || !res.OK {
		return nil
	}
	// 结论已在探针层算好并发布到 Data：这里直接读，避免在评分路径上
	// 把 59 个候选重算一遍。
	if errText := stringOf(res.Data, "fingerprint_error"); errText != "" {
		// 分析失败（数据不配套等）属于工具问题，记为不可判，不当证据。
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title:    fmt.Sprintf("指纹比对未能完成：%s", errText),
		}}
	}
	analysis := fingerprintAnalysisOf(res.Data)
	if analysis == nil {
		return nil
	}

	switch analysis.Decision {
	case fingerprint.DecisionUnscorable:
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title: "模型没有按要求给出足够多的整数，指纹无法比对" +
				"（这不等于有问题：有的模型会拒绝这类任务）",
		}}

	case fingerprint.DecisionPartial:
		// 只有排名没有置信度：可以提示"最像谁"，但**不产风险分**。
		// 样本不足时排名本身就不稳，拿它定罪是过度解读。
		message := fmt.Sprintf("指纹样本不足，仅供参考：三条回答中最接近 %s（%s）",
			displayOrID(analysis), analysis.FamilyName)
		if requestedModel != "" && !sameFamily(analysis.Family, requestedModel) {
			message += fmt.Sprintf("，与声明的 %s 不同族——但样本不足，不下结论", requestedModel)
		}
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title:    message,
		}}
	}

	// ---- DecisionScorable：三条有效样本，有排名也有置信度 ----
	gap := 0.0
	if hint := fingerprint.DistanceHint(analysis.Results); hint != nil {
		gap = *hint
	}

	confidence := ""
	if analysis.Probability != nil {
		confidence = fmt.Sprintf("置信度 %.1f%%（参考库内相对值）", *analysis.Probability*100)
	}

	declared := InferFamilies(requestedModel)
	if len(declared) == 0 {
		// 认不出声明型号属于哪个家族（自定义别名、新发布型号等）：
		// 没有可比对象，既不能说一致也不能说不符。如实说"无法比较"。
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title: fmt.Sprintf("实测指纹最接近 %s（%s）%s，但认不出声明型号 %s 属于哪个家族，无法比较是否相符",
				displayOrID(analysis), analysis.FamilyName, confidenceOrEmpty(confidence), requestedModel),
		}}
	}
	matchesDeclared := declaredMatches(analysis, declared)

	if matchesDeclared {
		// 对上了：这是"没发现矛盾"，不是"证明了正品"。
		// 分数必须是 0——把"像"当成"是"，会让一次只能证伪的检测
		// 被读成官方认证。
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title: fmt.Sprintf("指纹与声明的 %s 一致，最接近 %s（%s）%s；"+
				"这只说明没有矛盾，不能作为正品证明", requestedModel, displayOrID(analysis), analysis.FamilyName, confidenceOrEmpty(confidence)),
		}}
	}

	// 不符：先看分差。分差小说明榜首本身就不稳，此时不下结论。
	if gap < fingerprintGapThreshold {
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title: fmt.Sprintf("指纹最接近 %s（%s），但与第二名仅差 %.2f，难以区分；"+
				"声明是 %s，样本不足以判断是否名副其实", displayOrID(analysis), analysis.FamilyName, gap, requestedModel),
		}}
	}

	return []Finding{{
		Probe:    ProbeFingerprint,
		Severity: SeverityMedium,
		Score:    fingerprintMismatchScore,
		Title: fmt.Sprintf("声明的 %s 与实测指纹不符：三条回答的取值习惯最接近 %s（%s）%s。"+
			"这是行为证据，不是模型自报——但参考库只覆盖有限型号，若该型号不在库内，也可能出现这种偏差",
			requestedModel, displayOrID(analysis), analysis.FamilyName, confidenceOrEmpty(confidence)),
	}}
}

// declaredMatches 判断指纹结论是否与声明型号相容。
//
// 判据刻意放宽到**家族级**：参考库在相邻小版本之间本来就难分
// （同代 opus 4.6 与 4.7 的分布余弦相似度 0.997），拿版本号去卡会大量误报。
// 只有"连家族都不对"才是可靠信号。
func declaredMatches(analysis *fingerprint.Analysis, declared []string) bool {
	// 调用方已保证 declared 非空（空的情况在上面单独处理成"无法比较"）。
	for _, family := range declared {
		if family == analysis.Family {
			return true
		}
	}
	return false
}

// sameFamily 判断某个家族是否落在声明型号的家族范围内。
func sameFamily(family, requestedModel string) bool {
	for _, declared := range InferFamilies(requestedModel) {
		if declared == family {
			return true
		}
	}
	return false
}

// displayOrID 优先用可读名，缺了再退回 id。
func displayOrID(analysis *fingerprint.Analysis) string {
	if analysis.PredictionName != "" {
		return analysis.PredictionName
	}
	return analysis.Prediction
}

// confidenceOrEmpty 让调用处的格式串保持可读。
func confidenceOrEmpty(confidence string) string {
	if confidence == "" {
		return ""
	}
	return "，" + confidence
}

// fingerprintAnalysisOf 从探针结果里还原判定所需的最小信息。
//
// 刻意只取结论字段，不碰 outputs：评分路径上没有理由再走一遍解析与推理。
func fingerprintAnalysisOf(data map[string]any) *fingerprint.Analysis {
	decision := stringOf(data, "fingerprint_decision")
	if decision == "" {
		return nil
	}
	a := &fingerprint.Analysis{
		Decision:       decision,
		Prediction:     stringOf(data, "fingerprint_prediction"),
		PredictionName: stringOf(data, "fingerprint_prediction_name"),
		Family:         stringOf(data, "fingerprint_family"),
		FamilyName:     stringOf(data, "fingerprint_family_name"),
		Reason:         stringOf(data, "fingerprint_reason"),
	}
	if p, ok := data["fingerprint_probability"].(float64); ok {
		a.Probability = &p
	}
	if candidates, ok := data["fingerprint_candidates"].([]map[string]any); ok {
		for _, c := range candidates {
			candidate := fingerprint.Candidate{
				Model:       stringOf(c, "model"),
				DisplayName: stringOf(c, "display_name"),
				Family:      stringOf(c, "family"),
				FamilyName:  stringOf(c, "family_name"),
			}
			if score, ok := c["score"].(float64); ok {
				candidate.Score = score
			}
			if p, ok := c["probability"].(float64); ok {
				pp := p
				candidate.Probability = &pp
			}
			a.Results = append(a.Results, candidate)
		}
	}
	if gap, ok := data["fingerprint_gap"].(float64); ok && len(a.Results) >= 2 {
		// DistanceHint 在判定时只用于取分差，这里直接把两名填好即可。
		a.RankingScore = a.Results[0].Score
		_ = gap
	}
	return a
}
