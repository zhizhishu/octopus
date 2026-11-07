package op

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// 本文件只验收**执行侧**的记账不变量：谁有权写、什么时候必须停、停下来的
// 那一份报告还自不自洽。它不碰网络（一条上游请求都不发）。

// hlwyQueueEnable 打开 hlwy 入队开关。
func hlwyQueueEnable(t *testing.T) {
	t.Helper()
	if err := SettingSetString(model.SettingKeyModelAuditHlwyEnabled, "true"); err != nil {
		t.Fatalf("enable hlwy setting: %v", err)
	}
}

// hlwyQueueBuiltinID 取一份内置基准的 id（内置件不入库，按 id 可解析）。
func hlwyQueueBuiltinID(t *testing.T) string {
	t.Helper()
	builtins, err := numberdist.LoadBuiltins()
	if err != nil || len(builtins) == 0 {
		t.Fatalf("load builtins: %v (n=%d)", err, len(builtins))
	}
	return builtins[0].ID
}

// hlwyQueueEnqueueOne 走真实入队路径建一条 hlwy 任务。
func hlwyQueueEnqueueOne(t *testing.T, ctx context.Context, ch *model.Channel, upstream string, samples int) int64 {
	t.Helper()
	jobID, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{upstream},
		Kind:      model.AuditKindHlwy,
		Hlwy: &HlwyEnqueueInput{
			Samples:    samples,
			BaselineID: hlwyQueueBuiltinID(t),
			ProtocolID: numberdist.ProtocolID,
		},
	})
	if err != nil {
		t.Fatalf("enqueue hlwy: %v", err)
	}
	return jobID
}

// hlwyQueuePending 直接落一条可领取的 hlwy 结果行。
func hlwyQueuePending(t *testing.T, opts model.HlwyOptions) (model.ModelAuditResult, model.ModelAuditJob) {
	t.Helper()
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(opts))
	res := hlwyOpSeedResult(t, job.ID, model.AuditResultPending, "")
	return res, job
}

// hlwyQueueSeedRunning 落一条**正在运行且持有租约**的结果行，并给出对应的 claim。
//
// 用它来验收尾：一条真正跑满 100 格的记录要 100 次事务，而收尾关心的是
// 「账目与终态对不对得上」，不需要真的一次次写。
func hlwyQueueSeedRunning(t *testing.T, opts model.HlwyOptions, rep model.HlwyAuditReport) (model.ModelAuditResult, HlwyClaim) {
	t.Helper()
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(opts))
	token := hlwyOpEncodeReport(t, rep)
	now := time.Now().UTC()
	lease := now.Add(2 * time.Minute)
	res := model.ModelAuditResult{
		JobID:         job.ID,
		ChannelID:     1,
		ChannelName:   "hlwy-test",
		Model:         "test-model",
		UpstreamModel: "test-model-upstream",
		Status:        model.AuditResultRunning,
		Report:        token,
		Attempt:       1,
		NextRetryAt:   &lease,
		CreatedAt:     now,
	}
	if err := db.GetDB().Create(&res).Error; err != nil {
		t.Fatalf("create running result: %v", err)
	}
	return res, HlwyClaim{
		ResultID:   res.ID,
		JobID:      job.ID,
		Attempt:    1,
		Options:    opts,
		LeaseUntil: lease,
		StartedAt:  now,
		Token:      token,
	}
}

// hlwyQueueReload 读回结果行与它的报告。
func hlwyQueueReload(t *testing.T, ctx context.Context, resultID int64) (model.ModelAuditResult, model.HlwyAuditReport) {
	t.Helper()
	var res model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", resultID).First(&res).Error; err != nil {
		t.Fatalf("reload result %d: %v", resultID, err)
	}
	rep, err := model.ParseHlwyReport(res.Report)
	if err != nil {
		t.Fatalf("parse report %d: %v", resultID, err)
	}
	return res, rep
}

// TestHlwyRunTimeoutFromSamples 守住总时长公式：它决定一次对照最多占用 worker 多久。
func TestHlwyRunTimeoutFromSamples(t *testing.T) {
	cases := []struct {
		samples int
		want    int
	}{
		{model.HlwyDefaultSamples, 1300}, // 300 + 10*100
		{model.HlwyMaxSamples, 1800},     // 300 + 10*200 = 2300 → 封顶 1800
		{0, 1300},                        // 非法值退回默认次数，不是 300
		{-5, 1300},
		{50, 800},
	}
	for _, c := range cases {
		if got := HlwyRunTimeout(c.samples); got != c.want {
			t.Fatalf("HlwyRunTimeout(%d) = %d, want %d", c.samples, got, c.want)
		}
	}
	// 单格超时与总时长必须是同一个量级里的两个数：单格 60 秒 × 100 格远大于
	// 300 秒，否则「一格慢」会直接把整轮拖成超时。
	if HlwySampleTimeout != 60*time.Second {
		t.Fatalf("单格超时 = %v, want 60s", HlwySampleTimeout)
	}
}

