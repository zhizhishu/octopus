package op

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"gorm.io/gorm"
)

// hlwy 基准对照的排队语义。
//
// 与普通检测共用同一张任务/结果表、同一把入队锁、同一套租约与回收循环，
// 但执行体完全不同：这里是 N 次串行的「一个数字」采样，钱是按时序一格格花出去的，
// 因此账目必须**逐格**落库，而不是等整轮跑完再写一份汇总。
//
// 三条不可动摇的规则：
//  1. 每一次采样**先记账再发包**（预留），所以 started_slots 是「可能已经花掉的钱」
//     的保守上界；崩溃只会留下 unresolved，不会让钱凭空消失。
//  2. 领取之后**绝不重跑**：租约过期只证明心跳停了，不证明上游没收到请求，
//     重跑就是替用户再买一次单。中断就标 interrupted，保留已采到的证据。
//  3. 所有权 = (status=running, attempt=领取时自增的值, 租约未过期)。
//     所有运行期写入都带这三条断言，过期租约被回收后再复活的那次运行写不进来。
const (
	// HlwySampleTimeout 是单次采样的超时上限。
	//
	// 它必须比整轮超时小得多，也要和「整轮超时」区分开：一次采样超时只说明这一格
	// 没采到（failed），整轮继续；而整轮超时意味着这一轮到此为止（timed_out）。
	// 两者混成一个错误码，报告里就再也分不清「手气差」和「跑不完」。
	HlwySampleTimeout = 60 * time.Second

	// hlwySecondsPerSample 每次采样在基础时长之外追加的秒数。
	hlwySecondsPerSample = 10

	// HlwyMinRunTimeoutSeconds 是整轮采样的基础时长。
	HlwyMinRunTimeoutSeconds = 300

	// HlwyMaxRunTimeoutSeconds 是整轮采样的上限（30 分钟），
	// 与旧检测放大后的上限同一口径，避免出现第二种「最长能跑多久」。
	HlwyMaxRunTimeoutSeconds = AuditScaledMaxTimeoutSeconds
)

// hlwy 排队路径的哨兵错误。调用方靠它们区分「被取消」「抢不到」「名额用完」，
// 而不是去解析错误字符串。
var (
	// ErrHlwyJobCanceled 任务已被用户取消：不再预留新的采样槽位。
	ErrHlwyJobCanceled = errors.New("hlwy 对照任务已取消")
	// ErrHlwyNotPending 结果已经不是待跑状态（别人先领走了，或已经终态）。
	ErrHlwyNotPending = errors.New("hlwy 对照结果不是待跑状态")
	// ErrHlwyOwnershipLost 这次运行已经不再拥有该结果（租约过期/被回收/报告被别人改过）。
	ErrHlwyOwnershipLost = errors.New("hlwy 对照结果的执行权已不属于本次运行")
	// ErrHlwySlotsExhausted 计划内的采样槽位已经全部用满。
	ErrHlwySlotsExhausted = errors.New("hlwy 采样槽位已用满")
)

// HlwyRunTimeout 按采样次数算整轮超时：min(300 + 10*N, 1800) 秒。
//
// 采样是串行的，单次最长 60 秒：沿用旧检测那个 300 秒封顶，100 次采样会在半路
// 被整轮超时掐断，留下一份「超时结束」的半份证据。这里按次数放大，并把上限压在
// 30 分钟——再长就该有人看看是不是卡住了，而不是让它继续烧额度。
func HlwyRunTimeout(samples int) int {
	if samples <= 0 {
		samples = model.HlwyDefaultSamples
	}
	total := HlwyMinRunTimeoutSeconds + hlwySecondsPerSample*samples
	if total > HlwyMaxRunTimeoutSeconds {
		total = HlwyMaxRunTimeoutSeconds
	}
	return total
}

// HlwyEnqueueInput 是入队时客户端**能**决定的三件事。
//
// 其余一切（候选基准、采样超时、整轮超时）都由服务端在入队那一刻定死并冻结进
// 任务行：客户端可以指定「跟谁比」，但不能指定「拿什么分布去比」——否则一份自造的
// 分布就能把相似度刷到想要的位置。
type HlwyEnqueueInput struct {
	Samples    int
	BaselineID string
	ProtocolID string
}

// ModelAuditEnqueueHlwy 入队一轮 hlwy 基准对照。
//
// 与普通检测相同的部分：一次只允许一个模型；同一个 (渠道, 模型) 已经在排队或
// 正在跑就不再入队（重复点击、多标签页都会走到这里，而每一次都是钱）；任务行与
// 结果行在同一个事务里落地。
//
// 刻意**不同**的部分：
//   - 采样次数只允许 100/200，超时由采样次数算出，不接受调用方指定；
//   - 不绑定日志、不跑探针：这不是在查某条日志，也没有探针集合；
//   - 候选基准在这里冻结成完整快照写进 OptionsJSON，运行期不再回查目录；
//   - 开关只拦**新入队**：关掉之后在途任务照旧跑到终态。
func ModelAuditEnqueueHlwy(ctx context.Context, in ModelAuditEnqueueInput) (int64, error) {
	if kind := strings.TrimSpace(in.Kind); kind != "" && kind != model.AuditKindHlwy {
		return 0, fmt.Errorf("hlwy 入队的任务种类必须是 %q，收到 %q", model.AuditKindHlwy, kind)
	}
	if in.Hlwy == nil {
		return 0, errors.New("hlwy 对照缺少 hlwy 参数")
	}
	// 普通检测的入参一个都不许混进来：探针集合会被写进任务行、日志引用会让
	// 结果看起来像「某条日志的结论」、手工超时会和按次数算出来的超时打架。
	if len(in.Probes) > 0 {
		return 0, errors.New("hlwy 对照不接受探针（probes 必须为空）")
	}
	if in.LogID != 0 {
		return 0, errors.New("hlwy 对照不绑定日志（log_id 必须为 0）")
	}
	if in.TimeoutSeconds != 0 {
		return 0, errors.New("hlwy 对照的整轮超时由采样次数算出，不接受手工指定")
	}
	if trigger := strings.TrimSpace(in.Trigger); trigger != "" && trigger != "manual" {
		return 0, fmt.Errorf("hlwy 对照只支持手动触发，收到 %q", trigger)
	}

	models := make([]string, 0, len(in.Models))
	seen := make(map[string]struct{}, len(in.Models))
	for _, m := range in.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		models = append(models, m)
	}
	if len(models) == 0 {
		return 0, errors.New("至少要选一个模型")
	}
	// 一轮就是一个模型的 N 次采样。允许多个模型会让「这一轮的账」变成几份账的
	// 混合，而报告里的 counts/sufficiency 只有一套。
	if len(models) > 1 {
		return 0, fmt.Errorf("hlwy 对照一次只能测一个模型，收到 %d 个", len(models))
	}

	samples := in.Hlwy.Samples
	if samples != model.HlwyDefaultSamples && samples != model.HlwyMaxSamples {
		return 0, fmt.Errorf("采样次数只能 %d 或 %d，收到 %d",
			model.HlwyDefaultSamples, model.HlwyMaxSamples, samples)
	}
	if protocolID := strings.TrimSpace(in.Hlwy.ProtocolID); protocolID != numberdist.ProtocolID {
		return 0, fmt.Errorf("不支持的协议 %q（只支持 %q）", protocolID, numberdist.ProtocolID)
	}
	baselineID := strings.TrimSpace(in.Hlwy.BaselineID)
	if baselineID == "" {
		return 0, errors.New("缺少主对照基准 id")
	}

	// 闸门放在花钱之前的第一位：关着的时候连渠道都不查。
	enabled, err := SettingGetBool(model.SettingKeyModelAuditHlwyEnabled)
	if err != nil {
		return 0, fmt.Errorf("hlwy 对照开关不可读: %w", err)
	}
	if !enabled {
		return 0, fmt.Errorf("hlwy 基准对照未启用（%s）", model.SettingKeyModelAuditHlwyEnabled)
	}

	if in.ChannelID <= 0 {
		return 0, errors.New("渠道不能为空")
	}
	channel, err := ChannelGet(in.ChannelID, ctx)
	if err != nil {
		return 0, errors.New("渠道不存在")
	}
	// 只对 chat 家族采样：图片/嵌入/决策模型渠道要么没有「回一个数字」这回事，
	// 要么请求体会被重新组装成另一种形状，采到的数字不是同一道题。在这里挡住，
	// 报错发生在零上游请求之前。
	if !outbound.IsChatChannelType(channel.Type) {
		return 0, fmt.Errorf("渠道类型 %d 不支持 hlwy 数字采样（需要 chat 家族）", int(channel.Type))
	}

	resolved, err := HlwyBaselineResolve(ctx, baselineID)
	if err != nil {
		return 0, err
	}
	candidates, err := hlwyEnqueueCandidates(resolved.Baseline)
	if err != nil {
		return 0, err
	}
	opts := model.HlwyOptions{
		Samples:    samples,
		BaselineID: resolved.Baseline.ID,
		ProtocolID: numberdist.ProtocolID,
		Candidates: candidates,
	}
	optionsJSON, err := opts.Encode()
	if err != nil {
		return 0, fmt.Errorf("hlwy 选项不可用: %w", err)
	}

	// 查重 → 建任务同样是不可分割的动作，理由与普通检测完全一致（见 ModelAuditEnqueue）。
	// 这里刻意**不按种类过滤**：同一渠道同一个模型的采样，不管挂着哪种任务名，
	// 打的都是同一个上游、花的是同一笔钱；跨种类去重才能真的挡住重复付费。
	auditEnqueueMu.Lock()
	defer auditEnqueueMu.Unlock()

	var inflight []string
	if err := db.GetDB().WithContext(ctx).
		Model(&model.ModelAuditResult{}).
		Where("channel_id = ? AND model IN ? AND status IN ?",
			channel.ID, models, []string{model.AuditResultPending, model.AuditResultRunning}).
		Distinct().
		Pluck("model", &inflight).Error; err != nil {
		return 0, err
	}
	if len(inflight) > 0 {
		return 0, ErrAuditAlreadyQueued
	}

	if auditEnqueueBetweenCheckAndInsert != nil {
		auditEnqueueBetweenCheckAndInsert()
	}

	job := model.ModelAuditJob{
		ChannelID:      channel.ID,
		ChannelName:    channel.Name,
		Status:         model.AuditJobQueued,
		Total:          1,
		Probes:         "",
		TimeoutSeconds: HlwyRunTimeout(samples),
		Kind:           model.AuditKindHlwy,
		OptionsJSON:    optionsJSON,
		Trigger:        "manual",
	}
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		res := model.ModelAuditResult{
			JobID:       job.ID,
			ChannelID:   channel.ID,
			ChannelName: channel.Name,
			Model:       models[0],
			Status:      model.AuditResultPending,
		}
		return tx.Create(&res).Error
	}); err != nil {
		return 0, fmt.Errorf("hlwy 对照入队失败: %w", err)
	}
	return job.ID, nil
}

