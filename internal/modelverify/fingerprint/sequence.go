package fingerprint

// 序列形态把关：这串数字像不像模型"手写"的？
//
// 模型凭第一反应写数时，相邻两个数通常跳得很远：相邻自相关明显为负，
// 相差不超过 20 的"小步"很少。脚本生成的随机数没有负自相关，排序过的数字
// 几乎全是小步，打乱顺序的回答两者都不像。中转站若拿脚本凑一串数来糊弄检测，
// 排名器照样会给出一个"最像"的候选——所以排名之前先把这类回答挡掉。
//
// 两条界限取自 lm-detector（MIT）文档公布的参考库统计（按提示环境分组的
// 三条回答组上 99.5% 分位数）。只有两项**同时**越界才判"不像"：模型回答可能
// 自相关偏弱，但小步依旧很少，单看一项会冤枉真模型。
//
// 这只是形态筛选，不是来源证明：通过它不等于确认是模型写的，没通过也只是
// "证据不足"，评分层绝不据此定罪。
const (
	sequenceAutocorrelationLimit = -0.23
	sequenceSmallStepLimit       = 0.05
	sequenceSmallStep            = 20
)

// SequenceShape 是有效回答的序列形态（各项取有效回答的中位数）。
type SequenceShape struct {
	Autocorrelation float64 `json:"autocorrelation"`
	SmallStepShare  float64 `json:"small_step_share"`
	ModelLike       bool    `json:"model_like"`
}

// sequenceShape 计算一组有效回答的形态。调用方保证 sequences 非空。
func sequenceShape(sequences [][]int) SequenceShape {
	autos := make([]float64, 0, len(sequences))
	smalls := make([]float64, 0, len(sequences))
	for _, seq := range sequences {
		a, s := shapeOf(seq)
		autos = append(autos, a)
		smalls = append(smalls, s)
	}
	shape := SequenceShape{Autocorrelation: median(autos), SmallStepShare: median(smalls)}
	shape.ModelLike = shape.Autocorrelation <= sequenceAutocorrelationLimit ||
		shape.SmallStepShare <= sequenceSmallStepLimit
	return shape
}

// shapeOf 返回单条序列的 lag-1 自相关与小步占比。
//
// 自相关 = mean((x[i+1]-m)(x[i]-m)) / var(x)；方差为 0（全是同一个数）时记 1，
// 等于"最不像模型"。少于两个数时没有步长，也按最不像处理。
func shapeOf(seq []int) (autocorrelation, smallStepShare float64) {
	if len(seq) < 2 {
		return 1, 1
	}
	values := float64s(seq)
	m := meanOf(values)
	variance := 0.0
	for _, v := range values {
		variance += (v - m) * (v - m)
	}
	variance /= float64(len(values))

	products := 0.0
	small := 0
	for i := 0; i+1 < len(values); i++ {
		products += (values[i+1] - m) * (values[i] - m)
		step := seq[i+1] - seq[i]
		if step < 0 {
			step = -step
		}
		if step <= sequenceSmallStep {
			small++
		}
	}
	steps := float64(len(values) - 1)
	if variance == 0 {
		autocorrelation = 1
	} else {
		autocorrelation = (products / steps) / variance
	}
	return autocorrelation, float64(small) / steps
}

// anomalousMinimum：一条回答里最小的有效整数都不低于它，说明取值整体挤在区间上段。
// 这种分布多由提示词引起（换题后通常消失），排名不可靠，要单独提示。
const anomalousMinimum = 200

func minimumOf(seq []int) int {
	if len(seq) == 0 {
		return 0
	}
	low := seq[0]
	for _, v := range seq[1:] {
		if v < low {
			low = v
		}
	}
	return low
}