// TestHlwyEnqueueDispatchRejectsMixedParams 守住「两套参数不许混用」。
func TestHlwyEnqueueDispatchRejectsMixedParams(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	hlwyQueueEnable(t)
	ch := ensureAuditChannel(t, "hlwy-dispatch")
	base := hlwyQueueBuiltinID(t)

	good := func() ModelAuditEnqueueInput {
		return ModelAuditEnqueueInput{
			ChannelID: ch.ID,
			Models:    []string{"m1"},
			Kind:      model.AuditKindHlwy,
			Hlwy: &HlwyEnqueueInput{
				Samples:    model.HlwyDefaultSamples,
				BaselineID: base,
				ProtocolID: numberdist.ProtocolID,
			},
		}
	}

	cases := []struct {
		name string
		mut  func(in *ModelAuditEnqueueInput)
		want string
	}{
		{"未知种类", func(in *ModelAuditEnqueueInput) { in.Kind = "hlwy_typo" }, "未知的检测任务种类"},
		{"旧种类夹带hlwy", func(in *ModelAuditEnqueueInput) { in.Kind = model.AuditKindProbe }, "hlwy 参数只能用于"},
		{"缺hlwy参数", func(in *ModelAuditEnqueueInput) { in.Hlwy = nil }, "缺少 hlwy 参数"},
		{"带探针", func(in *ModelAuditEnqueueInput) { in.Probes = []string{"p1"} }, "不接受探针"},
		{"绑日志", func(in *ModelAuditEnqueueInput) { in.LogID = 7 }, "不绑定日志"},
		{"手工超时", func(in *ModelAuditEnqueueInput) { in.TimeoutSeconds = 60 }, "整轮超时由采样次数算出"},
		{"非手动触发", func(in *ModelAuditEnqueueInput) { in.Trigger = "cron" }, "只支持手动触发"},
		{"多模型", func(in *ModelAuditEnqueueInput) { in.Models = []string{"m1", "m2"} }, "一次只能测一个模型"},
		{"采样次数", func(in *ModelAuditEnqueueInput) { in.Hlwy.Samples = 150 }, "采样次数只能"},
		{"采样次数为零", func(in *ModelAuditEnqueueInput) { in.Hlwy.Samples = 0 }, "采样次数只能"},
		{"协议", func(in *ModelAuditEnqueueInput) { in.Hlwy.ProtocolID = "numbers-0-9" }, "不支持的协议"},
		{"空基准", func(in *ModelAuditEnqueueInput) { in.Hlwy.BaselineID = "  " }, "缺少主对照基准"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := good()
			c.mut(&in)
			if _, err := ModelAuditEnqueue(ctx, in); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want 含 %q", err, c.want)
			}
		})
	}
}

// TestHlwyEnqueueGateOffTouchesNothing 守住开关的边界：关着的时候连渠道路径都不走。
func TestHlwyEnqueueGateOffTouchesNothing(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	if err := SettingSetString(model.SettingKeyModelAuditHlwyEnabled, "false"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	var before int64
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).Count(&before)

	_, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: 987654, // 故意不存在：关着的时候不该走到查渠道那一步
		Models:    []string{"m1"},
		Kind:      model.AuditKindHlwy,
		Hlwy: &HlwyEnqueueInput{
			Samples:    model.HlwyDefaultSamples,
			BaselineID: hlwyQueueBuiltinID(t),
			ProtocolID: numberdist.ProtocolID,
		},
	})
	if err == nil || !strings.Contains(err.Error(), string(model.SettingKeyModelAuditHlwyEnabled)) {
		t.Fatalf("err = %v, want 提到开关 key", err)
	}
	if strings.Contains(err.Error(), "渠道不存在") {
		t.Fatalf("关着的时候不该去查渠道: %v", err)
	}
	var after int64
	db.GetDB().WithContext(ctx).Model(&model.ModelAuditJob{}).Count(&after)
	if before != after {
		t.Fatalf("关着的时候不该建任务: before=%d after=%d", before, after)
	}
}

