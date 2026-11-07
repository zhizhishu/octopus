package model

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// hlwyTestReport 组一份自洽的对照报告（计数/分布/统计都由数字推出）。
func hlwyTestReport(t *testing.T, numbers []int, planned int, execution string) HlwyAuditReport {
	t.Helper()
	if len(numbers) > planned {
		t.Fatalf("有效样本 %d 不能多于计划 %d", len(numbers), planned)
	}
	base := hlwyTestBaseline(t, "hlwy:base@"+strings.Repeat("a", 12), "base", hlwyTestNumbers(100, 99))
	cmp, err := numberdist.Compare(numbers, []numberdist.Baseline{base}, base.ID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	bc := HlwyBaselineComparison{
		SchemaVersion:    numberdist.SchemaVersion,
		AlgorithmVersion: numberdist.AlgorithmVer,
		Execution:        execution,
		Sufficiency:      HlwySufficiency(len(numbers)),
		Comparability:    HlwyComparabilityMatched,
		ReasonCodes:      []string{},
		Counts: HlwyCounts{
			PlannedCalls: planned,
			StartedSlots: planned,
			Valid:        len(numbers),
			Invalid:      planned - len(numbers),
		},
		RawNumbers:        numbers,
		Conditions:        hlwyTestConditions(),
		BaselineSnapshots: []numberdist.Baseline{HlwySnapshotOf(base)},
		Scorable:          HlwyScorable(len(numbers), HlwyComparabilityMatched),
		Reason:            cmp.Reason,
		ValidCount:        cmp.ValidCount,
		Distribution:      cmp.Distribution,
		Stats:             cmp.Stats,
		ChosenBaselineID:  cmp.ChosenBaselineID,
		Matches:           cmp.Matches,
		Quality:           HlwyQuality{StrictInteger: len(numbers)},
		StopReason:        "",
	}
	return HlwyAuditReport{
		Kind:               AuditKindHlwy,
		RequestedModel:     "test-model",
		UpstreamModel:      "test-model",
		Score:              0,
		Verdict:            HlwyVerdictUnknown,
		Plain:              HlwyPlainOf(bc, false),
		BaselineComparison: bc,
	}.Filled()
}

func TestParseHlwyReport_RoundTrip(t *testing.T) {
	report := hlwyTestReport(t, hlwyTestNumbers(HlwyDefaultSamples, 5), HlwyDefaultSamples, HlwyExecutionComplete)
	encoded, err := report.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := ParseHlwyReport(encoded)
	if err != nil {
		t.Fatalf("ParseHlwyReport: %v", err)
	}
	if back.Kind != AuditKindHlwy {
		t.Fatalf("Kind = %q", back.Kind)
	}
	if back.BaselineComparison.Counts.Valid != HlwyDefaultSamples {
		t.Fatalf("valid = %d", back.BaselineComparison.Counts.Valid)
	}
	if back.BaselineComparison.Sufficiency != HlwySufficiencyScorable {
		t.Fatalf("sufficiency = %q", back.BaselineComparison.Sufficiency)
	}
	// 数组必须序列化成 []，不能是 null（前端直接遍历）。
	for name, raw := range map[string][]json.RawMessage{
		"results": back.Results, "findings": back.Findings, "errors": back.Errors,
	} {
		if raw == nil {
			t.Fatalf("%s 为 nil，应为空数组", name)
		}
	}
	if back.BaselineComparison.Matches == nil {
		t.Fatal("matches 为 nil，应为空数组")
	}
}

func TestParseHlwyReport_EmptySnapshot(t *testing.T) {
	_, err := ParseHlwyReport("   ")
	if !errors.Is(err, ErrHlwyReportEmpty) {
		t.Fatalf("err = %v, want ErrHlwyReportEmpty", err)
	}
}

func TestParseHlwyReport_Rejects(t *testing.T) {
	good := hlwyTestReport(t, hlwyTestNumbers(HlwyDefaultSamples, 6), HlwyDefaultSamples, HlwyExecutionComplete)

	cases := map[string]func(r *HlwyAuditReport){
		"wrong_kind":            func(r *HlwyAuditReport) { r.Kind = AuditKindProbe },
		"wrong_schema":          func(r *HlwyAuditReport) { r.BaselineComparison.SchemaVersion = 2 },
		"missing_algorithm":     func(r *HlwyAuditReport) { r.BaselineComparison.AlgorithmVersion = "" },
		"unknown_execution":     func(r *HlwyAuditReport) { r.BaselineComparison.Execution = "halfway" },
		"unknown_sufficiency":   func(r *HlwyAuditReport) { r.BaselineComparison.Sufficiency = "great" },
		"unknown_comparability": func(r *HlwyAuditReport) { r.BaselineComparison.Comparability = "sameish" },
		"bad_planned":           func(r *HlwyAuditReport) { r.BaselineComparison.Counts.PlannedCalls = 37 },
		"count_mismatch": func(r *HlwyAuditReport) {
			r.BaselineComparison.Counts.StartedSlots++
			r.BaselineComparison.Counts.PlannedCalls++
		},
		"started_gt_planned": func(r *HlwyAuditReport) {
			r.BaselineComparison.Counts.PlannedCalls--
			r.BaselineComparison.Counts.Unresolved++
		},
		"valid_vs_raw": func(r *HlwyAuditReport) {
			r.BaselineComparison.Counts.Valid--
			r.BaselineComparison.Counts.Invalid++
			r.BaselineComparison.ValidCount--
		},
		"raw_out_of_range": func(r *HlwyAuditReport) {
			r.BaselineComparison.RawNumbers[0] = 356
		},
		"raw_zero": func(r *HlwyAuditReport) {
			r.BaselineComparison.RawNumbers[0] = 0
		},
		"distribution_corrupted": func(r *HlwyAuditReport) {
			r.BaselineComparison.Distribution[0] += 0.25
		},
		"stats_corrupted": func(r *HlwyAuditReport) {
			r.BaselineComparison.Stats.Mean += 3
		},
		"sufficiency_lies": func(r *HlwyAuditReport) {
			r.BaselineComparison.Counts.Valid = 10
			r.BaselineComparison.Counts.Invalid = HlwyDefaultSamples - 10
			r.BaselineComparison.ValidCount = 10
			r.BaselineComparison.RawNumbers = r.BaselineComparison.RawNumbers[:10]
			r.BaselineComparison.Quality = HlwyQuality{StrictInteger: 10}
			r.BaselineComparison.Sufficiency = HlwySufficiencyScorable
		},
		"quality_overflow": func(r *HlwyAuditReport) {
			r.BaselineComparison.Quality.Permissive = HlwyDefaultSamples
		},
		"negative_counts": func(r *HlwyAuditReport) {
			r.BaselineComparison.Counts.Failed = -1
			r.BaselineComparison.Counts.Invalid = HlwyDefaultSamples - HlwyDefaultSamples + 1
		},
		"score_not_zero":      func(r *HlwyAuditReport) { r.Score = 55 },
		"verdict_not_unknown": func(r *HlwyAuditReport) { r.Verdict = "high" },
		"too_many_snapshots": func(r *HlwyAuditReport) {
			snap := r.BaselineComparison.BaselineSnapshots[0]
			for i := 0; i <= HlwyMaxCandidates; i++ {
				r.BaselineComparison.BaselineSnapshots =
					append(r.BaselineComparison.BaselineSnapshots, snap)
			}
		},
		// 下面这几条对应「快照/账目/可评分」三条容易互相掩护的规则：
		// 单看计数都对得上，但一句话就把结论拔高了。
		"truncated_gt_started": func(r *HlwyAuditReport) {
			r.BaselineComparison.Quality.Truncated = HlwyDefaultSamples + 1
		},
		"complete_unresolved": func(r *HlwyAuditReport) {
			c := &r.BaselineComparison.Counts
			c.PlannedCalls = HlwyMaxSamples
			c.StartedSlots = HlwyMaxSamples
			c.Invalid = HlwyMaxSamples - c.Valid - 1
			c.Unresolved = 1
		},
		"scorable_when_incompatible": func(r *HlwyAuditReport) {
			r.BaselineComparison.Comparability = HlwyComparabilityIncompatible
		},
		"matches_missing_snapshot": func(r *HlwyAuditReport) {
			r.BaselineComparison.Matches[0].BaselineID = "hlwy:ghost@" + strings.Repeat("d", 12)
		},
		"rank_not_exact": func(r *HlwyAuditReport) {
			r.BaselineComparison.Matches[0].Rank = 2
		},
		"chosen_not_in_snapshots": func(r *HlwyAuditReport) {
			r.BaselineComparison.ChosenBaselineID = "hlwy:ghost@" + strings.Repeat("d", 12)
		},
		"duplicate_snapshot": func(r *HlwyAuditReport) {
			bc := &r.BaselineComparison
			bc.BaselineSnapshots = append(bc.BaselineSnapshots, bc.BaselineSnapshots[0])
		},
		"not_scorable_but_matches": func(r *HlwyAuditReport) {
			bc := &r.BaselineComparison
			bc.RawNumbers = bc.RawNumbers[:numberdist.MinScorable-1]
			bc.Counts.Valid = len(bc.RawNumbers)
			bc.Counts.Invalid = bc.Counts.PlannedCalls - len(bc.RawNumbers)
			bc.ValidCount = len(bc.RawNumbers)
			bc.Quality = HlwyQuality{StrictInteger: len(bc.RawNumbers)}
			bc.Sufficiency = HlwySufficiency(len(bc.RawNumbers))
			bc.Scorable = false
			bc.Reason = "insufficient_samples"
			counts, err := HlwyBinCounts(bc.RawNumbers)
			if err != nil {
				return
			}
			for i := 0; i < numberdist.DistLength; i++ {
				bc.Distribution[i] = float64(counts[i]) / float64(len(bc.RawNumbers))
			}
			bc.Stats = numberdist.StatsOf(bc.RawNumbers)
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			report := good
			report.BaselineComparison.RawNumbers = append([]int(nil), good.BaselineComparison.RawNumbers...)
			report.BaselineComparison.BaselineSnapshots =
				append([]numberdist.Baseline(nil), good.BaselineComparison.BaselineSnapshots...)
			report.BaselineComparison.Matches =
				append([]numberdist.ScoreResult(nil), good.BaselineComparison.Matches...)
			mutate(&report)
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := ParseHlwyReport(string(raw)); err == nil {
				t.Fatal("坏报告必须被拒绝，不能被修正后继续用")
			}
		})
	}
}

