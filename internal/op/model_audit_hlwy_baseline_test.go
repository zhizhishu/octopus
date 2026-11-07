package op

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// ---- 共用测试夹具（op 包内 hlwy 相关用例共享） ----

// hlwyOpNumbers 生成确定性数字序列。
func hlwyOpNumbers(n, seed int) []int {
	out := make([]int, n)
	x := uint32(seed)*2654435761 + 1
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = int(x%numberdist.DistLength) + 1
	}
	return out
}

// hlwyOpBoolPtr / hlwyOpIntPtr 造「已观测」的可空字段。
func hlwyOpBoolPtr(b bool) *bool { return &b }
func hlwyOpIntPtr(i int) *int    { return &i }

// hlwyOpConditions 是「全部观测到」的条件快照。
func hlwyOpConditions() numberdist.Conditions {
	return numberdist.Conditions{
		Protocol:         "openai-chat",
		ProtocolID:       numberdist.ProtocolID,
		ParserVersion:    numberdist.ParserVersion,
		PromptSHA256:     model.HlwyPromptSHA256(),
		TransportProfile: numberdist.TransportProfile,
		TemperatureState: numberdist.StateAbsent,
		TopPState:        numberdist.StateAbsent,
		Stream:           hlwyOpBoolPtr(true),
		MaxTokenField:    "max_tokens",
		MaxTokens:        hlwyOpIntPtr(1024),
		Thinking:         "disabled",
		ReasoningEffort:  "none",
		SystemInjected:   hlwyOpBoolPtr(false),
		ToolsInjected:    hlwyOpBoolPtr(false),
		Completeness:     "complete",
	}
}

// hlwyOpBaseline 组一份自洽基准（计数/统计由数字重算）。
func hlwyOpBaseline(t *testing.T, id, label string, numbers []int) numberdist.Baseline {
	t.Helper()
	counts, err := model.HlwyBinCounts(numbers)
	if err != nil {
		t.Fatalf("HlwyBinCounts: %v", err)
	}
	return numberdist.Baseline{
		SchemaVersion: numberdist.SchemaVersion,
		ID:            id,
		Label:         label,
		PayloadSHA256: strings.Repeat("a", 64),
		Source:        numberdist.Source{Kind: numberdist.SourceImported, Repo: "hlwy"},
		Conditions:    hlwyOpConditions(),
		RawNumbers:    numbers,
		Counts:        counts,
		Stats:         numberdist.StatsOf(numbers),
	}
}

// hlwyOpLegacyJSON 造一份上游旧格式基准文件（对象形态；单元素数组也接受）。
func hlwyOpLegacyJSON(t *testing.T, name string, numbers []int) []byte {
	t.Helper()
	if len(numbers) == 0 {
		t.Fatal("需要至少一个数字")
	}
	counts := make(map[int]int, 8)
	for _, n := range numbers {
		counts[n]++
	}
	bins := make([]string, numberdist.DistLength)
	for i := range bins {
		bins[i] = "0"
	}
	for n, c := range counts {
		bins[n-1] = strconv.FormatFloat(float64(c)/float64(len(numbers)), 'g', 17, 64)
	}
	raw, err := json.Marshal(numbers)
	if err != nil {
		t.Fatalf("marshal numbers: %v", err)
	}
	return []byte(fmt.Sprintf(
		`{"name":%q,"model":%q,"iterations":%d,"rawData":%s,"distribution":[%s]}`,
		name, name, len(numbers), raw, strings.Join(bins, ",")))
}

// hlwyReportSpec 描述一份报告的采样账目。started 由各项推出，保证恒等式成立。
type hlwyReportSpec struct {
	Numbers    []int
	Planned    int
	Invalid    int
	Failed     int
	Unresolved int
	Execution  string
	Conditions numberdist.Conditions
	Chosen     numberdist.Baseline
}

