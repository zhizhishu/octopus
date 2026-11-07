package op

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// hlwyOpImportBaseline 导入一份合成基准并返回它的完整快照。
func hlwyOpImportBaseline(t *testing.T, ctx context.Context, name string, numbers []int) numberdist.Baseline {
	t.Helper()
	saved, err := HlwyBaselineImport(ctx, hlwyOpLegacyJSON(t, name, numbers), time.Now().UTC())
	if err != nil {
		t.Fatalf("import %s: %v", name, err)
	}
	loaded, err := HlwyBaselineLoad(ctx, saved.Baseline.ID)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loaded
}

// hlwyOpOptionsFor 组一份冻结了给定候选的选项。
func hlwyOpOptionsFor(t *testing.T, chosenID string, candidates ...numberdist.Baseline) model.HlwyOptions {
	t.Helper()
	opts := model.HlwyOptions{
		Samples:    model.HlwyDefaultSamples,
		BaselineID: chosenID,
		ProtocolID: numberdist.ProtocolID,
		Candidates: candidates,
	}
	if err := opts.Validate(); err != nil {
		t.Fatalf("options: %v", err)
	}
	return opts
}

func TestHlwyRecompareSwitchesBaseline(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	a := hlwyOpImportBaseline(t, ctx, "base-a", hlwyOpNumbers(100, 31))
	b := hlwyOpImportBaseline(t, ctx, "base-b", hlwyOpNumbers(100, 32))

	numbers := hlwyOpNumbers(100, 33)
	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers: numbers, Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: a,
	})
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, a.ID, a)))
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, report))

	out, err := HlwyRecompare(ctx, res.ID, b.ID)
	if err != nil {
		t.Fatalf("HlwyRecompare: %v", err)
	}
	if out.Kind != model.AuditKindHlwy {
		t.Fatalf("kind = %q", out.Kind)
	}
	if out.Score != 0 || out.Verdict != model.HlwyVerdictUnknown {
		t.Fatalf("hlwy 报告不应带风险分: score=%d verdict=%q", out.Score, out.Verdict)
	}
	if out.BaselineComparison.ChosenBaselineID != b.ID {
		t.Fatalf("主对照 = %q, want %q", out.BaselineComparison.ChosenBaselineID, b.ID)
	}
	// 本地/导入类基准只留「本次选中的那一份」：它们是一次具体采样，
	// 把上一份和这一份同时排在一起，看着像在比两个目标，其实是在比两次采样。
	if len(out.BaselineComparison.Matches) != 1 {
		t.Fatalf("候选数 = %d, want 1", len(out.BaselineComparison.Matches))
	}
	if got := out.BaselineComparison.Matches[0].BaselineID; got != b.ID {
		t.Fatalf("候选 = %q, want %q", got, b.ID)
	}
	for i, m := range out.BaselineComparison.Matches {
		if m.Rank != i+1 {
			t.Fatalf("排名未重算: %+v", m)
		}
	}
	if len(out.BaselineComparison.BaselineSnapshots) != 1 {
		t.Fatalf("快照数 = %d, want 1", len(out.BaselineComparison.BaselineSnapshots))
	}
	if out.BaselineComparison.BaselineSnapshots[0].ID != b.ID {
		t.Fatalf("快照 = %q, want %q", out.BaselineComparison.BaselineSnapshots[0].ID, b.ID)
	}
	// 采样账目和原始数字来自原报告，不能被这次重算改写。
	if out.BaselineComparison.Counts != report.BaselineComparison.Counts {
		t.Fatalf("计数被改写: %+v vs %+v", out.BaselineComparison.Counts, report.BaselineComparison.Counts)
	}
	if len(out.BaselineComparison.RawNumbers) != len(numbers) {
		t.Fatalf("原始数字丢了: %d", len(out.BaselineComparison.RawNumbers))
	}
	if out.BaselineComparison.Execution != model.HlwyExecutionComplete {
		t.Fatalf("execution = %q", out.BaselineComparison.Execution)
	}
	// 采样侧条件全部观测到，而导入基准的条件是 unknown → 判 unknown（不是 matched）。
	if out.BaselineComparison.Comparability != model.HlwyComparabilityUnknown {
		t.Fatalf("comparability = %q, codes=%v",
			out.BaselineComparison.Comparability, out.BaselineComparison.ReasonCodes)
	}
	if out.BaselineComparison.SelfCompare {
		t.Fatal("导入件不构成自比")
	}

	// 离线重算不写库：落库的报告必须一字未改。
	var stored model.ModelAuditResult
	if err := db.GetDB().First(&stored, res.ID).Error; err != nil {
		t.Fatalf("reload result: %v", err)
	}
	if stored.Report != res.Report {
		t.Fatal("离线重算修改了已保存的报告（历史证据被覆盖）")
	}
}

