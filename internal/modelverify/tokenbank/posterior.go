package tokenbank

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Model 是 TOKENIZER_MODEL 的逐项照抄（lm-detector shared/tokenizer-posterior.ts）。
//
// 三类假设上的开放集贝叶斯判定：
//   - 已收录类 c：上游用 c 的分词器，个别探测文本只因测量噪声偏离；
//   - c 的近亲：库外、与 c 接近、在相当比例的文本上计数不同；
//   - 外来：与所有类都无关的库外分词器，每条差分服从全部类的平滑分布。
//
// 残差服从带下限的双分量离散拉普拉斯混合；污染率在网格上边缘化，
// 隐藏开销在基线附近 ±OffsetWindow 窗口内边缘化。
var Model = struct {
	PriorKnown, PriorRelative, PriorAlien float64
	KnownRates, RelativeRates             [][2]float64
	Narrow, Wide, Floor                   float64
	OffsetWindow                          int
	AlienSpread                           float64
	MinimumProbes, MaximumProbes          int
	StopAt, MinimumGain                   float64
	Planned                               int
}{
	PriorKnown: 0.8, PriorRelative: 0.1, PriorAlien: 0.1,
	KnownRates:    [][2]float64{{0.02, 0.4}, {0.06, 0.4}, {0.15, 0.2}},
	RelativeRates: [][2]float64{{0.2, 0.3}, {0.4, 0.4}, {0.7, 0.3}},
	Narrow:        0.02,
	Wide:          0.85,
	Floor:         1e-6,
	OffsetWindow:  3,
	AlienSpread:   0.15,
	MinimumProbes: 4,
	MaximumProbes: 20,
	StopAt:        0.99,
	MinimumGain:   1e-4,
	Planned:       24,
}

// baselineRate 基线不含探测文本，残差只衡量开销噪声：所有假设下用同一污染率，
// 否则漂移的开销会被算成"库外分词器"的证据。
var baselineRate = Model.KnownRates[0][0]

// Observation 一次已答复的请求。Probe 为空串表示基线（只发前后缀）。
type Observation struct {
	Probe         string `json:"probe"`
	Baseline      bool   `json:"baseline"`
	Tokens        int    `json:"tokens"`
	ResponseModel string `json:"response_model,omitempty"`
}

// ClassScore 单个分词器类的后验。
type ClassScore struct {
	ID string `json:"id"`
	// Exact 上游恰好用这个类的分词器的后验。
	Exact float64 `json:"exact"`
	// Related 上游用这个类的库外近亲的后验。
	Related  float64 `json:"related"`
	Matched  int     `json:"matched"`
	Compared int     `json:"compared"`
	Overhead int     `json:"overhead"`
}

// LabScore 按厂商汇总的后验。
type LabScore struct {
	Lab     string  `json:"lab"`
	LabName string  `json:"lab_name"`
	Total   float64 `json:"total"`
}

// Posterior 当前全部观测下的后验。
type Posterior struct {
	Classes []ClassScore `json:"classes"`
	Labs    []LabScore   `json:"labs"`
	// Unknown 全部近亲与外来假设之和。
	Unknown  float64 `json:"unknown"`
	Alien    float64 `json:"alien"`
	Answered int     `json:"answered"`
}

type prepared struct {
	index      map[string]int
	values     [][][]int // values[c][j]：类 c 在探测 j 上允许的取值
	alienScale []float64
	plans      []*plan
}

func (b *Bank) prep() *prepared {
	b.once.Do(func() {
		p := &prepared{index: map[string]int{}}
		for j, probe := range b.Probes {
			p.index[probe.ID] = j
		}
		p.values = make([][][]int, len(b.Classes))
		for c, cls := range b.Classes {
			p.values[c] = make([][]int, len(b.Probes))
			for j, v := range cls.Counts {
				if alt, ok := cls.Alternatives[strconv.Itoa(j)]; ok {
					p.values[c][j] = alt
				} else {
					p.values[c][j] = []int{v}
				}
			}
		}
		p.alienScale = make([]float64, len(b.Probes))
		for j := range b.Probes {
			sorted := make([]int, len(b.Classes))
			for c, cls := range b.Classes {
				sorted[c] = cls.Counts[j]
			}
			sort.Ints(sorted)
			median := float64(sorted[len(sorted)/2])
			p.alienScale[j] = math.Exp(-1 / math.Max(1.5, Model.AlienSpread*median))
		}
		b.prepared = p
	})
	return b.prepared
}