func TestValidateHlwyScore(t *testing.T) {
	gap := 0.1
	good := numberdist.ScoreResult{
		BaselineID: "hlwy:x@" + strings.Repeat("a", 12),
		Cosine:     0.5, JSD: 0.2, ModeScore: 1, Overall: 0.7, Rank: 1,
		GapToPrevious: &gap,
	}
	if err := validateHlwyScore(good); err != nil {
		t.Fatalf("合法分数应通过: %v", err)
	}

	bad := []struct {
		name string
		f    func(*numberdist.ScoreResult)
	}{
		{"missing_id", func(m *numberdist.ScoreResult) { m.BaselineID = "" }},
		{"nan", func(m *numberdist.ScoreResult) { m.Overall = math.NaN() }},
		{"inf", func(m *numberdist.ScoreResult) { m.Cosine = math.Inf(1) }},
		{"over_one", func(m *numberdist.ScoreResult) { m.JSD = 1.5 }},
		{"negative", func(m *numberdist.ScoreResult) { m.ModeScore = -0.5 }},
		{"rank_zero", func(m *numberdist.ScoreResult) { m.Rank = 0 }},
		{"negative_gap", func(m *numberdist.ScoreResult) { g := -0.2; m.GapToPrevious = &g }},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			m := good
			c.f(&m)
			if err := validateHlwyScore(m); err == nil {
				t.Fatal("非法分数必须被拒绝")
			}
		})
	}
}