// TestHlwyEnqueueFreezesCandidates 守住在入队那一刻冻结的选项。
func TestHlwyEnqueueFreezesCandidates(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	hlwyQueueEnable(t)
	ch := ensureAuditChannel(t, "hlwy-freeze")

	jobID := hlwyQueueEnqueueOne(t, ctx, ch, "m-freeze", model.HlwyMaxSamples)
	job, results, err := ModelAuditJobDetail(ctx, jobID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if job.Kind != model.AuditKindHlwy {
		t.Fatalf("kind = %q", job.Kind)
	}
	if job.Probes != "" {
		t.Fatalf("hlwy 任务不应带探针: %q", job.Probes)
	}
	if job.TimeoutSeconds != HlwyRunTimeout(model.HlwyMaxSamples) {
		t.Fatalf("整轮超时 = %d, want %d", job.TimeoutSeconds, HlwyRunTimeout(model.HlwyMaxSamples))
	}
	if job.Total != 1 || len(results) != 1 {
		t.Fatalf("一次只能一个模型: total=%d results=%d", job.Total, len(results))
	}
	if results[0].Status != model.AuditResultPending {
		t.Fatalf("新结果应 pending，得到 %q", results[0].Status)
	}

	opts, err := model.ParseHlwyOptions(job.OptionsJSON)
	if err != nil {
		t.Fatalf("parse options: %v", err)
	}
	if opts.Samples != model.HlwyMaxSamples {
		t.Fatalf("samples = %d", opts.Samples)
	}
	if opts.ProtocolID != numberdist.ProtocolID {
		t.Fatalf("protocol = %q", opts.ProtocolID)
	}
	builtins, err := numberdist.LoadBuiltins()
	if err != nil {
		t.Fatalf("builtins: %v", err)
	}
	// 主对照在前，其余内置件在后；选中内置件时总份数就是内置件份数。
	if len(opts.Candidates) != len(builtins) || len(opts.Candidates) > model.HlwyMaxCandidates {
		t.Fatalf("候选 %d 份, want %d（上限 %d）",
			len(opts.Candidates), len(builtins), model.HlwyMaxCandidates)
	}
	if opts.Candidates[0].ID != opts.BaselineID {
		t.Fatalf("主对照应排第一: %q vs %q", opts.Candidates[0].ID, opts.BaselineID)
	}
	seen := map[string]struct{}{}
	for _, b := range opts.Candidates {
		if _, dup := seen[b.ID]; dup {
			t.Fatalf("候选重复: %q", b.ID)
		}
		seen[b.ID] = struct{}{}
		if err := model.ValidateHlwyBaseline(b); err != nil {
			t.Fatalf("候选 %q 不完整: %v", b.ID, err)
		}
	}
}

// TestHlwyEnqueueDedupesAcrossKinds 守住「跨种类查重」：同一渠道同一模型的采样
// 不管挂哪种任务名，打的都是同一个上游、花的是同一笔钱。
func TestHlwyEnqueueDedupesAcrossKinds(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	hlwyQueueEnable(t)
	ch := ensureAuditChannel(t, "hlwy-dedupe")

	if _, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"m-same"},
	}); err != nil {
		t.Fatalf("plain enqueue: %v", err)
	}
	_, err := ModelAuditEnqueue(ctx, ModelAuditEnqueueInput{
		ChannelID: ch.ID,
		Models:    []string{"m-same"},
		Kind:      model.AuditKindHlwy,
		Hlwy: &HlwyEnqueueInput{
			Samples:    model.HlwyDefaultSamples,
			BaselineID: hlwyQueueBuiltinID(t),
			ProtocolID: numberdist.ProtocolID,
		},
	})
	if !errors.Is(err, ErrAuditAlreadyQueued) {
		t.Fatalf("err = %v, want ErrAuditAlreadyQueued", err)
	}
}

