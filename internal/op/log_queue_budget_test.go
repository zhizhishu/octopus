package op

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// 这一组测试守的是「待落库队列的 payload 预算」（relayLogPendingByteBudget）。
//
// 口径先说清楚，免得被误读成更强的承诺：
//   - 64 MiB 是**待落库缓存队列**的保守 payload 估值上限，不是进程内存上限，
//     也不是「全部在途只占 64 MiB」；
//   - flush 在解锁后会持有一份快照，与队列同时存在（最多两份），两边都按同一预算封顶，
//     所以上界是「≤预算 pending + ≤预算 held batch」；
//   - 估计器是保守上界而不是精确 heap 计量：字符串共享、slice 容量、GC 未回收都不算。

// resetRelayLogQueueForTest 把队列与计数器一起清干净（直接改写 relayLogCache 的用例必须
// 走它，否则计数器会与队列漂移）。
func resetRelayLogQueueForTest(t *testing.T) {
	t.Helper()
	relayLogCacheLock.Lock()
	relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
	relayLogPendingBytes = 0
	relayLogPendingDropped = 0
	relayLogDropWarnAt = time.Time{}
	relayLogCacheLock.Unlock()
	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
		relayLogPendingBytes = 0
		relayLogCacheLock.Unlock()
	})
}

// pendingSnapshot 读一份 (行数, 估值字节) 的一致快照。
func pendingSnapshot() (int, int64) {
	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	return len(relayLogCache), relayLogPendingBytes
}

func pendingBytesSumOf(t *testing.T) int64 {
	t.Helper()
	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	var total int64
	for i := range relayLogCache {
		total += relayLogPendingBytesOf(relayLogCache[i])
	}
	return total
}

// ───────────────── 估计器：每个动态字段都计入、零分配 ─────────────────

// 估计器必须把每一个「可能很大」的字段都算进去。这里不声称它 <= json 序列化长度
// （DebugInfo 的 48 KiB 保守定额、结构余量都会打破这种关系），只守三件事：
// 字段计入、slice/条目递增、零分配。
func TestRelayLogPendingBudgetEstimatorCountsEveryDynamicField(t *testing.T) {
	base := relayLogPendingBytesOf(model.RelayLog{})

	big := strings.Repeat("x", 1000)
	cases := map[string]struct {
		mutate  func(*model.RelayLog)
		atLeast int64
	}{
		"request_content":        {func(l *model.RelayLog) { l.RequestContent = big }, int64(len(big))},
		"response_content":       {func(l *model.RelayLog) { l.ResponseContent = big }, int64(len(big))},
		"upstream_request_body":  {func(l *model.RelayLog) { l.UpstreamRequestBody = big }, int64(len(big))},
		"error":                  {func(l *model.RelayLog) { l.Error = big }, int64(len(big))},
		"model_audit_report":     {func(l *model.RelayLog) { l.ModelAuditReport = big }, int64(len(big))},
		"request_path":           {func(l *model.RelayLog) { l.RequestPath = big }, int64(len(big))},
		"request_user_agent":     {func(l *model.RelayLog) { l.RequestUserAgent = big }, int64(len(big))},
		"access_plan_name":       {func(l *model.RelayLog) { l.AccessPlanName = big }, int64(len(big))},
		"session_key":            {func(l *model.RelayLog) { l.SessionKey = big }, int64(len(big))},
		"prompt_override_source": {func(l *model.RelayLog) { l.PromptOverrideSources = []string{big} }, int64(len(big))},
		// ImageHashes 是 gorm:"-" 不落库，但落库前确实在内存里，必须计入。
		"image_hashes": {func(l *model.RelayLog) {
			l.ImageHashes = []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
		}, 128},
		"attempts": {func(l *model.RelayLog) {
			l.Attempts = []model.ChannelAttempt{
				{ChannelName: big, Msg: big},
				{ModelName: big},
			}
		}, int64(len(big)) * 3},
		"debug_info": {func(l *model.RelayLog) {
			l.DebugInfo = &model.RelayLogDebugInfo{}
		}, model.RelayLogDebugMaxTotalBytes},
		"passive_summary": {func(l *model.RelayLog) {
			l.PassiveSummary = &model.RelayLogPassiveSummary{
				Version: "v2", Status: "insufficient_evidence", Reason: "blank",
				Features: &model.RelayLogPassiveFeatures{ServiceTier: big},
			}
		}, int64(len(big))},
		"passive_probes": {func(l *model.RelayLog) {
			l.PassiveProbes = []model.RelayProbeFinding{
				{Code: "token_arithmetic", Title: big, Severity: "medium", Confidence: "high",
					Evidence: map[string]any{"a": 1, "b": 2}},
				{Code: "cache_exceeds_prompt", Title: big},
			}
		}, int64(len(big)) * 2},
		"passive_source": {func(l *model.RelayLog) {
			l.PassiveSource = &model.RelayLogPassiveSource{
				Signals:         []string{big},
				CounterEvidence: []string{big},
				HeaderFamilies:  []string{big},
				Hints:           []model.RelayLogSourceHint{{Platform: "aws", Strength: "weak", Code: big, Evidence: big}},
			}
		}, int64(len(big)) * 4},
	}

	for name, tc := range cases {
		var l model.RelayLog
		tc.mutate(&l)
		delta := relayLogPendingBytesOf(l) - base
		if delta < tc.atLeast {
			t.Errorf("%s: 估计增量 %d < 至少应计入 %d（该字段没被计入）", name, delta, tc.atLeast)
		}
	}

	// 条目数递增：多为一条 attempt / 一条 probe，估计必须变大。
	many := model.RelayLog{}
	for i := 0; i < 5; i++ {
		many.Attempts = append(many.Attempts, model.ChannelAttempt{ChannelName: "c"})
	}
	few := many
	few.Attempts = many.Attempts[:1]
	if relayLogPendingBytesOf(many) <= relayLogPendingBytesOf(few) {
		t.Fatal("attempts 条数增加时估计必须递增")
	}
}