// hlwyOpReport 按 spec 组一份自洽报告（分布/统计都由 Numbers 重算）。
func hlwyOpReport(t *testing.T, spec hlwyReportSpec) model.HlwyAuditReport {
	t.Helper()
	if spec.Conditions.Completeness == "" {
		spec.Conditions = hlwyOpConditions()
	}
	cmp, err := numberdist.Compare(spec.Numbers, []numberdist.Baseline{spec.Chosen}, spec.Chosen.ID)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	started := len(spec.Numbers) + spec.Invalid + spec.Failed + spec.Unresolved
	bc := model.HlwyBaselineComparison{
		SchemaVersion:    numberdist.SchemaVersion,
		AlgorithmVersion: numberdist.AlgorithmVer,
		Execution:        spec.Execution,
		Sufficiency:      model.HlwySufficiency(len(spec.Numbers)),
		Comparability:    model.HlwyComparabilityUnknown,
		ReasonCodes:      []string{},
		Counts: model.HlwyCounts{
			PlannedCalls: spec.Planned,
			StartedSlots: started,
			Valid:        len(spec.Numbers),
			Invalid:      spec.Invalid,
			Failed:       spec.Failed,
			Unresolved:   spec.Unresolved,
		},
		RawNumbers:        spec.Numbers,
		Conditions:        spec.Conditions,
		BaselineSnapshots: []numberdist.Baseline{model.HlwySnapshotOf(spec.Chosen)},
		Scorable:          model.HlwyScorable(len(spec.Numbers), model.HlwyComparabilityUnknown),
		Reason:            cmp.Reason,
		ValidCount:        cmp.ValidCount,
		Distribution:      cmp.Distribution,
		Stats:             cmp.Stats,
		ChosenBaselineID:  cmp.ChosenBaselineID,
		Matches:           cmp.Matches,
		Quality:           model.HlwyQuality{StrictInteger: len(spec.Numbers)},
	}
	return model.HlwyAuditReport{
		Kind:               model.AuditKindHlwy,
		RequestedModel:     "test-model",
		UpstreamModel:      "test-model-upstream",
		Verdict:            model.HlwyVerdictUnknown,
		Plain:              model.HlwyPlainOf(bc, false),
		BaselineComparison: bc,
	}.Filled()
}

// hlwyOpSeedJob 直接落一条任务行（不走入队路径：这里验的是读侧不变量）。
func hlwyOpSeedJob(t *testing.T, kind string, opts *model.HlwyOptions) model.ModelAuditJob {
	t.Helper()
	job := model.ModelAuditJob{
		ChannelID:   1,
		ChannelName: "hlwy-test",
		Status:      model.AuditJobDone,
		Total:       1,
		Finished:    1,
		Kind:        kind,
		Trigger:     "manual",
		CreatedAt:   time.Now().UTC(),
	}
	if opts != nil {
		encoded, err := opts.Encode()
		if err != nil {
			t.Fatalf("encode options: %v", err)
		}
		job.OptionsJSON = encoded
	}
	if err := db.GetDB().Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	return job
}

// hlwyOpSeedResult 直接落一条结果行。
func hlwyOpSeedResult(t *testing.T, jobID int64, status, report string) model.ModelAuditResult {
	t.Helper()
	now := time.Now().UTC()
	res := model.ModelAuditResult{
		JobID:         jobID,
		ChannelID:     1,
		ChannelName:   "hlwy-test",
		Model:         "test-model",
		UpstreamModel: "test-model-upstream",
		Status:        status,
		Report:        report,
		Attempt:       1,
		CreatedAt:     now,
		FinishedAt:    &now,
	}
	if err := db.GetDB().Create(&res).Error; err != nil {
		t.Fatalf("create result: %v", err)
	}
	return res
}

// hlwyOpEncodeReport 把报告编码成落库字符串。
func hlwyOpEncodeReport(t *testing.T, report model.HlwyAuditReport) string {
	t.Helper()
	encoded, err := report.Encode()
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	return encoded
}

// ---- 基准目录 ----

