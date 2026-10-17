package fingerprint

import (
	"math"
	"strings"
	"unicode"
)

// 数值范围：所有挑战题都要求模型在 [1,355] 里取值。
//
// 这三个常量一旦与题库对不上，指纹就失去意义——参考库的每一维都按这个范围建。
const (
	ValueMin  = 1
	ValueMax  = 355
	Dimension = ValueMax - ValueMin + 1
	// alpha 是 Hellinger 嵌入的平滑量。它让"某数字一次都没出现"不等于概率 0，
	// 否则一条罕见取值就能把距离拉爆，把噪声读成换模型。
	alpha = 0.5
)

// ParseNumbers 从回答里摘出最长的一段合法整数序列。
//
// 上游规则（逐条照搬，因为解析错一位后面全错）：
//   - 连续数字算一个候选值；
//   - 字母（\p{L}，含中日韩文字）是**序列分隔符**：一旦出现，当前序列结束，另起一段；
//   - 超出 [1,355] 的数字不收集（是噪音，比如引用题号或年份）；
//   - 最后取最长的那一段——模型常会在序列前后加解释，最长段才是它真正在答题的部分。
func ParseNumbers(text string) []int {
	type match struct {
		start, end int
		value      int
	}
	var matches []match
	runes := []rune(text)
	i := 0
	for i < len(runes) {
		if !unicode.IsDigit(runes[i]) {
			i++
			continue
		}
		start := i
		value := 0
		overflow := false
		for i < len(runes) && unicode.IsDigit(runes[i]) {
			// 防溢出的同时保留"这是个数字"的事实：超长数字直接判为超范围。
			if value > 1_000_000 {
				overflow = true
			} else {
				value = value*10 + int(runes[i]-'0')
			}
			i++
		}
		if overflow {
			value = ValueMax + 1
		}
		matches = append(matches, match{start: start, end: i, value: value})
	}

	var runs [][]int
	var current []int
	previousEnd := 0
	for _, m := range matches {
		separator := runes[previousEnd:m.start]
		if len(current) > 0 && containsLetter(separator) {
			runs = append(runs, current)
			current = nil
		}
		if m.value >= ValueMin && m.value <= ValueMax {
			current = append(current, m.value)
		}
		previousEnd = m.end
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}

	best := []int{}
	for _, run := range runs {
		if len(run) > len(best) {
			best = run
		}
	}
	return best
}

func containsLetter(runes []rune) bool {
	for _, r := range runes {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// CountNumbers 把取值序列折成 355 维计数。
func CountNumbers(numbers []int) []int {
	counts := make([]int, Dimension)
	for _, n := range numbers {
		counts[n-ValueMin]++
	}
	return counts
}

// HellingerFeature 是平滑后开方的取值分布。
//
// 开方是为了让**特征空间的欧氏距离正比于 Hellinger 距离**——Hellinger 对分布差异
// 的度量比裸频率更稳，不会因为某个数字多出现几次就被主导。
func HellingerFeature(counts []int) []float64 {
	total := float64(alpha) * Dimension
	for _, c := range counts {
		total += float64(c)
	}
	out := make([]float64, Dimension)
	for i, c := range counts {
		out[i] = math.Sqrt((float64(c) + alpha) / total)
	}
	return out
}

// splitIntoFour 把序列切成四段等长（余数分给前面几段）。
func splitIntoFour(values []int) [][]int {
	base := len(values) / 4
	remainder := len(values) % 4
	chunks := make([][]int, 0, 4)
	start := 0
	for i := 0; i < 4; i++ {
		size := base
		if i < remainder {
			size++
		}
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[start:end])
		start = end
	}
	return chunks
}

// OrderedBlockFeature 是"位置与末位"特征，共 4*16+10 = 74 维。
//
// 它捕捉的是取值分布看不见的东西：同一个模型倾向把大数字放在开头还是结尾、
// 末位数字偏好哪几个。这两样都很难被"换个采样温度"抹掉。
func OrderedBlockFeature(numbers []int) []float64 {
	out := make([]float64, 0, 74*4)
	for _, chunk := range splitIntoFour(numbers) {
		bins := make([]float64, 16)
		for i := range bins {
			bins[i] = 0.5
		}
		for _, value := range chunk {
			index := int(float64(value-1) / 355 * 16)
			if index > 15 {
				index = 15
			}
			bins[index]++
		}
		total := 0.0
		for _, b := range bins {
			total += b
		}
		for _, b := range bins {
			out = append(out, math.Sqrt(b/total))
		}
	}
	lastDigits := make([]float64, 10)
	for i := range lastDigits {
		lastDigits[i] = 0.5
	}
	for _, value := range numbers {
		lastDigits[value%10]++
	}
	lastTotal := 0.0
	for _, d := range lastDigits {
		lastTotal += d
	}
	for _, d := range lastDigits {
		out = append(out, math.Sqrt(d/lastTotal))
	}
	return out
}

// ---------------------------------------------------------------- 向量小工具

func dot(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

func meanOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func norm(values []float64) float64 {
	return math.Sqrt(dot(values, values))
}

// normalized 单位化。分母与上游一致地下限到 1e-12，避免零向量除出 NaN。
func normalized(values []float64) []float64 {
	scale := math.Max(norm(values), 1e-12)
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = v / scale
	}
	return out
}

// zScore 标准化。同样用 1e-12 兜底。
func zScore(values []float64) []float64 {
	m := meanOf(values)
	variance := 0.0
	for _, v := range values {
		variance += (v - m) * (v - m)
	}
	variance /= float64(len(values))
	scale := math.Max(math.Sqrt(variance), 1e-12)
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = (v - m) / scale
	}
	return out
}

// subtractBasis 把向量投影掉给定子空间。
//
// 这是"去干扰"的核心动作：提示环境的措辞会让分布整体漂移，漂移方向由建库时的
// SVD 估计出来（至多 2 维）。先把工作向量里这一层投影掉，剩下的才是模型自身的差异。
func subtractBasis(values []float64, basis [][]float64) []float64 {
	out := append([]float64(nil), values...)
	for _, vector := range basis {
		projection := dot(out, vector)
		for i := range out {
			out[i] -= projection * vector[i]
		}
	}
	return out
}

// columnMean 对矩阵按列求均值（每行是一个样本，每个元素是一列）。
func columnMean(matrix [][]float64) []float64 {
	if len(matrix) == 0 {
		return nil
	}
	out := make([]float64, len(matrix[0]))
	for _, row := range matrix {
		for i, v := range row {
			out[i] += v
		}
	}
	for i := range out {
		out[i] /= float64(len(matrix))
	}
	return out
}

// median 中位数。用它而不是均值，是为了让个别离群样本不至于主导结果。
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	// 插入排序足够：样本数是个位数到几十。
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// splitTrailingLetters 用于判断"分隔符里有没有字母"，保留给解析层做可读性辅助。
func splitTrailingLetters(s string) string {
	return strings.TrimSpace(s)
}
