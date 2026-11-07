package numberdist

import (
	"fmt"
	"math"
	"sort"
)

// Distribution 把有效数字（1..355）归一化成 355 桶分布 p，Σp=1。
//
// 与 hlwy calculateDistribution（hlwy-ai-checker.html:2535-2540）一致：counts[n-1]++ 后
// 除以样本总数。空输入或含越界数字返回 ok=false（拒绝坏数据，不静默丢样本）。
func Distribution(numbers []int) (dist [DistLength]float64, ok bool) {
	var counts [DistLength]int
	for _, n := range numbers {
		if n < 1 || n > DistLength {
			return [DistLength]float64{}, false
		}
		counts[n-1]++
	}
	if len(numbers) == 0 {
		return [DistLength]float64{}, false
	}
	total := float64(len(numbers))
	for i := 0; i < DistLength; i++ {
		dist[i] = float64(counts[i]) / total
	}
	return dist, true
}

// StatsOf 计算描述统计。上中位数（sorted[n/2]）；众数并列取最小整数
// （对齐原 JS 整数属性升序枚举，hlwy-ai-checker.html:2542-2556）。空输入返回零值。
func StatsOf(numbers []int) Stats {
	if len(numbers) == 0 {
		return Stats{}
	}
	sorted := append([]int(nil), numbers...)
	sort.Ints(sorted)

	sum := 0
	for _, n := range numbers {
		sum += n
	}
	mean := float64(sum) / float64(len(numbers))
	var variance float64
	for _, n := range numbers {
		d := float64(n) - mean
		variance += d * d
	}
	variance /= float64(len(numbers))

	counts := map[int]int{}
	for _, n := range numbers {
		counts[n]++
	}
	mode, modeCount := 0, 0
	for k, c := range counts {
		if c > modeCount || (c == modeCount && k < mode) {
			mode, modeCount = k, c
		}
	}

	return Stats{
		Mean:      mean,
		StdDev:    math.Sqrt(variance),
		Median:    sorted[len(sorted)/2],
		Min:       sorted[0],
		Max:       sorted[len(sorted)-1],
		Unique:    len(counts),
		Mode:      mode,
		ModeCount: modeCount,
	}
}

// Score 对两条归一化 355 桶分布套用 hlwy 原公式（hlwy-ai-checker.html:2560-2598）：
//
//	m = (p+q)/2, eps = 1e-12, 自然对数
//	JSD = 0.5*Σp*ln((p+eps)/(m+eps)) + 0.5*Σq*ln((q+eps)/(m+eps))
//	cos = dot(p,q) / (|p|*|q|)
//	modeScore = 1 (同众数) 或 max(0, 1-|Δmode|/50)
//	overall = 0.5*modeScore + 0.5*cos*exp(-JSD)
//
// 输入契约：p/q 必须是有限、非负、且**已归一化**（Σ≈1，容差 1e-6）的分布；pMode/qMode
// 必须是有效众数（1..355）。否则返回错误——空/坏/原始计数不返回可展示的 0% 结论，也
// 不把任意原始计数静默当概率归一化。数值上只校正浮点误差（JSD 微小负值、最终值
// 落回 0..1），不改变公式。
func Score(p, q [DistLength]float64, pMode, qMode int) (ScoreResult, error) {
	if pMode < 1 || pMode > DistLength || qMode < 1 || qMode > DistLength {
		return ScoreResult{}, fmt.Errorf("众数越界: p=%d q=%d", pMode, qMode)
	}
	var sum1, sum2 float64
	for i := 0; i < DistLength; i++ {
		if !isFinite(p[i]) || !isFinite(q[i]) || p[i] < 0 || q[i] < 0 {
			return ScoreResult{}, fmt.Errorf("分布第 %d 桶非有限或为负", i)
		}
		sum1 += p[i]
		sum2 += q[i]
	}
	if !isFinite(sum1) || !isFinite(sum2) {
		return ScoreResult{}, fmt.Errorf("分布总和溢出为非有限值: Σp=%g Σq=%g", sum1, sum2)
	}
	if math.Abs(sum1-1) > sumTolerance || math.Abs(sum2-1) > sumTolerance {
		return ScoreResult{}, fmt.Errorf("分布未归一化: Σp=%g Σq=%g（要求≈1）", sum1, sum2)
	}

	const epsilon = 1e-12
	var dot, norm1, norm2, jsDiv float64
	for i := 0; i < DistLength; i++ {
		pi := p[i]
		qi := q[i]
		dot += pi * qi
		norm1 += pi * pi
		norm2 += qi * qi
		m := (pi + qi) / 2
		if pi > 0 {
			jsDiv += 0.5 * pi * math.Log((pi+epsilon)/(m+epsilon))
		}
		if qi > 0 {
			jsDiv += 0.5 * qi * math.Log((qi+epsilon)/(m+epsilon))
		}
	}
	if jsDiv < 0 {
		jsDiv = 0 // 仅校正浮点误差造成的微小负值；JSD 数学上非负
	}
	cosine := dot / (math.Sqrt(norm1) * math.Sqrt(norm2))
	distribScore := cosine * math.Exp(-jsDiv)

	var modeScore float64
	if pMode == qMode {
		modeScore = 1.0
	} else {
		diff := math.Abs(float64(pMode - qMode))
		modeScore = math.Max(0, 1-diff/50)
	}
	overall := clamp01(modeScore*0.5 + distribScore*0.5)

	if !isFinite(cosine) || !isFinite(jsDiv) || !isFinite(modeScore) || !isFinite(overall) {
		return ScoreResult{}, fmt.Errorf("评分出现非有限值")
	}
	return ScoreResult{
		Cosine:    cosine,
		JSD:       jsDiv,
		ModeScore: modeScore,
		Overall:   overall,
	}, nil
}

