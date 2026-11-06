package behavior

import (
	"strings"
	"testing"
)

// fingerprintResult 造一条 scorable 的指纹结果。
func highlightFingerprintResult(probability float64, declaredRank int, lowConfidence bool) Result {
	return Result{
		ProbeID: ProbeFingerprint,
		OK:      true,
		Data: map[string]any{
			"fingerprint_decision":        "scorable",
			"fingerprint_prediction":      "claude-opus-4-6",
			"fingerprint_prediction_name": "Claude Opus 4.6",
			"fingerprint_probability":     probability,
			"fingerprint_declared_rank":   declaredRank,
			"fingerprint_low_confidence":  lowConfidence,
		},
	}
}

// tokenizerResult 造一条可用的分词器结果。
func tokenizerResult(claimStatus string) Result {
	return Result{
		ProbeID: ProbeTokenizerID,
		OK:      true,
		Data: map[string]any{
			"usable":            true,
			"top_lab":           "OpenAI",
			"top_series":        "o200k",
			"claim_status":      claimStatus,
			"claim_probability": 0.91,
		},
	}
}

// systemPromptResult 造一条复述探针结果。
func systemPromptResult(refused bool, suspects []string) Result {
	return Result{
		ProbeID: ProbeSystemPrompt,
		OK:      true,
		Data: map[string]any{
			"refused":        refused,
			"suspect_brands": suspects,
			"recited_text":   "You are Claude, running on Bedrock.",
		},
	}
}

func highlightByKind(highlights []ViewHighlight, kind string) (ViewHighlight, bool) {
	for _, h := range highlights {
		if h.Kind == kind {
			return h, true
		}
	}
	return ViewHighlight{}, false
}

// 百分比只给指纹与分词器两类；其余必须为 nil——没有概率来源的数字比没有数字更误导。
func TestHighlightsPercentOnlyForFingerprintAndTokenizer(t *testing.T) {
	report := Report{
		RequestedModel:    "claude-opus-4-6",
		RequestedFamilies: []string{"anthropic"},
		Results: []Result{
			highlightFingerprintResult(0.82, 1, false),
			tokenizerResult("consistent"),
			systemPromptResult(false, []string{"Bedrock"}),
		},
		Findings: []Finding{{
			Probe: ProbeSignature, Severity: SeverityLow, Score: 0,
			Title: "签名结构与绑定模型对得上（没发现矛盾）",
		}},
		Provenance: ProvenanceSummary{
			Hints:    []ProvenanceHint{{Platform: "aws_bedrock", Strength: "strong", Code: "response_id_bedrock", Evidence: "msg_bdrk_"}},
			Observed: 3,
		},
	}
	highlights := ToView(report).Highlights

	wantOrder := []string{"fingerprint", "tokenizer", "provenance", "system_prompt", "signature"}
	if len(highlights) != len(wantOrder) {
		t.Fatalf("highlights = %+v, want %d lines", highlights, len(wantOrder))
	}
	for i, kind := range wantOrder {
		if highlights[i].Kind != kind {
			t.Fatalf("highlight[%d] = %q, want %q", i, highlights[i].Kind, kind)
		}
	}

	fingerprint := highlights[0]
	if fingerprint.Percent == nil || *fingerprint.Percent != 82 {
		t.Fatalf("fingerprint percent = %v, want 82", fingerprint.Percent)
	}
	if fingerprint.Tone != viewToneOK || !strings.Contains(fingerprint.Text, "Claude Opus 4.6") {
		t.Fatalf("fingerprint highlight = %+v", fingerprint)
	}
	tokenizer := highlights[1]
	if tokenizer.Percent == nil || *tokenizer.Percent != 91 {
		t.Fatalf("tokenizer percent = %v, want 91", tokenizer.Percent)
	}
	for _, h := range highlights[2:] {
		if h.Percent != nil {
			t.Fatalf("%s must not carry a percent: %+v", h.Kind, h)
		}
	}

	// 措辞硬规矩：不说真伪、不说官方正品、不说已验真。
	for _, h := range highlights {
		for _, banned := range []string{"已验真", "官方正品", "是假模型", "是真模型"} {
			if strings.Contains(h.Text, banned) {
				t.Fatalf("highlight %q 用了禁用措辞 %q", h.Text, banned)
			}
		}
	}
}

// 指纹与声明不是同一个：tone=warn，文字要多说一句；低置信再补一句。
func TestHighlightsFingerprintMismatchAndLowConfidence(t *testing.T) {
	report := Report{
		RequestedModel:    "claude-opus-4-6",
		RequestedFamilies: []string{"anthropic"},
		Results:           []Result{highlightFingerprintResult(0.55, 3, true)},
	}
	highlights := ToView(report).Highlights
	h, ok := highlightByKind(highlights, "fingerprint")
	if !ok {
		t.Fatal("missing fingerprint highlight")
	}
	if h.Tone != viewToneWarn {
		t.Fatalf("tone = %q, want warn", h.Tone)
	}
	if !strings.Contains(h.Text, "和声明的不是同一个") || !strings.Contains(h.Text, "（把握不大）") {
		t.Fatalf("text = %q", h.Text)
	}
}

// 分词器与声明不一致：tone=warn。
func TestHighlightsTokenizerInconsistentIsWarn(t *testing.T) {
	report := Report{Results: []Result{tokenizerResult("inconsistent")}}
	h, ok := highlightByKind(ToView(report).Highlights, "tokenizer")
	if !ok {
		t.Fatal("missing tokenizer highlight")
	}
	if h.Tone != viewToneWarn || !strings.Contains(h.Text, "和声明的不是同一个") {
		t.Fatalf("highlight = %+v", h)
	}
}

