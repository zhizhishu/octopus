package model

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// hlwyTestNumbers 生成确定性的伪随机数字序列（避免用例之间漂移）。
func hlwyTestNumbers(n, seed int) []int {
	out := make([]int, n)
	x := uint32(seed)*2654435761 + 1
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = int(x%numberdist.DistLength) + 1
	}
	return out
}

// hlwyTestBoolPtr / hlwyTestIntPtr 造「已观测」的可空字段。
func hlwyTestBoolPtr(b bool) *bool { return &b }
func hlwyTestIntPtr(i int) *int    { return &i }

// hlwyTestConditions 是一份「全部观测到」的条件快照（completeness=complete
// 就必须字面意义上每个字段都观测到，否则 matched 判定会被 unknown 拖成 unknown）。
func hlwyTestConditions() numberdist.Conditions {
	return numberdist.Conditions{
		Protocol:         "openai-chat",
		ProtocolID:       numberdist.ProtocolID,
		ParserVersion:    numberdist.ParserVersion,
		PromptSHA256:     strings.Repeat("b", 64),
		TransportProfile: numberdist.TransportProfile,
		TemperatureState: numberdist.StateAbsent,
		TopPState:        numberdist.StateAbsent,
		Stream:           hlwyTestBoolPtr(true),
		MaxTokenField:    "max_tokens",
		MaxTokens:        hlwyTestIntPtr(1024),
		Thinking:         "disabled",
		ReasoningEffort:  "none",
		SystemInjected:   hlwyTestBoolPtr(false),
		ToolsInjected:    hlwyTestBoolPtr(false),
		Completeness:     "complete",
	}
}

// hlwyTestBaseline 组一份自洽的基准（计数/统计都由数字重算）。
func hlwyTestBaseline(t *testing.T, id, label string, numbers []int) numberdist.Baseline {
	t.Helper()
	counts, err := HlwyBinCounts(numbers)
	if err != nil {
		t.Fatalf("HlwyBinCounts: %v", err)
	}
	return numberdist.Baseline{
		SchemaVersion: numberdist.SchemaVersion,
		ID:            id,
		Label:         label,
		PayloadSHA256: strings.Repeat("a", 64),
		Source:        numberdist.Source{Kind: numberdist.SourceImported, Repo: "hlwy"},
		Conditions:    hlwyTestConditions(),
		RawNumbers:    numbers,
		Counts:        counts,
		Stats:         numberdist.StatsOf(numbers),
	}
}

func TestHlwyOptionsValidate(t *testing.T) {
	a := hlwyTestBaseline(t, "hlwy:a@"+strings.Repeat("a", 12), "a", hlwyTestNumbers(100, 1))
	b := hlwyTestBaseline(t, "hlwy:b@"+strings.Repeat("c", 12), "b", hlwyTestNumbers(100, 2))

	for _, samples := range []int{HlwyDefaultSamples, HlwyMaxSamples} {
		opts := HlwyOptions{
			Samples:    samples,
			BaselineID: a.ID,
			ProtocolID: numberdist.ProtocolID,
			Candidates: []numberdist.Baseline{a, b},
		}
		if err := opts.Validate(); err != nil {
			t.Fatalf("samples=%d 应通过: %v", samples, err)
		}
	}

	base := HlwyOptions{
		Samples:    HlwyDefaultSamples,
		BaselineID: a.ID,
		ProtocolID: numberdist.ProtocolID,
		Candidates: []numberdist.Baseline{a, b},
	}

	t.Run("samples_not_exact", func(t *testing.T) {
		for _, samples := range []int{0, 1, 37, 99, 101, 199, 201, 500} {
			opts := base
			opts.Samples = samples
			if err := opts.Validate(); err == nil {
				t.Fatalf("samples=%d 必须被拒绝（只允许 100/200，不做就近取整）", samples)
			}
		}
	})

	t.Run("protocol", func(t *testing.T) {
		opts := base
		opts.ProtocolID = "hlwy-number355-v2"
		if err := opts.Validate(); err == nil {
			t.Fatal("非首版协议必须被拒绝")
		}
	})

	t.Run("no_candidates", func(t *testing.T) {
		opts := base
		opts.Candidates = nil
		if err := opts.Validate(); err == nil {
			t.Fatal("空候选必须被拒绝")
		}
	})

	t.Run("chosen_missing", func(t *testing.T) {
		opts := base
		opts.BaselineID = "hlwy:zzz@" + strings.Repeat("d", 12)
		if err := opts.Validate(); err == nil {
			t.Fatal("主对照不在候选集合里必须被拒绝")
		}
	})

	t.Run("duplicate_candidates", func(t *testing.T) {
		opts := base
		opts.Candidates = []numberdist.Baseline{a, a}
		if err := opts.Validate(); err == nil {
			t.Fatal("重复候选必须被拒绝")
		}
	})

	t.Run("too_many_candidates", func(t *testing.T) {
		opts := base
		cands := []numberdist.Baseline{a}
		for i := 0; i < HlwyMaxCandidates; i++ {
			id := "hlwy:x" + strings.Repeat("e", i+1) + "@" + strings.Repeat("f", 12)
			cands = append(cands, hlwyTestBaseline(t, id, "x", hlwyTestNumbers(100, 10+i)))
		}
		opts.Candidates = cands
		if err := opts.Validate(); err == nil {
			t.Fatalf("%d 份候选必须被拒绝（上限 %d）", len(cands), HlwyMaxCandidates)
		}
	})
}

