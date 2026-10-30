package tokenbank

import (
	"math"
	"strconv"
	"sync"
)

// 选题：成对 Bhattacharyya 上界的贪心批（lm-detector nextProbes 的逐行移植）。
//
//	F(S) = Σ_{a<b, g(a)≠g(b)} sqrt(w̃a w̃b) · (1 − Π_{j∈S} BC_j(a,b))
//
// F 是 MAP 错误率的上界且单调次模，贪心批在最优批的 1−1/e 以内；计数精确时退化为
// EC²（等价类切边）。每个候选类贡献"恰为该类"与"该类库外近亲"两个原子，近亲与外来
// 同属"库外"组，所以选题也会拿领先者去对抗库外分词器。

// planRate 规划时代入各网格的中间污染率。
var planRate = map[string]float64{"known": 0.06, "relative": 0.4}

// planWindow 规划时在各类取值两侧额外考虑的结果数。
const planWindow = 24

type atom struct {
	group  string
	weight float64
	kind   string // known | relative | alien
	c      int
}

type plan struct {
	low, size int
	mu        sync.Mutex
	roots     map[string][]float64
}

var (
	densityMu     sync.Mutex
	densityTables = map[float64][]float64{}
)

// densityOf = exp(logResidual(d, rate))，|d| < 2048 查表，更远只剩下限。
func densityOf(distance int, rate float64) float64 {
	densityMu.Lock()
	table, ok := densityTables[rate]
	if !ok {
		table = make([]float64, 2048)
		for d := range table {
			table[d] = math.Exp(logResidual(float64(d), rate))
		}
		densityTables[rate] = table
	}
	densityMu.Unlock()
	if distance < 0 {
		distance = -distance
	}
	if distance > 2047 {
		distance = 2047
	}
	return table[distance]
}

var plansMu sync.Mutex

func (b *Bank) planFor(state *prepared, j int) *plan {
	plansMu.Lock()
	defer plansMu.Unlock()
	if state.plans == nil {
		state.plans = make([]*plan, len(b.Probes))
		for p := range b.Probes {
			low, high := math.MaxInt, math.MinInt
			for c := range state.values {
				v := state.values[c][p][0]
				if v < low {
					low = v
				}
				if v > high {
					high = v
				}
			}
			low -= planWindow
			state.plans[p] = &plan{low: low, size: high + planWindow - low + 1, roots: map[string][]float64{}}
		}
	}
	return state.plans[j]
}

// rootsOf 原子在探测 j 结果窗口上归一化预测分布的平方根；两原子的 BC 系数即根向量点积。
func (b *Bank) rootsOf(state *prepared, a atom, j int) []float64 {
	pl := b.planFor(state, j)
	key := a.kind + ":" + strconv.Itoa(a.c)
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if roots, ok := pl.roots[key]; ok {
		return roots
	}
	raw := make([]float64, pl.size)
	norm := 0.0
	for i := range raw {
		y := pl.low + i
		if a.kind != "alien" {
			raw[i] = densityOf(y-state.values[a.c][j][0], planRate[a.kind])
		} else {
			sum := 0.0
			for c := range state.values {
				sum += math.Exp(logGeometric(float64(y-state.values[c][j][0]), state.alienScale[j]))
			}
			raw[i] = sum
		}
	}
	for _, v := range raw {
		norm += v
	}
	roots := make([]float64, pl.size)
	for i, v := range raw {
		roots[i] = math.Sqrt(v / norm)
	}
	pl.roots[key] = roots
	return roots
}

func dot(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// NextProbes 对应 nextProbes：规划下一批 count 条探测文本。posterior 为 nil 表示
// 尚无答复（全部类等权参与）。没有候选能明显降低上界时返回空。
func (b *Bank) NextProbes(posterior *Posterior, asked map[string]bool, count int) []string {
	state := b.prep()
	position := map[string]int{}
	for c, cls := range b.Classes {
		position[cls.ID] = c
	}
	var scores []ClassScore
	if posterior != nil {
		scores = posterior.Classes
		if len(scores) > Model.Planned {
			scores = scores[:Model.Planned]
		}
	} else {
		scores = make([]ClassScore, len(b.Classes))
		for c, cls := range b.Classes {
			scores[c] = ClassScore{ID: cls.ID, Exact: 1}
		}
	}
	atoms := make([]atom, 0, 2*len(scores)+1)
	for _, s := range scores {
		c := position[s.ID]
		atoms = append(atoms, atom{group: s.ID, weight: s.Exact, kind: "known", c: c})
		if s.Related > 0 {
			atoms = append(atoms, atom{group: "unknown", weight: s.Related, kind: "relative", c: c})
		}
	}
	if posterior != nil && posterior.Alien > 0 {
		atoms = append(atoms, atom{group: "unknown", weight: posterior.Alien, kind: "alien", c: -1})
	}
	total := 0.0
	for _, a := range atoms {
		total += a.weight
	}
	if total == 0 {
		total = 1
	}
	type pair struct {
		a, b   int
		weight float64
	}
	pairs := []pair{}
	for a := 0; a < len(atoms); a++ {
		for bb := a + 1; bb < len(atoms); bb++ {
			if atoms[a].group != atoms[bb].group {
				pairs = append(pairs, pair{a: a, b: bb, weight: math.Sqrt(atoms[a].weight*atoms[bb].weight) / total})
			}
		}
	}
	type candidate struct {
		id string
		bc []float64
	}
	candidates := []candidate{}
	for j, probe := range b.Probes {
		if asked[probe.ID] {
			continue
		}
		roots := make([][]float64, len(atoms))
		for i, a := range atoms {
			roots[i] = b.rootsOf(state, a, j)
		}
		bc := make([]float64, len(pairs))
		for t, p := range pairs {
			bc[t] = dot(roots[p.a], roots[p.b])
		}
		candidates = append(candidates, candidate{id: probe.ID, bc: bc})
	}
	remaining := make([]float64, len(pairs))
	for t := range remaining {
		remaining[t] = 1
	}
	chosen := []string{}
	picked := map[string]bool{}
	for len(chosen) < count {
		bestIdx := -1
		bestGain := 0.0
		for i, cand := range candidates {
			if picked[cand.id] {
				continue
			}
			gain := 0.0
			for t, p := range pairs {
				gain += p.weight * remaining[t] * (1 - cand.bc[t])
			}
			if bestIdx < 0 || gain > bestGain {
				bestIdx, bestGain = i, gain
			}
		}
		if bestIdx < 0 || bestGain < Model.MinimumGain {
			break
		}
		best := candidates[bestIdx]
		chosen = append(chosen, best.id)
		picked[best.id] = true
		for t, v := range best.bc {
			remaining[t] *= v
		}
	}
	return chosen
}