// hlwyEnqueueCandidates 组装本轮冻结的候选基准：主对照在前，其余内置件在后。
//
// 主对照放第一位是为了让「我先选的那份」在报告里可读；顺序不影响打分（分数按
// overall 降序排）。超上限时报错而不截断：悄悄丢掉一份候选，界面就会少一行排名，
// 而用户以为那就是全部可比对象。
func hlwyEnqueueCandidates(chosen numberdist.Baseline) ([]numberdist.Baseline, error) {
	if err := model.ValidateHlwyBaseline(chosen); err != nil {
		return nil, fmt.Errorf("主对照基准不可用: %w", err)
	}
	builtins, err := numberdist.LoadBuiltins()
	if err != nil {
		return nil, fmt.Errorf("加载内置基准失败: %w", err)
	}
	out := make([]numberdist.Baseline, 0, len(builtins)+1)
	out = append(out, model.HlwySnapshotOf(chosen))
	for _, b := range builtins {
		if b.ID == chosen.ID {
			continue
		}
		out = append(out, model.HlwySnapshotOf(b))
	}
	if len(out) > model.HlwyMaxCandidates {
		return nil, fmt.Errorf("候选基准 %d 份超过上限 %d", len(out), model.HlwyMaxCandidates)
	}
	return out, nil
}

// HlwyClaimInput 是领取一次执行权所需的外部信息（全部来自 worker 的本地判断）。
type HlwyClaimInput struct {
	ResultID int64
	// UpstreamModel 是本轮实际发往上游的型号（有映射时与请求名不同）。
	UpstreamModel string
	// Conditions 是起始条件快照：此刻还没观测过任何东西，只有我们自己决定的标记。
	Conditions numberdist.Conditions
}

// HlwyClaim 是一次所有权领取的结果。
//
// Attempt 是所有权代号：此后每一次写都必须带 attempt==Attempt 且租约未过期，
// 于是「过期租约被回收、原运行又醒来」的那次写入会被库拒绝。
type HlwyClaim struct {
	ResultID int64
	JobID    int64
	Attempt  int
	Options  model.HlwyOptions
	// Conditions 是本轮写进初始报告的那份条件快照：运行期的合并基线必须是库里
	// 那一份，不能在这里另造一份空条件。
	Conditions numberdist.Conditions
	// LeaseUntil 本次租约的到期时间；续租只会把它往后推。
	LeaseUntil time.Time
	// StartedAt 领取时刻，用于算耗时。
	StartedAt time.Time
	// Token 是最近一次成功落库的报告字符串，所有运行期写都以它做 CAS。
	Token string
	// SelfCompare 表示主对照基准就是本结果自己采出来的那份样本。
	SelfCompare bool
}

// HlwyClaimResult 领取一条 hlwy 结果的执行权。
//
// 顺序不可调换：**先**锁任务行（把取消、预留、落定、收尾串行化），再读任务状态
// （已取消就不领），再抢结果行（只认 pending），最后一次写入 running + 初始报告。
// 报告与 running 在同一个事务里落地，于是「正在跑」和「有一份能读的采样账」永远
// 同时存在——崩溃后回收读到的是一个自洽的快照，而不是一个空的 report 列。
func HlwyClaimResult(ctx context.Context, in HlwyClaimInput, now time.Time) (HlwyClaim, error) {
	if in.ResultID <= 0 {
		return HlwyClaim{}, errors.New("缺少检测结果 id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	// 先不加锁读一次，只为拿冻结选项与「是不是自比」：这两件事决定初始报告的内容，
	// 而选项在入队后不可变。真正的判定仍然在事务里重做。
	res, job, err := hlwyResultAndJob(ctx, in.ResultID)
	if err != nil {
		return HlwyClaim{}, err
	}
	opts, err := model.ParseHlwyOptions(job.OptionsJSON)
	if err != nil {
		return HlwyClaim{}, fmt.Errorf("任务 #%d 的选项不可用: %w", job.ID, err)
	}
	selfCompare := hlwyLocalSourceResultID(ctx, opts.BaselineID) == in.ResultID

	conditions := in.Conditions
	if err := model.ValidateHlwyConditions(conditions); err != nil {
		conditions = HlwyUnknownConditions(conditions.Protocol)
	}
	if err := model.ValidateHlwyConditions(conditions); err != nil {
		return HlwyClaim{}, fmt.Errorf("起始条件快照不可用: %w", err)
	}

	claim := HlwyClaim{
		ResultID:    res.ID,
		JobID:       job.ID,
		Options:     opts,
		Conditions:  conditions,
		StartedAt:   now,
		SelfCompare: selfCompare,
	}
	err = db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := hlwyLockJob(tx, job.ID)
		if err != nil {
			return err
		}
		if locked.Status == model.AuditJobCanceled {
			return ErrHlwyJobCanceled
		}
		var cur model.ModelAuditResult
		if err := tx.Where("id = ?", res.ID).First(&cur).Error; err != nil {
			return err
		}
		if cur.Status != model.AuditResultPending {
			return fmt.Errorf("%w: 当前状态 %q", ErrHlwyNotPending, cur.Status)
		}
		attempt := cur.Attempt + 1
		lease := AuditLeaseUntil(now)
		rep := hlwyInitialReport(cur, opts, in.UpstreamModel, conditions, selfCompare)
		if err := rep.Validate(); err != nil {
			return fmt.Errorf("初始报告不自洽: %w", err)
		}
		token, err := rep.Encode()
		if err != nil {
			return err
		}
		upd := tx.Model(&model.ModelAuditResult{}).
			Where("id = ? AND status = ?", res.ID, model.AuditResultPending).
			Updates(map[string]any{
				"status":        model.AuditResultRunning,
				"attempt":       attempt,
				"next_retry_at": lease,
				"report":        token,
				"verdict":       model.HlwyVerdictUnknown,
				"score":         0,
				"finding_n":     0,
				"error_msg":     "",
				"duration_ms":   0,
				"finished_at":   nil,
			})
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return ErrHlwyNotPending
		}
		claim.Attempt = attempt
		claim.LeaseUntil = lease
		claim.Token = token
		return nil
	})
	if err != nil {
		return HlwyClaim{}, err
	}
	return claim, nil
}

