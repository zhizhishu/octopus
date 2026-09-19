package behavior

import "fmt"

// 评分层：把若干探针结果折算成一份可读的审计报告。
//
// 与上游（AI-Infra-Guard，Apache-2.0）的两处刻意偏离，都在注释里写明理由：
//
//  1. **只保留一套量纲**。上游同时存在"风险分（40/70 分档）"和"安全分（30/70 分档）"
//     两套不同源的分档，换算边界方向相反（风险分 70 = 安全分 30，一个判高危一个判中危）。
//     这里只输出风险分：0 分最好，100 分最差，越高越可疑。
//  2. **探针故障不产生风险项**。上游的流式探针把 HTTP 错误也记成"流完整性异常"，
//     于是网络抖动会被读成中转站作弊。这里把「探针跑不动」（Err）与「探针跑通了
//     但发现问题」（!OK）分成两个通道，只有后者进 findings。

// Severity 是风险项档位。
type Severity string

const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

// Verdict 是整体结论。
type Verdict string

const (
	// VerdictNone 表示所有探针都跑通、且没有发现问题。
	VerdictNone Verdict = "none"
	// VerdictLow/Medium/High 由风险分分档得到。
	VerdictLow    Verdict = "low"
	VerdictMedium Verdict = "medium"
	VerdictHigh   Verdict = "high"
	// VerdictUnknown 表示证据不足：没有任何风险项，但多数探针根本没跑通。
	// 它必须与 VerdictNone 分开——"查过没问题"和"没查成"是两回事，
	// 混为一谈会让一个挂掉的渠道看起来清白。
	VerdictUnknown Verdict = "unknown"
)

// 风险分档阈值。与上游 risk_verdict 一致（40 / 70）。
const (
	riskMediumThreshold = 40
	riskHighThreshold   = 70
)

// minUsableProbes 是判定"证据是否充分"所需的最少跑通探针数。
const minUsableProbes = 3

// Finding 是一条风险项。
type Finding struct {
	Probe          ProbeID
	Severity       Severity
	Score          int
	Title          string
	Evidence       map[string]any
	Recommendation string
}

// ProbeError 记录一个跑不动（或跑出异常）的探针。
//
// 它进入报告但不进风险分：探针失败是"我们没查成"，不是"对方有问题"。
type ProbeError struct {
	Probe ProbeID
	Error string
}

// Report 是一次渠道行为审计的结论。
type Report struct {
	// RequestedModel 是本次请求的模型名，用于家族比对。
	RequestedModel string
	// RequestedFamilies 是从模型名推断出的家族。
	RequestedFamilies []string
	// Score 是风险分：0 最好，100 最差。
	Score int
	// Verdict 是整体结论。
	Verdict Verdict
	// Findings 是风险项，按分值降序。
	Findings []Finding
	// Results 是全部探针的原始结果，供展示与复核。
	Results []Result
	// Errors 是未能完成判定的探针。
	Errors []ProbeError
}

// BuildReport 把探针结果折算为报告。
func BuildReport(results []Result, requestedModel string) Report {
	report := Report{
		RequestedModel:    requestedModel,
		RequestedFamilies: InferFamilies(requestedModel),
		Results:           results,
	}

	usable := 0
	for _, res := range results {
		if res.Err != nil {
			report.Errors = append(report.Errors, ProbeError{Probe: res.ProbeID, Error: res.Err.Error()})
			continue
		}
		usable++
		report.Findings = append(report.Findings, findingsFor(res, report.RequestedModel, report.RequestedFamilies)...)
	}

	total := 0
	for _, f := range report.Findings {
		total += f.Score
	}
	if total > 100 {
		total = 100
	}
	report.Score = total

	switch {
	case len(report.Findings) == 0 && usable < minUsableProbes:
		// 没有风险项，但跑通的探针太少——不能说"没问题"，只能说"没查清"。
		report.Verdict = VerdictUnknown
	case total >= riskHighThreshold:
		report.Verdict = VerdictHigh
	case total >= riskMediumThreshold:
		report.Verdict = VerdictMedium
	case total > 0:
		report.Verdict = VerdictLow
	default:
		report.Verdict = VerdictNone
	}
	return report
}

