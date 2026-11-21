package balancer

import (
	"strings"
	"time"
)

// ── 熔断探测 · 结算（身份复核 + 探测专用转移）─────────────────────────────
//
// 这一段刻意**不**复用真实流量的 RecordFailureWithStatusScoped。那条路对已经 OPEN 的
// 条目只更新时间戳（trip 不加、退避不升），对 HalfOpen 条目才 trip+1——这是真实流量的
// 既定口径，本文件一个字都不改它，也不改 IsTrippedScoped 的到期判定。
//
// 探测要的是另一件事：同一个条目连着探失败，它自己的冷却必须真的变长，否则
// "探一次、等 30s、再探一次"的循环永远教不出更长的冷却。所以这里是一条**探测专用**
// 的窄转移，触发条件只有一个：**准入时那一条、结算时还是同一条**。
//
// 身份靠 circuitEntry.writeSeq / holdEntry.writeSeq 判断，不靠"剩余冷却 > 0"，
// 也不靠时间戳毫秒：
//   - 真实失败/成功、Open→HalfOpen、重置删表、hold 延长，都会让 writeSeq 前进或让
//     条目消失 ⇒ 复核失败 ⇒ 结论作废（stale），一个字节都不改。
//   - 因此"到期前发出、到期后才回来的探针"，只要条目还是同一条未变的 OPEN，结论
//     依然作数（这正是我们要的）。

// probeTripState 是 hold 行上"这一段 hold 已经记过几次探测失败"的计数。
// 它不落检查点：属于分钟级的临时升级状态，重启后由下一次失败重新起算。
type probeTripState struct {
	generation string
	trip       int
}

// probeIdentityUnchanged 在**共享状态自己的锁内**复核"准入时那一条还是不是现在这一条"。
//
// 这是结算唯一允许的刷新判定：
//   - 熔断条目：仍然存在、状态仍是 OPEN、writeSeq 与准入时一致；
//   - hold 行：仍然存在、writeSeq 与准入时一致（新建/延长都会让它前进）。
//
// 全部字段都拷贝到局部变量再解锁使用，不做解锁后的二次读。
func probeIdentityUnchanged(claim CircuitProbeClaim) bool {
	target := claim.Target
	if claim.FromHold {
		value, ok := globalHolds.Load(holdKey(target.ChannelID, target.KeyID, target.Model))
		if !ok {
			return false
		}
		entry, ok := value.(holdEntry)
		if !ok {
			return false
		}
		return entry.writeSeq == claim.EntrySeq
	}
	value, ok := globalBreaker.Load(target.key())
	if !ok {
		return false
	}
	entry, ok := value.(*circuitEntry)
	if !ok {
		return false
	}
	entry.mu.Lock()
	state, writeSeq := entry.State, entry.writeSeq
	entry.mu.Unlock()
	return state == StateOpen && writeSeq == claim.EntrySeq
}

// probeResetClaimed 是"成功"唯一允许的状态写入：确切的 ResetScoped(channel, key, model)。
//
// 分三步，全部在**同一把 tuple 级提交锁**里（与真实失败计数、hold 延长互斥）：
//  1. 复核被领到的那一条：条目仍在、仍是 OPEN、且 writeSeq（全局唯一版本号）未变；
//  2. 在同一段条目临界区里把这一条从表上删掉；
//  3. 再走既有 ResetScoped，把同一条 channel:key:model 的**所有作用域变体**、hold
//     与软冷却一并清掉（这就是"确切、正向、覆盖全部作用域变体"，不再有 mixed_scope
//     例外）。
//
// 因为"插入新条目"与"延长 hold"都要拿同一把 tuple 锁，这三步之间漏不进任何新写入：
// 复核通过之后不可能再冒出一条新一代被扫掉。它只碰这一个 channel+key+model，
// 别的 key/model 一个都不动（hold 也是只删 model 作用域那几条，通配 hold 不碰）。
func probeResetClaimed(claim CircuitProbeClaim) bool {
	target := claim.Target
	if target.ChannelID <= 0 || target.KeyID <= 0 || strings.TrimSpace(target.Model) == "" {
		return false
	}
	// tuple 级提交锁盖住整个"复核 → 删除 → 清理"：真实失败计数（唯一会新建条目的
	// 路径）与 hold 延长都要拿同一把锁，所以这三步之间漏不进任何新写入。
	unlock := lockCircuitTuple(target.ChannelID, target.KeyID, target.Model)
	defer unlock()

	if claim.FromHold {
		if !probeIdentityUnchanged(claim) {
			return false
		}
		// 同一把 tuple 锁已经把 hold 延长拦住，所以这里的"复核通过后清理"不会
		// 误杀一条刚被延长的 hold。
		ResetScoped(target.ChannelID, target.KeyID, target.Model)
		return true
	}

	key := target.key()
	value, ok := globalBreaker.Load(key)
	if !ok {
		return false
	}
	entry, ok := value.(*circuitEntry)
	if !ok {
		return false
	}
	entry.mu.Lock()
	unchanged := entry.State == StateOpen && entry.writeSeq == claim.EntrySeq
	if unchanged {
		// 复核与删除在同一段条目临界区（同时也在 tuple 锁里）。
		globalBreaker.Delete(key)
	}
	entry.mu.Unlock()
	if !unchanged {
		return false
	}
	// 扫描同一 channel:key:model 的所有作用域变体 + hold + 软冷却，全程仍在 tuple 锁里：
	// 没有新条目能在这段扫描中插进来，所以不会误删别人的新一代。
	ResetScoped(target.ChannelID, target.KeyID, target.Model)
	return true
}