// 请求路径上每来一条日志都要调用它，所以必须是零分配。
func TestRelayLogPendingBudgetEstimatorIsZeroAlloc(t *testing.T) {
	sample := model.RelayLog{
		RequestContent:  strings.Repeat("r", 4096),
		ResponseContent: strings.Repeat("s", 4096),
		Error:           "boom",
		Attempts:        []model.ChannelAttempt{{ChannelName: "c", Msg: "m"}},
		DebugInfo:       &model.RelayLogDebugInfo{},
		ImageHashes:     []string{"abc"},
		PassiveProbes:   []model.RelayProbeFinding{{Code: "x", Title: "y", Evidence: map[string]any{"k": 1}}},
	}
	if allocs := testing.AllocsPerRun(50, func() { _ = relayLogPendingBytesOf(sample) }); allocs != 0 {
		t.Fatalf("估计器必须零分配，实测 %.2f allocs/op", allocs)
	}
}

// ───────────────── 入队/淘汰：字节预算、行数预算、单行超预算 ─────────────────

// 大行先撞字节预算：丢最旧，保留最新的，计数器不低于 0 且不超预算。
func TestRelayLogPendingBudgetEvictsOldestByBytes(t *testing.T) {
	resetRelayLogQueueForTest(t)

	const rows = 120
	big := strings.Repeat("z", 1<<20) // 1 MiB/行
	for i := 0; i < rows; i++ {
		relayLogAddPending(model.RelayLog{ID: int64(i + 1), ResponseContent: big})
	}

	keptRows, keptBytes := pendingSnapshot()
	if keptBytes > relayLogPendingByteBudget {
		t.Fatalf("pending 估值 %d 超过预算 %d", keptBytes, relayLogPendingByteBudget)
	}
	// 每行 ≈ 1 MiB + 定额，64 MiB 预算下应远少于行数上限。
	if keptRows >= rows {
		t.Fatalf("120 行 × 1 MiB 应该触发字节淘汰，实际保留 %d 行", keptRows)
	}
	if keptRows < 1 {
		t.Fatal("最新的一行必须保留（至少留一行）")
	}
	// 保留的是最新的那几行（队头被丢）。
	relayLogCacheLock.Lock()
	newestKept := relayLogCache[len(relayLogCache)-1].ID
	oldestKept := relayLogCache[0].ID
	relayLogCacheLock.Unlock()
	if newestKept != rows {
		t.Fatalf("最新一行应保留，实际最新保留 ID=%d", newestKept)
	}
	if oldestKept <= 1 {
		t.Fatalf("最旧的几行应被丢弃，实际最旧保留 ID=%d", oldestKept)
	}
	relayLogCacheLock.Lock()
	dropped := relayLogPendingDropped
	relayLogCacheLock.Unlock()
	if dropped != int64(oldestKept-1) {
		t.Fatalf("丢弃条数 %d 与 ID 推断 %d 不一致", dropped, oldestKept-1)
	}
	if got := pendingBytesSumOf(t); got != keptBytes {
		t.Fatalf("计数器 %d 与逐行重算 %d 不一致", keptBytes, got)
	}
}