// HlwySlot 是一个已预留的采样槽位。
type HlwySlot struct {
	// Index 是本次采样在计划内的序号（从 1 起），只用于进度与日志。
	Index int
	// Token 是预留后落库的报告字符串，落定时以它做 CAS。
	Token string
	// Counts 是预留后的账目快照。
	Counts model.HlwyCounts
}

// HlwyReserveSlot 预留下一格采样额度，**先落库再发包**。
//
// 这条顺序是整轮记账的线性化点：进程在「落库之后、发包之前」崩溃，库里会留下一个
// unresolved 槽位，读起来就是「这一格可能已经花掉、但没有证据」。反过来把顺序调成
// 先发包再记账，崩溃就会让已经付过钱的调用彻底消失。
//
// 任务已取消时返回 ErrHlwyJobCanceled 且不占额度：取消之后不再花新钱，但已经预留的
// 那一格由落定路径正常收尾（它的钱很可能已经花出去了）。
func HlwyReserveSlot(ctx context.Context, claim HlwyClaim, now time.Time) (HlwySlot, error) {
	if claim.ResultID <= 0 {
		return HlwySlot{}, errors.New("缺少检测结果 id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	var slot HlwySlot
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := hlwyLockJob(tx, claim.JobID)
		if err != nil {
			return err
		}
		if locked.Status == model.AuditJobCanceled {
			return ErrHlwyJobCanceled
		}
		_, rep, err := hlwyLoadOwned(tx, claim, now)
		if err != nil {
			return err
		}
		bc := &rep.BaselineComparison
		if bc.Counts.StartedSlots >= claim.Options.Samples {
			return ErrHlwySlotsExhausted
		}
		bc.Counts.StartedSlots++
		bc.Counts.Unresolved++
		if err := hlwyCheckCounts(bc.Counts, claim.Options.Samples); err != nil {
			return err
		}
		hlwyRefreshReport(&rep, claim.Options)
		token, err := rep.Encode()
		if err != nil {
			return err
		}
		upd := hlwyOwnedQuery(tx, claim, now, claim.Token).Updates(map[string]any{
			"report":        token,
			"next_retry_at": AuditLeaseUntil(now),
		})
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return ErrHlwyOwnershipLost
		}
		slot = HlwySlot{Index: bc.Counts.StartedSlots, Token: token, Counts: bc.Counts}
		return nil
	})
	if err != nil {
		return HlwySlot{}, err
	}
	return slot, nil
}

// hlwy 槽位落定的三种结局。名字进报告，因此是稳定的对外字符串。
const (
	// HlwySlotValid 采到了一个 1..355 的数字，进 raw_numbers。
	HlwySlotValid = "valid"
	// HlwySlotInvalid 上游确实回了话，但那不是一道能解析成数字的答案。
	HlwySlotInvalid = "invalid"
	// HlwySlotFailed 这一格没拿到可用响应（网络/5xx/超时）。
	HlwySlotFailed = "failed"
)

// HlwySlotOutcome 是一格采样的结局。
type HlwySlotOutcome struct {
	// Result 见 HlwySlotValid/Invalid/Failed。
	Result string
	// Number 只在 Result=valid 时有意义，必须是 1..355。
	Number int
	// StrictInteger 表示响应正文就是一个裸整数（没有多余文字）。
	StrictInteger bool
	// Truncated 表示上游是因为长度上限被截断的（是证据，不是错误）。
	Truncated bool
	// Conditions 非 nil 时并进报告的条件快照（观测到真实参数时才填）。
	Conditions *numberdist.Conditions
	// StopReason 非空时与这一格证据一起落库（经过白名单）。
	//
	// 它必须和出问题的那一格在同一个事务里写下去：只靠收尾时的内存状态，
	// 崩溃/取消就会让「这一轮已经不可比」这件事彻底消失。
	StopReason string
}

// HlwySettleSlot 落定一格采样并写回报告。
//
// 即使任务已经取消也要能落定：这一格的钱很可能已经花了，证据必须留下来。因此这里
// **不检查**任务状态，只检查所有权（running + attempt + 未过期 + 报告 CAS）。
//
// 返回本次落库的报告字符串，调用方用它继续做下一次 CAS。
func HlwySettleSlot(ctx context.Context, claim HlwyClaim, slot HlwySlot, out HlwySlotOutcome, now time.Time) (string, error) {
	if claim.ResultID <= 0 {
		return "", errors.New("缺少检测结果 id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	next := ""
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 仍然先锁任务行：加锁顺序必须与取消/预留一致，否则两条路径会互相死等。
		if _, err := hlwyLockJob(tx, claim.JobID); err != nil {
			return err
		}
		_, rep, err := hlwyLoadOwned(tx, claim, now)
		if err != nil {
			return err
		}
		bc := &rep.BaselineComparison
		if bc.Counts.Unresolved <= 0 {
			return fmt.Errorf("结果 #%d 没有待落定的采样槽位", claim.ResultID)
		}
		bc.Counts.Unresolved--
		switch out.Result {
		case HlwySlotValid:
			if out.Number < 1 || out.Number > numberdist.DistLength {
				return fmt.Errorf("有效数字 %d 越界（1..%d）", out.Number, numberdist.DistLength)
			}
			bc.Counts.Valid++
			bc.RawNumbers = append(bc.RawNumbers, out.Number)
			if out.StrictInteger {
				bc.Quality.StrictInteger++
			} else {
				bc.Quality.Permissive++
			}
		case HlwySlotInvalid:
			bc.Counts.Invalid++
		case HlwySlotFailed:
			bc.Counts.Failed++
		default:
			return fmt.Errorf("未知的采样结局 %q", out.Result)
		}
		if out.Truncated {
			bc.Quality.Truncated++
		}
		if out.Conditions != nil {
			if err := model.ValidateHlwyConditions(*out.Conditions); err != nil {
				return fmt.Errorf("条件快照不可用: %w", err)
			}
			bc.Conditions = HlwyMergeConditions(bc.Conditions, *out.Conditions)
		}
		// 停止原因与那一格证据一起落库：不可比这件事从此只依赖报告本身，
		// 后续的收尾/回收都不得把它改写成一句「只是跑断了」。
		if r := HlwySanitizeStopReason(out.StopReason); r != "" {
			bc.StopReason = r
		}
		if err := hlwyCheckCounts(bc.Counts, claim.Options.Samples); err != nil {
			return err
		}
		hlwyRefreshReport(&rep, claim.Options)
		if err := rep.Validate(); err != nil {
			return fmt.Errorf("采样报告不自洽: %w", err)
		}
		token, err := rep.Encode()
		if err != nil {
			return err
		}
		upd := hlwyOwnedQuery(tx, claim, now, slot.Token).Updates(map[string]any{
			"report":        token,
			"next_retry_at": AuditLeaseUntil(now),
		})
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return ErrHlwyOwnershipLost
		}
		next = token
		return nil
	})
	if err != nil {
		return "", err
	}
	return next, nil
}

// HlwyFinishInput 是收尾时调用方的**意图**，最终状态还要和账目对账。
type HlwyFinishInput struct {
	// Execution 期望的执行状态（complete/canceled/timed_out/interrupted/failed）。
	Execution string
	// StopReason 为什么停（已脱敏的类别，如 consecutive_failures、http_401）。
	StopReason string
	// ErrorMsg 落到结果行的原因（只允许我们自己拼的文案）。
	ErrorMsg string
}