// signatureFindings 是签名验真探针的判定。
//
// 权重参照上游的 11 项检测，但只保留**我们能真正执行**的那些：上游的
// 「响应头指纹」（找 x-amz*）要读原始 HTTP 头，而本项目的出站链路把响应归一化
// 之后才交给我们，拿不到头；「随机数指纹」要额外 30 次采样，成本远超其余各项。
// 去掉这两项后，剩余判据按原来的相对权重配对到 0-100 的风险分。
//
// 与其它探针一样，判定分「跑不动」与「查出问题」两条通道：res.Err 非空时
// 返回 nil，由 BuildReport 记进 Errors，绝不当成替身证据。
func signatureFindings(res Result, requestedModel string) []Finding {
	// 探针自报不适用（非 Anthropic 出站），或压根没跑成。
	if v, ok := res.Data["applicable"].(bool); ok && !v {
		return nil
	}
	if res.Err != nil {
		return nil
	}

	// 拿不到签名是这一层里最重的信号，但成因不唯一：可能是模型不支持
	// extended thinking（正常），也可能是替身根本不产出签名（异常）。
	// 探针层区分不了，所以判高危并要求人工复核，而不是直接定罪。
	if v, ok := res.Data["signature_present"].(bool); ok && !v {
		return []Finding{{
			Probe: ProbeSignature, Severity: SeverityHigh, Score: 50,
			Title: "未能取得 thinking 签名",
			Evidence: map[string]any{
				"finish_reason":   stringOf(res.Data, "finish_reason"),
				"thinking_tokens": intOf(res.Data, "thinking_tokens"),
				"content_len":     intOf(res.Data, "content_len"),
			},
			Recommendation: "该模型可能不支持扩展思考，也可能是替身不产出签名；请换用明确支持思考的型号复测后再下结论。",
		}}
	}

	var findings []Finding

	// 结构判定：绑定模型名 + nonce 段数 + 密文熵。
	if v, ok := res.Data["structure_ok"].(bool); ok && !v {
		findings = append(findings, Finding{
			Probe: ProbeSignature, Severity: SeverityMedium, Score: 25,
			Title: fmt.Sprintf("签名结构异常（%s）", stringOf(res.Data, "structure_reason")),
			Evidence: map[string]any{
				"bound_model":      stringOf(res.Data, "bound_model"),
				"ciphertext_len":   intOf(res.Data, "ciphertext_len"),
				"entropy":          res.Data["ciphertext_entropy"],
				"nonce_segments":   intOf(res.Data, "nonce_segments"),
				"signature_len":    intOf(res.Data, "signature_len"),
				"structure_reason": stringOf(res.Data, "structure_reason"),
			},
			Recommendation: "签名不像 Anthropic 签发的 AEAD 密文；结合回放结果判断，必要时人工抽样复核。",
		})
	}

	// 回放解封：最硬的一条。签名被原样透传时，模型能理解「这段推理已经在我上文里」
	// 并把它复述出来；伪造的签名解封不出内容。
	replayApplicable, _ := res.Data["replay_applicable"].(bool)
	unsealed, _ := res.Data["unsealed"].(bool)
	if replayApplicable && !unsealed {
		findings = append(findings, Finding{
			Probe: ProbeSignature, Severity: SeverityHigh, Score: 50,
			Title: "签名回放无法解封",
			Evidence: map[string]any{
				"cot_found":          false,
				"replay_content_len": intOf(res.Data, "replay_content_len"),
				"bound_model":        stringOf(res.Data, "bound_model"),
			},
			Recommendation: "签名很可能不是上游原文透传。请确认该渠道是否为原生 Anthropic，并人工复核一次。",
		})
	}

	// 解封内容里的 AWS 系痕迹：不是「假 Claude」，而是「经 AWS 系转售」的强特征。
	if suspects := stringSliceOf(res.Data, "suspects"); len(suspects) > 0 {
		findings = append(findings, Finding{
			Probe: ProbeSignature, Severity: SeverityHigh, Score: 60,
			Title: "解封的推理内容含 AWS 系平台痕迹",
			Evidence: map[string]any{
				"suspects":    suspects,
				"bound_model": stringOf(res.Data, "bound_model"),
			},
			Recommendation: "该 Claude 很可能经 AWS 系基础设施（Bedrock / Amazon Q / Kiro）转售，与原生 Anthropic 存在能力与配额差异，请按业务要求评估。",
		})
	}

	// 绑定模型名与请求模型名不符。判低分：上游做别名解析（补日期后缀等）属正常，
	// 这条只作旁证，不足以单独定论。
	if bound := stringOf(res.Data, "bound_model"); bound != "" && requestedModel != "" {
		if !MatchSignatureModel(bound, requestedModel) {
			findings = append(findings, Finding{
				Probe: ProbeSignature, Severity: SeverityLow, Score: 15,
				Title: "签名绑定的模型名与请求模型不符",
				Evidence: map[string]any{
					"bound_model":     bound,
					"requested_model": requestedModel,
				},
				Recommendation: "可能是上游别名解析，也可能是签名被搬用；作为旁证，需结合其它探针判断。",
			})
		}
	}

	// thinking_tokens 为 0：请求了思考却没有思考产出。
	if res.Data["thinking_tokens"] != nil && intOf(res.Data, "thinking_tokens") == 0 {
		findings = append(findings, Finding{
			Probe: ProbeSignature, Severity: SeverityLow, Score: 10,
			Title:          "请求了扩展思考但思考 token 为 0",
			Evidence:       map[string]any{"thinking_tokens": 0},
			Recommendation: "上游可能丢弃了 thinking 参数，或该型号不支持思考；属轻微异常，建议观察。",
		})
	}

	return findings
}

