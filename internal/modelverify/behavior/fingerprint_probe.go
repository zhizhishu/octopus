package behavior

import (
	"context"
	"fmt"

	"github.com/bestruirui/octopus/internal/modelverify"
	"github.com/bestruirui/octopus/internal/modelverify/fingerprint"
)

// 随机数指纹探针。
//
// 做法：让模型"凭第一反应"挑几百个 1..355 的整数，重复三次（三道不同措辞的题），
// 然后把三条回答的取值分布与参考库比，看它最像哪个模型。
//
// 为什么这比"问你是谁"硬：口供可以被中转站改一个字，而"挑数字的习惯"改不了——
// 它来自训练数据分布与采样实现，模型自己也说不清为什么偏好某些数。中转站若用
// 别的模型冒充，指纹对不上；若它真的转发给了目标模型，指纹自然吻合。
//
// 但它**只能证伪，不能认证**：结论是参考库 59 个候选里的相对排名，库外模型照样
// 会得到一个"最像"的答案。所以这里只负责采样本，判定交给评分层，且措辞必须
// 保留"参考库内相对"这个限定，不能写成"确认是某模型"。
const (
	// ProbeFingerprint 是随机数指纹探针的 id。
	ProbeFingerprint ProbeID = "fingerprint"

	// fingerprintChallenges 是发送的题目数量。
	//
	// 3 是参考库校准时的前提：置信度是在"三条有效回答"上拟合的，
	// 少于三条就只能给排名、不能给概率。发更多并不会让结论更硬，
	// 只是多花额度，所以固定 3。
	fingerprintChallenges = 3

	// fingerprintMaxTokens 是单题输出预算。
	//
	// 题目要求 218~333 个整数，每个整数最多 4 个字符；再留出模型可能加的解释
	// 和代码块标记的余量。给得太紧会把回答截断，而截断的回答分布不可比
	// （参考库只收完整的样本），等于白花钱。
	fingerprintMaxTokens = 4096
)

// runFingerprint 采集三道题的原始回答。
//
// 探针层**不做判定**：只把三条原文和"每题期望多少个整数"带回给评分层。
// 这样即使以后换参考库、换特征，采集层都不用动。
func runFingerprint(ctx context.Context, s modelverify.Sender, _ string) Result {
	res := Result{ProbeID: ProbeFingerprint, Data: map[string]any{}}

	challenges, err := fingerprint.PickChallenges(fingerprintChallenges)
	if err != nil {
		res.Err = fmt.Errorf("加载挑战题库失败: %w", err)
		return res
	}

	outputs := make([]fingerprint.Output, 0, len(challenges))
	challengeIDs := make([]string, 0, len(challenges))
	for _, challenge := range challenges {
		if err := ctx.Err(); err != nil {
			res.Err = err
			return res
		}
		// 逐题串行发送：与整套审计的节奏一致，避免同时抬高上游限流压力。
		resp, askErr := s.Ask(ctx, modelverify.Ask{
			Prompt:    challenge.PromptWithPrefix(),
			MaxTokens: intPtr(fingerprintMaxTokens),
		})
		if askErr != nil {
			// 任何一题失败都算采集失败——缺一条会让置信度失去校准前提。
			// 宁可报"这次没测成"，也不拿两条样本硬凑一个概率出来。
			res.Err = fmt.Errorf("第 %d 题请求失败: %w", len(outputs)+1, askErr)
			return res
		}
		outputs = append(outputs, fingerprint.Output{
			Text:          resp.Content,
			ExpectedCount: challenge.ExpectedCount,
		})
		challengeIDs = append(challengeIDs, challenge.CHALLENGEID)
		res.Data[fmt.Sprintf("challenge_%d", len(outputs))] = map[string]any{
			"id":             challenge.CHALLENGEID,
			"condition":      challenge.Condition,
			"expected_count": challenge.ExpectedCount,
			"finish_reason":  resp.FinishReason,
			"content_len":    len(resp.Content),
		}
	}

	res.OK = true
	res.Data["challenge_ids"] = challengeIDs

	// 原始回答是要留的：它是"这条结论从哪来"的唯一凭据，出问题时要能回看。
	// 但不能无限大——真实回答常有几千字，还有模型会加长解释。单条封顶后
	// 只丢掉超出部分，判定用的是完整 outputs（内存里），不受截断影响。
	const maxEvidenceChars = 1200
	evidence := make([]map[string]any, 0, len(outputs))
	for i, o := range outputs {
		text := o.Text
		truncated := false
		if len(text) > maxEvidenceChars {
			text = text[:maxEvidenceChars]
			truncated = true
		}
		item := map[string]any{
			"index":          i,
			"expected_count": o.ExpectedCount,
			"text":           text,
		}
		if truncated {
			item["truncated"] = true
			item["full_length"] = len(o.Text)
		}
		evidence = append(evidence, item)
	}
	res.Data["answers"] = evidence
	res.Data["outputs"] = outputs

	// 这里就把结论算出来，而不是留给评分层再做一次：
	// 评分层需要 decision 来判断"这条探针算不算跑通"，而 outputs 是原始大对象，
	// 不适合在评分路径上反复解析。算一次，两个用途共用。
	analysis, analysisErr := fingerprint.Analyze(outputs)
	if analysisErr != nil {
		res.Data["fingerprint_decision"] = ""
		res.Data["fingerprint_error"] = analysisErr.Error()
		return res
	}
	res.Data["fingerprint_decision"] = analysis.Decision
	res.Data["fingerprint_prediction"] = analysis.Prediction
	res.Data["fingerprint_prediction_name"] = analysis.PredictionName
	res.Data["fingerprint_family"] = analysis.Family
	res.Data["fingerprint_family_name"] = analysis.FamilyName
	res.Data["fingerprint_reason"] = analysis.Reason
	res.Data["fingerprint_reference_built_at"] = analysis.ReferenceBuiltAt
	res.Data["fingerprint_used_outputs"] = analysis.UsedOutputs
	res.Data["fingerprint_probability_scope"] = analysis.ProbabilityNote
	if analysis.Probability != nil {
		res.Data["fingerprint_probability"] = *analysis.Probability
	}
	if hint := fingerprint.DistanceHint(analysis.Results); hint != nil {
		res.Data["fingerprint_gap"] = *hint
	}
	// 只带前五名进 Data：完整 59 名对判定没用，但会把结果对象撑得很大，
	// 前端和日志都不需要那么多。
	top := analysis.Results
	if len(top) > 5 {
		top = top[:5]
	}
	candidates := make([]map[string]any, 0, len(top))
	for _, c := range top {
		entry := map[string]any{
			"model":        c.Model,
			"display_name": c.DisplayName,
			"family":       c.Family,
			"family_name":  c.FamilyName,
			"score":        c.Score,
		}
		if c.Probability != nil {
			entry["probability"] = *c.Probability
		}
		candidates = append(candidates, entry)
	}
	res.Data["fingerprint_candidates"] = candidates
	return res
}
