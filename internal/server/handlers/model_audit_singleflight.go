package handlers

import (
	"fmt"
	"sync"
)

// modelAuditInFlight is the process-wide single-flight gate for MANUAL model audits.
//
// Why this exists (and why it is not "just" a UI concern):
//   - The detection column is the only place allowed to spend upstream tokens, and every
//     probe is a real billable request. The frontend already has a `inFlight` ref, but its
//     own comment concedes that the disabled attribute "is only visual and depends on
//     render timing". More importantly a ref cannot see OTHER callers: a second admin, a
//     second browser tab, or a direct API call all bypass it. Without a server-side gate,
//     two clicks can run the same audit twice and bill it twice.
//   - The audit contract requires "repeat clicks must not double-charge". This is that
//     guarantee, enforced where it actually holds: in the process that sends the probes.
//
// Scope of the lock is (channel, upstream model), NOT the whole endpoint:
// auditing model A must not block auditing model B on another channel — a global lock
// would turn the detection page into a single-user, single-model tool for no reason.
//
// Deliberately minimal (ponytail): no queue, no retry, no persistence, no TTL sweeper.
// A second caller gets a clear "already running" answer instead of being parked. The
// entry is released by defer as soon as the run returns, so a crashed/returned run can
// never wedge the key forever. The map is bounded by the number of (channel, model)
// pairs that have ever been audited concurrently — small and constant in practice.
var modelAuditInFlight sync.Map // key: "channelID\x00upstreamModel" -> struct{}

// modelAuditLockKey builds the single-flight key. NUL is used as the separator because
// it cannot appear in a model name, so two different pairs can never collide.
func modelAuditLockKey(channelID int, upstreamModel string) string {
	return fmt.Sprintf("%d\x00%s", channelID, upstreamModel)
}

// acquireModelAuditSlot claims the (channel, model) slot for one manual audit. It
// returns a release function that MUST be deferred by the caller, and ok=false when
// another audit for the same pair is already running — in which case nothing was
// claimed and release is a no-op.
func acquireModelAuditSlot(channelID int, upstreamModel string) (release func(), ok bool) {
	key := modelAuditLockKey(channelID, upstreamModel)
	if _, loaded := modelAuditInFlight.LoadOrStore(key, struct{}{}); loaded {
		return func() {}, false
	}
	return func() { modelAuditInFlight.Delete(key) }, true
}

// modelAuditYieldCap 是"同一渠道同一模型已有别的检测在跑"时，一条结果行最多让位几次。
//
// 让位不是失败：它不发请求、不消耗额度、也不涨 attempt。但排队必须有收敛上限——
// 持有者靠 defer 释放、探针本身有超时兜底，正常情况几十秒就轮到；万一哪天出现
// "持有者永不释放"（比如某条路径 acquire 之后忘了 defer），没有上限的话这条结果行
// 会每 30 秒空转一次、永远停在 pending，用户看到的就是"检测卡住不动、永远没结果"。
// 20 次 × 30 秒退避 ≈ 10 分钟，足够覆盖正常排队。
const modelAuditYieldCap = 20

// modelAuditYieldCount 记每条结果行已经让位过几次（key: result ID）。
// 进程内即可：这里约束的是"本进程内的无限自旋"，进程重启时闸门本身也一并清空。
var modelAuditYieldCount sync.Map // key: int64 result ID -> int

// noteModelAuditYield 记一次让位并返回累计次数。
func noteModelAuditYield(resultID int64) int {
	stored, _ := modelAuditYieldCount.LoadOrStore(resultID, 0)
	times, _ := stored.(int)
	times++
	modelAuditYieldCount.Store(resultID, times)
	return times
}

// clearModelAuditYield 在真正拿到闸门（或放弃）后清账，避免这张小表越积越长。
func clearModelAuditYield(resultID int64) {
	modelAuditYieldCount.Delete(resultID)
}
