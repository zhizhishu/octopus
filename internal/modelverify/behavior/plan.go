package behavior

import "github.com/zhizhishu/octopus/internal/modelverify/tokenbank"

// ProbePlan 是检测前给前端看的"这一项会发几次、能不能跑"——数字必须来自
// 真实实现，不许前端猜。
type ProbePlan struct {
	ID            ProbeID `json:"id"`
	Requests      int     `json:"requests"`
	Applicable    bool    `json:"applicable"`
	SkipReason    string  `json:"skip_reason,omitempty"`
	ReferenceOnly bool    `json:"reference_only,omitempty"`
	Advanced      bool    `json:"advanced,omitempty"`
}

// PlannedRequests 返回探针"打算发几次请求"。数字来自真实实现：
//
//	liveness/identity/echo_rewrite/context_canary/glitch = 1
//	token_delta = 2（基准 + 加长）
//	signature = 2（harvest + replay；不适用时 0）
//	fingerprint = 3（三道题）
//	tokenizer_id = MaximumProbes+2 的上界（自适应，约 12，最多 22）
//	tokenizer_baseline = 题库条数 + 2 次基线
//
// ExtraPlannedRequests 只给"超出默认整单超时预算"的那几项加秒数；这里给前端
// 展示的是完整计划量，所以分词器两项直接用同一套常量，其它项手写真实次数。
func PlannedRequests(id ProbeID) int {
	switch id {
	case ProbeLiveness, ProbeIdentity, ProbeEchoRewrite, ProbeContextCanary, ProbeGlitch:
		return 1
	case ProbeTokenDelta:
		return 2
	case ProbeSignature:
		return 2
	case ProbeFingerprint:
		return fingerprintChallenges
	case ProbeTokenizerID:
		return tokenbank.Model.MaximumProbes + 2
	case ProbeTokenizerBaseline:
		if b, err := tokenbank.Builtin(); err == nil {
			return len(b.Probes) + 2
		}
		return 74
	default:
		return 0
	}
}

// AllProbeIDs 按展示顺序返回全部探针（含默认关闭与高级项）。
func AllProbeIDs() []ProbeID {
	return []ProbeID{
		ProbeLiveness,
		ProbeEchoRewrite,
		ProbeContextCanary,
		ProbeTokenDelta,
		ProbeFingerprint,
		ProbeIdentity,
		ProbeTokenizerID,
		ProbeGlitch,
		ProbeSignature,
		ProbeTokenizerBaseline,
	}
}

// QuickProbeIDs 是"快速"方案：连通 + 改写 + 长内容。
func QuickProbeIDs() []ProbeID {
	return []ProbeID{ProbeLiveness, ProbeEchoRewrite, ProbeContextCanary}
}

// FullProbeIDs 是"全面"方案：标准默认集合（不含分词器两项——那是手动勾选）。
func FullProbeIDs() []ProbeID {
	return []ProbeID{
		ProbeLiveness,
		ProbeEchoRewrite,
		ProbeContextCanary,
		ProbeTokenDelta,
		ProbeFingerprint,
		ProbeIdentity,
		ProbeGlitch,
		ProbeSignature,
	}
}

// PlanFor 按渠道协议与型号，给出每项检查的适用性与请求数。
// protocol 用 ProbeSender.Protocol() 同口径（anthropic-messages / openai-chat / …）。
func PlanFor(protocol, model string) []ProbePlan {
	out := make([]ProbePlan, 0, len(AllProbeIDs()))
	for _, id := range AllProbeIDs() {
		p := ProbePlan{
			ID:            id,
			Requests:      PlannedRequests(id),
			Applicable:    true,
			ReferenceOnly: referenceOnlyProbe(id) || id == ProbeIdentity || id == ProbeGlitch,
			Advanced:      id == ProbeTokenizerBaseline,
		}
		switch id {
		case ProbeSignature:
			ok, reason := SignatureApplicable(protocol, model)
			p.Applicable = ok
			p.SkipReason = reason
			if !ok {
				p.Requests = 0
			}
		case ProbeGlitch:
			ok, reason := GlitchApplicable(model)
			p.Applicable = ok
			p.SkipReason = reason
			if !ok {
				p.Requests = 0
			}
		}
		out = append(out, p)
	}
	return out
}

// SumPlannedRequests 合计一组探针的计划请求数（不适用的按 0）。
func SumPlannedRequests(plan []ProbePlan, ids []ProbeID) int {
	want := make(map[ProbeID]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	total := 0
	for _, p := range plan {
		if _, ok := want[p.ID]; !ok {
			continue
		}
		if !p.Applicable {
			continue
		}
		total += p.Requests
	}
	return total
}
