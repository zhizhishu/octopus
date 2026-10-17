package fingerprint

import (
	"fmt"
	"math"
	"sort"
)

// transform 把两条特征块（取值分布 355 维 + 位置末位 74 维）标准化、单位化、加权拼接。
//
// 0.75 : 0.25 的权重意味着"取值分布"比"位置末位"更重要——后者只在分布接近时
// 才起区分作用。这个比例来自上游的离线评估，不要凭感觉调。
func transform(blocks [][]float64, params []Preprocessing) []float64 {
	out := make([]float64, 0, 429)
	for j, block := range blocks {
		p := params[j]
		scaled := make([]float64, len(block))
		for i, v := range block {
			scaled[i] = (v - p.Mean[i]) / p.Scale[i]
		}
		unit := normalized(scaled)
		weight := math.Sqrt(0.25)
		if j == 0 {
			weight = math.Sqrt(0.75)
		}
		for _, v := range unit {
			out = append(out, v*weight)
		}
	}
	return out
}

// centered 按特征库的均值/尺度标准化。
//
// OrderedBlocks 与 FeatureBank 的前两个字段语义相同，但类型不同（前者多一层
// 环境模板）。这里收两个切片而不是结构体，避免为了复用而把两种类型强行合并。
func centered(v []float64, mean, scale []float64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = (x - mean[i]) / scale[i]
	}
	return out
}

// nearest 是到参考样本的 k 近邻平方距离均值（k=7）。
//
// 用"与最近的 7 条参考回答的平均距离"而不是到质心的距离：质心会把多峰分布抹平，
// 而模型的输出分布未必是单峰。k=7 是上游留一验证选定的。
func nearest(x []float64, references [][]float64) float64 {
	const k = 7
	xx := dot(x, x)
	dists := make([]float64, len(references))
	for i, r := range references {
		d := xx + dot(r, r) - 2*dot(x, r)
		if d < 0 {
			d = 0
		}
		dists[i] = d
	}
	sort.Float64s(dists)
	limit := k
	if limit > len(dists) {
		limit = len(dists)
	}
	sum := 0.0
	for i := 0; i < limit; i++ {
		sum += dists[i]
	}
	return sum / float64(limit)
}

// baseline 是"去干扰中心"打分，即频率排名的第三项（权重 0.25）。
//
// 它做两件事：
//  1. 取值分布块：投影掉干扰基后，与各模型中心求余弦，再标准化；
//  2. 位置末位块：既与"各模型中心"比（nuisance），也与"每个环境各自的模板"比
//     （template，取最像的那个环境），最后把两者各半合成。
//
// 第 2 步的意义：同一条渠道可能换了提示词风格，若只跟全局中心比，风格差异会被
// 误读成换了模型。跟"最近的环境模板"比就把这层差异吸收掉了。
func baseline(blocks [][]float64, bank Bank) []float64 {
	h := bank.Hellinger
	o := bank.OrderedBlocks

	hUnit := normalized(subtractBasis(centered(blocks[0], h.FeatureMean, h.FeatureScale), h.NuisanceBasis))
	marginal := make([]float64, len(h.Centroids))
	for i, row := range h.Centroids {
		marginal[i] = dot(hUnit, row)
	}

	oCentered := centered(blocks[1], o.FeatureMean, o.FeatureScale)
	raw := normalized(oCentered)
	projected := normalized(subtractBasis(oCentered, o.NuisanceBasis))

	templates := make([]float64, len(o.Centroids))
	for i := range o.Centroids {
		best := math.Inf(-1)
		for _, rows := range o.EnvironmentCentroids {
			if i >= len(rows) {
				continue
			}
			if v := dot(raw, rows[i]); v > best {
				best = v
			}
		}
		if math.IsInf(best, -1) {
			best = 0
		}
		templates[i] = best
	}
	nuisance := make([]float64, len(o.Centroids))
	for i, row := range o.Centroids {
		nuisance[i] = dot(projected, row)
	}

	tz := zScore(templates)
	nz := zScore(nuisance)
	ordered := make([]float64, len(tz))
	for i := range tz {
		ordered[i] = 0.5*tz[i] + 0.5*nz[i]
	}
	ordered = zScore(ordered)
	mz := zScore(marginal)

	out := make([]float64, len(mz))
	for i := range mz {
		out[i] = 0.5*mz[i] + 0.5*ordered[i]
	}
	return out
}