// clamp01 把 v 夹到 [0,1]，仅用于消除浮点误差，不改变指标。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Compare 用本轮有效数字对一组基准打分。
//
// 前置校验（在返回任何结果前）：
//   - 数字必须在 1..355；
//   - 基准集合非空、ID 非空且互不相同、每个基准 raw_numbers/counts/stats 自洽
//     （负计数、全零、越界、众数不符均拒绝），避免损坏数据给出虚假分数；
//   - chosenID 若不为空，必须命中候选集合。
//
// 空样本返回不可评分结果（Reason=empty_distribution），但仍保留 chosenID 且 matches
// 为空数组（而非 null）。validCount<MinScorable 返回 insufficient_samples，不给排名。
// 其余情况对每个基准调用 Score，按 overall 降序、并列按 BaselineID 升序稳定排序，
// 填 Rank 与 GapToPrevious（单位同 overall）。
func Compare(numbers []int, baselines []Baseline, chosenID string) (Comparison, error) {
	cmp := Comparison{
		ValidCount:       len(numbers),
		ChosenBaselineID: chosenID,
		Matches:          []ScoreResult{},
	}

	for _, n := range numbers {
		if n < 1 || n > DistLength {
			return Comparison{}, fmt.Errorf("数字越界: %d", n)
		}
	}

	if len(baselines) == 0 {
		return Comparison{}, fmt.Errorf("没有可对照的基准")
	}
	seen := make(map[string]bool, len(baselines))
	for _, b := range baselines {
		if b.ID == "" {
			return Comparison{}, fmt.Errorf("基准 ID 为空")
		}
		if seen[b.ID] {
			return Comparison{}, fmt.Errorf("基准 ID 重复: %s", b.ID)
		}
		seen[b.ID] = true
		if err := validateBaseline(b); err != nil {
			return Comparison{}, err
		}
	}
	if chosenID != "" && !seen[chosenID] {
		return Comparison{}, fmt.Errorf("所选基准 %s 不在候选集合中", chosenID)
	}

	if len(numbers) == 0 {
		cmp.Reason = "empty_distribution"
		return cmp, nil
	}

	dist, ok := Distribution(numbers)
	if !ok {
		return Comparison{}, fmt.Errorf("无法归一化分布")
	}
	stats := StatsOf(numbers)
	cmp.Distribution = dist
	cmp.Stats = stats

	if len(numbers) < MinScorable {
		cmp.Reason = "insufficient_samples"
		return cmp, nil
	}

	scored := make([]ScoreResult, 0, len(baselines))
	for _, b := range baselines {
		baseDist, ok := distributionFromCounts(b.Counts)
		if !ok {
			return Comparison{}, fmt.Errorf("基准 %s 分布为空", b.ID)
		}
		sr, err := Score(dist, baseDist, stats.Mode, b.Stats.Mode)
		if err != nil {
			return Comparison{}, fmt.Errorf("基准 %s 评分失败: %w", b.ID, err)
		}
		sr.BaselineID = b.ID
		scored = append(scored, sr)
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Overall != scored[j].Overall {
			return scored[i].Overall > scored[j].Overall
		}
		return scored[i].BaselineID < scored[j].BaselineID
	})
	for i := range scored {
		scored[i].Rank = i + 1
		if i > 0 {
			gap := scored[i-1].Overall - scored[i].Overall
			scored[i].GapToPrevious = &gap
		}
	}
	cmp.Scorable = true
	cmp.Matches = scored
	return cmp, nil
}

