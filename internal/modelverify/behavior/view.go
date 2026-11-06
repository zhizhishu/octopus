package behavior

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/zhizhishu/octopus/internal/modelverify/tokenbank"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
)

// ViewReport 是给管理面板用的只读报告。
// 不落库；Result.Err 换成字符串，避免 JSON 丢错误。
type ViewReport struct {
	RequestedModel    string        `json:"requested_model"`
	RequestedFamilies []string      `json:"requested_families,omitempty"`
	Score             int           `json:"score"`
	Verdict           Verdict       `json:"verdict"`
	Findings          []ViewFinding `json:"findings"`
	Results           []ViewResult  `json:"results"`
	Errors            []ProbeError  `json:"errors"`
	Disclaimer        string        `json:"disclaimer"`
	// Plain 是同一份结论的"人话版"：标题、这意味着什么、不能说明什么、下一步。
	// 与 Verdict/Score 并存而不是替换，机器读档位、人读措辞。
	Plain PlainVerdict `json:"plain"`
	// Family 是从模型名推断出的主家族（openai/anthropic/qwen...），空串表示看不出。
	// 前端据此显示家族名并给出该家族特有的验真边界说明。
	Family string `json:"family,omitempty"`
	// Highlights 是结论顶部那几行「大白话要点」，按固定顺序生成：指纹 → 分词器 →
	// 来源线索 → 复述提示词 → 签名。没跑的项、没数据可说的项**一行都不出**——
	// “没这一行”本身就是一个信号：不是每项都值得说一句。
	Highlights []ViewHighlight `json:"highlights"`
	// Provenance 是本次检测顺手抄到的来源线索汇总（见 provenance.go）。
	// Observed=0 表示一条信封都没抄到（没跑到出站成功那一步），
	// 此时不能说“没发现转售线索”。
	Provenance ProvenanceSummary `json:"provenance"`
}

// ViewHighlight 是一行「大白话要点」。
//
// 为什么由后端生成而不是前端拼：同一批结论会在结果页、历史详情、通知里各出现一次，
// 散在前端就会各写一套、慢慢漂移；而且「什么时候不该说话」（没跑的项、没命中的项
// 一律不出一行）属于判定口径，必须和评分层同源。
//
// 措辞守 plain.go 顶部的硬规矩：不说「真/假」、不说「官方正品」、不说「已验真」。
type ViewHighlight struct {
	// Kind 机器可读：fingerprint / tokenizer / provenance / system_prompt / signature。
	Kind string `json:"kind"`
	// Text 一句话结论，直接展示。
	Text string `json:"text"`
	// Tone 取 ok / warn / info，前端据此选颜色。
	Tone string `json:"tone"`
	// Percent 只有「有参考概率」的两类（指纹、分词器）才给，其余必须为 nil——
	// 一个没有概率来源的数字比没有数字更误导。
	Percent *int `json:"percent,omitempty"`
}

// 要点行的语气档位。
const (
	viewToneOK   = "ok"
	viewToneWarn = "warn"
	viewToneInfo = "info"
)

// ViewFinding 是一条风险项的 JSON 形态。
type ViewFinding struct {
	Probe          ProbeID        `json:"probe"`
	Severity       Severity       `json:"severity"`
	Score          int            `json:"score"`
	Title          string         `json:"title"`
	Evidence       map[string]any `json:"evidence,omitempty"`
	Recommendation string         `json:"recommendation,omitempty"`
	// PlainEvidence 一两句人话证据，给 tooltip / 行展开用；空则前端退回 Title。
	PlainEvidence string `json:"plain_evidence,omitempty"`
}

// ViewResult 是单探针原始结果的 JSON 形态。
type ViewResult struct {
	ProbeID ProbeID        `json:"probe_id"`
	OK      bool           `json:"ok"`
	Data    map[string]any `json:"data,omitempty"`
	Error   string         `json:"error,omitempty"`
}

const viewDisclaimer = "检查结果提供参考，不等同模型真实性证明；身份、词元、签名仅为线索。"