// TestHlwyClaimReserveSettleAccounting 守住整轮记账的地基：
// 先预留再发包、预留即视为可能已花钱、每次落定都留下自洽快照。
func TestHlwyClaimReserveSettleAccounting(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "acct-base", hlwyOpNumbers(100, 41))
	opts := hlwyOpOptionsFor(t, base.ID, base)
	res, job := hlwyQueuePending(t, opts)

	claim, err := HlwyClaimResult(ctx, HlwyClaimInput{
		ResultID:      res.ID,
		UpstreamModel: "up-1",
		Conditions:    HlwyUnknownConditions("openai-chat"),
	}, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim.Attempt != 1 || claim.JobID != job.ID {
		t.Fatalf("claim = %+v", claim)
	}
	row, rep := hlwyQueueReload(t, ctx, res.ID)
	if row.Status != model.AuditResultRunning {
		t.Fatalf("领取后应 running，得到 %q", row.Status)
	}
	if row.NextRetryAt == nil || !row.NextRetryAt.After(time.Now()) {
		t.Fatalf("领取后必须写下租约: %v", row.NextRetryAt)
	}
	if rep.BaselineComparison.Execution != model.HlwyExecutionSampling {
		t.Fatalf("初始执行状态 = %q", rep.BaselineComparison.Execution)
	}
	if rep.BaselineComparison.Counts.PlannedCalls != opts.Samples {
		t.Fatalf("计划调用 = %d", rep.BaselineComparison.Counts.PlannedCalls)
	}
	if rep.BaselineComparison.Counts.StartedSlots != 0 {
		t.Fatalf("还没发包就记了 %d 格", rep.BaselineComparison.Counts.StartedSlots)
	}

	// 第一格：预留 → 落定 valid。claim.Token 必须跟着走，否则下一次写会被
	// 报告 CAS 挡下来（那正是我们要的防线，但不能误伤自己）。
	slot, err := HlwyReserveSlot(ctx, claim, time.Now())
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	claim.Token = slot.Token
	if slot.Index != 1 || slot.Counts.StartedSlots != 1 || slot.Counts.Unresolved != 1 {
		t.Fatalf("预留后账目 = %+v", slot.Counts)
	}
	_, rep = hlwyQueueReload(t, ctx, res.ID)
	if rep.BaselineComparison.Counts.Unresolved != 1 {
		t.Fatalf("预留必须先落库（unresolved=1），得到 %d",
			rep.BaselineComparison.Counts.Unresolved)
	}

	token, err := HlwySettleSlot(ctx, claim, slot, HlwySlotOutcome{
		Result:        HlwySlotValid,
		Number:        217,
		StrictInteger: true,
	}, time.Now())
	if err != nil {
		t.Fatalf("settle 1: %v", err)
	}
	claim.Token = token
	_, rep = hlwyQueueReload(t, ctx, res.ID)
	bc := rep.BaselineComparison
	if bc.Counts.StartedSlots != 1 || bc.Counts.Valid != 1 || bc.Counts.Unresolved != 0 {
		t.Fatalf("第一格落定后账目 = %+v", bc.Counts)
	}
	if len(bc.RawNumbers) != 1 || bc.RawNumbers[0] != 217 {
		t.Fatalf("原始数字 = %v", bc.RawNumbers)
	}
	if bc.Quality.StrictInteger != 1 {
		t.Fatalf("严格整数计数 = %d", bc.Quality.StrictInteger)
	}

	// 第二格：预留 → 落定 invalid（带截断证据）。这一步同时验证 token 串得上。
	slot, err = HlwyReserveSlot(ctx, claim, time.Now())
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	claim.Token = slot.Token
	token, err = HlwySettleSlot(ctx, claim, slot, HlwySlotOutcome{
		Result:    HlwySlotInvalid,
		Truncated: true,
	}, time.Now())
	if err != nil {
		t.Fatalf("settle 2: %v", err)
	}
	claim.Token = token
	_, rep = hlwyQueueReload(t, ctx, res.ID)
	bc = rep.BaselineComparison
	if bc.Counts.Invalid != 1 || bc.Quality.Truncated != 1 || bc.Counts.Valid != 1 {
		t.Fatalf("第二格落定后账目 = %+v quality=%+v", bc.Counts, bc.Quality)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("中途快照必须自洽: %v", err)
	}
	// 样本还远不够：可以不给分，但必须说明原因。
	if bc.Scorable || strings.TrimSpace(bc.Reason) == "" {
		t.Fatalf("不足样本时 scorable=%v reason=%q", bc.Scorable, bc.Reason)
	}
}

// TestHlwyClaimRejectsCanceledJob 守住「取消赢在领取之前」。
func TestHlwyClaimRejectsCanceledJob(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "cancel-base", hlwyOpNumbers(100, 42))
	res, job := hlwyQueuePending(t, hlwyOpOptionsFor(t, base.ID, base))

	if err := ModelAuditJobCancel(ctx, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err := HlwyClaimResult(ctx, HlwyClaimInput{ResultID: res.ID}, time.Now())
	if !errors.Is(err, ErrHlwyJobCanceled) {
		t.Fatalf("err = %v, want ErrHlwyJobCanceled", err)
	}
	row, _ := hlwyQueueReload(t, ctx, res.ID)
	if row.Status == model.AuditResultRunning {
		t.Fatalf("取消后的行不该被领成 running")
	}
}