// 小行行为必须与改动前**完全一致**：行数预算先生效，正好留 500 行。
func TestRelayLogPendingBudgetKeepsRowRuleForSmallRows(t *testing.T) {
	resetRelayLogQueueForTest(t)

	const rows = relayLogPendingMaxSize + 100
	small := strings.Repeat("s", 512)
	for i := 0; i < rows; i++ {
		relayLogAddPending(model.RelayLog{ID: int64(i + 1), ResponseContent: small})
	}

	keptRows, keptBytes := pendingSnapshot()
	if keptRows != relayLogPendingMaxSize {
		t.Fatalf("小行应正好留 %d 行，实际 %d", relayLogPendingMaxSize, keptRows)
	}
	if keptBytes > relayLogPendingByteBudget {
		t.Fatalf("小行不该撞字节预算，实际 %d", keptBytes)
	}
	relayLogCacheLock.Lock()
	dropped := relayLogPendingDropped
	newest := relayLogCache[len(relayLogCache)-1].ID
	relayLogCacheLock.Unlock()
	if dropped != 100 {
		t.Fatalf("应丢 100 条，实际 %d", dropped)
	}
	if newest != rows {
		t.Fatalf("最新一行应保留，实际 %d", newest)
	}
	if got := pendingBytesSumOf(t); got != keptBytes {
		t.Fatalf("计数器 %d 与逐行重算 %d 不一致", keptBytes, got)
	}
}

// 单行自身超预算：明确丢它（并在可观测处体现），计数器归零而不是变负数。
func TestRelayLogPendingBudgetDropsSingleOversizedRow(t *testing.T) {
	resetRelayLogQueueForTest(t)

	huge := strings.Repeat("h", int(relayLogPendingByteBudget)+(1<<20))
	relayLogAddPending(model.RelayLog{ID: 1, ResponseContent: huge})

	keptRows, keptBytes := pendingSnapshot()
	if keptRows != 0 || keptBytes != 0 {
		t.Fatalf("单行超预算应被丢弃，实际 rows=%d bytes=%d", keptRows, keptBytes)
	}
	relayLogCacheLock.Lock()
	dropped := relayLogPendingDropped
	relayLogCacheLock.Unlock()
	if dropped != 1 {
		t.Fatalf("应记 1 条丢弃，实际 %d", dropped)
	}
}

// ───────────────── 快照上限与「两份」上界 ─────────────────

// 快照（held batch）同样按预算截取，入队路径负责拒绝单条超预算。
func TestRelayLogNextBatchCapsSnapshotToBudget(t *testing.T) {
	resetRelayLogQueueForTest(t)

	// 直接摆队列（走 production 入队会被预算淘汰，这里要的是"满队列"的形状）。
	relayLogCacheLock.Lock()
	relayLogCache = make([]model.RelayLog, 0, 48)
	for i := 0; i < 40; i++ {
		relayLogCache = append(relayLogCache, model.RelayLog{ID: int64(i + 1), ResponseContent: strings.Repeat("b", 2<<20)})
	}
	relayLogPendingBytesSyncLocked()
	batch := relayLogNextBatchLocked()
	relayLogCacheLock.Unlock()

	if len(batch) == 0 {
		t.Fatal("快照不能为空")
	}
	var batchBytes int64
	for i := range batch {
		batchBytes += relayLogPendingBytesOf(batch[i])
	}
	if batchBytes > relayLogPendingByteBudget {
		t.Fatalf("快照估值 %d 超过预算 %d", batchBytes, relayLogPendingByteBudget)
	}
	if len(batch) >= 40 {
		t.Fatalf("40 行 × 2 MiB 的快照应按预算截断，实际全取 %d 行", len(batch))
	}
	// 必须是队头前缀。
	for i := range batch {
		if batch[i].ID != int64(i+1) {
			t.Fatalf("快照必须是队头前缀，位置 %d 的 ID=%d", i, batch[i].ID)
		}
	}
}