func TestHlwyRecompareDefaultsToFrozenChosen(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	a := hlwyOpImportBaseline(t, ctx, "base-c", hlwyOpNumbers(100, 34))
	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers: hlwyOpNumbers(100, 35), Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: a,
	})
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, a.ID, a)))
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, report))

	out, err := HlwyRecompare(ctx, res.ID, "   ")
	if err != nil {
		t.Fatalf("HlwyRecompare: %v", err)
	}
	if out.BaselineComparison.ChosenBaselineID != a.ID {
		t.Fatalf("空 baseline_id 应回落到冻结的主对照: %q", out.BaselineComparison.ChosenBaselineID)
	}
	if len(out.BaselineComparison.Matches) != 1 {
		t.Fatalf("候选数 = %d, want 1", len(out.BaselineComparison.Matches))
	}
}

func TestHlwyRecompareSelfCompare(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpBaseline(t, "hlwy:self@"+strings.Repeat("e", 12), "self", hlwyOpNumbers(100, 36))
	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers: hlwyOpNumbers(100, 37), Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: base,
	})
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, base.ID, base)))
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, report))

	saved, err := HlwyBaselineFromResult(ctx, res.ID, "自采")
	if err != nil {
		t.Fatalf("HlwyBaselineFromResult: %v", err)
	}

	out, err := HlwyRecompare(ctx, res.ID, saved.Baseline.ID)
	if err != nil {
		t.Fatalf("HlwyRecompare: %v", err)
	}
	if !out.BaselineComparison.SelfCompare {
		t.Fatal("用本结果自采的基准对照自己，必须标记 self_compare")
	}
	if !strings.Contains(out.Plain.Summary, "自比") {
		t.Fatalf("自比必须在人话里点明: %q", out.Plain.Summary)
	}
	if out.BaselineComparison.Matches[0].Overall < 0.999 {
		t.Fatalf("自比应与自身几乎完全一致: %v", out.BaselineComparison.Matches[0].Overall)
	}
}

// bogusScore 是一个不可能由正常算法算出的相似度，用来检验重算是否真的重算。
const bogusScore = 0.123456789

func TestHlwyRecompareIgnoresStoredScores(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	a := hlwyOpImportBaseline(t, ctx, "base-d", hlwyOpNumbers(100, 38))

	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers: hlwyOpNumbers(100, 39), Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: a,
	})
	// 把报告里的分数换成一份"好看的"假数据：重算必须完全忽略它们。
	bogus := report
	// 注意仍要是一份自洽的报告（id 在快照里、排名连续），否则它压根存不进去，
	// 就测不到「读侧是否信任存着的分数」这件事了。
	bogus.BaselineComparison.Matches = []numberdist.ScoreResult{{
		BaselineID: a.ID,
		Overall:    bogusScore,
		Rank:       1,
	}}
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, a.ID, a)))
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, bogus))

	out, err := HlwyRecompare(ctx, res.ID, a.ID)
	if err != nil {
		t.Fatalf("HlwyRecompare: %v", err)
	}
	if len(out.BaselineComparison.Matches) != 1 {
		t.Fatalf("候选数 = %d, want 1", len(out.BaselineComparison.Matches))
	}
	if math.Abs(out.BaselineComparison.Matches[0].Overall-bogusScore) < 1e-9 {
		t.Fatal("复用了报告里存着的分数")
	}
	if out.BaselineComparison.Matches[0].Rank != 1 {
		t.Fatalf("排名 = %d, want 1", out.BaselineComparison.Matches[0].Rank)
	}
}

