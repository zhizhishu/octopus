package behavior

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/fingerprint"
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
// 但它**只能证伪，不能认证**：结论是参考库几十个候选里的相对排名，库外模型照样
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

	// fingerprintLowConfidence：库内置信度低于它时，结果页提示"这一轮还不够确定，再测一轮"。
	// 只影响提示，不影响评分。
	fingerprintLowConfidence = 0.75

	// fingerprintNeighborCount 是结果页展示的"本来就难分开"的近邻个数。
	fingerprintNeighborCount = 3
)

// 出题方式。
const (
	// FingerprintPromptRandom 每次随机出题（默认）：语言、措辞、数量都随机，中转站无法靠字面认出检测题。
	FingerprintPromptRandom = "random"
	// FingerprintPromptFixed 重放参考库的 36 道固定题，作为对照。
	FingerprintPromptFixed = "fixed"
)

type fingerprintPromptModeKey struct{}

// WithFingerprintPromptMode 把出题方式放进 ctx。没放时按随机出题。
//
// 走 ctx 而不是全局变量：同一进程里手动检测与后台队列可以各自读当时的设置，
// 测试也能并行地指定不同模式。
func WithFingerprintPromptMode(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, fingerprintPromptModeKey{}, mode)
}

func fingerprintPromptMode(ctx context.Context) string {
	if mode, ok := ctx.Value(fingerprintPromptModeKey{}).(string); ok && mode == FingerprintPromptFixed {
		return FingerprintPromptFixed
	}
	return FingerprintPromptRandom
}

// fingerprintChallengesFor 按出题方式取题。
func fingerprintChallengesFor(mode string) ([]fingerprint.Challenge, error) {
	if mode == FingerprintPromptFixed {
		return fingerprint.PickChallenges(fingerprintChallenges)
	}
	return fingerprint.GenerateChallenges(fingerprintChallenges), nil
}

// runFingerprint 采集三道题的原始回答。
//
// 探针层**不做判定**：只把三条原文和"每题期望多少个整数"带回给评分层。
// 这样即使以后换参考库、换特征，采集层都不用动。
//
// 出站：题目只是一段普通的 user 文本，经 Sender（渠道检测的既有出站链路）发出，
// 与真实流量同一个 shape——不加 reasoning 参数、不提前掐断流、不另走直连或代理。
// 唯一可调的输出预算也交给出站链路决定是否采用（Anthropic 渠道保持 CLI 原值）。
func runFingerprint(ctx context.Context, s modelverify.Sender, targetModel string) Result {
	res := Result{ProbeID: ProbeFingerprint, Data: map[string]any{}}

	mode := fingerprintPromptMode(ctx)
	res.Data["fingerprint_prompt_mode"] = mode
	challenges, err := fingerprintChallengesFor(mode)
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
			"language":       challenge.Language,
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
	res.Data["fingerprint_model_count"] = analysis.ModelCount
	res.Data["fingerprint_samples"] = fingerprintSamples(analysis, challenges)
	if analysis.Sequence != nil {
		// 结果页只说"像模型手写 / 像脚本生成"；原始数值留在证据里备查。
		res.Data["fingerprint_model_like"] = analysis.Sequence.ModelLike
		res.Data["fingerprint_sequence"] = map[string]any{
			"autocorrelation":  analysis.Sequence.Autocorrelation,
			"small_step_share": analysis.Sequence.SmallStepShare,
			"model_like":       analysis.Sequence.ModelLike,
		}
	}
	if len(analysis.AnomalousSamples) > 0 {
		res.Data["fingerprint_anomalous_samples"] = analysis.AnomalousSamples
	}

	// 前端按 insufficient_evidence 决定这一行显示成"证据不足"而不是"通过"。
	// 没有这一位，一次"模型拒答"会被读成"查过没问题"——正是我们要避免的。
	if analysis.Decision != fingerprint.DecisionScorable {
		res.Data["insufficient_evidence"] = true
		res.Data["reason"] = analysis.Reason
	}
	if analysis.Probability != nil {
		res.Data["fingerprint_probability"] = *analysis.Probability
		res.Data["fingerprint_low_confidence"] = *analysis.Probability < fingerprintLowConfidence
	}
	if analysis.Prediction != "" {
		names := make([]string, 0, fingerprintNeighborCount)
		for _, n := range fingerprint.NeighborsOf(analysis.Prediction, fingerprintNeighborCount) {
			names = append(names, n.Model)
		}
		if len(names) > 0 {
			res.Data["fingerprint_neighbors"] = names
		}
	}
	if rank, c := declaredCandidate(analysis.Results, targetModel); c != nil {
		res.Data["fingerprint_declared_model"] = c.Model
		res.Data["fingerprint_declared_rank"] = rank
		if c.Probability != nil {
			res.Data["fingerprint_declared_probability"] = *c.Probability
		}
	}
	if hint := fingerprint.DistanceHint(analysis.Results); hint != nil {
		res.Data["fingerprint_gap"] = *hint
	}
	// 全部候选都带上：结果页要能"显示全部"，也要能标出声明型号排第几。
	// 64 个候选、每个五个字段，存进报告也只有几 KB。
	candidates := make([]map[string]any, 0, len(analysis.Results))
	for _, c := range analysis.Results {
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

// fingerprintSamples 是每条回答的体检结果（给结果页的三张样本小卡用）。
func fingerprintSamples(analysis *fingerprint.Analysis, challenges []fingerprint.Challenge) []map[string]any {
	out := make([]map[string]any, 0, len(analysis.Diagnostics))
	for _, d := range analysis.Diagnostics {
		item := map[string]any{
			"index":     d.Index,
			"parsed":    d.ParsedNumbers,
			"minimum":   d.MinimumNumbers,
			"accepted":  d.Accepted,
			"anomalous": d.Anomalous,
		}
		if d.Index < len(challenges) {
			item["expected"] = challenges[d.Index].ExpectedCount
			if lang := challenges[d.Index].Language; lang != "" {
				item["language"] = fingerprint.LanguageNames[lang]
			}
		}
		out = append(out, item)
	}
	return out
}

// declaredCandidate 在排名里找到声明型号，返回名次（从 1 起）与候选。
//
// 写法差异按 normalizeModelName 归一；声明带日期后缀（如 -20260101）或 -latest
// 时，退回不带后缀的库内型号。找不到返回 (0, nil)——库里没有这个型号，
// 结果页就不显示"排第几"，而不是硬凑一个最像的。
func declaredCandidate(results []fingerprint.Candidate, declared string) (int, *fingerprint.Candidate) {
	want := normalizeModelName(declared)
	if want == "" {
		return 0, nil
	}
	bestRank, bestLen := 0, 0
	for i := range results {
		id := normalizeModelName(results[i].Model)
		if id == "" {
			continue
		}
		if id == want {
			return i + 1, &results[i]
		}
		if strings.HasPrefix(want, id+"-") && isVersionSuffix(want[len(id)+1:]) && len(id) > bestLen {
			bestRank, bestLen = i+1, len(id)
		}
	}
	if bestRank > 0 {
		return bestRank, &results[bestRank-1]
	}
	return 0, nil
}

// isVersionSuffix：纯日期/数字，或 latest。
func isVersionSuffix(s string) bool {
	if s == "latest" {
		return true
	}
	if len(s) < 4 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