// 有阻塞 flusher 并发时：in-flight 的快照（held batch）与新入队的 pending 同时存在，
// 两边各自都不超过预算，也就是最多两份。
//
// 用 relayLogFlushLock 模拟"flush 正在跑"：入队路径**从不**碰这把锁，所以并发入队
// 既不会被阻塞，也不会把 held batch 算进 pending。
func TestRelayLogPendingBudgetHoldsWhileFlusherInFlight(t *testing.T) {
	resetRelayLogQueueForTest(t)

	// 模拟在途 flush 已经取走的那一份。
	relayLogAddPending(model.RelayLog{ID: 1, ResponseContent: strings.Repeat("p", 1<<20)})
	relayLogCacheLock.Lock()
	held := relayLogNextBatchLocked()
	relayLogCacheLock.Unlock()
	var heldBytes int64
	for i := range held {
		heldBytes += relayLogPendingBytesOf(held[i])
	}
	if heldBytes > relayLogPendingByteBudget {
		t.Fatalf("held batch 估值 %d 超过预算", heldBytes)
	}

	// 让"flush 在跑"：占住 flush 锁，另一个 goroutine 持续入队大行。
	relayLogFlushLock.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			relayLogAddPending(model.RelayLog{ID: int64(1000 + i), ResponseContent: strings.Repeat("q", 1<<20)})
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		// 用 t.Error 而不是 Fatal：不能让仍在跑的 goroutine 与后续清理抢 relayLogCache。
		t.Error("入队被阻塞了：入队路径不得依赖 flush 锁")
	}
	relayLogFlushLock.Unlock()
	<-done

	keptRows, keptBytes := pendingSnapshot()
	if keptBytes > relayLogPendingByteBudget {
		t.Fatalf("pending 估值 %d 超过预算（held batch %d 另外存在）", keptBytes, heldBytes)
	}
	if keptRows < 1 {
		t.Fatal("pending 不应被清空")
	}
	// 两份各自 ≤ 预算，合计 ≤ 2×预算。
	if total := keptBytes + heldBytes; total > 2*relayLogPendingByteBudget {
		t.Fatalf("两份合计 %d 超过 2×预算 %d", total, 2*relayLogPendingByteBudget)
	}
}

// ───────────────── 计数更新点：flush 成功/失败、清空（全局与按 scope）─────────────────

func TestRelayLogPendingBudgetAccountingAcrossFlushAndClear(t *testing.T) {
	ctx := setupRelayLogTest(t)
	resetRelayLogQueueForTest(t)

	for i := 0; i < 3; i++ {
		relayLogAddPending(model.RelayLog{
			ID:              int64(9000 + i),
			Time:            int64(9000 + i),
			UserID:          1,
			ResponseContent: strings.Repeat("r", 1024),
		})
	}
	before, beforeBytes := pendingSnapshot()
	if before != 3 || beforeBytes <= 0 {
		t.Fatalf("初始 pending 异常: rows=%d bytes=%d", before, beforeBytes)
	}

	// 失败（ctx 已取消）：队列与计数器都不该动。
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := relayLogFlushToDB(canceled); err == nil {
		t.Fatal("已取消的 flush 应返回错误")
	}
	if rows, bytes := pendingSnapshot(); rows != before || bytes != beforeBytes {
		t.Fatalf("flush 失败后不该改队列/计数: rows=%d bytes=%d", rows, bytes)
	}

	// 成功：全部落库，计数器归零。
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if rows, bytes := pendingSnapshot(); rows != 0 || bytes != 0 {
		t.Fatalf("flush 成功后应为空: rows=%d bytes=%d", rows, bytes)
	}

	// 按 scope 清空：只扣掉被清掉的那些行。
	relayLogAddPending(model.RelayLog{ID: 9101, Time: 1, UserID: 1, ResponseContent: strings.Repeat("a", 512)})
	relayLogAddPending(model.RelayLog{ID: 9102, Time: 1, UserID: 2, ResponseContent: strings.Repeat("b", 512)})
	if err := RelayLogClear(ctx, &model.RelayLogScope{UserID: 1}); err != nil {
		t.Fatalf("scoped clear: %v", err)
	}
	rows, bytes := pendingSnapshot()
	if rows != 1 {
		t.Fatalf("按 scope 清空后应剩 1 行，实际 %d", rows)
	}
	if want := pendingBytesSumOf(t); bytes != want {
		t.Fatalf("按 scope 清空后计数器 %d 与重算 %d 不一致", bytes, want)
	}

	// 全局清空：归零。
	if err := RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if rows, bytes := pendingSnapshot(); rows != 0 || bytes != 0 {
		t.Fatalf("全局清空后应为空: rows=%d bytes=%d", rows, bytes)
	}
}

