package fingerprint

import (
	"fmt"
	"math"
	"sort"
)

// Output 是一条回答（原文 + 期望数量）。
type Output struct {
	Text          string
	ExpectedCount int
}

// Diagnostic 是每条回答的解析体检，用于向用户解释"为什么这条没用上"。
type Diagnostic struct {
	Index          int  `json:"index"`
	ParsedNumbers  int  `json:"parsed_numbers"`
	MinimumNumbers int  `json:"minimum_numbers"`
	Accepted       bool `json:"accepted"`
}

// Candidate 是一个候选模型的排名结果。
type Candidate struct {
	Model       string   `json:"model"`
	DisplayName string   `json:"display_name"`
	Family      string   `json:"family"`
	FamilyName  string   `json:"family_name"`
	Score       float64  `json:"score"`
	Probability *float64 `json:"probability"`
}

// Decision 取值，语义与上游一致。
const (
	// DecisionScorable 三条回答都有效，给了完整排名。
	DecisionScorable = "scorable"
	// DecisionPartial 有效回答不足三条，只有排名、没有置信度。
	DecisionPartial = "partial"
	// DecisionUnscorable 有效回答太少，连排名都不该给。
	DecisionUnscorable = "unscorable"
)

// Analysis 是一次指纹检测的完整结论。
type Analysis struct {
	Decision        string       `json:"decision"`
	Prediction      string       `json:"prediction"`
	PredictionName  string       `json:"prediction_name"`
	Family          string       `json:"family_prediction"`
	FamilyName      string       `json:"family_prediction_name"`
	Results         []Candidate  `json:"results"`
	RankingScore    float64      `json:"ranking_score"`
	Probability     *float64     `json:"probability"`
	ProbabilityNote string       `json:"probability_scope"`
	UsedOutputs     int          `json:"used_outputs"`
	Diagnostics     []Diagnostic `json:"diagnostics"`
	// Reason 是给用户看的一句话解释（为何不足/为何没概率）。
	Reason string `json:"reason"`
	// ReferenceBuiltAt 暴露参考库版本，方便判断结论是否已经过时。
	ReferenceBuiltAt string `json:"reference_built_at"`
}

// minimumAccepted 是单条回答被采信所需的最少整数个数。
//
// 上游规则：max(80, ceil(0.55 * 期望数量))。设下限是为了挡掉"拒绝作答"和
// "半截被截断"——这类回答的分布根本不可比，拿去算会污染排名。
func minimumAccepted(expected int) int {
	floor := int(math.Ceil(float64(expected) * 0.55))
	if floor < 80 {
		return 80
	}
	return floor
}

// Analyze 执行完整推理：解析 → 校验 → 排名 → 校准 → 出结论。
func Analyze(outputs []Output) (*Analysis, error) {
	det, err := LoadDetector()
	if err != nil {
		return nil, err
	}
	models, err := LoadModels()
	if err != nil {
		return nil, err
	}
	// 库与检测器必须配套：上游把这条做成硬校验，因为错配的库会让排名
	// 完全失去意义，而且错得很隐蔽——照样给出一个"最像"的答案。
	if models.BuiltAt != det.BankBuiltAt {
		return nil, fmt.Errorf("参考库与检测器不配套：库 %q / 检测器 %q", models.BuiltAt, det.BankBuiltAt)
	}
	if len(models.Models) != len(det.ModelIDs) {
		return nil, fmt.Errorf("参考库模型数 %d 与检测器 %d 不一致", len(models.Models), len(det.ModelIDs))
	}
	for i, m := range models.Models {
		if m.ID != det.ModelIDs[i] || m.ResponseCount != det.ResponseCounts[i] {
			return nil, fmt.Errorf("参考库第 %d 个模型与检测器不一致", i)
		}
	}

	parsed := make([]([]int), len(outputs))
	diagnostics := make([]Diagnostic, len(outputs))
	usable := make([][]int, 0, len(outputs))
	for i, o := range outputs {
		nums := ParseNumbers(o.Text)
		minimum := minimumAccepted(o.ExpectedCount)
		accepted := len(nums) >= minimum
		parsed[i] = nums
		diagnostics[i] = Diagnostic{
			Index:          i,
			ParsedNumbers:  len(nums),
			MinimumNumbers: minimum,
			Accepted:       accepted,
		}
		if accepted {
			usable = append(usable, nums)
		}
	}

	base := &Analysis{
		UsedOutputs:      len(usable),
		Diagnostics:      diagnostics,
		ReferenceBuiltAt: det.BankBuiltAt,
	}

	if len(usable) == 0 {
		base.Decision = DecisionUnscorable
		base.Reason = "没有一条回答含足够多的合法整数，无法比对"
		return base, nil
	}

	ranking, err := RankNumbers(usable, det)
	if err != nil {
		return nil, err
	}
	probabilities := Calibrate(ranking, det)
	if len(usable) < 3 {
		// 不足三条只给排名：置信度是在三条样本上校准的，硬套会给出虚高的数字。
		probabilities = nil
	}

	order := make([]int, len(det.ModelIDs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return ranking[order[a]] > ranking[order[b]]
	})

	results := make([]Candidate, 0, len(order))
	for _, idx := range order {
		m := models.Models[idx]
		c := Candidate{
			Model:       det.ModelIDs[idx],
			DisplayName: m.DisplayName,
			Family:      m.Family,
			FamilyName:  m.FamilyName,
			Score:       ranking[idx],
		}
		if probabilities != nil {
			p := probabilities[idx]
			c.Probability = &p
		}
		results = append(results, c)
	}

	best := results[0]
	base.Results = results
	base.Prediction = best.Model
	base.PredictionName = best.DisplayName
	base.Family = best.Family
	base.FamilyName = best.FamilyName
	base.RankingScore = best.Score

	if len(usable) < 3 || probabilities == nil {
		base.Decision = DecisionPartial
		if len(usable) < 3 {
			base.Reason = fmt.Sprintf("只有 %d 条有效回答，给出的是排名而非置信度", len(usable))
		} else {
			base.Reason = "校准参数与当前检测器不匹配，只给排名不给置信度"
		}
		return base, nil
	}

	base.Decision = DecisionScorable
	base.Probability = best.Probability
	base.ProbabilityNote = "reference-closed-set"
	base.Reason = "置信度是参考库内的闭集相对概率，不包含库外模型；排名靠前只说明最像，不代表正品证明"
	return base, nil
}

// DistanceHint 给出前两名之间的分差，供判定"这次结论有多可信"。
//
// 分差小意味着"这两个模型都说得通"，此时任何斩钉截铁的结论都是过度解读。
// 返回 nil 表示候选不足两名。
func DistanceHint(results []Candidate) *float64 {
	if len(results) < 2 {
		return nil
	}
	gap := results[0].Score - results[1].Score
	return &gap
}