// 没跑的项、数据不够的项一行都不出。
func TestHighlightsSkipProbesWithoutData(t *testing.T) {
	// 什么都没跑：没有任何要点。
	if highlights := ToView(Report{RequestedModel: "gpt-5.6"}).Highlights; len(highlights) != 0 {
		t.Fatalf("highlights = %+v, want none", highlights)
	}

	// 指纹样本不足（partial）不该说"最像 X"：那是在拿样本不足冒充结论。
	partial := Result{ProbeID: ProbeFingerprint, OK: true, Data: map[string]any{
		"fingerprint_decision":        "partial",
		"fingerprint_prediction_name": "Claude Opus 4.6",
	}}
	if _, ok := highlightByKind(ToView(Report{Results: []Result{partial}}).Highlights, "fingerprint"); ok {
		t.Fatal("partial 指纹不该出要点")
	}

	// 复述探针正常没命中：不出要点（拒答才出）。
	if _, ok := highlightByKind(ToView(Report{Results: []Result{systemPromptResult(false, nil)}}).Highlights, "system_prompt"); ok {
		t.Fatal("没命中时不该出 system_prompt 要点")
	}

	// 一条信封都没抄到（Observed=0）：不能说"没发现转售线索"，一行都不出。
	if _, ok := highlightByKind(ToView(Report{Provenance: ProvenanceSummary{Observed: 0}}).Highlights, "provenance"); ok {
		t.Fatal("Observed=0 时不该出 provenance 要点")
	}
}

// 可疑来源：warn + 中文平台名；没线索但有分母：info。
func TestHighlightsProvenanceTones(t *testing.T) {
	suspicious := Report{Provenance: ProvenanceSummary{
		Hints:    []ProvenanceHint{{Platform: "aws_kiro", Strength: "strong", Code: "model_prefix_kiro", Evidence: "kiro-x"}},
		Observed: 2,
	}}
	h, ok := highlightByKind(ToView(suspicious).Highlights, "provenance")
	if !ok {
		t.Fatal("missing provenance highlight")
	}
	if h.Tone != viewToneWarn || !strings.Contains(h.Text, "Kiro（AWS）") {
		t.Fatalf("highlight = %+v", h)
	}

	clean := Report{Provenance: ProvenanceSummary{Observed: 4}}
	h, ok = highlightByKind(ToView(clean).Highlights, "provenance")
	if !ok {
		t.Fatal("missing provenance highlight for a clean but observed run")
	}
	if h.Tone != viewToneInfo || h.Text != "没发现转售线索" {
		t.Fatalf("highlight = %+v", h)
	}

	// 中途换模型这条 platform 为空，但它有话可说。
	switched := Report{Provenance: ProvenanceSummary{
		Hints:    []ProvenanceHint{{Platform: "", Strength: "medium", Code: "model_switch_mid_stream", Evidence: "a → b"}},
		Observed: 1,
	}}
	h, _ = highlightByKind(ToView(switched).Highlights, "provenance")
	if !strings.Contains(h.Text, "中途换了模型") {
		t.Fatalf("highlight = %+v", h)
	}
}

// 复述提示词：可疑品牌 warn，拒答 info。
func TestHighlightsSystemPrompt(t *testing.T) {
	hit := Report{Results: []Result{systemPromptResult(false, []string{"Bedrock", "Kiro"})}}
	h, ok := highlightByKind(ToView(hit).Highlights, "system_prompt")
	if !ok {
		t.Fatal("missing system_prompt highlight")
	}
	if h.Tone != viewToneWarn || !strings.Contains(h.Text, "Bedrock、Kiro") {
		t.Fatalf("highlight = %+v", h)
	}

	refused := Report{Results: []Result{systemPromptResult(true, nil)}}
	h, ok = highlightByKind(ToView(refused).Highlights, "system_prompt")
	if !ok {
		t.Fatal("missing refused highlight")
	}
	if h.Tone != viewToneInfo || !strings.Contains(h.Text, "没肯复述") {
		t.Fatalf("highlight = %+v", h)
	}
}

// 签名要点只对 Claude 出，且沿用现有签名 finding 的口径。
func TestHighlightsSignatureOnlyForClaude(t *testing.T) {
	signatureFinding := Finding{
		Probe: ProbeSignature, Severity: SeverityLow, Score: 0,
		Title: "签名结构与绑定模型对得上（没发现矛盾）",
	}
	claude := Report{
		RequestedModel:    "claude-opus-4-6",
		RequestedFamilies: []string{"anthropic"},
		Findings:          []Finding{signatureFinding},
	}
	h, ok := highlightByKind(ToView(claude).Highlights, "signature")
	if !ok {
		t.Fatal("missing signature highlight on a Claude channel")
	}
	if h.Text != signatureFinding.Title || h.Tone != viewToneInfo {
		t.Fatalf("highlight = %+v, want the finding title with info tone", h)
	}

	// 非 Claude 家族：签名这件事本来就谈不上，不该出这一行。
	openai := Report{
		RequestedModel:    "gpt-5.6",
		RequestedFamilies: []string{"openai"},
		Findings:          []Finding{signatureFinding},
	}
	if _, ok := highlightByKind(ToView(openai).Highlights, "signature"); ok {
		t.Fatal("非 Claude 家族不该出签名要点")
	}

	// 有分的签名问题 → warn。
	scored := Report{
		RequestedModel:    "claude-opus-4-6",
		RequestedFamilies: []string{"anthropic"},
		Findings: []Finding{{
			Probe: ProbeSignature, Severity: SeverityMedium, Score: 25,
			Title: "签名结构异常（密文长度不足）",
		}},
	}
	h, ok = highlightByKind(ToView(scored).Highlights, "signature")
	if !ok || h.Tone != viewToneWarn {
		t.Fatalf("highlight = %+v (%v), want warn", h, ok)
	}
}