// ToView 把内存报告转成可下发的 JSON 视图，错误串走密钥脱敏。
func ToView(report Report) ViewReport {
	out := ViewReport{
		RequestedModel:    report.RequestedModel,
		RequestedFamilies: report.RequestedFamilies,
		Score:             report.Score,
		Verdict:           report.Verdict,
		Disclaimer:        viewDisclaimer,
		Findings:          make([]ViewFinding, 0, len(report.Findings)),
		Results:           make([]ViewResult, 0, len(report.Results)),
		Errors:            make([]ProbeError, 0, len(report.Errors)),
	}
	for _, f := range report.Findings {
		out.Findings = append(out.Findings, ViewFinding{
			Probe:          f.Probe,
			Severity:       f.Severity,
			Score:          f.Score,
			Title:          f.Title,
			Evidence:       jsonSafeMap(f.Evidence),
			Recommendation: f.Recommendation,
			PlainEvidence:  plainEvidenceFor(f),
		})
	}
	for _, res := range report.Results {
		item := ViewResult{
			ProbeID: res.ProbeID,
			OK:      res.OK,
			Data:    jsonSafeMap(res.Data),
		}
		if res.Err != nil {
			item.Error = xredact.Secrets(res.Err.Error())
		}
		out.Results = append(out.Results, item)
	}
	for _, e := range report.Errors {
		out.Errors = append(out.Errors, ProbeError{
			Probe: e.Probe,
			Error: xredact.Secrets(e.Error),
		})
	}

	// 人话结论。usable 只数"真正拿到可判读样本"的探针：跑挂的、被拒答的
	// 都不算，否则一个挂了三条探针的渠道会看起来像"查过了"。
	//
	// 🔴 判据必须和 score.go 里算 usable 的那套一致，否则会出现
	// 「评分按 A 口径落到 none、文案按 B 口径说'查过没问题'」的分裂。
	// 这里刻意复用同一个函数，而不是各写一遍。
	usable := 0
	for _, res := range report.Results {
		if probeCountsTowardVerdict(res) {
			usable++
		}
	}
	out.Family = PrimaryFamily(report.RequestedFamilies)
	out.Plain = PlainVerdictFor(
		report.Verdict, report.Score, CountScoredFindings(report.Findings), usable, out.Family,
	)
	// 来源线索与要点：两者都由 report 自己派生，不下发任何原值（只有类别/前缀）。
	out.Provenance = report.Provenance
	out.Highlights = highlightsFor(report)
	return out
}

// highlightsFor 按固定顺序生成「大白话要点」。
//
// 顺序不是随意的：指纹与分词器是拿得到数据的硬指标（带百分比），放前面；
// 来源线索与复述提示词是环境证据；签名结论放最后——它只在 Claude 上才有。
func highlightsFor(report Report) []ViewHighlight {
	out := make([]ViewHighlight, 0, 5)
	if h, ok := fingerprintHighlight(report); ok {
		out = append(out, h)
	}
	if h, ok := tokenizerHighlight(report); ok {
		out = append(out, h)
	}
	if h, ok := provenanceHighlight(report.Provenance); ok {
		out = append(out, h)
	}
	if h, ok := systemPromptHighlight(report); ok {
		out = append(out, h)
	}
	if h, ok := signatureHighlight(report); ok {
		out = append(out, h)
	}
	return out
}

// fingerprintHighlight：最像哪个型号 + 库内相对置信度。
//
// 只用 scorable 那结论：partial 只有排名没有置信度，把它当“最像 X”说出去
// 就是拿样本不足的结果冒充结论。
func fingerprintHighlight(report Report) (ViewHighlight, bool) {
	res, ok := reportResult(report, ProbeFingerprint)
	if !ok || !probeCountsAsUsable(res) {
		return ViewHighlight{}, false
	}
	name := stringOf(res.Data, "fingerprint_prediction_name")
	if name == "" {
		name = stringOf(res.Data, "fingerprint_prediction")
	}
	if name == "" {
		return ViewHighlight{}, false
	}
	h := ViewHighlight{Kind: "fingerprint", Tone: viewToneOK, Text: "最像 " + name}
	if rank := intOf(res.Data, "fingerprint_declared_rank"); rank > 0 && rank != 1 {
		h.Tone = viewToneWarn
		h.Text += "，和声明的不是同一个"
	}
	if boolOf(res.Data, "fingerprint_low_confidence") {
		h.Text += "（把握不大）"
	}
	if probability, ok := floatOf(res.Data, "fingerprint_probability"); ok {
		h.Percent = percentOf(probability)
	}
	return h, true
}

// tokenizerHighlight：分词器像哪家 + 声明核对结果。
//
// 百分比优先用声明核对的后验（claim_probability）：它回答的是“声明与实测是否
// 一致这个判断有多肯定”，比单类后验 confidence 更贴近这一行的意思；
// 声明对不上库内型号时没有 claim，再退回 confidence。
func tokenizerHighlight(report Report) (ViewHighlight, bool) {
	res, ok := reportResult(report, ProbeTokenizerID)
	if !ok || !probeCountsAsUsable(res) {
		return ViewHighlight{}, false
	}
	lab := stringOf(res.Data, "top_lab")
	if lab == "" {
		lab = stringOf(res.Data, "top_series")
	}
	if lab == "" {
		return ViewHighlight{}, false
	}
	h := ViewHighlight{Kind: "tokenizer", Tone: viewToneOK, Text: "分词器像 " + lab + " 家"}
	if probability, ok := floatOf(res.Data, "claim_probability"); ok {
		h.Percent = percentOf(probability)
	} else if confidence, ok := floatOf(res.Data, "confidence"); ok {
		h.Percent = percentOf(confidence)
	}
	if stringOf(res.Data, "claim_status") == tokenbank.ClaimInconsistent {
		h.Tone = viewToneWarn
		h.Text += "，和声明的不是同一个"
	}
	return h, true
}