func logSumExp(values []float64) float64 {
	top := math.Inf(-1)
	for _, v := range values {
		if v > top {
			top = v
		}
	}
	if math.IsInf(top, -1) {
		return top
	}
	sum := 0.0
	for _, v := range values {
		sum += math.Exp(v - top)
	}
	return top + math.Log(sum)
}

func logGeometric(distance, p float64) float64 {
	return math.Log((1-p)/(1+p)) + math.Abs(distance)*math.Log(p)
}

func logResidual(distance, rate float64) float64 {
	return logSumExp([]float64{
		math.Log(1-rate) + logGeometric(distance, Model.Narrow),
		math.Log(rate) + logGeometric(distance, Model.Wide),
		math.Log(Model.Floor),
	})
}

// nearest 返回 delta 与最近允许值的带符号残差；允许值为空时为 +Inf。
// 并列时保留先出现的值（与参考实现的 reduce 一致）。
func nearest(delta int, allowed []int) float64 {
	best := math.Inf(1)
	for _, v := range allowed {
		d := float64(delta - v)
		if math.Abs(d) < math.Abs(best) {
			best = d
		}
	}
	return best
}

type datum struct {
	j      int // -1 = 基线
	tokens int
}

func (b *Bank) data(obs []Observation) ([]datum, error) {
	p := b.prep()
	rows := make([]datum, 0, len(obs))
	for _, o := range obs {
		if o.Baseline {
			rows = append(rows, datum{j: -1, tokens: o.Tokens})
			continue
		}
		j, ok := p.index[o.Probe]
		if !ok {
			return nil, fmt.Errorf("探测文本 %s 不在分词器库中", o.Probe)
		}
		rows = append(rows, datum{j: j, tokens: o.Tokens})
	}
	return rows, nil
}

func offsets(baseline int) []int {
	out := make([]int, 0, 2*Model.OffsetWindow+1)
	for i := 0; i < 2*Model.OffsetWindow+1; i++ {
		out = append(out, baseline-Model.OffsetWindow+i)
	}
	return out
}

var baselineAllowed = []int{0}

func (p *prepared) allowed(c, j int) []int {
	if j == -1 {
		return baselineAllowed
	}
	return p.values[c][j]
}

type evidence struct {
	log    float64
	offset int
}

// classEvidence 类 c 在一个污染率网格下的对数边缘似然，并给出用于展示的最优开销。
func classEvidence(p *prepared, rows []datum, c int, grid [][2]float64, baseline int) evidence {
	terms := make([]float64, 0, (2*Model.OffsetWindow+1)*len(grid))
	best := evidence{log: math.Inf(-1), offset: baseline}
	prior := -math.Log(float64(2*Model.OffsetWindow + 1))
	residuals := make([]float64, len(rows))
	for _, offset := range offsets(baseline) {
		for i, row := range rows {
			residuals[i] = nearest(row.tokens-offset, p.allowed(c, row.j))
		}
		for _, rw := range grid {
			rate, weight := rw[0], rw[1]
			log := 0.0
			for i, r := range residuals {
				rr := rate
				if rows[i].j == -1 {
					rr = baselineRate
				}
				log += logResidual(r, rr)
			}
			terms = append(terms, prior+math.Log(weight)+log)
			if log > best.log {
				best = evidence{log: log, offset: offset}
			}
		}
	}
	return evidence{log: logSumExp(terms), offset: best.offset}
}

