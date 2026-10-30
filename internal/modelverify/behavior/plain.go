package behavior

import "fmt"

// 结论的人话层：把 Verdict / Score / Finding 翻译成不依赖术语的自然语言。
//
// 为什么要在后端做而不是前端拼字符串：同一条结论会在 CLI、日志详情、结果页、
// 通知里出现多次，散在前端就会各写一套、慢慢漂移。这里给一份稳定口径，
// 前端只负责排版。
//
// 措辞的一条硬规矩：**绝不说"这是真模型"或"这是假模型"**。
// 这套检测测的是"行为像不像官方"，不是密码学意义上的真假。分数高只能说"很像"，
// 分数低只能说"不太像"，两者都不足以断言真伪。任何暗示"已验真"的措辞
// 都会让用户拿它去做本该人工复核的决定，所以在这里就掐死。

// PlainVerdict 是给用户看的结论。
type PlainVerdict struct {
	// Level 机器可读档位，与 Verdict 同值，前端用来选颜色。
	Level string `json:"level"`

	// Headline 一句话结论，最多十几个字，直接当标题用。
	Headline string `json:"headline"`

	// Meaning 这个结论**意味着什么**，一到两句。
	Meaning string `json:"meaning"`

	// Caveat 这个结论**不能说明什么**。诚实交代检测的边界，比多给一分更重要。
	Caveat string `json:"caveat"`

	// NextStep 建议的下一步动作。
	NextStep string `json:"next_step"`

	// Confidence 把握档：高 / 中 / 低。证据少或跑挂多时降档。
	Confidence string `json:"confidence,omitempty"`

	// Summary 一句话摘要，给历史行与渠道卡片用。
	Summary string `json:"summary,omitempty"`
}

// PlainVerdictFor 把判定档位翻译成人话。
//
// family 是被测模型的家族名（openai/anthropic/google/xai/qwen/deepseek...），
// 只用于在 Caveat 里点明"这个家族本来就无法验真"这类关键差异，
// 不参与任何判定。
// scoredFindings 只计 Score>0 的项（0 分参考项不进"有疑点"文案）。
func PlainVerdictFor(v Verdict, score int, scoredFindings int, usableProbes int, family string) PlainVerdict {
	conf := confidenceFor(v, usableProbes, scoredFindings)
	switch v {
	case VerdictNone:
		return PlainVerdict{
			Level:      string(VerdictNone),
			Headline:   "没发现问题",
			Meaning:    fmt.Sprintf("%d 项检查都正常。", usableProbes),
			Caveat:     caveatFor(family, "none"),
			NextStep:   "可以正常用，隔段时间再抽查一次。这只说明没查出矛盾，不是原厂证明。",
			Confidence: conf,
			Summary:    fmt.Sprintf("%d 项检查都正常。", usableProbes),
		}

	case VerdictLow:
		return PlainVerdict{
			Level:      string(VerdictLow),
			Headline:   "有小疑点",
			Meaning:    fmt.Sprintf("%d 项检查发现 %d 处轻微异常。多数情况是上游做了限流、截断或型号别名解析，不一定有问题。", usableProbes, scoredFindings),
			Caveat:     caveatFor(family, "low"),
			NextStep:   "先照常用；如果同一渠道反复出现，再展开下面的明细逐条看。",
			Confidence: conf,
			Summary:    fmt.Sprintf("%d 处小疑点。", scoredFindings),
		}

	case VerdictMedium, VerdictHigh:
		return PlainVerdict{
			Level:      string(v),
			Headline:   "有可疑点，建议复核",
			Meaning:    fmt.Sprintf("%d 项检查发现 %d 处异常，累计可疑度 %d 分。", usableProbes, scoredFindings, score),
			Caveat:     caveatFor(family, string(v)),
			NextStep:   "找一条你信得过的渠道，测同一个模型对比一下，再决定要不要停用。",
			Confidence: conf,
			Summary:    fmt.Sprintf("%d 项对不上。", scoredFindings),
		}

	default: // VerdictUnknown
		return PlainVerdict{
			Level:      string(VerdictUnknown),
			Headline:   "数据不够，没法判断",
			Meaning:    fmt.Sprintf("跑通的检查太少（%d 项），给不出结论。", usableProbes),
			Caveat:     "这不是「没问题」，也不是「有问题」——就是没查到东西。常见原因是上游限流、超时，或该型号不支持某些检查项。",
			NextStep:   "换个时间重测，或减少检查项后重跑；也可以先看明细里哪些项标了「没查」。",
			Confidence: "低",
			Summary:    "跑通的检查太少，没法判断。",
		}
	}
}

func confidenceFor(v Verdict, usable, scored int) string {
	if v == VerdictUnknown {
		return "低"
	}
	if usable >= 5 && scored == 0 {
		return "高"
	}
	if usable >= 3 {
		return "中"
	}
	return "低"
}