func TestHlwyOptionsEncodeParse(t *testing.T) {
	a := hlwyTestBaseline(t, "hlwy:a@"+strings.Repeat("a", 12), "a", hlwyTestNumbers(100, 1))
	opts := HlwyOptions{
		Samples:    HlwyMaxSamples,
		BaselineID: a.ID,
		ProtocolID: numberdist.ProtocolID,
		Candidates: []numberdist.Baseline{a},
	}
	encoded, err := opts.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := ParseHlwyOptions(encoded)
	if err != nil {
		t.Fatalf("ParseHlwyOptions: %v", err)
	}
	if back.Samples != opts.Samples || back.BaselineID != opts.BaselineID ||
		len(back.Candidates) != 1 {
		t.Fatalf("往返不一致: %+v", back)
	}
	if got, ok := back.ChosenBaseline(); !ok || got.ID != a.ID {
		t.Fatalf("ChosenBaseline = %v, %v", got.ID, ok)
	}

	if _, err := ParseHlwyOptions(""); err == nil {
		t.Fatal("空字符串必须报错")
	}
	if _, err := ParseHlwyOptions("{not json"); err == nil {
		t.Fatal("坏 JSON 必须报错")
	}
	// 未知字段拒绝：让「新版本写了旧代码不认识的字段」当场失败，而不是静默忽略。
	if _, err := ParseHlwyOptions(`{"samples":100,"baseline_id":"x","protocol_id":"` +
		numberdist.ProtocolID + `","candidates":[],"extra":1}`); err == nil {
		t.Fatal("未知字段必须报错")
	}
}

func TestHlwyOptionsSizeCap(t *testing.T) {
	old := hlwyMaxOptionsBytes
	hlwyMaxOptionsBytes = 64
	defer func() { hlwyMaxOptionsBytes = old }()

	a := hlwyTestBaseline(t, "hlwy:a@"+strings.Repeat("a", 12), "a", hlwyTestNumbers(100, 1))
	opts := HlwyOptions{
		Samples:    HlwyDefaultSamples,
		BaselineID: a.ID,
		ProtocolID: numberdist.ProtocolID,
		Candidates: []numberdist.Baseline{a},
	}
	if err := opts.Validate(); err == nil {
		t.Fatal("超过选项体积上限必须被拒绝")
	}
}

func TestHlwyOptionsParseRejectsTrailing(t *testing.T) {
	a := hlwyTestBaseline(t, "hlwy:a@"+strings.Repeat("a", 12), "a", hlwyTestNumbers(100, 1))
	opts := HlwyOptions{
		Samples:    HlwyDefaultSamples,
		BaselineID: a.ID,
		ProtocolID: numberdist.ProtocolID,
		Candidates: []numberdist.Baseline{a},
	}
	encoded, err := opts.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// 两个完整 JSON 贴在一起时只应有一种解读；解码器默认只读第一个值，
	// 剩下的会被静默丢掉，于是「后一个才是真实意图」就没人知道了。
	if _, err := ParseHlwyOptions(encoded + " " + encoded); err == nil {
		t.Fatal("尾随第二个 JSON 值必须被拒绝")
	}
}

