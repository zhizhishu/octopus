package behavior

import (
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/modelverify/fingerprint"
)

// 指纹探针的判定。
//
// 判定原则与其它探针一致，但这里要额外守一条：**只证伪，不认证**。
//
//   - 指纹与声明的型号对得上 → 无风险，但**不能**写成"确认是正品"：
//     参考库只有几十个候选，中转站用库里某个相近型号冒充也会对上。
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

	// fingerprintDowngradeScore 是"同族内换了型号"的风险分。
	//
	// 给 25，比跨族不符（35）低一档：同族换型号是真实信号，但参考库对不同型号的
	// 分辨力本身有限（相邻版本相似度可达 0.997），给到跟跨族一样高会把噪声判成事故。
	// 它必须 > 0——否则"opus 被 sonnet 顶替"就跟"完全没问题"显示成一个颜色。
	fingerprintDowngradeScore = 25

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
	// 把全部候选重算一遍。
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

	case fingerprint.DecisionNotModel:
		// 形态筛选只能说明"不像"，不能证明数字从哪来：记为证据不足，0 分，绝不定罪。
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title: "回答像脚本生成（排序、打乱或程序随机数），不像模型手写，未做指纹比对" +
				"（这只是证据不足，不据此判定渠道有问题；建议再测一轮）",
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

	// 有样本分布异常（取值整体挤在 200 以上）时，排名不可靠：只提示，不计分。
	if anomalous, ok := res.Data["fingerprint_anomalous_samples"].([]int); ok && len(anomalous) > 0 {
		return []Finding{{
			Probe:    ProbeFingerprint,
			Severity: SeverityLow,
			Score:    0,
			Title: fmt.Sprintf("有 %d 条回答取值分布异常（全部不低于 200），这一轮指纹结论不可靠，"+
				"最接近 %s 仅供参考；建议再测一轮", len(anomalous), displayOrID(analysis)),
		}}
	}
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
		// 家族对上了，但还要看**型号**对不对得上——这是本轮新增的一段。
		//
		// 家族级判定有个洞：opus 被 sonnet 顶替时同属 anthropic 家族，
		// 直接判"一致"0 分，而"同族换型号"恰恰是中转站最常见的偷法。
		if !modelMatchesDeclared(analysis, requestedModel) {
			predicted := displayOrID(analysis)
			// 分差够大才敢说是"同族降级"；分差小说明榜首本身不稳。
			if gap >= fingerprintGapThreshold {
				return []Finding{{
					Probe:    ProbeFingerprint,
					Severity: SeverityMedium,
					Score:    fingerprintDowngradeScore,
					Title: fmt.Sprintf("声明的 %s 与实测指纹指向的具体型号不同：三条回答最接近 %s（同属 %s）%s。"+
						"同一家族内换型号是最常见的一种掉包方式——这不代表一定有问题，"+
						"但也**不能**当成「一致」放过。参考库只覆盖有限型号，"+
						"若声明型号不在库内也会出现这种情况",
						requestedModel, predicted, analysis.FamilyName, confidenceOrEmpty(confidence)),
				}}
			}
			return []Finding{{
				Probe:    ProbeFingerprint,
				Severity: SeverityLow,
				Score:    0,
				Title: fmt.Sprintf("实测指纹最接近 %s（同属 %s），与第二名仅差 %.2f，难以区分；"+
					"声明是 %s，样本不足以判断是否同型号", predicted, analysis.FamilyName, gap, requestedModel),
			}}
		}
		// 型号与家族都对上了：这是"没发现矛盾"，不是"证明了正品"。
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
// 这里分三段，而不是简单比家族。原因是家族级比对会漏掉中转站最常见的一种偷法：
// **同级换型号**。opus 被 sonnet 顶替时两者同属 anthropic 家族，家族级判定直接
// 判"一致"，而现实里"100 组 Opus vs Sonnet 替换测试"恰恰是最典型的场景。
//
// 三段：
//  1. 榜首**型号级**与声明一致（含别名）→ 相容，0 分。
//  2. 同家族但型号不同，且分差 ≥ 阈值 → 视为"同族降级"，报 medium。
//     同族降级比跨族偷换更常见也更隐蔽，必须能报出来。
//  3. 同家族、型号不同、分差不足 → 保守判"难以区分"，不下结论。
//
// 为什么不再退到版本号粒度：参考库在相邻小版本之间确实难分（库内实测 opus 4.6
// 与 4.7 的余弦相似度 0.997），混淆发生在**版本**粒度而非**型号**粒度。
// 把判据整体退到家族，就把"本来能分的"也一起放过了。
func declaredMatches(analysis *fingerprint.Analysis, declared []string) bool {
	// 调用方已保证 declared 非空（空的情况在上面单独处理成"无法比较"）。
	for _, family := range declared {
		if family == analysis.Family {
			return true
		}
	}
	return false
}

// modelMatchesDeclared 判断榜首型号是否就是声明的那个型号。
//
// 只做宽松匹配，不做严格相等。三件必须容忍的事：
//   - 厂商前缀：声明常带 `anthropic/claude-3-5-sonnet`，库里未必带；
//   - 分隔符差异：声明 `claude-opus-4-6`、库里 `claude-opus-4.6` 是同一个型号；
//   - 大小写。
//
// 之所以要容忍这些，是因为把"同一个型号的不同写法"判成不同型号，会产生
// 一整批假警报——而假警报会让真警报也失去意义。
func modelMatchesDeclared(analysis *fingerprint.Analysis, requestedModel string) bool {
	if requestedModel == "" || analysis == nil {
		return false
	}
	declared := normalizeModelName(requestedModel)
	if declared == "" {
		return false
	}
	for _, c := range []string{analysis.Prediction, analysis.PredictionName} {
		if normalizeModelName(c) == declared {
			return true
		}
	}
	return false
}

// normalizeModelName 把型号写法归一，用于比较。
//
// 去掉厂商前缀、统一分隔符（`-` `.` `_` 空格都当同一个）、小写。
// 注意：只做"写法"归一，不做"版本"归一——4.6 和 4.7 仍然是不同的东西，
// 相邻版本本来就难以区分是已知事实，但不能因此假装它们一样。
func normalizeModelName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '-', '_', '.', ' ':
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
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