func TestHlwyBaselineListIncludesBuiltins(t *testing.T) {
	ctx := setupAuditQueueTest(t)

	entries, err := HlwyBaselineList(ctx)
	if err != nil {
		t.Fatalf("HlwyBaselineList: %v", err)
	}
	if len(entries) != 7 {
		t.Fatalf("空库时目录应只有 7 份内置基准，实际 %d 份", len(entries))
	}
	for _, e := range entries {
		if !e.Builtin {
			t.Fatalf("%s 应标记为内置", e.ID)
		}
		if e.SourceKind != string(numberdist.SourceBuiltin) {
			t.Fatalf("%s 来源 = %q", e.ID, e.SourceKind)
		}
		if e.ValidCount <= 0 || e.PayloadSHA256 == "" || e.Label == "" {
			t.Fatalf("%s 目录项不完整: %+v", e.ID, e)
		}
		if e.SourceResultID != 0 {
			t.Fatalf("%s 内置件不应有来源结果 id", e.ID)
		}
	}
	// 目录项刻意不带任何「与某目标有多像」的字段：没有采样目标时算不出相似度，
	// 预先标一个「可比/不可比」只会误导。这一点由 HlwyBaselineEntry 的类型本身保证
	// （它没有任何分数/排名/可对照性字段），所以这里不需要再断言一遍。
}