// The row cut must reduce byte accounting before deciding whether another row
// needs eviction. Previously 100 one-MiB rows were all discarded after a 50-row cut.
func TestRelayLogTrimRecountsBeforeByteCut(t *testing.T) {
	resetRelayLogQueueForTest(t)
	row := strings.Repeat("x", 1<<20)
	relayLogCacheLock.Lock()
	for i := 0; i < 100; i++ {
		relayLogCache = append(relayLogCache, model.RelayLog{ID: int64(i + 1), ResponseContent: row})
	}
	relayLogPendingBytesSyncLocked()
	relayLogTrimCacheLocked(50)
	rows, first, bytes := len(relayLogCache), relayLogCache[0].ID, relayLogPendingBytes
	relayLogCacheLock.Unlock()
	if rows != 50 || first != 51 || bytes > relayLogPendingByteBudget || bytes != pendingBytesSumOf(t) {
		t.Fatalf("row cut over-evicted or drifted: rows=%d first=%d bytes=%d", rows, first, bytes)
	}
}

func TestRelayLogScopedClearReleasesBackingReferences(t *testing.T) {
	ctx := setupRelayLogTest(t)
	resetRelayLogQueueForTest(t)
	relayLogAddPending(model.RelayLog{ID: 9201, UserID: 2, ResponseContent: "keep"})
	relayLogAddPending(model.RelayLog{ID: 9202, UserID: 1, ResponseContent: strings.Repeat("private", 1024)})
	relayLogCacheLock.Lock()
	backing := relayLogCache
	relayLogCacheLock.Unlock()
	if err := RelayLogClear(ctx, &model.RelayLogScope{UserID: 1}); err != nil {
		t.Fatal(err)
	}
	if backing[1].ID != 0 || backing[1].ResponseContent != "" {
		t.Fatal("scoped clear retained the removed row beyond slice length")
	}
	if rows, bytes := pendingSnapshot(); rows != 1 || bytes != pendingBytesSumOf(t) {
		t.Fatalf("clear accounting: rows=%d bytes=%d", rows, bytes)
	}
}

func TestRelayLogOversizedIncomingDoesNotEvictValidRows(t *testing.T) {
	resetRelayLogQueueForTest(t)
	relayLogAddPending(model.RelayLog{ID: 1, ResponseContent: "valid"})
	beforeRows, beforeBytes := pendingSnapshot()
	cycle := map[string]any{}
	cycle["self"] = cycle
	relayLogAddPending(model.RelayLog{ID: 2, PassiveProbes: []model.RelayProbeFinding{{Evidence: cycle}}})
	if rows, bytes := pendingSnapshot(); rows != beforeRows || bytes != beforeBytes {
		t.Fatalf("oversized incoming row evicted useful pending logs: rows=%d bytes=%d", rows, bytes)
	}
}

func TestRelayLogEvidencePayloadCountsNestedStringsAndRejectsCycles(t *testing.T) {
	base := relayLogPendingBytesOf(model.RelayLog{})
	big := strings.Repeat("e", 1<<20)
	sample := model.RelayLog{PassiveProbes: []model.RelayProbeFinding{{
		Evidence: map[string]any{big: []any{big, map[string]any{"nested": []string{big}}}},
	}}}
	if got := relayLogPendingBytesOf(sample) - base; got < 3*int64(len(big)) {
		t.Fatalf("nested evidence payload underestimated: %d", got)
	}
	if allocs := testing.AllocsPerRun(50, func() { _ = relayLogPendingBytesOf(sample) }); allocs != 0 {
		t.Fatalf("nested evidence estimator allocated: %v", allocs)
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	sample.PassiveProbes[0].Evidence = cycle
	if got := relayLogPendingBytesOf(sample); got <= relayLogPendingByteBudget {
		t.Fatalf("cyclic evidence did not conservatively exceed budget: %d", got)
	}
}