func alienEvidence(p *prepared, rows []datum, size, baseline int) float64 {
	prior := -math.Log(float64(2*Model.OffsetWindow + 1))
	terms := make([]float64, 0, 2*Model.OffsetWindow+1)
	for _, offset := range offsets(baseline) {
		// 求和顺序与参考实现一致：先对各行求和，再加先验。
		sum := 0.0
		for _, row := range rows {
			if row.j == -1 {
				sum += logResidual(float64(row.tokens-offset), baselineRate)
				continue
			}
			density := 0.0
			for c := range p.values {
				density += math.Exp(logGeometric(nearest(row.tokens-offset, p.values[c][row.j]), p.alienScale[row.j]))
			}
			density /= float64(size)
			sum += math.Log(density + Model.Floor)
		}
		terms = append(terms, prior+sum)
	}
	return logSumExp(terms)
}

// ComputePosterior 对应 tokenizerPosterior。首条基线未答复前返回 nil（它锚定开销）。
func (b *Bank) ComputePosterior(obs []Observation) (*Posterior, error) {
	rows, err := b.data(obs)
	if err != nil {
		return nil, err
	}
	first := -1
	for i, r := range rows {
		if r.j == -1 {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, nil
	}
	state := b.prep()
	size := len(b.Classes)
	baseline := rows[first].tokens
	known := make([]evidence, size)
	relative := make([]evidence, size)
	logKnown := make([]float64, size)
	logRelative := make([]float64, size)
	all := make([]float64, 0, 2*size+1)
	for c := 0; c < size; c++ {
		known[c] = classEvidence(state, rows, c, Model.KnownRates, baseline)
		relative[c] = classEvidence(state, rows, c, Model.RelativeRates, baseline)
		logKnown[c] = math.Log(Model.PriorKnown/float64(size)) + known[c].log
		logRelative[c] = math.Log(Model.PriorRelative/float64(size)) + relative[c].log
	}
	logAlien := math.Log(Model.PriorAlien) + alienEvidence(state, rows, size, baseline)
	all = append(all, logKnown...)
	all = append(all, logRelative...)
	all = append(all, logAlien)
	total := logSumExp(all)

	answered := 0
	for _, r := range rows {
		if r.j != -1 {
			answered++
		}
	}
	classes := make([]ClassScore, size)
	for c, cls := range b.Classes {
		matched := 0
		for _, r := range rows {
			if r.j == -1 {
				continue
			}
			if nearest(r.tokens-known[c].offset, state.values[c][r.j]) == 0 {
				matched++
			}
		}
		classes[c] = ClassScore{
			ID:       cls.ID,
			Exact:    math.Exp(logKnown[c] - total),
			Related:  math.Exp(logRelative[c] - total),
			Matched:  matched,
			Compared: answered,
			Overhead: known[c].offset,
		}
	}
	// 与参考实现的比较式逐字一致（b.exact + b.related - a.exact - a.related），
	// 避免近似并列时因浮点求和顺序不同而换位。
	sort.SliceStable(classes, func(a, b int) bool {
		return classes[b].Exact+classes[b].Related-classes[a].Exact-classes[a].Related < 0
	})

	labIndex := map[string]int{}
	labs := []LabScore{}
	for c, cls := range b.Classes {
		i, ok := labIndex[cls.Lab]
		if !ok {
			i = len(labs)
			labIndex[cls.Lab] = i
			labs = append(labs, LabScore{Lab: cls.Lab, LabName: cls.LabName})
		}
		labs[i].Total += math.Exp(logKnown[c]-total) + math.Exp(logRelative[c]-total)
	}
	sort.SliceStable(labs, func(a, b int) bool { return labs[a].Total > labs[b].Total })

	alien := math.Exp(logAlien - total)
	related := 0.0
	for _, s := range classes {
		related += s.Related
	}
	unknown := alien + related
	return &Posterior{Classes: classes, Labs: labs, Unknown: unknown, Alien: alien, Answered: answered}, nil
}

// Done 对应 probingDone：一个类或"库外"后验达到 StopAt（且至少答复 MinimumProbes 条），
// 或探测预算用完。
func Done(p *Posterior, maximum int) bool {
	if maximum <= 0 {
		maximum = Model.MaximumProbes
	}
	if p.Answered >= maximum {
		return true
	}
	if p.Answered < Model.MinimumProbes {
		return false
	}
	return math.Max(p.Classes[0].Exact, p.Unknown) >= Model.StopAt
}