// HlwyFinish 收尾：把这一轮定格成终态，并只在这里算一次完整的对照。
//
// 与普通检测的收尾有三处关键差异，都是为了不让「花过的钱买不到结论」或
// 「结论被人随手改写」：
//   - 任务已取消则执行状态一律 canceled（取消赢过完成）；
//   - 声明 complete 但账目没落定时自动降级为 interrupted——complete 不能由写字
//     的人单独决定，它必须与「每个槽位都落定」对得上；
//   - 报告先过 Validate 再落库：不自洽就报错退出，绝不把一份自相矛盾的报告写成
//     终态（那时已经没有第二次机会修正它了）。
//
// 返回 ErrHlwyOwnershipLost 表示所有权已不在本次运行手上（被回收/租约过期）：
// 调用方应当安静退出并把结果交给回收路径，而不是重试写入。
func HlwyFinish(ctx context.Context, claim HlwyClaim, in HlwyFinishInput, now time.Time) error {
	if claim.ResultID <= 0 {
		return errors.New("缺少检测结果 id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	execution := strings.TrimSpace(in.Execution)
	if !hlwyIsExecution(execution) {
		return fmt.Errorf("未知的执行状态 %q", execution)
	}
	// sampling 不是终态：收尾用写入把它写下去，一份「永远在采样中」的报告就
	// 再也不会变成结论。调用方必须自己说明到底是怎么结束的。
	if execution == model.HlwyExecutionSampling {
		return errors.New("收尾不能写 sampling，必须给出终态执行结果")
	}
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := hlwyLockJob(tx, claim.JobID)
		if err != nil {
			return err
		}
		_, rep, err := hlwyLoadOwned(tx, claim, now)
		if err != nil {
			return err
		}
		bc := &rep.BaselineComparison
		if locked.Status == model.AuditJobCanceled {
			execution = model.HlwyExecutionCanceled
		}
		if execution == model.HlwyExecutionComplete &&
			(bc.Counts.StartedSlots != claim.Options.Samples || bc.Counts.Unresolved != 0) {
			execution = model.HlwyExecutionInterrupted
		}
		bc.Execution = execution
		// 已经因证据问题停过的原因不被收尾覆盖：取消/超时只说明「这一轮怎么
		// 结束的」，不会让那批数字重新变成同一个实验的产物。
		if !hlwyInvalidatesEvidence(bc.StopReason) {
			bc.StopReason = HlwySanitizeStopReason(in.StopReason)
		}
		hlwyRefreshReport(&rep, claim.Options)
		if err := rep.Validate(); err != nil {
			return fmt.Errorf("终态报告不自洽: %w", err)
		}
		token, err := rep.Encode()
		if err != nil {
			return err
		}
		status, errMsg := hlwyTerminalOf(execution, in.ErrorMsg)
		duration := int64(0)
		if !claim.StartedAt.IsZero() {
			if d := now.Sub(claim.StartedAt); d > 0 {
				duration = d.Milliseconds()
			}
		}
		upd := hlwyOwnedQuery(tx, claim, now, claim.Token).Updates(map[string]any{
			"status":        status,
			"verdict":       model.HlwyVerdictUnknown,
			"score":         0,
			"finding_n":     0,
			"report":        token,
			"error_msg":     errMsg,
			"next_retry_at": nil,
			"duration_ms":   duration,
			"finished_at":   now,
		})
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return ErrHlwyOwnershipLost
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 进度在事务之后重算：事务里再开一条连接写同一个库，SQLite 会直接卡住。
	ModelAuditJobProgress(ctx, claim.JobID)
	return nil
}

// hlwyTerminalOf 把执行状态映射成结果行状态与默认原因。
//
// skipped 只给「用户取消」：那是主动放弃，不是失败；其余非 complete 一律 failed，
// 因为「没跑完」本身就是需要人看一眼的事。
func hlwyTerminalOf(execution, errMsg string) (string, string) {
	msg := strings.TrimSpace(errMsg)
	switch execution {
	case model.HlwyExecutionComplete:
		return model.AuditResultDone, ""
	case model.HlwyExecutionCanceled:
		if msg == "" {
			msg = "已取消；已采到的样本保留，取消不重跑"
		}
		return model.AuditResultSkipped, msg
	case model.HlwyExecutionTimedOut:
		if msg == "" {
			msg = "采样超时结束；已采到的样本保留，请手动重检"
		}
		return model.AuditResultFailed, msg
	case model.HlwyExecutionInterrupted:
		if msg == "" {
			msg = "执行中断（进程退出或卡住）；已采到的样本保留，为避免重复计费不自动重跑"
		}
		return model.AuditResultFailed, msg
	default:
		if msg == "" {
			msg = "对照失败；已采到的样本保留"
		}
		return model.AuditResultFailed, msg
	}
}

// hlwyIsExecution 只接受报告支持的那六个执行状态。
func hlwyIsExecution(v string) bool {
	switch v {
	case model.HlwyExecutionSampling, model.HlwyExecutionComplete,
		model.HlwyExecutionCanceled, model.HlwyExecutionTimedOut,
		model.HlwyExecutionFailed, model.HlwyExecutionInterrupted:
		return true
	default:
		return false
	}
}

// HlwySanitizeStopReason 只接受我们自己的类别词。
//
// stop_reason 会进报告、进列表、被前端展示。上游的响应正文、URL、密钥一个字节都
// 不能顺着它漏出去，因此这里是**白名单**而不是字符集过滤：拼写不一致的那个（比如
// 把上游一句话塞进 stop_reason）只会得到 unknown_error，不会原样落库。
func HlwySanitizeStopReason(v string) string {
	trimmed := strings.TrimSpace(v)
	switch trimmed {
	case "",
		HlwyStopPromptMismatch, HlwyStopProtocolMismatch, HlwyStopConditionsChanged,
		HlwyStopCompareFailed, HlwyStopRequestRejected,
		"canceled", "interrupted", "run_timeout", "consecutive_failures",
		"reserve_failed", "settle_failed",
		"temperature_changed", "top_p_changed", "stream_changed", "max_tokens_changed",
		"thinking_changed", "reasoning_effort_changed", "system_changed", "tools_changed":
		return trimmed
	}
	// 上游状态码只接受 http_<4xx>：那是我们自己根据状态码拼的类别词。
	if code, ok := strings.CutPrefix(trimmed, "http_"); ok && len(code) == 3 &&
		code[0] == '4' && code[1] >= '0' && code[1] <= '9' && code[2] >= '0' && code[2] <= '9' {
		return trimmed
	}
	return "unknown_error"
}

// HlwyRenewOwnership 续租。返回 false 表示这行已经不该由本次运行继续跑了
// （被回收、租约过期、或已经不是 running），调用方应当立刻停手。
//
// 只推后到期时间：绝不把已过期的租约「补」回来——那就等于让一个可能已经停摆的
// 运行把回收路径刚写下的终态又盖回去。
//
// 续租本身也要先拿任务行锁（与回收同一把、同一顺序）：否则「读到未过期」与
// 「回收读到已过期」可以交错，回收把刚续上的行写成终态，而这次运行还以为自己
// 还持有它。拿到锁之后再取时间，避免用到阻塞前的旧时间。
func HlwyRenewOwnership(ctx context.Context, claim HlwyClaim, until time.Time) bool {
	if claim.ResultID <= 0 || claim.JobID <= 0 || until.IsZero() {
		return false
	}
	renewed := false
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := hlwyLockJob(tx, claim.JobID); err != nil {
			return err
		}
		now := time.Now()
		upd := tx.Model(&model.ModelAuditResult{}).
			Where("id = ? AND status = ? AND attempt = ?",
				claim.ResultID, model.AuditResultRunning, claim.Attempt).
			Where("next_retry_at IS NOT NULL AND next_retry_at > ?", now).
			Update("next_retry_at", until)
		if upd.Error != nil {
			return upd.Error
		}
		renewed = upd.RowsAffected > 0
		return nil
	})
	return err == nil && renewed
}

