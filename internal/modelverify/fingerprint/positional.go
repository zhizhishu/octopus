package fingerprint

import "math"

// 位置/顺序判别的常量。**顺序不能改**：块顺序与权重必须与导出时的训练完全一致，
// 差一位就对不上（上游明确标注 "block order and weights must match the export"）。
var (
	positionalWeights = []float64{0.6, 0.15, 0.125, 0.125}

	// centres 是 24 个数值中心，覆盖 1..355。RBF 核半径固定 20，
	// 用来表达"某类数值更常出现在开头/中间/结尾"。
	positionalCentres = buildCentres(24)

	// lags 是序列自相关的滞后步长。取斐波那契数列是刻意的：
	// 相邻、短距、中距、长距都能覆盖，又不会因为等差而和"周期"混在一起。
	sequenceLags = []int{1, 2, 3, 5, 8, 13, 21, 34}
)

func buildCentres(n int) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = 1 + float64(i)*354/float64(n-1)
	}
	return out
}

// positionalDecision 计算单条回答在 59 个候选上的判别值。
//
// 输入是 4 个特征块拼成 751 维，按权重与标准化参数线性投影到候选数。
func positionalDecision(numbers []int, head PositionalHead) []float64 {
	parts := [][]float64{
		HellingerFeature(CountNumbers(numbers)),
		OrderedBlockFeature(numbers),
		positionKernel(numbers),
		sequenceFeatures(numbers),
	}

	var x []float64
	for blockIndex, block := range parts {
		p := head.Params[blockIndex]
		scaled := make([]float64, len(block))
		for i, v := range block {
			scaled[i] = (v - p.Mean[i]) / p.Scale[i]
		}
		length := math.Max(math.Sqrt(dot(scaled, scaled)), 1e-12)
		weight := math.Sqrt(positionalWeights[blockIndex])
		for _, v := range scaled {
			x = append(x, v/length*weight)
		}
	}

	out := make([]float64, len(head.Weights))
	for k, row := range head.Weights {
		sum := head.Bias[k]
		for i, w := range row {
			sum += w * x[i]
		}
		out[k] = sum
	}
	return out
}

// positionKernel 描述"数值 vs 相对位置"的交互：24 个 RBF + 10 个末位指示，
// 各自乘以 3 个勒让德多项式基（常数、线性、二次曲线），共 34*3 = 102 维。
//
// 直白说：它量化的是"模型是否倾向于把 300 以上的数字放在前三分之一"这类习惯。
// 这类习惯几乎不受提示词影响，因此比"分布长得像"更难伪装。
func positionKernel(x []int) []float64 {
	n := len(x)
	out := make([]float64, 34*3)
	if n == 0 {
		return out
	}
	for position, value := range x {
		t := (float64(position)+0.5)*2/float64(n) - 1
		basis := [3]float64{t, 0.5 * (3*t*t - 1), 0.5 * (5*t*t*t - 3*t)}
		features := make([]float64, 34)
		for k, centre := range positionalCentres {
			ratio := (float64(value) - centre) / 20
			features[k] = math.Exp(-0.5 * ratio * ratio)
		}
		for digit := 0; digit < 10; digit++ {
			if digit == value%10 {
				features[24+digit] = 1
			}
		}
		for k, w := range features {
			for j, b := range basis {
				out[k*3+j] += w * b / float64(n)
			}
		}
	}
	return out
}

// centredTransitions 是去边际的转移矩阵：只保留"从 A 到 B 比随机更多/更少"的部分。
//
// 不减边际的话，矩阵里绝大部分信息只是"哪些取值常见"，而那部分分布特征已经算过了；
// 减掉之后剩下的才是真正的顺序结构（值之间的依赖关系）。
func centredTransitions(symbols []int, bins int) []float64 {
	joint := make([]float64, bins*bins)
	for i := range joint {
		joint[i] = 0.5
	}
	total := float64(len(symbols)-1) + 0.5*float64(bins*bins)
	for i := 1; i < len(symbols); i++ {
		joint[symbols[i-1]*bins+symbols[i]]++
	}
	for i := range joint {
		joint[i] /= total
	}
	rows := make([]float64, bins)
	for a := 0; a < bins; a++ {
		sum := 0.0
		for b := 0; b < bins; b++ {
			sum += joint[a*bins+b]
		}
		rows[a] = sum
	}
	columns := make([]float64, bins)
	for b := 0; b < bins; b++ {
		sum := 0.0
		for a := 0; a < bins; a++ {
			sum += joint[a*bins+b]
		}
		columns[b] = sum
	}
	out := make([]float64, len(joint))
	for i, v := range joint {
		out[i] = v - rows[i/bins]*columns[i%bins]
	}
	return out
}