func TestHlwyBaselineImportIdempotent(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	raw := hlwyOpLegacyJSON(t, "local-a", hlwyOpNumbers(100, 21))

	first, err := HlwyBaselineImport(ctx, raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if first.Existed {
		t.Fatal("首次导入不应标记为已存在")
	}
	if first.Baseline.Builtin {
		t.Fatal("导入件不是内置件")
	}
	if first.Baseline.SourceKind != string(numberdist.SourceImported) {
		t.Fatalf("来源 = %q", first.Baseline.SourceKind)
	}
	if first.Baseline.ValidCount != 100 {
		t.Fatalf("有效数 = %d", first.Baseline.ValidCount)
	}

	second, err := HlwyBaselineImport(ctx, raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if !second.Existed {
		t.Fatal("同内容重复导入必须命中已有行")
	}
	if second.Baseline.ID != first.Baseline.ID {
		t.Fatalf("id 漂移: %q vs %q", second.Baseline.ID, first.Baseline.ID)
	}

	var rows int64
	if err := db.GetDB().Model(&model.ModelAuditHlwyBaseline{}).Count(&rows).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("重复导入不应产生第二行，实际 %d 行", rows)
	}

	entries, err := HlwyBaselineList(ctx)
	if err != nil {
		t.Fatalf("HlwyBaselineList: %v", err)
	}
	if len(entries) != 8 {
		t.Fatalf("目录应有 7 内置 + 1 本地，实际 %d", len(entries))
	}
	if entries[7].Builtin {
		t.Fatal("本地件排在后面且不应标记为内置")
	}

	loaded, err := HlwyBaselineLoad(ctx, first.Baseline.ID)
	if err != nil {
		t.Fatalf("HlwyBaselineLoad: %v", err)
	}
	if len(loaded.RawNumbers) != 100 || loaded.PayloadSHA256 != first.Baseline.PayloadSHA256 {
		t.Fatalf("载入内容不一致: %+v", loaded)
	}
}

func TestHlwyBaselineImportRejects(t *testing.T) {
	ctx := setupAuditQueueTest(t)

	t.Run("empty", func(t *testing.T) {
		if _, err := HlwyBaselineImport(ctx, nil, time.Now().UTC()); err == nil {
			t.Fatal("空内容必须报错")
		}
	})

	t.Run("out_of_range_digit", func(t *testing.T) {
		// 356 不能进最后一桶：坏数据既不修也不丢。
		bad := []byte(`{"name":"x","rawData":[356],"distribution":[]}`)
		if _, err := HlwyBaselineImport(ctx, bad, time.Now().UTC()); err == nil {
			t.Fatal("越界数字必须报错")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		huge := make([]byte, numberdist.DefaultMaxImportBytes+16)
		for i := range huge {
			huge[i] = ' '
		}
		if _, err := HlwyBaselineImport(ctx, huge, time.Now().UTC()); err == nil {
			t.Fatal("超过体积上限必须报错")
		}
	})

	var rows int64
	if err := db.GetDB().Model(&model.ModelAuditHlwyBaseline{}).Count(&rows).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("被拒绝的导入不应留下任何行，实际 %d 行", rows)
	}
}

func TestHlwyBaselineResolveMiss(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	if _, err := HlwyBaselineLoad(ctx, "hlwy:nope@"+strings.Repeat("a", 12)); !errors.Is(err, ErrHlwyBaselineNotFound) {
		t.Fatalf("err = %v, want ErrHlwyBaselineNotFound", err)
	}
	if _, err := HlwyBaselineLoad(ctx, ""); err == nil {
		t.Fatal("空 id 必须报错")
	}
	if _, err := HlwyBaselineLoad(ctx, strings.Repeat("x", model.HlwyMaxBaselineIDBytes+1)); err == nil {
		t.Fatal("超长 id 必须报错")
	}
}

func TestHlwyBaselineCorruptionFailsClosed(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	baseline := hlwyOpBaseline(t, "hlwy:seed@"+strings.Repeat("c", 12), "seed", hlwyOpNumbers(100, 22))
	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers:   hlwyOpNumbers(100, 23),
		Planned:   model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete,
		Chosen:    baseline,
	})
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, hlwyOpEncodeReport(t, report))

	saved, err := HlwyBaselineFromResult(ctx, res.ID, "自采")
	if err != nil {
		t.Fatalf("HlwyBaselineFromResult: %v", err)
	}
	capturedID := saved.Baseline.ID

	t.Run("capture_tamper_detected", func(t *testing.T) {
		var row model.ModelAuditHlwyBaseline
		if err := db.GetDB().Where("id = ?", capturedID).First(&row).Error; err != nil {
			t.Fatalf("load row: %v", err)
		}
		var b numberdist.Baseline
		if err := json.Unmarshal([]byte(row.ContentJSON), &b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// 改动一个数字并把计数/统计重算好：结构自洽，但规范化哈希对不上。
		b.RawNumbers[0] = b.RawNumbers[0]%numberdist.DistLength + 1
		counts, err := model.HlwyBinCounts(b.RawNumbers)
		if err != nil {
			t.Fatalf("counts: %v", err)
		}
		b.Counts = counts
		b.Stats = numberdist.StatsOf(b.RawNumbers)
		content, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := db.GetDB().Model(&model.ModelAuditHlwyBaseline{}).
			Where("id = ?", capturedID).Update("content_json", string(content)).Error; err != nil {
			t.Fatalf("update: %v", err)
		}

		if _, err := HlwyBaselineLoad(ctx, capturedID); err == nil {
			t.Fatal("自采基准被改动后必须读取失败（不能算出分数再让人去猜）")
		}
		// 目录不能因为一行坏了就整表不可读；但那一行必须显式标成不可用，
		// 而不是静默消失或带着坏数据继续展示。
		entries, err := HlwyBaselineList(ctx)
		if err != nil {
			t.Fatalf("单行损坏不应让整个目录报错: %v", err)
		}
		var found *HlwyBaselineEntry
		for i := range entries {
			if entries[i].ID == capturedID {
				found = &entries[i]
			}
		}
		if found == nil {
			t.Fatal("损坏的行必须仍然出现在目录里（可见但不可用）")
		}
		if !found.Unavailable || found.ReasonCode != HlwyEntryReasonStoredInvalid {
			t.Fatalf("坏行应标 unavailable/%s，实际 %+v", HlwyEntryReasonStoredInvalid, *found)
		}
		if found.Builtin {
			t.Fatal("自采件不是内置件")
		}
		for _, e := range entries {
			if e.ID != capturedID && e.Unavailable {
				t.Fatalf("%s 不应被一起标成不可用", e.ID)
			}
		}
	})
}