// HlwyFailWithoutRun 把一条**一次上游请求都没发过**的结果收成终态失败。
//
// 只用在领取之前的失败（选项坏了、渠道没了、sender 建不起来）：此时没有任何采样
// 证据，留空报告比伪造一份 planned=N、counts 全 0 的报告诚实——后者看起来像
// 「跑了但一个都没采到」，会让人以为上游真的没回答。
//
// 只改 pending 的行：如果另一个执行者已经把它领走（running），它自己会收尾，
// 这里插手只会把一份正在增长的证据链打破。任务已取消则什么都不写（取消赢）。
func HlwyFailWithoutRun(ctx context.Context, resultID int64, errMsg string, now time.Time) error {
	if resultID <= 0 {
		return errors.New("缺少检测结果 id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	var res model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).
		Select("id", "job_id").
		Where("id = ?", resultID).First(&res).Error; err != nil {
		return err
	}
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if res.JobID > 0 {
			locked, err := hlwyLockJob(tx, res.JobID)
			if err != nil {
				return err
			}
			if locked.Status == model.AuditJobCanceled {
				// 取消路径已经把待跑的行收尾了，这里不再改写。
				return nil
			}
		}
		msg := strings.TrimSpace(errMsg)
		if msg == "" {
			msg = "对照未能开始"
		}
		upd := tx.Model(&model.ModelAuditResult{}).
			Where("id = ? AND status = ?", res.ID, model.AuditResultPending).
			Updates(map[string]any{
				"status":        model.AuditResultFailed,
				"error_msg":     msg,
				"next_retry_at": nil,
				"finished_at":   now,
			})
		return upd.Error
	})
	if err != nil {
		return err
	}
	if res.JobID > 0 {
		ModelAuditJobProgress(ctx, res.JobID)
	}
	return nil
}

// HlwyDeferPending 把一条**还没被领取**的结果推后，等下一次轮询再试。
//
// 让位不涨 attempt、不写 status：它既不是失败也不是重新开始，只是「这次没轮到」。
// 带 status=pending 谓词是必须的：只按 id 写会让一个已经跑完/取消的行被复活成
// 待跑，下一轮就再花一遍钱。任务已取消则直接告诉调用方别再等了。
func HlwyDeferPending(ctx context.Context, resultID int64, reason string, delay time.Duration, now time.Time) error {
	if resultID <= 0 {
		return errors.New("缺少检测结果 id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if delay <= 0 {
		delay = AuditRetryBackoff(1)
	}
	var res model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).
		Select("id", "job_id").
		Where("id = ?", resultID).First(&res).Error; err != nil {
		return err
	}
	return db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if res.JobID > 0 {
			locked, err := hlwyLockJob(tx, res.JobID)
			if err != nil {
				return err
			}
			if locked.Status == model.AuditJobCanceled {
				return ErrHlwyJobCanceled
			}
		}
		upd := tx.Model(&model.ModelAuditResult{}).
			Where("id = ? AND status = ?", res.ID, model.AuditResultPending).
			Updates(map[string]any{
				"next_retry_at": now.Add(delay),
				"error_msg":     strings.TrimSpace(reason),
			})
		return upd.Error
	})
}

// hlwyLockJob 是所有 hlwy 写路径的**第一条**语句：对任务行做一次自赋值 UPDATE。
//
// `UPDATE ... SET status = status` 在任何引擎上都是写操作：SQLite 拿到库级写锁，
// MySQL/Postgres 拿到行锁，于是「取消 / 预留 / 落定 / 收尾」被串行化，不需要再
// 引入一个 owner 列或一把进程内锁（进程内锁跳不出本进程，也跳不出副本）。
//
// 不用 RowsAffected 判存在性（自赋值可以报 0 行）：语句成功后再 SELECT 确认。
func hlwyLockJob(tx *gorm.DB, jobID int64) (model.ModelAuditJob, error) {
	var job model.ModelAuditJob
	if jobID <= 0 {
		return job, errors.New("缺少任务 id")
	}
	if err := tx.Exec("UPDATE model_audit_jobs SET status = status WHERE id = ?", jobID).Error; err != nil {
		return job, err
	}
	if err := tx.Where("id = ?", jobID).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return job, fmt.Errorf("检测任务 #%d 不存在", jobID)
		}
		return job, err
	}
	return job, nil
}

// hlwyLoadOwned 在事务内重读结果行并校验所有权，返回可改写的报告副本。
func hlwyLoadOwned(tx *gorm.DB, claim HlwyClaim, now time.Time) (model.ModelAuditResult, model.HlwyAuditReport, error) {
	var res model.ModelAuditResult
	if err := tx.Where("id = ?", claim.ResultID).First(&res).Error; err != nil {
		return res, model.HlwyAuditReport{}, err
	}
	if res.Status != model.AuditResultRunning || res.Attempt != claim.Attempt {
		return res, model.HlwyAuditReport{}, ErrHlwyOwnershipLost
	}
	if res.NextRetryAt == nil || !res.NextRetryAt.After(now) {
		// 租约已过期：回收路径随时会接手，这里绝不再写。
		return res, model.HlwyAuditReport{}, ErrHlwyOwnershipLost
	}
	rep, err := model.ParseHlwyReport(res.Report)
	if err != nil {
		return res, model.HlwyAuditReport{}, fmt.Errorf("结果 #%d 的报告不可解析: %w", res.ID, err)
	}
	return res, rep, nil
}

// hlwyOwnedQuery 是所有运行期写共用的一条所有权断言。
//
// 除了 status/attempt/租约，还带 report 的 CAS（token 是上一次成功落库的字符串）：
// 万一有哪条路径绕过任务行锁直接改了报告，这里会以 0 行受影响把它挡下来，而不是把
// 别人的写入覆盖掉。
func hlwyOwnedQuery(tx *gorm.DB, claim HlwyClaim, now time.Time, token string) *gorm.DB {
	q := tx.Model(&model.ModelAuditResult{}).
		Where("id = ? AND status = ?", claim.ResultID, model.AuditResultRunning).
		Where("attempt = ?", claim.Attempt).
		Where("next_retry_at IS NOT NULL AND next_retry_at > ?", now)
	if token != "" {
		q = q.Where("report = ?", token)
	}
	return q
}

// hlwyCheckCounts 校验采样账目的恒等式（报告 Validate 也会查，这里提前拦）。
//
// 恒等式 started = valid + invalid + failed + unresolved 是整套记账的地基：
// 它一旦不成立，「花了多少钱」和「买到了多少证据」就再也对不上账。
func hlwyCheckCounts(c model.HlwyCounts, samples int) error {
	if c.PlannedCalls != samples {
		return fmt.Errorf("计划调用 %d 与本轮采样次数 %d 不符", c.PlannedCalls, samples)
	}
	for _, kv := range []struct {
		name string
		v    int
	}{
		{"started_slots", c.StartedSlots}, {"valid", c.Valid}, {"invalid", c.Invalid},
		{"failed", c.Failed}, {"unresolved", c.Unresolved},
	} {
		if kv.v < 0 {
			return fmt.Errorf("%s=%d 为负数", kv.name, kv.v)
		}
		if kv.v > c.PlannedCalls {
			return fmt.Errorf("%s=%d 超过计划调用 %d", kv.name, kv.v, c.PlannedCalls)
		}
	}
	if c.StartedSlots != c.Valid+c.Invalid+c.Failed+c.Unresolved {
		return fmt.Errorf("采样计数不自洽: started=%d ≠ valid+invalid+failed+unresolved=%d",
			c.StartedSlots, c.Valid+c.Invalid+c.Failed+c.Unresolved)
	}
	return nil
}

// hlwy 采样过程中的停止原因。名字会进报告并随库保存，因此是稳定的对外字符串。
const (
	// HlwyStopPromptMismatch 实际发出去的题目与我们预期的不一样。
	HlwyStopPromptMismatch = "prompt_mismatch"
	// HlwyStopProtocolMismatch 同一轮里观测到的出站协议变了。
	HlwyStopProtocolMismatch = "protocol_mismatch"
	// HlwyStopConditionsChanged 同一轮里观测到的采样参数变了。
	HlwyStopConditionsChanged = "conditions_changed"
	// HlwyStopRequestRejected 上游明确拒收这个请求（非 429 的其余 4xx）。
	//
	// 与 http_<status> 共用一条路径，只是不给未知的 4xx 转一个看起来像
	// 已知原因的状态码（比如配置错导致的 405 不是「上游限流」）。
	HlwyStopRequestRejected = "request_rejected"
	// HlwyStopCompareFailed 按冻结快照算分失败（库里数据坏了）。
	HlwyStopCompareFailed = "compare_failed"
)

// hlwyInvalidatingStopReasons 是让整轮证据失去可比性的停止原因。
//
// 一旦出现，即使样本够、即使基准侧条件未知，也不给分：这几类原因的意思是
// 「这一轮的数字不是同一个实验的产物」，拿它们去算相似度是在给随机分布排名。
// 判定只依赖报告里已有的 stop_reason，所以崩溃后重算得出同样结论。
var hlwyInvalidatingStopReasons = map[string]struct{}{
	HlwyStopPromptMismatch:    {},
	HlwyStopProtocolMismatch:  {},
	HlwyStopConditionsChanged: {},
	HlwyStopCompareFailed:     {},
}

