package tokenbank

import (
	"regexp"
	"strings"
)

// 判定种类。
const (
	KindExact   = "exact"
	KindRelated = "related"
	KindUnknown = "unknown"
)

// 声明核对状态。
const (
	ClaimConsistent   = "consistent"
	ClaimInconsistent = "inconsistent"
	ClaimUncertain    = "uncertain"
)

// 声明核对阈值（参考实现 tokenizerVerdict：≥0.9 一致，≤0.1 不一致，其余不确定）。
const (
	claimConsistentAt   = 0.9
	claimInconsistentAt = 0.1
)

// ClaimCheck 观测到的分词器与声明型号是否相容。
type ClaimCheck struct {
	// Expected 声明型号对应的分词器类；厂商未公开分词器时为空。
	Expected []string `json:"expected"`
	Vendor   string   `json:"vendor"`
	// FirstParty 声明来自厂商 API 型号（厂商公开了确切分词器），此时库外近亲不算支持。
	// 开源谱系的别名声明则也接受近亲（如重训变体）。
	FirstParty  bool    `json:"first_party"`
	Probability float64 `json:"probability"`
	Status      string  `json:"status"`
}

// Verdict 分词器判定。
type Verdict struct {
	Kind       string      `json:"kind"`
	Top        ClassScore  `json:"top"`
	Confidence float64     `json:"confidence"`
	Claim      *ClaimCheck `json:"claim"`
}

var (
	providerPrefix = regexp.MustCompile(`^.*/`)
	tagSuffix      = regexp.MustCompile(`:.*$`)
)

// NormalizeModel 去掉厂商前缀（最后一个 / 之前）与 :tag 后缀，转小写。
func NormalizeModel(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	id = providerPrefix.ReplaceAllString(id, "")
	return tagSuffix.ReplaceAllString(id, "")
}

// ExpectedClasses 把型号映射到它应使用的分词器类：先查厂商映射，再查各类别名。
// 都不命中返回 nil（无法核对）。
func (b *Bank) ExpectedClasses(model string) *ClaimCheck {
	id := NormalizeModel(model)
	if id == "" {
		return nil
	}
	for _, m := range b.APIModels {
		if b.match(m.Pattern, id) {
			out := &ClaimCheck{Expected: []string{}, Vendor: m.Vendor, FirstParty: true}
			if m.Class != nil {
				out.Expected = []string{*m.Class}
			}
			return out
		}
	}
	var expected []string
	vendor := ""
	for _, c := range b.Classes {
		for _, a := range c.Aliases {
			if b.match(a, id) {
				if vendor == "" {
					vendor = c.LabName
				}
				expected = append(expected, c.ID)
				break
			}
		}
	}
	if len(expected) == 0 {
		return nil
	}
	return &ClaimCheck{Expected: expected, Vendor: vendor, FirstParty: false}
}

// ClassPosterior 上游使用 classes 之一的后验；relatives 为真时也计入它们的库外近亲。
func ClassPosterior(p *Posterior, classes []string, relatives bool) float64 {
	want := map[string]bool{}
	for _, c := range classes {
		want[c] = true
	}
	sum := 0.0
	for _, s := range p.Classes {
		if !want[s.ID] {
			continue
		}
		sum += s.Exact
		if relatives {
			sum += s.Related
		}
	}
	return sum
}

// Judge 对应 tokenizerVerdict。
func (b *Bank) Judge(p *Posterior, model string) Verdict {
	byExact, byRelated := p.Classes[0], p.Classes[0]
	for _, s := range p.Classes {
		if s.Exact > byExact.Exact {
			byExact = s
		}
		if s.Related > byRelated.Related {
			byRelated = s
		}
	}
	var v Verdict
	switch {
	case byExact.Exact >= p.Unknown:
		v = Verdict{Kind: KindExact, Top: byExact, Confidence: byExact.Exact}
	case byRelated.Related >= p.Alien:
		v = Verdict{Kind: KindRelated, Top: byRelated, Confidence: byRelated.Related}
	default:
		v = Verdict{Kind: KindUnknown, Top: p.Classes[0], Confidence: p.Unknown}
	}
	if model != "" {
		if exp := b.ExpectedClasses(model); exp != nil {
			prob := p.Unknown
			if len(exp.Expected) > 0 {
				prob = ClassPosterior(p, exp.Expected, !exp.FirstParty)
			}
			exp.Probability = prob
			switch {
			case prob >= claimConsistentAt:
				exp.Status = ClaimConsistent
			case prob <= claimInconsistentAt:
				exp.Status = ClaimInconsistent
			default:
				exp.Status = ClaimUncertain
			}
			v.Claim = exp
		}
	}
	return v
}