// TestHlwySettleAfterCancelKeepsSlot 守住「取消之后不再花新钱，但已花的那格必须记账」。
func TestHlwySettleAfterCancelKeepsSlot(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "late-cancel-base", hlwyOpNumbers(100, 43))
	res, job := hlwyQueuePending(t, hlwyOpOptionsFor(t, base.ID, base))

	claim, err := HlwyClaimResult(ctx, HlwyClaimInput{ResultID: res.ID}, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	slot, err := HlwyReserveSlot(ctx, claim, time.Now())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	claim.Token = slot.Token

	// 就在这一格飞出去的时候用户点了取消。
	if err := ModelAuditJobCancel(ctx, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	token, err := HlwySettleSlot(ctx, claim, slot, HlwySlotOutcome{
		Result: HlwySlotValid, Number: 88, StrictInteger: true,
	}, time.Now())
	if err != nil {
		t.Fatalf("取消后仍必须能落定已花的这一格: %v", err)
	}
	claim.Token = token
	_, rep := hlwyQueueReload(t, ctx, res.ID)
	if rep.BaselineComparison.Counts.Valid != 1 || len(rep.BaselineComparison.RawNumbers) != 1 {
		t.Fatalf("已采到的样本必须保留: %+v", rep.BaselineComparison.Counts)
	}

	// 但下一格不许再开。
	if _, err := HlwyReserveSlot(ctx, claim, time.Now()); !errors.Is(err, ErrHlwyJobCanceled) {
		t.Fatalf("err = %v, want ErrHlwyJobCanceled", err)
	}
}

// TestHlwyOwnershipLostOnStaleAttempt 守住所有权代号：过期运行的写入必须被拒。
func TestHlwyOwnershipLostOnStaleAttempt(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "stale-base", hlwyOpNumbers(100, 44))
	res, _ := hlwyQueuePending(t, hlwyOpOptionsFor(t, base.ID, base))

	claim, err := HlwyClaimResult(ctx, HlwyClaimInput{ResultID: res.ID}, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// 模拟「回收路径接手后又重新开始」：attempt 不再是我们的。
	if err := db.GetDB().Model(&model.ModelAuditResult{}).Where("id = ?", res.ID).
		Update("attempt", claim.Attempt+1).Error; err != nil {
		t.Fatalf("bump attempt: %v", err)
	}
	if _, err := HlwyReserveSlot(ctx, claim, time.Now()); !errors.Is(err, ErrHlwyOwnershipLost) {
		t.Fatalf("err = %v, want ErrHlwyOwnershipLost", err)
	}
	if HlwyRenewOwnership(ctx, claim, time.Now().Add(time.Minute)) {
		t.Fatalf("过期运行不该续租成功")
	}
}

// TestHlwyFinishCompleteWhenAllSlotsSettled 守住「跑完了就是跑完了」。
func TestHlwyFinishCompleteWhenAllSlotsSettled(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "finish-base", hlwyOpNumbers(100, 51))
	opts := hlwyOpOptionsFor(t, base.ID, base)
	rep := hlwyOpReport(t, hlwyReportSpec{
		Numbers:   hlwyOpNumbers(100, 52),
		Planned:   model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionComplete,
		Chosen:    base,
	})
	res, claim := hlwyQueueSeedRunning(t, opts, rep)

	if err := HlwyFinish(ctx, claim, HlwyFinishInput{
		Execution: model.HlwyExecutionComplete,
	}, time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	row, out := hlwyQueueReload(t, ctx, res.ID)
	if row.Status != model.AuditResultDone {
		t.Fatalf("status = %q, want done", row.Status)
	}
	if row.ErrorMsg != "" {
		t.Fatalf("成功收尾不该留错误文案: %q", row.ErrorMsg)
	}
	if row.NextRetryAt != nil {
		t.Fatalf("终态不该再挂租约: %v", row.NextRetryAt)
	}
	if out.BaselineComparison.Execution != model.HlwyExecutionComplete {
		t.Fatalf("execution = %q", out.BaselineComparison.Execution)
	}
	if !out.BaselineComparison.Scorable {
		t.Fatalf("样本充足且无致命停止原因时应可评分: %+v",
			out.BaselineComparison.ReasonCodes)
	}
}

// TestHlwyFinishDemotesIncompleteToInterrupted 守住「complete 不能由写字的人单方面
// 决定」：账没落定就必须降级为中断，并且不重跑。
func TestHlwyFinishDemotesIncompleteToInterrupted(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "demote-base", hlwyOpNumbers(100, 53))
	opts := hlwyOpOptionsFor(t, base.ID, base)
	rep := hlwyOpReport(t, hlwyReportSpec{
		Numbers:   hlwyOpNumbers(3, 54),
		Planned:   model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionSampling,
		Chosen:    base,
	})
	res, claim := hlwyQueueSeedRunning(t, opts, rep)

	if err := HlwyFinish(ctx, claim, HlwyFinishInput{
		Execution: model.HlwyExecutionComplete,
	}, time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	row, out := hlwyQueueReload(t, ctx, res.ID)
	if row.Status != model.AuditResultFailed {
		t.Fatalf("status = %q, want failed", row.Status)
	}
	if out.BaselineComparison.Execution != model.HlwyExecutionInterrupted {
		t.Fatalf("execution = %q, want interrupted", out.BaselineComparison.Execution)
	}
	if !strings.Contains(row.ErrorMsg, "不自动重跑") {
		t.Fatalf("必须说清楚不自动重跑: %q", row.ErrorMsg)
	}
	// 已采到的证据不丢：3 个数字还在报告里。
	if len(out.BaselineComparison.RawNumbers) != 3 {
		t.Fatalf("已采到的数字被抹了: %v", out.BaselineComparison.RawNumbers)
	}
	if row.Attempt != claim.Attempt {
		t.Fatalf("收尾不该改所有权代号: %d → %d", claim.Attempt, row.Attempt)
	}
}

// TestHlwyFinishCanceledJobWins 守住「取消赢过完成」。
func TestHlwyFinishCanceledJobWins(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "finish-cancel-base", hlwyOpNumbers(100, 55))
	opts := hlwyOpOptionsFor(t, base.ID, base)
	rep := hlwyOpReport(t, hlwyReportSpec{
		Numbers:   hlwyOpNumbers(100, 56),
		Planned:   model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionSampling,
		Chosen:    base,
	})
	res, claim := hlwyQueueSeedRunning(t, opts, rep)

	// 直接把任务标成取消（绕过取消路径，确保测的是收尾时的对账）。
	if err := db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", claim.JobID).
		Update("status", model.AuditJobCanceled).Error; err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
	if err := HlwyFinish(ctx, claim, HlwyFinishInput{
		Execution: model.HlwyExecutionComplete,
	}, time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	row, out := hlwyQueueReload(t, ctx, res.ID)
	if row.Status != model.AuditResultSkipped {
		t.Fatalf("status = %q, want skipped", row.Status)
	}
	if out.BaselineComparison.Execution != model.HlwyExecutionCanceled {
		t.Fatalf("execution = %q, want canceled", out.BaselineComparison.Execution)
	}
	if !strings.Contains(row.ErrorMsg, "不重跑") {
		t.Fatalf("取消文案 = %q", row.ErrorMsg)
	}
}

// TestHlwyReclaimExpiredKeepsEvidenceNoResume 守住崩溃/掉租约的收敛方式：
// 留证据、写终态、不改 attempt、绝不自动重跑。
func TestHlwyReclaimExpiredKeepsEvidenceNoResume(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "reclaim-base", hlwyOpNumbers(100, 57))
	opts := hlwyOpOptionsFor(t, base.ID, base)
	res, _ := hlwyQueuePending(t, opts)

	claim, err := HlwyClaimResult(ctx, HlwyClaimInput{ResultID: res.ID}, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	slot, err := HlwyReserveSlot(ctx, claim, time.Now())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	claim.Token = slot.Token
	token, err := HlwySettleSlot(ctx, claim, slot, HlwySlotOutcome{
		Result: HlwySlotValid, Number: 42, StrictInteger: true,
	}, time.Now())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	claim.Token = token

	// 进程卡死：租约过期。
	past := time.Now().Add(-time.Minute)
	if err := db.GetDB().Model(&model.ModelAuditResult{}).Where("id = ?", res.ID).
		Update("next_retry_at", past).Error; err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	var row model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", res.ID).First(&row).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	isHlwy, touched := hlwyReclaimExpiredResult(ctx, row, time.Now())
	if !isHlwy || !touched {
		t.Fatalf("isHlwy=%v touched=%v", isHlwy, touched)
	}
	final, out := hlwyQueueReload(t, ctx, res.ID)
	if final.Status != model.AuditResultFailed {
		t.Fatalf("status = %q, want failed", final.Status)
	}
	if final.Attempt != row.Attempt {
		t.Fatalf("回收不该改所有权代号: %d → %d", row.Attempt, final.Attempt)
	}
	if final.NextRetryAt != nil {
		t.Fatalf("终态不该再挂租约: %v", final.NextRetryAt)
	}
	bc := out.BaselineComparison
	if bc.Execution != model.HlwyExecutionInterrupted || bc.StopReason != "interrupted" {
		t.Fatalf("execution=%q stop=%q", bc.Execution, bc.StopReason)
	}
	if len(bc.RawNumbers) != 1 || bc.RawNumbers[0] != 42 {
		t.Fatalf("已采到的样本必须留着: %v", bc.RawNumbers)
	}
	// 那一格飞出去但没落定：账上仍要承认它可能已经花掉。
	if bc.Counts.Unresolved != 0 || bc.Counts.Valid != 1 {
		t.Fatalf("账目 = %+v", bc.Counts)
	}
	if !strings.Contains(final.ErrorMsg, "不自动重跑") {
		t.Fatalf("文案 = %q", final.ErrorMsg)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("回收后的报告必须自洽: %v", err)
	}
}

// TestHlwyCancelJobSkipsPendingKeepsRunning 守住取消的范围：没开跑的直接跳过，
// 正在跑的不动（让它在下一格之前自己收尾）。
func TestHlwyCancelJobSkipsPendingKeepsRunning(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "cancel-scope-base", hlwyOpNumbers(100, 58))
	opts := hlwyOpOptionsFor(t, base.ID, base)
	job := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(opts))
	pending := hlwyOpSeedResult(t, job.ID, model.AuditResultPending, "")
	running := hlwyOpSeedResult(t, job.ID, model.AuditResultRunning, "")

	if err := hlwyCancelJob(ctx, job.ID, time.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var p model.ModelAuditResult
	if err := db.GetDB().Where("id = ?", pending.ID).First(&p).Error; err != nil {
		t.Fatalf("reload pending: %v", err)
	}
	if p.Status != model.AuditResultSkipped || p.ErrorMsg != "已取消" {
		t.Fatalf("待跑行 = %q / %q", p.Status, p.ErrorMsg)
	}
	if strings.TrimSpace(p.Report) == "" {
		t.Fatalf("未开跑的取消也要给一份可读的空报告（计划已知、证据为空）")
	}
	if rep, err := model.ParseHlwyReport(p.Report); err != nil {
		t.Fatalf("取消报告不可解析: %v", err)
	} else if rep.BaselineComparison.Execution != model.HlwyExecutionCanceled {
		t.Fatalf("取消报告 execution = %q", rep.BaselineComparison.Execution)
	}

	var r model.ModelAuditResult
	if err := db.GetDB().Where("id = ?", running.ID).First(&r).Error; err != nil {
		t.Fatalf("reload running: %v", err)
	}
	if r.Status != model.AuditResultRunning {
		t.Fatalf("正在跑的行不该被取消路径改动: %q", r.Status)
	}

	var j model.ModelAuditJob
	if err := db.GetDB().Where("id = ?", job.ID).First(&j).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if j.Status != model.AuditJobCanceled || j.EndedAt == nil {
		t.Fatalf("任务 = %q ended=%v", j.Status, j.EndedAt)
	}
}