// hlwyInvalidatesEvidence 判断这个停止原因是否让证据不可比。
func hlwyInvalidatesEvidence(stopReason string) bool {
	_, ok := hlwyInvalidatingStopReasons[strings.TrimSpace(stopReason)]
	return ok
}

// HlwyNotScorableReason 给出「样本够但不给分」时的原因码。
//
// 这时 numberdist 不会给原因（从它的角度看分布是能算的），但报告层要求不可评分
// 必须说明原因，而且理由必须与 insufficient_samples 分开：前者再采样也没用。
func HlwyNotScorableReason(comparability string) string {
	if comparability == model.HlwyComparabilityIncompatible {
		return model.HlwyReasonIncompatibleProtocol
	}
	return "not_scorable"
}

// HlwyUnknownConditions 是「什么都还没观测到」的条件快照。
//
// 三态字段必须是 unknown 而不是空串：空串过不了 ValidateHlwyConditions，而且
// 「没观测到」与「没有这个字段」是两件不同的事，混在一起就解释不了可比性。
// 协议与三个标记由我们自己决定（不从响应里猜），其余如实记进 unknown_fields。
//
// 题目哈希也属于「观测才知道」的那一组：这里不能填上我们自己的哈希——那是
// 「我打算发什么」，不是「实际发出去的是什么」，混为一谈会让一份从没观测过
// 题目的报告声称自己确认过题目。
func HlwyUnknownConditions(protocol string) numberdist.Conditions {
	return numberdist.Conditions{
		Protocol:         strings.TrimSpace(protocol),
		ProtocolID:       numberdist.ProtocolID,
		ParserVersion:    numberdist.ParserVersion,
		TransportProfile: numberdist.TransportProfile,
		TemperatureState: numberdist.StateUnknown,
		TopPState:        numberdist.StateUnknown,
		Completeness:     "unknown",
		UnknownFields: []string{
			"prompt", "temperature", "top_p", "stream", "max_tokens",
			"thinking", "reasoning_effort", "system", "tools",
		},
	}
}

// HlwyMergeConditions 把一次真实观测并进报告的条件快照。
//
// 三条纪律：
//   - 我们自己的标记（协议号/解析版本/传输形态）一律以本轮的为准，观测不能改写；
//   - 本次观测到的值优先；本次没观测到但之前确认过的，保留旧值**并把这个字段
//     标回未知**：一个字段只有在每一次采样都确认过时，整批才配称已知；
//   - unknown_fields 取并集（本次已确认的字段从中剔除）：错误的“一致”比诚实
//     的“未知”危险得多。
func HlwyMergeConditions(base, observed numberdist.Conditions) numberdist.Conditions {
	unknown := make([]string, 0, 12)
	seen := make(map[string]struct{}, 12)
	addUnknown := func(field string) {
		field = strings.TrimSpace(field)
		if field == "" {
			return
		}
		if _, dup := seen[field]; dup {
			return
		}
		seen[field] = struct{}{}
		unknown = append(unknown, field)
	}
	// 本次真正观测到的字段：它们从「未知」名单里剔除。
	confirmed := make(map[string]struct{}, 9)
	confirm := func(field string, ok bool) {
		if ok {
			confirmed[field] = struct{}{}
		}
	}
	confirm("prompt", hlwyKnownString(observed.PromptSHA256))
	confirm("temperature", observed.TemperatureState == numberdist.StateValue)
	confirm("top_p", observed.TopPState == numberdist.StateValue)
	confirm("stream", observed.Stream != nil)
	confirm("max_tokens", observed.MaxTokens != nil || hlwyKnownString(observed.MaxTokenField))
	confirm("thinking", hlwyKnownString(observed.Thinking))
	confirm("reasoning_effort", hlwyKnownString(observed.ReasoningEffort))
	confirm("system", observed.SystemInjected != nil)
	confirm("tools", observed.ToolsInjected != nil)

	keep := func(field string) {
		if _, ok := confirmed[field]; ok {
			return
		}
		addUnknown(field)
	}
	keepString := func(field, obs, baseV string) string {
		if hlwyKnownString(obs) {
			return obs
		}
		if hlwyKnownString(baseV) {
			keep(field)
			return baseV
		}
		return obs
	}
	keepFloat := func(field, state string, v *float64, baseState string, baseV *float64) (string, *float64) {
		if state == numberdist.StateValue {
			return state, v
		}
		if baseState == numberdist.StateValue {
			keep(field)
			return baseState, baseV
		}
		return state, v
	}
	keepBool := func(field string, obs, baseV *bool) *bool {
		if obs != nil {
			return obs
		}
		if baseV != nil {
			keep(field)
			return baseV
		}
		return nil
	}

	out := observed
	out.Protocol = base.Protocol
	out.ProtocolID = base.ProtocolID
	out.ParserVersion = base.ParserVersion
	out.TransportProfile = base.TransportProfile
	out.PromptSHA256 = keepString("prompt", observed.PromptSHA256, base.PromptSHA256)
	out.TemperatureState, out.Temperature = keepFloat("temperature", observed.TemperatureState,
		observed.Temperature, base.TemperatureState, base.Temperature)
	out.TopPState, out.TopP = keepFloat("top_p", observed.TopPState, observed.TopP,
		base.TopPState, base.TopP)
	out.Stream = keepBool("stream", observed.Stream, base.Stream)
	out.MaxTokens = observed.MaxTokens
	if out.MaxTokens == nil && base.MaxTokens != nil {
		keep("max_tokens")
		out.MaxTokens = base.MaxTokens
	}
	out.MaxTokenField = keepString("max_tokens", observed.MaxTokenField, base.MaxTokenField)
	out.Thinking = keepString("thinking", observed.Thinking, base.Thinking)
	out.ReasoningEffort = keepString("reasoning_effort", observed.ReasoningEffort, base.ReasoningEffort)
	out.SystemInjected = keepBool("system", observed.SystemInjected, base.SystemInjected)
	out.ToolsInjected = keepBool("tools", observed.ToolsInjected, base.ToolsInjected)

	for _, list := range [][]string{base.UnknownFields, observed.UnknownFields} {
		for _, f := range list {
			if _, ok := confirmed[strings.TrimSpace(f)]; ok {
				continue
			}
			addUnknown(f)
		}
	}
	out.UnknownFields = unknown
	switch {
	case len(unknown) == 0:
		out.Completeness = "complete"
	case base.Completeness == "unknown" && len(confirmed) == 0:
		out.Completeness = "unknown"
	default:
		out.Completeness = "partial"
	}
	return out
}

// HlwyPromptMismatch 判断观测到的题目哈希是否与我们发出的题目不符。
//
// 空哈希（拿不到请求体、或请求被改写后无法定位唯一用户消息）表示观测不到，
// 不算不符；“unknown”字样同理。只有观测到一个不同的哈希才算——那是硬证据：
// 这一轮发出去的不是我们以为的那道题，后面的数字与基准无关了。
func HlwyPromptMismatch(expected string, observed numberdist.Conditions) bool {
	got := strings.TrimSpace(observed.PromptSHA256)
	if !hlwyKnownString(got) {
		return false
	}
	return !strings.EqualFold(got, strings.TrimSpace(expected))
}