// provenanceHighlight：主动检测顺手抄到的来源线索。
//
// 三种情况必须分开：
//   - 有强/中线索 → “疑似 X”（warn）；
//   - 没线索但确实抄到了信封 → “没发现转售线索”（info），这是有分母的结论；
//   - Observed=0（一条信封都没抄到）→ 一行都不出。
//
// 把后两者泥在一起，一个挂掉的渠道会显示成“没发现转售线索”，那是假绿。
func provenanceHighlight(summary ProvenanceSummary) (ViewHighlight, bool) {
	if hint, ok := summary.topHint(); ok {
		return ViewHighlight{
			Kind: "provenance",
			Tone: viewToneWarn,
			Text: "疑似 " + ProvenancePlatformText(hint.Platform),
		}, true
	}
	if summary.Observed > 0 {
		return ViewHighlight{Kind: "provenance", Tone: viewToneInfo, Text: "没发现转售线索"}, true
	}
	return ViewHighlight{}, false
}

// systemPromptHighlight：复述系统提示词的结论。
//
// 拒答只说“没肯复述”，不说“没问题”：不肯复述与“提示词里没别家痕迹”不是同一件事。
func systemPromptHighlight(report Report) (ViewHighlight, bool) {
	res, ok := reportResult(report, ProbeSystemPrompt)
	if !ok || res.Err != nil || !res.OK {
		return ViewHighlight{}, false
	}
	if suspects := stringSliceOf(res.Data, "suspect_brands"); len(suspects) > 0 {
		return ViewHighlight{
			Kind: "system_prompt",
			Tone: viewToneWarn,
			Text: "复述的提示词里出现 " + strings.Join(suspects, "、"),
		}, true
	}
	if boolOf(res.Data, "refused") {
		return ViewHighlight{Kind: "system_prompt", Tone: viewToneInfo, Text: "模型没肯复述系统提示词"}, true
	}
	return ViewHighlight{}, false
}

// signatureHighlight：已有签名结论时把它的一句话抬到要点里。
//
// 只对 Claude 出：其它家族本来就没有可验证签名，那一行不仅无用，还会让人以为
// “没签名结论 = 没查”与“查了没有”是一回事。文案直接沿用现有签名 finding 的 Title
// （口径已在 score.go 里：不定真伪，只说结构与回放观察）。
func signatureHighlight(report Report) (ViewHighlight, bool) {
	if !FamiliesIntersect(report.RequestedFamilies, []string{"anthropic"}) {
		return ViewHighlight{}, false
	}
	var best *Finding
	for i := range report.Findings {
		f := &report.Findings[i]
		if f.Probe != ProbeSignature {
			continue
		}
		if best == nil || f.Score > best.Score {
			best = f
		}
	}
	if best == nil {
		return ViewHighlight{}, false
	}
	tone := viewToneInfo
	if best.Score > 0 {
		tone = viewToneWarn
	}
	return ViewHighlight{Kind: "signature", Tone: tone, Text: best.Title}, true
}

// reportResult 在报告里找某条探针的结果。
func reportResult(report Report, id ProbeID) (Result, bool) {
	for _, res := range report.Results {
		if res.ProbeID == id {
			return res, true
		}
	}
	return Result{}, false
}

// percentOf 把 0~1 的概率换成展示用的 0~100 整数百分比（四舍五入）。
//
// 夹到 0~100：概率本该落在区间内，真出现越界值宁可按边界展示，
// 也不要把 123% 这种东西放到界面上。
func percentOf(probability float64) *int {
	if probability < 0 {
		probability = 0
	}
	if probability > 1 {
		probability = 1
	}
	n := int(math.Round(probability * 100))
	return &n
}

// plainEvidenceFor 把一条 Finding 收成一两句人话（不重复 Title）。
func plainEvidenceFor(f Finding) string {
	// 复述系统提示词这条：Title 说的是“出现了谁”，人话要补一句“这意味着什么”，
	// 否则用户只看到一串品牌名，不知道该怎么看。
	if f.Probe == ProbeSystemPrompt {
		return "模型照抄系统提示词时把别家平台的措辞一起带了出来，这通常说明这一路不是直连，而是经第三方平台转售。"
	}
	if f.Recommendation != "" {
		return f.Recommendation
	}
	return f.Title
}

func jsonSafeMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return map[string]any{"_omitted": "unserializable"}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