// TestHlwyFailWithoutRunKeepsReportEmpty 守住「开不了跑就直说，不伪造采样账」。
func TestHlwyFailWithoutRunKeepsReportEmpty(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "fail-base", hlwyOpNumbers(100, 59))
	res, _ := hlwyQueuePending(t, hlwyOpOptionsFor(t, base.ID, base))

	if err := HlwyFailWithoutRun(ctx, res.ID, "  ", time.Now()); err != nil {
		t.Fatalf("fail without run: %v", err)
	}
	var row model.ModelAuditResult
	if err := db.GetDB().Where("id = ?", res.ID).First(&row).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.AuditResultFailed {
		t.Fatalf("status = %q", row.Status)
	}
	if row.ErrorMsg != "对照未能开始" {
		t.Fatalf("默认文案 = %q", row.ErrorMsg)
	}
	if strings.TrimSpace(row.Report) != "" {
		t.Fatalf("没跑过就不该有采样账: %q", row.Report)
	}
}

// TestHlwyJobSummaryOfReport 守住列表汇总只搬运报告里已经算好的值。
func TestHlwyJobSummaryOfReport(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "summary-base", hlwyOpNumbers(100, 60))
	rep := hlwyOpReport(t, hlwyReportSpec{
		Numbers:   hlwyOpNumbers(97, 61),
		Planned:   model.HlwyDefaultSamples,
		Invalid:   2,
		Failed:    1,
		Execution: model.HlwyExecutionComplete,
		Chosen:    base,
	})
	raw := hlwyOpEncodeReport(t, rep)

	sum := HlwyJobSummaryOf(raw)
	if sum == nil {
		t.Fatalf("summary = nil")
	}
	if sum.Counts.PlannedCalls != model.HlwyDefaultSamples || sum.Counts.StartedSlots != 100 {
		t.Fatalf("planned=%d started=%d", sum.Counts.PlannedCalls, sum.Counts.StartedSlots)
	}
	if sum.Counts.Valid != 97 || sum.Counts.Invalid != 2 || sum.Counts.Failed != 1 || sum.Counts.Unresolved != 0 {
		t.Fatalf("账目 = %+v", sum.Counts)
	}
	if sum.Execution != model.HlwyExecutionComplete || sum.Sufficiency != model.HlwySufficiency(97) {
		t.Fatalf("execution=%q sufficiency=%q", sum.Execution, sum.Sufficiency)
	}
	if sum.Scorable != rep.BaselineComparison.Scorable {
		t.Fatalf("scorable 应与报告一致: %v vs %v", sum.Scorable, rep.BaselineComparison.Scorable)
	}
	// 报告坏了就退回旧展示，而不是渲染一份半截数。
	if got := HlwyJobSummaryOf(""); got != nil {
		t.Fatalf("空报告应返回 nil，得到 %+v", *got)
	}
	if got := HlwyJobSummaryOf("{\"kind\":\"hlwy_compare\"}"); got != nil {
		t.Fatalf("残缺报告应返回 nil，得到 %+v", *got)
	}
}