func TestHlwyRecompareRejects(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	a := hlwyOpImportBaseline(t, ctx, "base-e", hlwyOpNumbers(100, 40))
	numbers := hlwyOpNumbers(100, 41)
	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers: numbers, Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: a,
	})
	encoded := hlwyOpEncodeReport(t, report)

	t.Run("old_audit_job", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindProbe, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, encoded)
		if _, err := HlwyRecompare(ctx, res.ID, a.ID); err == nil {
			t.Fatal("普通检测任务的结果不能离线重算成 hlwy 对照")
		}
	})

	t.Run("missing_snapshot", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, a.ID, a)))
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, "")
		if _, err := HlwyRecompare(ctx, res.ID, a.ID); !errors.Is(err, model.ErrHlwyReportEmpty) {
			t.Fatalf("err = %v, want ErrHlwyReportEmpty", err)
		}
	})

	t.Run("still_running", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, a.ID, a)))
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultRunning, encoded)
		if _, err := HlwyRecompare(ctx, res.ID, a.ID); err == nil {
			t.Fatal("还在跑的结果没有完整快照，不能重算")
		}
	})

	t.Run("unknown_baseline", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, a.ID, a)))
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, encoded)
		_, err := HlwyRecompare(ctx, res.ID, "hlwy:nope@"+strings.Repeat("a", 12))
		if !errors.Is(err, ErrHlwyBaselineNotFound) {
			t.Fatalf("err = %v, want ErrHlwyBaselineNotFound", err)
		}
	})

	t.Run("candidate_cap", func(t *testing.T) {
		// 冻结的内置类候选已满 8 份时，再显式指定一份不在其中的基准 → 报错而不是悄悄截断。
		// 只有内置类快照会跨轮保留，所以这里造 8 份 builtin 来源的冻结快照。
		cands := make([]numberdist.Baseline, 0, model.HlwyMaxCandidates)
		for i := 0; i < model.HlwyMaxCandidates; i++ {
			b := hlwyOpBaseline(t,
				"hlwy:c"+strings.Repeat("a", i)+"@"+strings.Repeat("b", 12),
				"c", hlwyOpNumbers(100, 50+i))
			b.Source.Kind = numberdist.SourceBuiltin
			cands = append(cands, b)
		}
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, cands[0].ID, cands...)))
		full := hlwyOpReport(t, hlwyReportSpec{
			Numbers: numbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: cands[0],
		})
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, full))
		other := hlwyOpImportBaseline(t, ctx, "base-f", hlwyOpNumbers(100, 42))
		if _, err := HlwyRecompare(ctx, res.ID, other.ID); err == nil {
			t.Fatal("超过候选上限必须报错（不能悄悄少排一份）")
		}
	})

	t.Run("bad_result_id", func(t *testing.T) {
		if _, err := HlwyRecompare(ctx, 0, a.ID); err == nil {
			t.Fatal("缺少结果 id 必须报错")
		}
	})
}

// ptrOptions 把选项转成指针（nil 表示"这条任务没有 hlwy 选项"）。
func ptrOptions(o model.HlwyOptions) *model.HlwyOptions { return &o }

func TestHlwyRecompareSwitchesLocalBaseline(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpBaseline(t, "hlwy:rot@"+strings.Repeat("a", 12), "rot", hlwyOpNumbers(100, 60))

	// 第一轮：自采成基准 A。
	first := hlwyOpReport(t, hlwyReportSpec{
		Numbers: hlwyOpNumbers(100, 61), Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: base,
	})
	jobA := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, base.ID, base)))
	resA := hlwyOpSeedResult(t, jobA.ID, model.AuditResultDone, hlwyOpEncodeReport(t, first))
	savedA, err := HlwyBaselineFromResult(ctx, resA.ID, "A")
	if err != nil {
		t.Fatalf("capture A: %v", err)
	}

	// 第二轮：另一批数字，也自采成基准 B。
	second := hlwyOpReport(t, hlwyReportSpec{
		Numbers: hlwyOpNumbers(100, 62), Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: base,
	})
	jobB := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, base.ID, base)))
	resB := hlwyOpSeedResult(t, jobB.ID, model.AuditResultDone, hlwyOpEncodeReport(t, second))
	savedB, err := HlwyBaselineFromResult(ctx, resB.ID, "B")
	if err != nil {
		t.Fatalf("capture B: %v", err)
	}

	// 用第一轮的结果换到 B：候选里只应有 B，A 不能跟着留下。
	out, err := HlwyRecompare(ctx, resA.ID, savedB.Baseline.ID)
	if err != nil {
		t.Fatalf("HlwyRecompare: %v", err)
	}
	if len(out.BaselineComparison.Matches) != 1 {
		t.Fatalf("候选数 = %d, want 1（上一次选中的本地基准不应继续参与）",
			len(out.BaselineComparison.Matches))
	}
	if got := out.BaselineComparison.Matches[0].BaselineID; got != savedB.Baseline.ID {
		t.Fatalf("候选 = %q, want %q", got, savedB.Baseline.ID)
	}
	if out.BaselineComparison.SelfCompare {
		t.Fatal("B 是另一条结果的自采，不构成本结果自比")
	}
	if out.BaselineComparison.ChosenBaselineID != savedB.Baseline.ID {
		t.Fatalf("主对照 = %q", out.BaselineComparison.ChosenBaselineID)
	}
	if savedA.Baseline.ID == savedB.Baseline.ID {
		t.Fatal("两批不同的数字必须得到两份不同的基准")
	}
}