// sequenceFeatures 是 8 个滞后 × 6 项 + 两张转移矩阵 + 5 项局部统计，
// 共 48 + 64 + 100 + 5 + 3 = 220 维。
func sequenceFeatures(x []int) []float64 {
	n := len(x)
	out := make([]float64, 0, 220)
	if n < 2 {
		// 样本太短：长度检查会在上层把这条件判为无效，这里只需不崩。
		return make([]float64, 220)
	}

	m := meanOf(float64s(x))
	sd := 0.0
	for _, v := range x {
		sd += (float64(v) - m) * (float64(v) - m)
	}
	sd /= float64(n)
	sd = math.Max(math.Sqrt(sd), 1)
	s := make([]float64, n)
	for i, v := range x {
		s[i] = (float64(v) - m) / sd
	}

	for _, lag := range sequenceLags {
		count := n - lag
		if count <= 0 {
			out = append(out, 0, 0, 0, 0, 0, 0)
			continue
		}
		d := make([]float64, count)
		for i := 0; i < count; i++ {
			d[i] = float64(x[i+lag] - x[i])
		}
		autocorr := 0.0
		absMean := 0.0
		positive := 0
		zero := 0
		within5 := 0
		within20 := 0
		for i, diff := range d {
			autocorr += s[i+lag] * s[i]
			absMean += math.Abs(diff)
			if diff > 0 {
				positive++
			}
			if diff == 0 {
				zero++
			}
			if math.Abs(diff) <= 5 {
				within5++
			}
			if math.Abs(diff) <= 20 {
				within20++
			}
		}
		out = append(out,
			autocorr/float64(count),
			absMean/float64(count)/355,
			float64(positive)/float64(count),
			float64(zero)/float64(count),
			float64(within5)/float64(count),
			float64(within20)/float64(count),
		)
	}

	valueBins := make([]int, n)
	for i, v := range x {
		b := int(float64(v-1) * 8 / 355)
		if b > 7 {
			b = 7
		}
		valueBins[i] = b
	}
	out = append(out, centredTransitions(valueBins, 8)...)

	digitBins := make([]int, n)
	for i, v := range x {
		digitBins[i] = v % 10
	}
	out = append(out, centredTransitions(digitBins, 10)...)

	difference := make([]float64, n-1)
	zStep := make([]float64, n-1)
	for i := 0; i < n-1; i++ {
		difference[i] = float64(x[i+1] - x[i])
		zStep[i] = math.Abs(s[i+1] - s[i])
	}

	sameSign := 0
	for i := 0; i < n-2; i++ {
		if sign(difference[i+1]) == sign(difference[i]) {
			sameSign++
		}
	}
	sameDelta := 0
	for i := 0; i < n-2; i++ {
		if difference[i+1] == difference[i] {
			sameDelta++
		}
	}
	sameDigit := 0
	for i := 0; i < n-1; i++ {
		if x[i]%10 == x[i+1]%10 {
			sameDigit++
		}
	}
	out = append(out,
		float64(sameSign)/float64(n-2),
		float64(sameDelta)/float64(n-2),
		float64(sameDigit)/float64(n-1),
		fraction(n-1, func(i int) bool { return zStep[i] > 1 }),
		fraction(n-1, func(i int) bool { return zStep[i] > 2 }),
	)

	buckets := make([]int, n)
	for i, v := range x {
		b := int(float64(v-1) * 16 / 355)
		if b > 15 {
			b = 15
		}
		buckets[i] = b
	}
	for _, width := range []int{4, 8, 16} {
		sum := 0.0
		limit := n - width
		for i := 0; i <= limit; i++ {
			seen := make(map[int]struct{}, width)
			for j := i; j < i+width; j++ {
				seen[buckets[j]] = struct{}{}
			}
			sum += float64(len(seen)) / float64(width)
		}
		out = append(out, sum/float64(n-width+1))
	}
	return out
}

func fraction(length int, test func(i int) bool) float64 {
	if length <= 0 {
		return 0
	}
	hits := 0
	for i := 0; i < length; i++ {
		if test(i) {
			hits++
		}
	}
	return float64(hits) / float64(length)
}

func sign(v float64) float64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

func float64s(values []int) []float64 {
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = float64(v)
	}
	return out
}