// HlwyConditionsDrift 判断同一轮里后一次观测是否已经偏离前一次。
//
// 只比对**两边都已知**的字段：未知不是变化，把未知当变化会让每一次观测都变成
// 异常。返回空串表示没漂移，否则返回原因码（直接当 stop_reason 用）。
func HlwyConditionsDrift(prev, next numberdist.Conditions) string {
	if s := hlwyFloatDrift("temperature", prev.TemperatureState, prev.Temperature,
		next.TemperatureState, next.Temperature); s != "" {
		return s
	}
	if s := hlwyFloatDrift("top_p", prev.TopPState, prev.TopP,
		next.TopPState, next.TopP); s != "" {
		return s
	}
	if prev.Stream != nil && next.Stream != nil && *prev.Stream != *next.Stream {
		return "stream_changed"
	}
	if prev.MaxTokens != nil && next.MaxTokens != nil && *prev.MaxTokens != *next.MaxTokens {
		return "max_tokens_changed"
	}
	if prev.MaxTokenField != "" && next.MaxTokenField != "" &&
		prev.MaxTokenField != next.MaxTokenField {
		return "max_tokens_changed"
	}
	if hlwyKnownString(prev.Thinking) && hlwyKnownString(next.Thinking) &&
		!strings.EqualFold(prev.Thinking, next.Thinking) {
		return "thinking_changed"
	}
	if hlwyKnownString(prev.ReasoningEffort) && hlwyKnownString(next.ReasoningEffort) &&
		!strings.EqualFold(prev.ReasoningEffort, next.ReasoningEffort) {
		return "reasoning_effort_changed"
	}
	if prev.SystemInjected != nil && next.SystemInjected != nil &&
		*prev.SystemInjected != *next.SystemInjected {
		return "system_changed"
	}
	if prev.ToolsInjected != nil && next.ToolsInjected != nil &&
		*prev.ToolsInjected != *next.ToolsInjected {
		return "tools_changed"
	}
	if hlwyKnownString(prev.Protocol) && hlwyKnownString(next.Protocol) &&
		!strings.EqualFold(prev.Protocol, next.Protocol) {
		return HlwyStopProtocolMismatch
	}
	return ""
}

// hlwyFloatDrift 比对两个三态浮点字段，返回 "<name>_changed" 或空串。
func hlwyFloatDrift(name, prevState string, prev *float64, nextState string, next *float64) string {
	if prevState != numberdist.StateValue || nextState != numberdist.StateValue {
		return ""
	}
	if prev == nil || next == nil {
		return ""
	}
	if *prev == *next {
		return ""
	}
	return name + "_changed"
}

// hlwyKnownString 判断一个字符串字段是否真的观测到过值。
// “unknown”与空串同义，都是「没观测到」。
func hlwyKnownString(v string) bool {
	t := strings.TrimSpace(v)
	return t != "" && !strings.EqualFold(t, numberdist.StateUnknown)
}

// hlwyRefreshReport 从 raw_numbers 重算全部派生字段。
//
// 每一次落库（初始、预留、落定、收尾、回收）都要走这里，理由很直接：库里任何
// 一份报告都可能变成「最终的那一份」（进程随时可能消失），因此**每一次写入都
// 必须自洽**，而不是等收尾时再补齐。代价是每格多算一次 355 桶分布与最多 7 份
// 分数，相对一次真实上游请求可以忽略。
func hlwyRefreshReport(rep *model.HlwyAuditReport, opts model.HlwyOptions) {
	bc := &rep.BaselineComparison
	rep.Kind = model.AuditKindHlwy
	rep.Score = 0
	rep.Verdict = model.HlwyVerdictUnknown
	bc.SchemaVersion = numberdist.SchemaVersion
	bc.AlgorithmVersion = numberdist.AlgorithmVer
	bc.ValidCount = len(bc.RawNumbers)
	bc.ChosenBaselineID = opts.BaselineID
	// 快照只在第一次（报告里还没有快照时）从冻结选项拷进来，之后一律沿用库里那份：
	// 历史证据不能因为「目录变了」而被改写。
	if len(bc.BaselineSnapshots) == 0 && len(opts.Candidates) > 0 {
		snaps := make([]numberdist.Baseline, 0, len(opts.Candidates))
		for _, b := range opts.Candidates {
			snaps = append(snaps, model.HlwySnapshotOf(b))
		}
		bc.BaselineSnapshots = snaps
	}

	comparability := model.HlwyComparabilityIncompatible
	codes := []string{"chosen_baseline_missing"}
	reason := "chosen_baseline_missing"
	matches := []numberdist.ScoreResult{}
	if chosen, ok := opts.ChosenBaseline(); ok {
		comparability, codes = model.HlwyComparability(bc.Conditions, chosen.Conditions)
		if cmp, err := numberdist.Compare(bc.RawNumbers, opts.Candidates, chosen.ID); err != nil {
			// 冻结的候选在入队时逐份校验过，走到这里说明库里的数据坏了：
			// 既不给分，也不谎称两边规则一致。
			comparability = model.HlwyComparabilityIncompatible
			reason = HlwyStopCompareFailed
			if strings.TrimSpace(bc.StopReason) == "" {
				bc.StopReason = HlwyStopCompareFailed
			}
		} else {
			bc.Distribution = cmp.Distribution
			bc.Stats = cmp.Stats
			bc.ValidCount = cmp.ValidCount
			reason = cmp.Reason
			matches = append(matches, cmp.Matches...)
		}
	}
	// 本轮观测到「题目/条件已经不是同一套」时，整轮证据不可比：即使样本够、
	// 基准侧条件未知，也不能给分。类别留在 stop_reason 上随报告落库，
	// 因此崩溃后重算得到同样结论。
	if hlwyInvalidatesEvidence(bc.StopReason) {
		comparability = model.HlwyComparabilityIncompatible
		codes = append(codes, strings.TrimSpace(bc.StopReason))
	}
	bc.Comparability = comparability
	bc.ReasonCodes = codes
	// 不可比的理由优先于“样本不够”：两者处置相反，写成 insufficient_samples
	// 会让人以为多采几次就有分。
	if comparability == model.HlwyComparabilityIncompatible &&
		(reason == "" || reason == "insufficient_samples") {
		reason = model.HlwyReasonIncompatibleProtocol
	}
	bc.Scorable = model.HlwyScorable(bc.ValidCount, comparability)
	if !bc.Scorable {
		// 不可评分就绝不下发分数：一份「不兼容但排名第一」的表格
		// 一定会被当成结论看。
		matches = []numberdist.ScoreResult{}
		if strings.TrimSpace(reason) == "" {
			reason = HlwyNotScorableReason(comparability)
		}
	}
	bc.Reason = reason
	bc.Matches = matches
	bc.Sufficiency = model.HlwySufficiency(bc.ValidCount)
	rep.Plain = model.HlwyPlainOf(*bc, bc.SelfCompare)
}

// hlwyInitialReport 构造领取时的初始报告：计划已知、证据为空。
//
// 它必须此刻就能通过 Validate：这份报告会在崩溃后被回收路径当作终态的起点，
// 一份「要等跑完才自洽」的初始报告等于把一个坏快照留在了库里。
func hlwyInitialReport(res model.ModelAuditResult, opts model.HlwyOptions,
	upstreamModel string, conditions numberdist.Conditions, selfCompare bool) model.HlwyAuditReport {
	rep := model.HlwyAuditReport{
		Kind:           model.AuditKindHlwy,
		RequestedModel: hlwyResultModelName(res),
		UpstreamModel:  strings.TrimSpace(upstreamModel),
		Verdict:        model.HlwyVerdictUnknown,
		BaselineComparison: model.HlwyBaselineComparison{
			Execution:   model.HlwyExecutionSampling,
			Counts:      model.HlwyCounts{PlannedCalls: opts.Samples},
			Conditions:  conditions,
			RawNumbers:  []int{},
			SelfCompare: selfCompare,
		},
	}
	hlwyRefreshReport(&rep, opts)
	return rep
}

// hlwyCanceledReport 给「还没开跑就被取消」的结果造一份初始 canceled 报告。
//
// 第二个返回值 false 表示冻结选项不可用（存量数据坏了）：这时只标 skipped，
// 不伪造报告——一份 counts 全 0 的报告会被当成「跑了但一个都没采到」。
func hlwyCanceledReport(res model.ModelAuditResult, opts model.HlwyOptions) (string, bool) {
	if opts.Samples != model.HlwyDefaultSamples && opts.Samples != model.HlwyMaxSamples {
		return "", false
	}
	if opts.BaselineID == "" || len(opts.Candidates) == 0 {
		return "", false
	}
	if err := opts.Validate(); err != nil {
		return "", false
	}
	rep := model.HlwyAuditReport{
		Kind:           model.AuditKindHlwy,
		RequestedModel: hlwyResultModelName(res),
		UpstreamModel:  strings.TrimSpace(res.UpstreamModel),
		Verdict:        model.HlwyVerdictUnknown,
		BaselineComparison: model.HlwyBaselineComparison{
			Execution:  model.HlwyExecutionCanceled,
			StopReason: "canceled",
			Counts:     model.HlwyCounts{PlannedCalls: opts.Samples},
			Conditions: HlwyUnknownConditions(""),
			RawNumbers: []int{},
		},
	}
	hlwyRefreshReport(&rep, opts)
	if err := rep.Validate(); err != nil {
		return "", false
	}
	out, err := rep.Encode()
	if err != nil {
		return "", false
	}
	return out, true
}