// TestHlwyRefreshInvalidatingStopReason 守住「致命停止原因 ⇒ 不给分」。
func TestHlwyRefreshInvalidatingStopReason(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "invalidate-base", hlwyOpNumbers(100, 62))
	opts := hlwyOpOptionsFor(t, base.ID, base)
	rep := hlwyOpReport(t, hlwyReportSpec{
		Numbers:   hlwyOpNumbers(100, 63),
		Planned:   model.HlwyDefaultSamples,
		Execution: model.HlwyExecutionFailed,
		Chosen:    base,
	})
	if len(rep.BaselineComparison.Matches) == 0 {
		t.Fatalf("前提不成立：这份报告本来是有对照结果的")
	}

	rep.BaselineComparison.StopReason = HlwyStopPromptMismatch
	hlwyRefreshReport(&rep, opts)

	bc := rep.BaselineComparison
	if bc.Comparability != model.HlwyComparabilityIncompatible {
		t.Fatalf("comparability = %q, want incompatible", bc.Comparability)
	}
	if bc.Scorable {
		t.Fatalf("条件漂移之后绝不能给分")
	}
	if len(bc.Matches) != 0 {
		t.Fatalf("不可评分时不得下发对照结果: %+v", bc.Matches)
	}
	if strings.TrimSpace(bc.Reason) == "" {
		t.Fatalf("不可评分必须说明原因")
	}
	count := 0
	for _, code := range bc.ReasonCodes {
		if code == HlwyStopPromptMismatch {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("原因码 %q 应恰好出现一次: %v", HlwyStopPromptMismatch, bc.ReasonCodes)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("刷新后的报告必须仍然自洽: %v", err)
	}
}