// caveatFor 给出**按家族区分**的"这个结论不能说明什么"。
//
// 关键差异（这条不是客套，是这套检测的真实边界）：
//   - Anthropic 有可密码学验证的 thinking 签名，是目前唯一能拿到"硬证据"的家族；
//   - 其它家族（OpenAI/Gemini/Grok 以及全部国产模型）**没有任何可验证的签名机制**，
//     只能靠行为统计比对，本质上无法证明真伪。
//
// 所以对非 Anthropic 家族，"没查出问题"的含金量本来就低一档，必须写明。
func caveatFor(family, level string) string {
	switch family {
	case "anthropic":
		if level == "none" {
			return "Anthropic 有可验证的思考签名，是目前证据最硬的一项；但签名通过只说明「是 Anthropic 签发的」，不排除上游是正规转售（如云厂商渠道）。"
		}
		return "Anthropic 的签名验证是硬证据，其余检查仍是行为比对；两者要分开看。"
	case "openai", "google", "xai", "meta", "mistral", "mimo":
		return "这个家族没有可验证的签名机制，只能比「行为像不像」。所以本结论无法证明真假：很像也可能是高仿，不太像也可能只是上游做了改写。"
	case "qwen", "deepseek", "zhipu", "moonshot", "bytedance", "baidu", "minimax":
		return "国产模型普遍没有可验证的签名机制，且同家族不同版本的输出风格差异本来就大，只能比「大致像不像」。结论仅供参考，不能当作真假依据。"
	default:
		return "本检测比较的是「行为像不像官方」，不是密码学验真。所以结论既不能证明是真，也不能证明是假。"
	}
}

// FamilyDisplayName 返回家族的展示名。空家族返回空串，由调用方决定怎么显示。
func FamilyDisplayName(family string) string {
	switch family {
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "google":
		return "Google"
	case "xai":
		return "xAI"
	case "meta":
		return "Meta"
	case "mistral":
		return "Mistral"
	case "mimo":
		return "MiMo"
	case "qwen":
		return "通义千问"
	case "deepseek":
		return "DeepSeek"
	case "zhipu":
		return "智谱"
	case "moonshot":
		return "月之暗面"
	case "bytedance":
		return "豆包"
	case "baidu":
		return "文心"
	case "minimax":
		return "MiniMax"
	default:
		return ""
	}
}

// PrimaryFamily 从一组家族里挑一个"主家族"用于展示。
//
// 命中多个家族时（例如模型自称里同时出现 gpt 和 claude）**不猜**：
// 按固定顺序取第一个已知家族，保证同一输入每次结果一致。
func PrimaryFamily(families []string) string {
	order := []string{
		"anthropic", "openai", "google", "xai",
		"qwen", "deepseek", "zhipu", "moonshot", "bytedance", "baidu", "minimax", "mimo",
		"meta", "mistral",
	}
	seen := make(map[string]struct{}, len(families))
	for _, f := range families {
		seen[f] = struct{}{}
	}
	for _, f := range order {
		if _, ok := seen[f]; ok {
			return f
		}
	}
	return ""
}

// ChipKind 是渠道页芯片的机器可读档：ok / warn / fail / none。
// Tokenizer 等参考项绝不会把 chip 推成 warn。
type ChipKind string

const (
	ChipOK   ChipKind = "ok"
	ChipWarn ChipKind = "warn"
	ChipFail ChipKind = "fail"
	ChipNone ChipKind = "none"
)

// ChipForResult 把一条结果行映射成渠道页芯片。
// status 用 model.AuditResult* 常量；verdict 用 Verdict 常量。
func ChipForResult(status, verdict string) ChipKind {
	switch status {
	case "failed", "skipped":
		return ChipFail
	case "done":
		switch Verdict(verdict) {
		case VerdictNone, VerdictLow:
			return ChipOK
		case VerdictMedium, VerdictHigh:
			return ChipWarn
		case VerdictUnknown:
			return ChipFail // 没测成 / 数据不够 → 检测失败色
		default:
			return ChipNone
		}
	default:
		return ChipNone
	}
}

// ChipLabel 返回芯片的短中文。
func ChipLabel(k ChipKind) string {
	switch k {
	case ChipOK:
		return "正常"
	case ChipWarn:
		return "待复核"
	case ChipFail:
		return "检测失败"
	default:
		return "没检测过"
	}
}

// CountScoredFindings 只计 Score>0 的风险项。0 分参考项（签名拒收提示、
// 指纹"一致"提示、分词器不一致等）不进"需要复核"的计数。
func CountScoredFindings(findings []Finding) int {
	n := 0
	for _, f := range findings {
		if f.Score > 0 {
			n++
		}
	}
	return n
}

// CountScoredViewFindings 同上，对 ViewFinding。
func CountScoredViewFindings(findings []ViewFinding) int {
	n := 0
	for _, f := range findings {
		if f.Score > 0 {
			n++
		}
	}
	return n
}