// findingsFor 依据单个探针结果产出风险项。
//
// 每条判定都配一条中文动作建议：结论要能直接转成"下一步做什么"，
// 否则操作者拿到一个分数仍然不知道该怎么办。
func findingsFor(res Result, requestedModel string, requestedFamilies []string) []Finding {
	switch res.ProbeID {
	case ProbeSignature:
		return signatureFindings(res, requestedModel)
	case ProbeLiveness:
		if res.OK {
			return nil
		}
		// 被长度上限截断时降级：模型还没来得及回显就被切断，这证明不了什么。
		if boolOf(res.Data, "truncated") {
			return []Finding{{
				Probe: ProbeLiveness, Severity: SeverityLow, Score: 5,
				Title:          "存活探针未能定论（回答被截断）",
				Evidence:       res.Data,
				Recommendation: "提高该渠道的探针输出预算后复测，暂不判定。",
			}}
		}
		return []Finding{{
			Probe: ProbeLiveness, Severity: SeverityHigh, Score: 50,
			Title:          "渠道未按指令回显随机串",
			Evidence:       res.Data,
			Recommendation: "确认该渠道是否真的在转发请求；检查是否被替换为固定应答或中间层拦截。",
		}}

	case ProbeEchoRewrite:
		if res.OK {
			return nil
		}
		if boolOf(res.Data, "truncated") {
			return []Finding{{
				Probe: ProbeEchoRewrite, Severity: SeverityLow, Score: 5,
				Title:          "回显探针未能定论（回答被截断）",
				Evidence:       res.Data,
				Recommendation: "提高该渠道的探针输出预算后复测，暂不判定。",
			}}
		}
		return []Finding{{
			Probe: ProbeEchoRewrite, Severity: SeverityHigh, Score: 35,
			Title:          "逐字回显被改写",
			Evidence:       res.Data,
			Recommendation: "疑似注入了隐藏指令或改写了命令内容；不宜用于编码与运维类工作流。",
		}}

	case ProbeContextCanary:
		if res.OK {
			return nil
		}
		// 只有"没被截断却仍然丢了尾部哨兵"才是上下文被砍的证据。
		if boolOf(res.Data, "truncated") {
			return nil
		}
		return []Finding{{
			Probe: ProbeContextCanary, Severity: SeverityMedium, Score: 20,
			Title:          "长上下文尾部内容丢失",
			Evidence:       res.Data,
			Recommendation: "疑似上下文被截断；避免用于需要长上下文的任务，或下调可用上下文长度。",
		}}

	case ProbeIdentity:
		if !res.OK {
			return nil
		}
		// 双方家族都识别出来、且完全不交集，才记一条弱信号。
		//
		// 这里必须显式判空，不能只靠 FamiliesIntersect 的返回值：
		// 它返回 false 有两种含义——"双方都有值但无交集"（真信号）与
		// "任一方为空"（无从判断）。把后者当信号，模型答一句"我是个 AI 助手"
		// 就会被判成家族不符。
		identityFamilies := stringSliceOf(res.Data, "identity_families")
		if len(identityFamilies) == 0 || len(requestedFamilies) == 0 {
			return nil
		}
		if !FamiliesIntersect(requestedFamilies, identityFamilies) {
			return []Finding{{
				Probe: ProbeIdentity, Severity: SeverityLow, Score: 15,
				Title:          "模型自称的家族与请求模型不符",
				Evidence:       res.Data,
				Recommendation: "弱信号，需结合其他探针佐证；模型的口供可由中转站随意改写，不足以单独定论。",
			}}
		}
		return nil

	case ProbeGlitch:
		if !res.OK {
			return nil
		}
		candidates := candidateSliceOf(res.Data, "candidates")
		if len(candidates) == 0 {
			return nil
		}
		best := candidates[0]
		// 只有"错法符合某家族特征、且该家族与请求不符"才算信号。
		// consistent 为假意味着模型的错法不符合任何家族签名，更可能是题目没做对。
		if !best.Consistent || FamiliesIntersect(requestedFamilies, []string{best.Family}) {
			return nil
		}
		return []Finding{{
			Probe: ProbeGlitch, Severity: SeverityLow, Score: 15,
			Title:          "glitch 错法特征指向其它模型家族",
			Evidence:       res.Data,
			Recommendation: "版本相关的弱指纹，需结合统计指纹或其他探针佐证。",
		}}

	case ProbeTokenDelta:
		if !res.OK {
			return nil
		}
		delta := intOf(res.Data, "delta")
		if delta <= tokenDeltaThreshold {
			return nil
		}
		return []Finding{{
			Probe: ProbeTokenDelta, Severity: SeverityMedium, Score: 25,
			Title:          "上游自报的输入 token 明显虚高",
			Evidence:       res.Data,
			Recommendation: "疑似请求前被附加了额外内容（隐藏提示词/路由前缀），或使用了不同的分词器。",
		}}
	}
	return nil
}

// tokenDeltaThreshold 是 token 虚高的告警阈值。
//
// 期望值来自 len(prompt)/4 + 12 的粗估，本身不精确，所以阈值必须宽松：
// 只抓"异常偏高"。轻微偏差属于分词器差异与脚手架附加，是正常的。
const tokenDeltaThreshold = 200

// ---------------------------------------------------------------- 取值助手
//
// 探针结果走 map[string]any，取值必须防御式：缺字段、类型不符都要
// 安静地退化成零值，绝不允许在审计路径上 panic。

func stringOf(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	v, _ := data[key].(string)
	return v
}

func boolOf(data map[string]any, key string) bool {
	v, ok := data[key].(bool)
	return ok && v
}

func intOf(data map[string]any, key string) int {
	if data == nil {
		return 0
	}
	if v, ok := data[key].(int); ok {
		return v
	}
	return 0
}

func stringSliceOf(data map[string]any, key string) []string {
	v, ok := data[key].([]string)
	if !ok {
		return nil
	}
	return v
}

func candidateSliceOf(data map[string]any, key string) []GlitchCandidate {
	v, ok := data[key].([]GlitchCandidate)
	if !ok {
		return nil
	}
	return v
}