// TestHlwyDoesNotStealLatestChip 守住渠道卡片上那条结论：hlwy 对照参与不了风险判定，
// 它再新也不能把旧检测留给同一个模型的结论顶掉。
func TestHlwyDoesNotStealLatestChip(t *testing.T) {
	ctx := setupAuditQueueTest(t)
	base := hlwyOpImportBaseline(t, ctx, "chip-base", hlwyOpNumbers(100, 64))

	older := time.Now().UTC().Add(-time.Hour)
	plainJob := hlwyOpSeedJob(t, model.AuditKindProbe, nil)
	plain := model.ModelAuditResult{
		JobID: plainJob.ID, ChannelID: 1, ChannelName: "chip", Model: "m-chip",
		UpstreamModel: "m-chip", Status: model.AuditResultDone, Report: "{}",
		Attempt: 1, FinishedAt: &older, CreatedAt: older,
	}
	if err := db.GetDB().Create(&plain).Error; err != nil {
		t.Fatalf("create plain: %v", err)
	}

	newer := time.Now().UTC()
	hlwyJob := hlwyOpSeedJob(t, model.AuditKindHlwy, ptrOptions(hlwyOpOptionsFor(t, base.ID, base)))
	hlwyRes := model.ModelAuditResult{
		JobID: hlwyJob.ID, ChannelID: 1, ChannelName: "chip", Model: "m-chip",
		UpstreamModel: "m-chip", Status: model.AuditResultDone,
		Report: hlwyOpEncodeReport(t, hlwyOpReport(t, hlwyReportSpec{
			Numbers: hlwyOpNumbers(100, 65), Planned: model.HlwyDefaultSamples,
			Execution: model.HlwyExecutionComplete, Chosen: base,
		})),
		Attempt: 1, FinishedAt: &newer, CreatedAt: newer,
	}
	if err := db.GetDB().Create(&hlwyRes).Error; err != nil {
		t.Fatalf("create hlwy result: %v", err)
	}

	rows, err := ModelAuditLatestByChannel(ctx, 1)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("同一模型应只剩一条结论，得到 %d 条", len(rows))
	}
	if rows[0].JobID != plainJob.ID {
		t.Fatalf("卡片被 hlwy 对照顶掉了: job %d（期望 %d）", rows[0].JobID, plainJob.ID)
	}
}