func TestHlwyBaselineFromResultGates(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	baseline := hlwyOpBaseline(t, "hlwy:gate@"+strings.Repeat("d", 12), "gate", hlwyOpNumbers(100, 24))
	goodNumbers := hlwyOpNumbers(100, 25)

	build := func(t *testing.T, spec hlwyReportSpec) string {
		t.Helper()
		return hlwyOpEncodeReport(t, hlwyOpReport(t, spec))
	}

	t.Run("ok", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline,
		}))
		saved, err := HlwyBaselineFromResult(ctx, res.ID, "")
		if err != nil {
			t.Fatalf("应可保存: %v", err)
		}
		if !strings.HasPrefix(saved.Baseline.Label, "test-model") {
			t.Fatalf("空 label 应回落到结果型号: %q", saved.Baseline.Label)
		}
		if saved.Baseline.SourceResultID != res.ID {
			t.Fatalf("来源结果 id = %d, want %d", saved.Baseline.SourceResultID, res.ID)
		}

		// 同一条结果再存一次：幂等命中，不产生第二行。
		again, err := HlwyBaselineFromResult(ctx, res.ID, "")
		if err != nil {
			t.Fatalf("再次保存: %v", err)
		}
		if !again.Existed || again.Baseline.ID != saved.Baseline.ID {
			t.Fatalf("同一条结果必须幂等: existed=%v id=%q", again.Existed, again.Baseline.ID)
		}
	})

	reject := map[string]hlwyReportSpec{
		"canceled_execution": {
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionCanceled, Chosen: baseline,
		},
		"unresolved_slot": {
			Numbers: goodNumbers, Planned: model.HlwyMaxSamples, Unresolved: 1,
			Execution: model.HlwyExecutionComplete, Chosen: baseline,
		},
		"slots_not_consumed": {
			Numbers: goodNumbers, Planned: model.HlwyMaxSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline,
		},
		"too_few_valid": {
			Numbers: hlwyOpNumbers(50, 26), Planned: model.HlwyDefaultSamples, Invalid: 50,
			Execution: model.HlwyExecutionComplete, Chosen: baseline,
		},
	}
	for name, spec := range reject {
		t.Run(name, func(t *testing.T) {
			job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
			res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, spec))
			if _, err := HlwyBaselineFromResult(ctx, res.ID, "x"); err == nil {
				t.Fatal("不满足门槛的结果不能存为基准")
			}
		})
	}

	t.Run("partial_conditions_saved_faithfully", func(t *testing.T) {
		// 真实本地采样可能大片参数未观测。那是要原样保留的证据，
		// 不能因为“不完整”就拒存，更不允许事后补值。
		cond := hlwyOpConditions()
		cond.Completeness = "partial"
		cond.UnknownFields = []string{"thinking"}
		cond.Thinking = numberdist.StateUnknown
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline, Conditions: cond,
		}))
		saved, err := HlwyBaselineFromResult(ctx, res.ID, "部分条件")
		if err != nil {
			t.Fatalf("条件不完整但真实，应该可以存: %v", err)
		}
		if saved.Baseline.Conditions.Completeness != "partial" {
			t.Fatalf("完整性被改写了: %q", saved.Baseline.Conditions.Completeness)
		}
		if saved.Baseline.Conditions.Thinking != numberdist.StateUnknown {
			t.Fatalf("未观测的 thinking 被填成了 %q", saved.Baseline.Conditions.Thinking)
		}
		// 未知条件绝不等于“可对照”：与同一份条件比也只能是 unknown。
		state, codes := model.HlwyComparability(cond, saved.Baseline.Conditions)
		if state != model.HlwyComparabilityUnknown {
			t.Fatalf("state = %q codes=%v", state, codes)
		}
	})

	t.Run("parser_mismatch", func(t *testing.T) {
		cond := hlwyOpConditions()
		cond.ParserVersion = "hlwy-extract-v2"
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline, Conditions: cond,
		}))
		if _, err := HlwyBaselineFromResult(ctx, res.ID, "x"); err == nil {
			t.Fatal("解析版本不一致不能存为基准")
		}
	})

	t.Run("prompt_mismatch", func(t *testing.T) {
		cond := hlwyOpConditions()
		cond.PromptSHA256 = strings.Repeat("e", 64)
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline, Conditions: cond,
		}))
		if _, err := HlwyBaselineFromResult(ctx, res.ID, "x"); err == nil {
			t.Fatal("另一道题跑出来的数字不能存为本基准")
		}
	})

	t.Run("protocol_mismatch", func(t *testing.T) {
		cond := hlwyOpConditions()
		cond.ProtocolID = "hlwy-number999-v1"
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline, Conditions: cond,
		}))
		if _, err := HlwyBaselineFromResult(ctx, res.ID, "x"); err == nil {
			t.Fatal("协议不一致不能存为基准")
		}
	})

	t.Run("result_not_done", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultFailed, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline,
		}))
		if _, err := HlwyBaselineFromResult(ctx, res.ID, "x"); err == nil {
			t.Fatal("失败的检测结果不能存为基准")
		}
	})

	t.Run("old_audit_job", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindProbe, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline,
		}))
		if _, err := HlwyBaselineFromResult(ctx, res.ID, "x"); err == nil {
			t.Fatal("普通检测任务的结果不能存成 hlwy 基准")
		}
	})

	t.Run("missing_report", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, "")
		if _, err := HlwyBaselineFromResult(ctx, res.ID, "x"); !errors.Is(err, model.ErrHlwyReportEmpty) {
			t.Fatalf("err = %v, want ErrHlwyReportEmpty", err)
		}
	})

	t.Run("label_too_long", func(t *testing.T) {
		job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
		res := hlwyOpSeedResult(t, job.ID, model.AuditResultDone, build(t, hlwyReportSpec{
			Numbers: goodNumbers, Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: baseline,
		}))
		if _, err := HlwyBaselineFromResult(ctx, res.ID, strings.Repeat("长", model.HlwyMaxLabelBytes)); err == nil {
			t.Fatal("超长 label 必须被拒绝")
		}
	})
}