// validateBaseline 校验一份基准数据的内部自洽性，防止损坏/伪造数据得到虚假分数。
//
// 拒绝：越界/负计数、全零计数（零统计）、counts 总和与 raw_numbers 数量不符、
// raw_numbers 直方图与 counts 不符、stats 众数与 counts 不符、超长（>DefaultMaxNumbers）。
// 逐桶上限检查同时防止累加溢出。
func validateBaseline(b Baseline) error {
	if len(b.RawNumbers) > DefaultMaxNumbers {
		return fmt.Errorf("基准 %s raw_numbers %d 超过上限 %d", b.ID, len(b.RawNumbers), DefaultMaxNumbers)
	}
	var total int
	for i, c := range b.Counts {
		if c < 0 {
			return fmt.Errorf("基准 %s counts[%d]=%d 为负", b.ID, i, c)
		}
		if c > DefaultMaxNumbers {
			return fmt.Errorf("基准 %s counts[%d]=%d 超过上限", b.ID, i, c)
		}
		total += c
	}
	if total == 0 {
		return fmt.Errorf("基准 %s counts 全为 0（无有效样本）", b.ID)
	}
	if len(b.RawNumbers) > 0 {
		if len(b.RawNumbers) != total {
			return fmt.Errorf("基准 %s raw_numbers %d 与 counts 总和 %d 不一致", b.ID, len(b.RawNumbers), total)
		}
		var recomputed [DistLength]int
		for _, n := range b.RawNumbers {
			if n < 1 || n > DistLength {
				return fmt.Errorf("基准 %s raw_numbers 含越界值 %d", b.ID, n)
			}
			recomputed[n-1]++
		}
		if recomputed != b.Counts {
			return fmt.Errorf("基准 %s raw_numbers 直方图与 counts 不一致", b.ID)
		}
	}
	wantMode, wantCount := modeOfCounts(b.Counts)
	if b.Stats.Mode != wantMode || b.Stats.ModeCount != wantCount {
		return fmt.Errorf("基准 %s stats 众数 %d(%d) 与 counts 众数 %d(%d) 不一致",
			b.ID, b.Stats.Mode, b.Stats.ModeCount, wantMode, wantCount)
	}
	return nil
}

// modeOfCounts 从 355 桶计数取众数；并列取最小整数（与 StatsOf 相同的 tie-break）。
func modeOfCounts(counts [DistLength]int) (mode, count int) {
	for i, c := range counts {
		if c > count {
			mode, count = i+1, c
		}
	}
	return mode, count
}

// distributionFromCounts 把基准的 355 桶计数归一化成分布。
func distributionFromCounts(counts [DistLength]int) ([DistLength]float64, bool) {
	total := 0
	for _, c := range counts {
		if c < 0 {
			return [DistLength]float64{}, false
		}
		total += c
	}
	if total == 0 {
		return [DistLength]float64{}, false
	}
	var dist [DistLength]float64
	tf := float64(total)
	for i := 0; i < DistLength; i++ {
		dist[i] = float64(counts[i]) / tf
	}
	return dist, true
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