// RankNumbers 是完整的排名计算：输入若干条已解析的合法序列，输出每个候选的排名分。
//
// 合成方式（照搬上游）：
//
//	frequency = 0.5*z(LDA) + 0.25*z(近邻) + 0.25*z(去干扰中心)
//	positional = z(每条回答的位置判别值取平均)
//	ranking   = 0.5*frequency + 0.5*positional
func RankNumbers(sequences [][]int, det *Detector) ([]float64, error) {
	if len(sequences) == 0 {
		return nil, fmt.Errorf("没有可用的数列")
	}
	a := det.Ranker
	modelCount := len(det.ModelIDs)

	full := make([][][]float64, len(sequences))
	ax := make([][]float64, len(sequences))
	for i, seq := range sequences {
		full[i] = [][]float64{HellingerFeature(CountNumbers(seq)), OrderedBlockFeature(seq)}
		ax[i] = transform(full[i], a.FullParams)
	}

	// LDA：只用前 128 个整数——前段的取值偏好更接近模型的"第一反应"，
	// 后段更容易被"避免重复"之类的自我修正污染。
	ldaRows := make([][]float64, len(sequences))
	for i, seq := range sequences {
		head := seq
		if len(head) > 128 {
			head = head[:128]
		}
		x := transform([][]float64{HellingerFeature(CountNumbers(head)), OrderedBlockFeature(head)}, a.HeadParams)
		row := make([]float64, len(a.LDAWeights))
		for k, w := range a.LDAWeights {
			row[k] = dot(w, x) + a.LDABias[k]
		}
		ldaRows[i] = zScore(row)
	}
	l := zScore(columnMean(ldaRows))

	// 近邻项：先取负距离（越近越大），多条回答取中位数（抗单条离群）。
	nearRaw := make([]float64, modelCount)
	for m := 0; m < modelCount && m < len(a.References); m++ {
		perAnswer := make([]float64, len(ax))
		for i := range ax {
			perAnswer[i] = -nearest(ax[i], a.References[m])
		}
		nearRaw[m] = median(perAnswer)
	}
	near := zScore(nearRaw)

	// 去干扰中心项：每条回答各算一份，再取列均值。
	baseRows := make([][]float64, len(full))
	for i := range full {
		baseRows[i] = baseline(full[i], a.Bank)
	}
	base := zScore(columnMean(baseRows))

	frequency := make([]float64, modelCount)
	for i := 0; i < modelCount; i++ {
		frequency[i] = 0.5*l[i] + 0.25*near[i] + 0.25*base[i]
	}
	frequency = zScore(frequency)

	// 位置/顺序判别：每条回答出一份，标准化后取列均值，再标准化。
	positionalRows := make([][]float64, len(sequences))
	for i, seq := range sequences {
		positionalRows[i] = zScore(positionalDecision(seq, a.Positional))
	}
	positional := zScore(columnMean(positionalRows))

	ranking := make([]float64, modelCount)
	for i := 0; i < modelCount; i++ {
		ranking[i] = a.Blend.Frequency*frequency[i] + a.Blend.Positional*positional[i]
	}
	for _, v := range ranking {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("排名计算出现无效数值")
		}
	}
	return ranking, nil
}

// Calibrate 把排名分转成参考库内的相对概率。
//
// **这不是"确实是该模型"的概率**：它是闭集 softmax，只在这 59 个候选之间分配，
// 合计 100%。库外模型也会被摊到某个候选头上。校验收紧到与上游完全一致：
// 校准参数与当前检测器一旦不匹配（数据换了、模型数变了），直接返回 nil 表示
// "只排名、不给置信度"，绝不拿错配的校准编造一个看起来很像真的概率。
func Calibrate(ranking []float64, det *Detector) []float64 {
	head := det.Calibration
	if head == nil {
		return nil
	}
	binding := head.Binding
	if head.Schema != "shared-confidence-v2" || head.Method != "ranking-temperature" {
		return nil
	}
	if math.IsNaN(head.Tau) || math.IsInf(head.Tau, 0) || head.Tau < 0.001 || head.Tau > 1000 {
		return nil
	}
	if binding.BaseSHA256 != det.BaseSHA256 || binding.ReferenceSHA256 != det.SourceReferenceSHA256 {
		return nil
	}
	if len(binding.ModelIDs) != len(ranking) || len(ranking) != len(det.ModelIDs) {
		return nil
	}
	for i, id := range binding.ModelIDs {
		if id != det.ModelIDs[i] {
			return nil
		}
	}

	logits := make([]float64, len(ranking))
	maxLogit := math.Inf(-1)
	for i, score := range ranking {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil
		}
		logits[i] = head.Tau * score
		if logits[i] > maxLogit {
			maxLogit = logits[i]
		}
	}
	weights := make([]float64, len(logits))
	sum := 0.0
	for i, v := range logits {
		weights[i] = math.Exp(v - maxLogit)
		sum += weights[i]
	}
	if sum <= 0 {
		return nil
	}
	for i := range weights {
		weights[i] /= sum
	}
	return weights
}