func TestHlwyRecompareIncompatibleProtocol(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	// 条件里写着另一套协议：这份基准与本轮根本不是同一个实验。
	alien := hlwyOpBaseline(t, "hlwy:alien@"+strings.Repeat("a", 12), "alien", hlwyOpNumbers(100, 63))
	alien.Conditions.ProtocolID = "hlwy-number999-v1"

	numbered := hlwyOpNumbers(100, 64)
	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers: numbered, Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: alien,
	})
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, alien.ID, alien)))
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, report))

	out, err := HlwyRecompare(ctx, res.ID, alien.ID)
	if err != nil {
		t.Fatalf("HlwyRecompare: %v", err)
	}
	if out.BaselineComparison.Comparability != model.HlwyComparabilityIncompatible {
		t.Fatalf("comparability = %q", out.BaselineComparison.Comparability)
	}
	// 样本再多也不给分：一份「不兼容但排名第一」的表格一定会被当成结论看。
	if out.BaselineComparison.Scorable {
		t.Fatal("不可对照的基准不应可评分")
	}
	if len(out.BaselineComparison.Matches) != 0 {
		t.Fatalf("不可评分却带 %d 条候选分数", len(out.BaselineComparison.Matches))
	}
	if out.BaselineComparison.Reason != model.HlwyReasonIncompatibleProtocol {
		t.Fatalf("reason = %q, want %q",
			out.BaselineComparison.Reason, model.HlwyReasonIncompatibleProtocol)
	}
	// 分布与统计仍然保留：它们是这批数字自身的事实，与能否对照无关。
	if out.BaselineComparison.ValidCount != len(numbered) {
		t.Fatalf("valid = %d", out.BaselineComparison.ValidCount)
	}
	var sum float64
	for _, v := range out.BaselineComparison.Distribution {
		sum += v
	}
	if sum < 0.999 {
		t.Fatalf("分布丢了: sum=%v", sum)
	}
	if !strings.Contains(out.Plain.Summary, "不给出相似度分数") {
		t.Fatalf("人话里必须点明不给分: %q", out.Plain.Summary)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("重算结果必须自洽: %v", err)
	}
}

func TestHlwyRecompareKeepsFrozenPayload(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	catalog := hlwyOpImportBaseline(t, ctx, "base-frozen", hlwyOpNumbers(100, 65))

	// 同 id 但内容不同的冻结快照：模拟库里那一行事后被动过。
	frozen := catalog
	frozenNums := hlwyOpNumbers(100, 66)
	counts, err := model.HlwyBinCounts(frozenNums)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	frozen.RawNumbers = frozenNums
	frozen.Counts = counts
	frozen.Stats = numberdist.StatsOf(frozenNums)

	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers: hlwyOpNumbers(100, 67), Planned: model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete, Chosen: frozen,
	})
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, frozen.ID, frozen)))
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, report))

	out, err := HlwyRecompare(ctx, res.ID, frozen.ID)
	if err != nil {
		t.Fatalf("HlwyRecompare: %v", err)
	}
	if len(out.BaselineComparison.BaselineSnapshots) != 1 {
		t.Fatalf("快照数 = %d, want 1", len(out.BaselineComparison.BaselineSnapshots))
	}
	// 「当时是拿哪一份比的」必须可复现：用冻结内容，而不是今天库里的那一行。
	got := out.BaselineComparison.BaselineSnapshots[0].RawNumbers
	if len(got) != len(frozenNums) || got[0] != frozenNums[0] || got[len(got)-1] != frozenNums[len(frozenNums)-1] {
		t.Fatalf("重算用了库里的新内容而不是冻结快照")
	}
}
