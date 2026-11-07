package numberdist

import (
	"encoding/json"
	"math"
	"os"
	"path"
	"strings"
	"testing"
)

// mustBuiltins 按「文件名 stem」建索引。golden 与 index.json 用的是文件名
// （如 deepseek-v4.1-flash），而个别文件内部 name 字段不同（deepseek-v4.1-flash.json
// 的 name 是 "deepseek-flash"），故以 Source.Path 的 stem 为准，同时保留 Label 别名。
func mustBuiltins(t *testing.T) map[string]Baseline {
	t.Helper()
	bs, err := LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	m := make(map[string]Baseline, len(bs)*2)
	for _, b := range bs {
		stem := strings.TrimSuffix(path.Base(b.Source.Path), ".json")
		if stem != "" && stem != "." {
			m[stem] = b
		}
		m[b.Label] = b
	}
	return m
}

type pairwiseFile struct {
	Scope  string `json:"scope"`
	Source string `json:"source"`
	Pairs  []struct {
		A        string  `json:"a"`
		B        string  `json:"b"`
		ScorePct float64 `json:"score_pct"`
		SameMode bool    `json:"same_mode"`
	} `json:"pairs"`
}

// TestScore_PairwiseGoldens 用独立提供的 hlwy 基对黄金值验证公式实现，
// 容差 = 显示精度（黄金值 2 位小数，故允许 0.005），不拿上游输出反推自身。
func TestScore_PairwiseGoldens(t *testing.T) {
	raw, err := os.ReadFile("testdata/hlwy-pairwise.json")
	if err != nil {
		t.Fatalf("read goldens: %v", err)
	}
	var pf pairwiseFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatalf("parse goldens: %v", err)
	}
	if len(pf.Pairs) != 21 {
		t.Fatalf("黄金对数量 = %d, want 21", len(pf.Pairs))
	}
	builtins := mustBuiltins(t)

	for _, p := range pf.Pairs {
		ba, okA := builtins[p.A]
		if !okA {
			t.Fatalf("缺少基准 %q", p.A)
		}
		bb, okB := builtins[p.B]
		if !okB {
			t.Fatalf("缺少基准 %q", p.B)
		}
		da, ok := distributionFromCounts(ba.Counts)
		if !ok {
			t.Fatalf("基准 %q 分布为空", p.A)
		}
		db, ok := distributionFromCounts(bb.Counts)
		if !ok {
			t.Fatalf("基准 %q 分布为空", p.B)
		}
		got, err := Score(da, db, ba.Stats.Mode, bb.Stats.Mode)
		if err != nil {
			t.Fatalf("Score(%s,%s): %v", p.A, p.B, err)
		}
		if math.Abs(got.Overall*100-p.ScorePct) > 0.005+1e-9 {
			t.Errorf("Score(%s,%s) = %.4f%%, want %.2f", p.A, p.B, got.Overall*100, p.ScorePct)
		}
		if same := ba.Stats.Mode == bb.Stats.Mode; same != p.SameMode {
			t.Errorf("(%s,%s) same_mode = %v, want %v", p.A, p.B, same, p.SameMode)
		}
	}
}

// 黄金值里两条被点名展示的分数（92.14 / 91.57）单独固定。
func TestScore_KnownDisplayValues(t *testing.T) {
	builtins := mustBuiltins(t)
	check := func(a, b string, want float64) {
		t.Helper()
		da, ok := distributionFromCounts(builtins[a].Counts)
		if !ok {
			t.Fatalf("基准 %q 分布为空", a)
		}
		db, ok := distributionFromCounts(builtins[b].Counts)
		if !ok {
			t.Fatalf("基准 %q 分布为空", b)
		}
		got, err := Score(da, db, builtins[a].Stats.Mode, builtins[b].Stats.Mode)
		if err != nil {
			t.Fatalf("Score(%s,%s): %v", a, b, err)
		}
		if math.Abs(got.Overall*100-want) > 0.005+1e-9 {
			t.Fatalf("%s vs %s = %.4f%%, want %.2f", a, b, got.Overall*100, want)
		}
	}
	check("claude-opus-5", "deepseek-v4.1-flash", 92.14)
	check("deepseek-v4.1-flash-1017", "deepseek-v4.1-flash", 91.57)
}