// hlwyJobByID 读一个任务行。
func hlwyJobByID(ctx context.Context, jobID int64) (model.ModelAuditJob, error) {
	var job model.ModelAuditJob
	if jobID <= 0 {
		return job, errors.New("缺少任务 id")
	}
	if err := db.GetDB().WithContext(ctx).Where("id = ?", jobID).First(&job).Error; err != nil {
		return job, err
	}
	return job, nil
}

// hlwyJobKindOfID 读任务种类。读失败返回 error，而不是空串。
//
// 读不到就退回旧路径，等于让一次库抖动把真实任务的处理默默吞掉；取消/回收必须
// 能区分「读不到」与「不是 hlwy」，宁可不处理也不能误处理。
func hlwyJobKindOfID(ctx context.Context, jobID int64) (string, error) {
	job, err := hlwyJobByID(ctx, jobID)
	if err != nil {
		return "", err
	}
	return hlwyJobKind(job), nil
}

// hlwyReclaimExpiredResult 把一条租约过期的 hlwy 结果收敛成终态。
//
// 第一个返回值表示「这行是不是 hlwy」，第二个表示「有没有写到东西」。
//
// 与旧检测共享的原则：过期一律终止，**不自动重跑**。hlwy 还有两处额外约束：
//   - 不碰 attempt：这一轮并没有重新开始，改 attempt 会让“所有权代号”失真；
//   - 报告按已采到的证据重新收尾（execution=interrupted/canceled），而不是
//     把已经付过钱的样本抹掉。报告坏到不能解析时只标失败、保留原文，
//     绝不拿一份“修正过”的报告去覆盖它。
func hlwyReclaimExpiredResult(ctx context.Context, res model.ModelAuditResult, now time.Time) (bool, bool) {
	if res.JobID <= 0 {
		return false, false
	}
	kind, err := hlwyJobKindOfID(ctx, res.JobID)
	if err != nil {
		// 读不到任务行就不猜：本轮不动它，也不退回旧路径（旧路径会把一份
		// hlwy 报告当普通检测结果收尾），留给下一轮。
		return true, false
	}
	if kind != model.AuditKindHlwy {
		return false, false
	}
	touched := false
	err = db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := hlwyLockJob(tx, res.JobID)
		if err != nil {
			return err
		}
		var cur model.ModelAuditResult
		if err := tx.Where("id = ?", res.ID).First(&cur).Error; err != nil {
			return err
		}
		// 拿到任务行锁之后再取时间：之前读到、可能已经过期很久的那个时间不能
		// 用来判「租约已到期」——否则会把刚续租成功的行误收成终态。
		at := time.Now()
		// 复查：期间它可能已经自己收尾、或续租成功了。
		if cur.Status != model.AuditResultRunning {
			return nil
		}
		if cur.NextRetryAt != nil && cur.NextRetryAt.After(at) {
			return nil
		}
		execution := model.HlwyExecutionInterrupted
		stopReason := "interrupted"
		status := model.AuditResultFailed
		msg := "执行中断（进程退出或卡住）；已采到的样本保留，为避免重复计费不自动重跑"
		if locked.Status == model.AuditJobCanceled {
			execution = model.HlwyExecutionCanceled
			stopReason = "canceled"
			status = model.AuditResultSkipped
			msg = "已取消；已采到的样本保留，取消不重跑"
		}
		report := cur.Report
		if opts, oerr := model.ParseHlwyOptions(locked.OptionsJSON); oerr == nil {
			if rep, rerr := model.ParseHlwyReport(cur.Report); rerr == nil {
				// 报告里已经记下「本轮不可比」的原因时不得改写：回收只改变
				// 「怎么结束的」，不能让一批已经作废的证据重新变得可用。
				if !hlwyInvalidatesEvidence(rep.BaselineComparison.StopReason) {
					rep.BaselineComparison.StopReason = stopReason
				}
				rep.BaselineComparison.Execution = execution
				hlwyRefreshReport(&rep, opts)
				if enc, eerr := rep.Encode(); eerr == nil && rep.Validate() == nil {
					report = enc
				}
			}
		}
		// UPDATE 必须带 attempt 与租约谓词：只查 status 的话，一个刚接管了这行
		// （attempt 已变、租约全新）的运行会被这条语句把它刚写的报告盖掉。
		upd := tx.Model(&model.ModelAuditResult{}).
			Where("id = ? AND status = ? AND attempt = ?", cur.ID, model.AuditResultRunning, cur.Attempt).
			Where("next_retry_at IS NULL OR next_retry_at <= ?", at).
			Updates(map[string]any{
				"status":        status,
				"report":        report,
				"error_msg":     msg,
				"next_retry_at": nil,
				"finished_at":   at,
			})
		if upd.Error != nil {
			return upd.Error
		}
		touched = upd.RowsAffected > 0
		return nil
	})
	if err != nil {
		// 回收是清理路径：一条脏行不能把整个循环拖死，交给下一轮重试。
		touched = false
	}
	return true, touched
}

// hlwyCancelJob 取消一个 hlwy 任务。
//
// 顺序很关键：**先把任务标 canceled**，再去处理待跑的行。正在采样的线程在下一次
// 预留时会看到这个状态并停手；反过来先处理行、再标任务，中间的那一小段时间里
// 采样线程会多花一次钱。
//
// 已经在跑的那一行不动：它的钱可能已经花出去了，让它在下一格之前自己看到取消、
// 把已采到的证据收尾，才能得到一份 counts 自洽的报告。
func hlwyCancelJob(ctx context.Context, jobID int64, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	return db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := hlwyLockJob(tx, jobID)
		if err != nil {
			return err
		}
		if job.Status == model.AuditJobCanceled {
			return nil
		}
		// 已经跑完的任务再点取消不能改成 canceled：那会把一次真实的采样
		// 说成「用户主动放弃」，而它已经在报告里给出了结论。
		if job.Status != model.AuditJobQueued && job.Status != model.AuditJobRunning {
			return nil
		}
		if err := tx.Model(&model.ModelAuditJob{}).
			Where("id = ?", jobID).
			Updates(map[string]any{
				"status":   model.AuditJobCanceled,
				"ended_at": now,
			}).Error; err != nil {
			return err
		}
		var pending []model.ModelAuditResult
		if err := tx.Where("job_id = ? AND status = ?", jobID, model.AuditResultPending).
			Order("id ASC").Find(&pending).Error; err != nil {
			return err
		}
		opts, optsErr := model.ParseHlwyOptions(job.OptionsJSON)
		for _, res := range pending {
			updates := map[string]any{
				"status":        model.AuditResultSkipped,
				"error_msg":     "已取消",
				"next_retry_at": nil,
				"finished_at":   now,
			}
			if optsErr == nil {
				if report, ok := hlwyCanceledReport(res, opts); ok {
					updates["report"] = report
				}
			}
			if err := tx.Model(&model.ModelAuditResult{}).
				Where("id = ? AND status = ?", res.ID, model.AuditResultPending).
				Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// HlwyJobSummary 是 hlwy 对照任务在列表页上的汇总，全部来自那份唯一的报告。
//
// 只搬运报告里已经算好的值：列表一次性渲染几十个任务，在列表路径上重算分数
// 要读回全部快照；两边各算一份还会漂移。
type HlwyJobSummary struct {
	Execution     string           `json:"execution"`
	StopReason    string           `json:"stop_reason,omitempty"`
	Counts        model.HlwyCounts `json:"counts"`
	Sufficiency   string           `json:"sufficiency"`
	Comparability string           `json:"comparability"`
	Scorable      bool             `json:"scorable"`
}

// HlwyJobSummaryOf 从落库报告生成列表汇总；报告不可解析时返回 nil（列表退回旧展示）。
func HlwyJobSummaryOf(raw string) *HlwyJobSummary {
	rep, err := model.ParseHlwyReport(raw)
	if err != nil {
		return nil
	}
	bc := rep.BaselineComparison
	return &HlwyJobSummary{
		Execution:     bc.Execution,
		StopReason:    bc.StopReason,
		Counts:        bc.Counts,
		Sufficiency:   bc.Sufficiency,
		Comparability: bc.Comparability,
		Scorable:      bc.Scorable,
	}
}