func TestHlwyOptionsParseSizeCap(t *testing.T) {
	old := hlwyMaxOptionsBytes
	hlwyMaxOptionsBytes = 32
	defer func() { hlwyMaxOptionsBytes = old }()

	// 这一段是合法 JSON，只是超过上限：必须在解码前就拦下。
	if _, err := ParseHlwyOptions(`{"samples":100,"baseline_id":"x"}`); err == nil {
		t.Fatal("超长选项必须在解码前被拒绝")
	}
}

func TestValidateHlwyBaseline(t *testing.T) {
	numbers := hlwyTestNumbers(100, 3)
	good := hlwyTestBaseline(t, "hlwy:g@"+strings.Repeat("a", 12), "g", numbers)
	if err := ValidateHlwyBaseline(good); err != nil {
		t.Fatalf("合法基准应通过: %v", err)
	}

	t.Run("counts_tampered", func(t *testing.T) {
		b := good
		b.Counts = [numberdist.DistLength]int{}
		b.Counts[0]++
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("计数与 raw_numbers 不符必须被拒绝（不能换成重算值继续用）")
		}
	})

	t.Run("stats_tampered", func(t *testing.T) {
		b := good
		b.Stats.Mean += 0.5
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("统计与 raw_numbers 不符必须被拒绝")
		}
	})

	t.Run("raw_out_of_range", func(t *testing.T) {
		b := good
		b.RawNumbers = []int{356}
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("越界数字必须被拒绝（不静默丢弃）")
		}
	})

	t.Run("zero", func(t *testing.T) {
		b := good
		b.RawNumbers = []int{0}
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("0 不在 1..355")
		}
	})

	t.Run("bad_sha", func(t *testing.T) {
		b := good
		b.PayloadSHA256 = "ABCDEF"
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("非 64 位小写十六进制必须被拒绝")
		}
	})

	t.Run("empty_label", func(t *testing.T) {
		b := good
		b.Label = "   "
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("空 label 必须被拒绝")
		}
	})

	t.Run("label_too_long", func(t *testing.T) {
		b := good
		b.Label = strings.Repeat("长", HlwyMaxLabelBytes/3+1)
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("label 超长必须被拒绝")
		}
	})

	t.Run("note_too_long", func(t *testing.T) {
		b := good
		b.Source.Note = strings.Repeat("n", HlwyMaxNoteBytes+1)
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("来源备注超长必须被拒绝")
		}
	})

	t.Run("schema_version", func(t *testing.T) {
		b := good
		b.SchemaVersion = numberdist.SchemaVersion + 1
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("不认识的结构版本必须被拒绝")
		}
	})

	t.Run("source_kind", func(t *testing.T) {
		b := good
		b.Source.Kind = "guessed"
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("未知来源类别必须被拒绝")
		}
	})

	t.Run("conditions_tampered", func(t *testing.T) {
		b := good
		b.Conditions.TemperatureState = "maybe"
		if err := ValidateHlwyBaseline(b); err == nil {
			t.Fatal("条件三态写错必须被拒绝")
		}
	})
}

func TestHlwyBinCounts(t *testing.T) {
	counts, err := HlwyBinCounts([]int{1, 355, 355})
	if err != nil {
		t.Fatalf("HlwyBinCounts: %v", err)
	}
	if counts[0] != 1 || counts[354] != 2 {
		t.Fatalf("桶映射错: first=%d last=%d", counts[0], counts[354])
	}
	if _, err := HlwyBinCounts(nil); err == nil {
		t.Fatal("空输入必须报错")
	}
	if _, err := HlwyBinCounts(make([]int, numberdist.DefaultMaxNumbers+1)); err == nil {
		t.Fatal("超过数量上限必须报错")
	}
}