// 同一分布对同一分布：余弦 1、JSD 0、同众数模式分 1 → overall 1。
func TestScore_Identity(t *testing.T) {
	var d [DistLength]float64
	d[216] = 1 // 数字 217
	got, err := Score(d, d, 217, 217)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if math.Abs(got.Cosine-1) > 1e-12 || math.Abs(got.JSD) > 1e-12 ||
		math.Abs(got.ModeScore-1) > 1e-12 || math.Abs(got.Overall-1) > 1e-12 {
		t.Fatalf("identity score = %+v, want all 1/0", got)
	}
}

func TestScore_RejectsEmptyAndBad(t *testing.T) {
	var zero [DistLength]float64
	if _, err := Score(zero, zero, 217, 217); err == nil {
		t.Fatal("空分布应报错")
	}
	var neg [DistLength]float64
	neg[0] = -1
	if _, err := Score(neg, zero, 217, 217); err == nil {
		t.Fatal("负值分布应报错")
	}
	var nan [DistLength]float64
	nan[0] = math.NaN()
	if _, err := Score(nan, zero, 217, 217); err == nil {
		t.Fatal("NaN 分布应报错")
	}
	var one [DistLength]float64
	one[216] = 1
	if _, err := Score(one, one, 0, 217); err == nil {
		t.Fatal("非法众数 0 应报错")
	}
	if _, err := Score(one, one, 217, 356); err == nil {
		t.Fatal("非法众数越界应报错")
	}
	// 未归一化（Σ≠1）的原始计数不能静默当概率：应拒绝。
	var counts [DistLength]float64
	counts[216] = 100
	counts[8] = 50
	if _, err := Score(counts, one, 217, 217); err == nil {
		t.Fatal("未归一化分布应报错")
	}
	// 归一化后同一数据应可评分。
	var norm [DistLength]float64
	norm[216] = 100.0 / 150
	norm[8] = 50.0 / 150
	if _, err := Score(norm, one, 217, 217); err != nil {
		t.Fatalf("归一化分布应可评分: %v", err)
	}
}

// 众数并列取最小整数、上中位数。
func TestStatsOf(t *testing.T) {
	// 众数 5 与 9 各 2 次 → 取 5；长度 5 → 上中位数 sorted[2]。
	nums := []int{9, 5, 9, 5, 3}
	s := StatsOf(nums)
	if s.Mode != 5 || s.ModeCount != 2 {
		t.Fatalf("Mode = %d (count %d), want 5 (2)", s.Mode, s.ModeCount)
	}
	if s.Median != 5 {
		t.Fatalf("Median = %d, want 5 (upper median of {3,5,5,9,9})", s.Median)
	}
	if s.Min != 3 || s.Max != 9 || s.Unique != 3 {
		t.Fatalf("stats = %+v", s)
	}
	if StatsOf(nil) != (Stats{}) {
		t.Fatalf("空输入应返回零值")
	}
}

func TestDistribution(t *testing.T) {
	d, ok := Distribution([]int{1, 1, 355})
	if !ok {
		t.Fatal("Distribution ok=false")
	}
	var sum float64
	for _, v := range d {
		sum += v
	}
	if math.Abs(d[0]-2.0/3) > 1e-12 || math.Abs(d[354]-1.0/3) > 1e-12 || math.Abs(sum-1) > 1e-12 {
		t.Fatalf("dist[0]=%v dist[354]=%v sum=%v", d[0], d[354], sum)
	}
	if _, ok := Distribution(nil); ok {
		t.Fatal("空输入应 ok=false")
	}
	if _, ok := Distribution([]int{0}); ok {
		t.Fatal("越界数字应 ok=false")
	}
	if _, ok := Distribution([]int{356}); ok {
		t.Fatal("越界数字应 ok=false")
	}
}