// applyProbeFailure 把一次**可归因**的探测失败落成"这一段冷却的 trip+1"。
// 返回是否真的升级、升级后的 trip 次数、以及新的冷却时长。
func (s *circuitProbeScheduler) applyProbeFailure(claim CircuitProbeClaim, statusCode int, now time.Time) (escalated bool, tripCount int, cooldown time.Duration) {
	policy := failurePolicyForStatusAndCapability(statusCode, claim.Target.Model, claim.Target.Capability)
	key := claim.Target.key()

	// tuple 级提交锁：探测失败也会写共享状态（hold 行甚至会新建条目），
	// 必须与真实失败计数/重置串行。
	unlock := lockCircuitTuple(claim.Target.ChannelID, claim.Target.KeyID, claim.Target.Model)
	defer unlock()

	if !claim.FromHold {
		value, ok := globalBreaker.Load(key)
		if !ok {
			// 条目在结算前消失了（真实流量/重置已经把它清掉）：不替它记 trip。
			return false, 0, 0
		}
		entry, ok := value.(*circuitEntry)
		if !ok {
			return false, 0, 0
		}
		entry.mu.Lock()
		defer entry.mu.Unlock()
		if entry.State != StateOpen || entry.writeSeq != claim.EntrySeq {
			// 已经不是准入时那一条（被重置/被真实流量改过/已经转半开）：不动。
			return false, 0, 0
		}
		entry.TripCount++
		trip := entry.TripCount
		entry.Policy = probeEscalationPolicy(entry.Policy, policy, trip)
		entry.ConsecutiveFailures = 0
		entry.LastFailureTime = now
		entry.writeSeq = nextCircuitRevision()
		return true, trip, getCooldownForPolicy(trip, entry.Policy)
	}

	// hold 行：先复核这条 hold 没被换过，再按"探测专用 trip 计数"累加。
	if !probeIdentityUnchanged(claim) {
		return false, 0, 0
	}
	s.mu.Lock()
	previous := s.trips[key]
	trip := 1
	if previous.generation == claim.Generation && previous.trip > 0 {
		trip = previous.trip + 1
	}
	s.trips[key] = probeTripState{generation: claim.Generation, trip: trip}
	s.mu.Unlock()

	entry := getOrCreateEntry(key)
	entry.mu.Lock()
	state := entry.State
	if state != StateHalfOpen {
		// hold 行底下通常没有熔断条目：这里把它开成 OPEN 并记上 trip，真实转发的准入
		// 因此看到更长的冷却；hold 自己的到期时刻不动（那是人定的策略）。
		if state == StateClosed {
			entry.State = StateOpen
		}
		if entry.TripCount < trip {
			entry.TripCount = trip
		}
		trip = entry.TripCount
		entry.Policy = probeEscalationPolicy(entry.Policy, policy, trip)
		entry.ConsecutiveFailures = 0
		entry.LastFailureTime = now
		entry.writeSeq = nextCircuitRevision()
		state = entry.State
	}
	entry.mu.Unlock()
	if state != StateOpen {
		// 半开：真实流量的探测正在进行，别替它加 trip。
		return false, 0, 0
	}
	return true, trip, getCooldownForPolicy(trip, policy)
}

// probeEscalationPolicy 决定一次探测失败升级沿用哪份失败策略。
//
// 既有口径是"记失败就换成本次状态的策略"（RecordFailureWithStatusScoped 就是这么写的），
// 但探测升级是**加长**冷却的动作：如果本次状态的策略比条目原来那份更窄（典型例子：
// 条目原本按通用 5xx 拿到 120s 上限，这一次上游回 503、transient 策略只有 30s 上限），
// 直接换过去会把既有冷却缩短——升级反而降级。所以只在"新策略不会让冷却变短"时采用它，
// 其余情况保留条目原来那份。同状态/同上限的策略照旧采用，不受影响。
func probeEscalationPolicy(existing, incoming circuitFailurePolicy, trip int) circuitFailurePolicy {
	if getCooldownForPolicy(trip, incoming) >= getCooldownForPolicy(trip, existing) {
		return incoming
	}
	return existing
}