// hlwyOpSeedResultNoFinish 落一条没有结束时间的结果行（模拟存量/异常路径）。
func hlwyOpSeedResultNoFinish(t *testing.T, jobID int64, report string) model.ModelAuditResult {
	t.Helper()
	res := model.ModelAuditResult{
		JobID:         jobID,
		ChannelID:     1,
		ChannelName:   "hlwy-test",
		Model:         "test-model",
		UpstreamModel: "test-model-upstream",
		Status:        model.AuditResultDone,
		Report:        report,
		Attempt:       1,
		CreatedAt:     time.Now().UTC(),
	}
	if err := db.GetDB().Create(&res).Error; err != nil {
		t.Fatalf("create result: %v", err)
	}
	return res
}

func TestHlwyBaselineCaptureKeepsUnknownCollectedAt(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	baseline := hlwyOpBaseline(t, "hlwy:notime@"+strings.Repeat("a", 12), "notime", hlwyOpNumbers(100, 31))
	report := hlwyOpReport(t, hlwyReportSpec{
		Numbers:   hlwyOpNumbers(100, 32),
		Planned:   model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete,
		Chosen:    baseline,
	})
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, nil)
	res := hlwyOpSeedResultNoFinish(t, job.ID, hlwyOpEncodeReport(t, report))

	saved, err := HlwyBaselineFromResult(ctx, res.ID, "无结束时间")
	if err != nil {
		t.Fatalf("HlwyBaselineFromResult: %v", err)
	}
	// 采集时间只认结果真正的结束时刻；没有就留空。
	// 拿 CreatedAt（甚至今天）顶上，会让一份旧基准看起来像新的。
	if saved.Baseline.CollectedAt != nil {
		t.Fatalf("没有结束时间时 collected_at 必须为空，实际 %v", saved.Baseline.CollectedAt)
	}
}