func TestHlwyComparability(t *testing.T) {
	complete := func() numberdist.Conditions { return hlwyTestConditions() }
	withTemp := func(v float64) numberdist.Conditions {
		c := complete()
		c.TemperatureState = numberdist.StateValue
		c.Temperature = &v
		return c
	}

	t.Run("matched", func(t *testing.T) {
		state, codes := HlwyComparability(complete(), complete())
		if state != HlwyComparabilityMatched {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
		if len(codes) != 0 {
			t.Fatalf("matched 不应有原因码: %v", codes)
		}
	})

	t.Run("both_unknown_is_not_matched", func(t *testing.T) {
		run, base := complete(), complete()
		run.TemperatureState = numberdist.StateUnknown
		base.TemperatureState = numberdist.StateUnknown
		state, codes := HlwyComparability(run, base)
		if state != HlwyComparabilityUnknown {
			t.Fatalf("两侧都未知不能算 same: state=%q codes=%v", state, codes)
		}
	})

	t.Run("builtin_conditions_unknown", func(t *testing.T) {
		// 内置基准的条件快照全是 unknown（旧文件本来就没记）。
		builtin := numberdist.Conditions{
			TemperatureState: numberdist.StateUnknown,
			TopPState:        numberdist.StateUnknown,
			Completeness:     "unknown",
		}
		state, _ := HlwyComparability(complete(), builtin)
		if state != HlwyComparabilityUnknown {
			t.Fatalf("state = %q, want unknown（未知不等于不可比）", state)
		}
	})

	t.Run("different_temperature", func(t *testing.T) {
		run := withTemp(0.7)
		state, codes := HlwyComparability(run, complete())
		if state != HlwyComparabilityDifferent {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
		if !hlwyHasCode(codes, "temperature_differs") {
			t.Fatalf("缺少 temperature_differs: %v", codes)
		}
	})

	t.Run("protocol_mismatch_is_incompatible", func(t *testing.T) {
		base := complete()
		base.ProtocolID = "hlwy-number999-v1"
		state, codes := HlwyComparability(complete(), base)
		if state != HlwyComparabilityIncompatible {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
		if !hlwyHasCode(codes, "protocol_mismatch") {
			t.Fatalf("缺少 protocol_mismatch: %v", codes)
		}
	})

	t.Run("prompt_mismatch_is_incompatible", func(t *testing.T) {
		base := complete()
		base.PromptSHA256 = strings.Repeat("c", 64)
		state, _ := HlwyComparability(complete(), base)
		if state != HlwyComparabilityIncompatible {
			t.Fatalf("state = %q", state)
		}
	})

	t.Run("transport_differs", func(t *testing.T) {
		base := complete()
		base.TransportProfile = "bare-client"
		state, codes := HlwyComparability(complete(), base)
		if state != HlwyComparabilityDifferent {
			t.Fatalf("state = %q", state)
		}
		if !hlwyHasCode(codes, "transport_differs") {
			t.Fatalf("缺少 transport_differs: %v", codes)
		}
	})

	t.Run("incomplete_conditions", func(t *testing.T) {
		run := complete()
		run.Completeness = "partial"
		state, codes := HlwyComparability(run, complete())
		if state != HlwyComparabilityUnknown {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
		if !hlwyHasCode(codes, "conditions_incomplete") {
			t.Fatalf("缺少 conditions_incomplete: %v", codes)
		}
	})

	t.Run("protocol_family_differs", func(t *testing.T) {
		// 同一个题目跑在 openai-chat 与 anthropic-messages 上：已知条件不同，
		// 仍可对照（必须留下痕迹），不是不可比。
		base := complete()
		base.Protocol = "anthropic-messages"
		state, codes := HlwyComparability(complete(), base)
		if state != HlwyComparabilityDifferent {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
		if !hlwyHasCode(codes, "api_protocol_differs") {
			t.Fatalf("缺少 api_protocol_differs: %v", codes)
		}
	})

	t.Run("protocol_family_unknown", func(t *testing.T) {
		base := complete()
		base.Protocol = ""
		state, codes := HlwyComparability(complete(), base)
		if state != HlwyComparabilityUnknown {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
		if !hlwyHasCode(codes, "api_protocol_unknown") {
			t.Fatalf("缺少 api_protocol_unknown: %v", codes)
		}
	})

	t.Run("thinking_unknown_literal", func(t *testing.T) {
		// 存成字符串 "unknown" 与空值等价：不能因为两边都写着 unknown 就算一致。
		base := complete()
		base.Thinking = numberdist.StateUnknown
		state, codes := HlwyComparability(complete(), base)
		if state != HlwyComparabilityUnknown {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
		if !hlwyHasCode(codes, "thinking_unknown") {
			t.Fatalf("缺少 thinking_unknown: %v", codes)
		}
	})

	t.Run("incompatible_beats_different", func(t *testing.T) {
		run := withTemp(0.2)
		base := complete()
		base.ProtocolID = "other"
		state, _ := HlwyComparability(run, base)
		if state != HlwyComparabilityIncompatible {
			t.Fatalf("优先级错: state = %q", state)
		}
	})
}

func hlwyHasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

func TestHlwyPlainOf(t *testing.T) {
	bc := HlwyBaselineComparison{
		Execution:        HlwyExecutionComplete,
		Counts:           HlwyCounts{PlannedCalls: HlwyDefaultSamples, Valid: HlwyDefaultSamples},
		ValidCount:       HlwyDefaultSamples,
		ChosenBaselineID: "hlwy:x@" + strings.Repeat("a", 12),
	}
	plain := HlwyPlainOf(bc, false)
	if plain.Headline != "对照完成" {
		t.Fatalf("headline = %q", plain.Headline)
	}
	if strings.Contains(plain.Headline, "检测失败") {
		t.Fatal("正常的对照不能被说成检测失败")
	}
	if !strings.Contains(plain.Summary, "不判定真伪") {
		t.Fatalf("summary 缺少边界说明: %q", plain.Summary)
	}

	bc.Execution = HlwyExecutionInterrupted
	if got := HlwyPlainOf(bc, false); !strings.Contains(got.Headline, "中断") {
		t.Fatalf("中断标题 = %q", got.Headline)
	}

	bc.Execution = HlwyExecutionComplete
	bc.ValidCount = numberdist.MinScorable - 1
	bc.Counts.Valid = bc.ValidCount
	if got := HlwyPlainOf(bc, false); !strings.Contains(got.Summary, "不足") {
		t.Fatalf("样本不足时应明说: %q", got.Summary)
	}

	bc.ValidCount = HlwyDefaultSamples
	bc.Counts.Valid = HlwyDefaultSamples
	if got := HlwyPlainOf(bc, true); !strings.Contains(got.Summary, "自比") {
		t.Fatalf("自比必须点明: %q", got.Summary)
	}
}

func TestBuildHlwyCapture(t *testing.T) {
	numbers := hlwyTestNumbers(HlwyDefaultSamples, 11)
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	in := HlwyCaptureInput{
		SourceResultID: 42,
		Label:          "本地自采",
		DeclaredModel:  "some-model",
		Conditions:     hlwyTestConditions(),
		RawNumbers:     numbers,
	}
	b, err := BuildHlwyCapture(in, now)
	if err != nil {
		t.Fatalf("BuildHlwyCapture: %v", err)
	}
	if !strings.HasPrefix(b.ID, "hlwy-capture:") {
		t.Fatalf("id = %q", b.ID)
	}
	if b.ID != "hlwy-capture:"+b.PayloadSHA256[:12] {
		t.Fatalf("id 必须由哈希限定: id=%q sha=%q", b.ID, b.PayloadSHA256)
	}
	if err := ValidateHlwyBaseline(b); err != nil {
		t.Fatalf("生成的基准必须自洽: %v", err)
	}
	if b.Source.Kind != numberdist.SourceLocal {
		t.Fatalf("来源 = %q", string(b.Source.Kind))
	}
	if !strings.Contains(b.Source.Note, "#42") {
		t.Fatalf("来源备注应指明结果 id: %q", b.Source.Note)
	}
	if !b.ImportedAt.Equal(now) {
		t.Fatalf("ImportedAt = %v", b.ImportedAt)
	}

	// 幂等：同一条结果重复保存 → 同 id、同哈希。
	again, err := BuildHlwyCapture(in, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildHlwyCapture again: %v", err)
	}
	if again.ID != b.ID || again.PayloadSHA256 != b.PayloadSHA256 {
		t.Fatalf("同输入必须同 id/sha: %q vs %q", again.ID, b.ID)
	}

	// 换来源结果 id → 必须换哈希（否则两条不同结果的基准会被当成同一份）。
	other := in
	other.SourceResultID = 43
	otherB, err := BuildHlwyCapture(other, now)
	if err != nil {
		t.Fatalf("BuildHlwyCapture other: %v", err)
	}
	if otherB.ID == b.ID {
		t.Fatal("不同结果 id 必须产生不同基准")
	}

	// 改标签不换哈希（展示字段不进哈希）。
	renamed := in
	renamed.Label = "改个名字"
	renamedB, err := BuildHlwyCapture(renamed, now)
	if err != nil {
		t.Fatalf("BuildHlwyCapture renamed: %v", err)
	}
	if renamedB.ID != b.ID {
		t.Fatal("改标签不应产生新基准（否则幂等性失效）")
	}

	bad := in
	bad.Label = "  "
	if _, err := BuildHlwyCapture(bad, now); err == nil {
		t.Fatal("空标签必须报错")
	}
	long := in
	long.Label = strings.Repeat("长", HlwyMaxLabelBytes/3+1)
	if _, err := BuildHlwyCapture(long, now); err == nil {
		t.Fatal("超长标签必须报错")
	}
	noSource := in
	noSource.SourceResultID = 0
	if _, err := BuildHlwyCapture(noSource, now); err == nil {
		t.Fatal("没有来源结果 id 的自采必须报错")
	}
}

func TestHlwyCaptureHash(t *testing.T) {
	numbers := hlwyTestNumbers(10, 12)
	sha, err := HlwyCaptureHash(numbers, hlwyTestConditions(), 7)
	if err != nil {
		t.Fatalf("HlwyCaptureHash: %v", err)
	}
	if len(sha) != 64 {
		t.Fatalf("sha 长度 = %d", len(sha))
	}
	if _, err := HlwyCaptureHash(numbers, hlwyTestConditions(), 0); err == nil {
		t.Fatal("缺少来源结果 id 必须报错")
	}
	tampered := append([]int(nil), numbers...)
	tampered[0] = 355
	other, err := HlwyCaptureHash(tampered, hlwyTestConditions(), 7)
	if err != nil {
		t.Fatalf("HlwyCaptureHash tampered: %v", err)
	}
	if other == sha {
		t.Fatal("改动数字必须改变哈希")
	}
}

func TestHlwySnapshotOf(t *testing.T) {
	numbers := hlwyTestNumbers(50, 13)
	b := hlwyTestBaseline(t, "hlwy:s@"+strings.Repeat("a", 12), "s", numbers)
	snap := HlwySnapshotOf(b)
	if snap.ID != b.ID || snap.PayloadSHA256 != b.PayloadSHA256 {
		t.Fatalf("快照 id/sha 不对: id=%q sha=%q", snap.ID, snap.PayloadSHA256)
	}
	if len(snap.RawNumbers) != len(numbers) {
		t.Fatalf("快照应带完整原始数字: %d", len(snap.RawNumbers))
	}
	// 有效数就是 len(raw_numbers)，不再另存派生副本。
	if snap.Counts != b.Counts {
		t.Fatal("快照的 355 桶计数应与源基准一致")
	}
	if err := ValidateHlwyBaseline(snap); err != nil {
		t.Fatalf("快照必须自洽: %v", err)
	}

	// 深拷贝：改快照不能改到源基准，否则「不可变快照」只是一句话。
	snap.RawNumbers[0] = b.RawNumbers[0]%numberdist.DistLength + 1
	if b.RawNumbers[0] == snap.RawNumbers[0] {
		t.Fatal("改动快照污染了源基准的 raw_numbers")
	}
	if b.Conditions.MaxTokens == nil || snap.Conditions.MaxTokens == nil {
		t.Fatal("条件快照应保留 max_tokens")
	}
	*snap.Conditions.MaxTokens = 4096
	if *b.Conditions.MaxTokens == 4096 {
		t.Fatal("改动快照污染了源基准的条件快照")
	}
}

func TestValidateHlwyConditions(t *testing.T) {
	if err := ValidateHlwyConditions(hlwyTestConditions()); err != nil {
		t.Fatalf("完整快照应通过: %v", err)
	}
	// 真实采集到的条件可能大片未知：未知是要保留的证据，不是错。
	partial := numberdist.Conditions{
		TemperatureState: numberdist.StateUnknown,
		TopPState:        numberdist.StateUnknown,
		Completeness:     "partial",
		UnknownFields:    []string{"temperature", "thinking"},
	}
	if err := ValidateHlwyConditions(partial); err != nil {
		t.Fatalf("未知条件应允许: %v", err)
	}

	bad := map[string]func(c *numberdist.Conditions){
		"bad_temperature_state": func(c *numberdist.Conditions) {
			c.TemperatureState = "maybe"
		},
		"value_without_number": func(c *numberdist.Conditions) {
			c.TemperatureState = numberdist.StateValue
		},
		"number_without_value_state": func(c *numberdist.Conditions) {
			zero := 0.0
			c.Temperature = &zero
		},
		"nan_temperature": func(c *numberdist.Conditions) {
			nan := math.NaN()
			c.TemperatureState = numberdist.StateValue
			c.Temperature = &nan
		},
		"bad_completeness": func(c *numberdist.Conditions) { c.Completeness = "nearly" },
		"complete_with_unknown": func(c *numberdist.Conditions) {
			c.UnknownFields = []string{"temperature"}
		},
		"empty_unknown_field": func(c *numberdist.Conditions) {
			c.Completeness = "partial"
			c.UnknownFields = []string{"  "}
		},
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			c := hlwyTestConditions()
			mutate(&c)
			if err := ValidateHlwyConditions(c); err == nil {
				t.Fatal("条件快照写错时必须被拒绝")
			}
		})
	}
}
