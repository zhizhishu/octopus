package balancer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/utils/log"
)

// ── 熔断探测 · 调度 ───────────────────────────────────────────────────────
//
// 一次探测的生命周期是 tick → claim → (接线层跑 A) → complete：
//
//   - tick 重算候选与排期（只在候选"冷却段"变化时重挑提前量、重记 skip）；
//   - claim 做准入并占住唯一的探测车道（全局 1 条，因此每渠道最多 1 条）；
//   - complete 结算：成功只做确切的 ResetScoped；失败只做探测专用的 trip+1 退避
//     （见 circuit_probe_failure.go）。条目在这期间变了，结论就整条作废。
//
// 时序口径（本文件最容易改错的地方）：
//
//   - 只有"确切的、处于 OPEN 或被 hold 住、作用域是已知普通文本"的条目才是候选。
//   - 剩余冷却 > 60s：**不算到期、不可领**，也不写任何持久的下次时间（每一轮 tick
//     重新算，不靠轮询把状态推进）；视图只报"窗口还要多久才开"。
//   - 剩余冷却进入 [10s, 60s]：**每段冷却只挑一次**提前量
//     lead ∈ [10s, min(60s, remaining)]（均匀、闭区间），due = expiry - lead，
//     必然落在到期之前；剩余恰好 10s 时 lead = 10s、due = 现在。
//   - 剩余 < 10s：不再新排；但本段已经挑好的 due 只要还没过到期，晚到的 tick/claim
//     仍然可以领它。到期之后一律不再开跑。
//   - claim 自己再核一遍（到期/是否挑过 due/是否已领过），不依赖 tick 是否刚跑过。

// probeCandidate 是一个"真的在挡路"的确切条目。
type probeCandidate struct {
	target     CircuitProbeTarget
	generation string
	expiry     time.Time
	remaining  time.Duration
	writeSeq   uint64
	fromHold   bool
}

// probeResolved 是接线层对候选的补充回答（路由与渠道开关）。
type probeResolved struct {
	kind           probeScopeKind
	route          CircuitProbeRoute
	routeOK        bool
	channelEnabled bool
	// channel 是这个渠道生效的探测口径覆写（零值 = 无覆写，沿用全局）。
	channel CircuitProbeChannelOverrides
}

// probeDecision 是一次求值结论（纯计算，不改状态）。
type probeDecision struct {
	state     string
	remaining time.Duration
	due       time.Time
	// skipReason 非空表示这一轮是一次"决定不探"，值是稳定的小写理由串
	// （skipped_traffic / wait_upstream / capped / streak），用于计数与历史。
	skipReason string
}

// skip 理由（写进历史行的 result 尾巴：skipped:<reason>）。
const (
	probeSkipTraffic = "skipped_traffic"
	probeSkipWait    = "wait_upstream"
	probeSkipCapped  = "capped"
	probeSkipStreak  = "streak"
)

// probeCandidates 抓一遍熔断表与 hold 表里"真的在挡路"的确切条目。
//
// 只认：keyID>0、model 非空且不是 * 通配、熔断器确实还是 OPEN、hold 还没到期、并且
// 冷却还没过期（过期条目不再归这里管，真实流量那条路自己会处理）。
func probeCandidates(now time.Time) []probeCandidate {
	candidates := make([]probeCandidate, 0, 8)
	seen := make(map[string]struct{}, 8)

	globalBreaker.Range(func(key, value any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		parsed, ok := parseCircuitKey(keyString)
		if !ok || parsed.keyID <= 0 || parsed.model == "" || parsed.model == holdAnyModel {
			return true
		}
		entry, ok := value.(*circuitEntry)
		if !ok {
			return true
		}
		entry.mu.Lock()
		state, tripCount, lastFailure, policy, writeSeq := entry.State, entry.TripCount, entry.LastFailureTime, entry.Policy, entry.writeSeq
		entry.mu.Unlock()
		if state != StateOpen {
			return true
		}
		expiry := lastFailure.Add(getCooldownForPolicy(tripCount, policy))
		remaining := expiry.Sub(now)
		if remaining <= 0 {
			return true
		}
		target := CircuitProbeTarget{
			ChannelID:  parsed.channelID,
			KeyID:      parsed.keyID,
			Model:      parsed.model,
			Endpoint:   parsed.endpoint,
			Capability: parsed.capability,
		}
		if _, duplicate := seen[target.key()]; duplicate {
			return true
		}
		seen[target.key()] = struct{}{}
		candidates = append(candidates, probeCandidate{
			target:     target,
			generation: fmt.Sprintf("open:%d", writeSeq),
			expiry:     expiry,
			remaining:  remaining,
			writeSeq:   writeSeq,
		})
		return true
	})

	globalHolds.Range(func(key, value any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		parsed, ok := parseCircuitKey(keyString)
		if !ok || parsed.keyID <= 0 || parsed.model == "" || parsed.model == holdAnyModel {
			return true
		}
		entry, ok := value.(holdEntry)
		if !ok {
			return true
		}
		remaining := entry.until.Sub(now)
		if remaining <= 0 {
			return true
		}
		target := CircuitProbeTarget{
			ChannelID:  parsed.channelID,
			KeyID:      parsed.keyID,
			Model:      parsed.model,
			Endpoint:   entry.endpoint,
			Capability: entry.capability,
		}
		if _, duplicate := seen[target.key()]; duplicate {
			return true
		}
		seen[target.key()] = struct{}{}
		candidates = append(candidates, probeCandidate{
			target:     target,
			generation: fmt.Sprintf("hold:%d", entry.writeSeq),
			expiry:     entry.until,
			remaining:  remaining,
			writeSeq:   entry.writeSeq,
			fromHold:   true,
		})
		return true
	})

	sort.SliceStable(candidates, func(i, j int) bool { return probeTargetLess(candidates[i].target, candidates[j].target) })
	return candidates
}

// probeTargetLess 给候选一个固定顺序：同一时刻只有一条车道，顺序必须是确定的。
func probeTargetLess(left, right CircuitProbeTarget) bool {
	if left.ChannelID != right.ChannelID {
		return left.ChannelID < right.ChannelID
	}
	if left.KeyID != right.KeyID {
		return left.KeyID < right.KeyID
	}
	if left.Model != right.Model {
		return left.Model < right.Model
	}
	if left.Endpoint != right.Endpoint {
		return left.Endpoint < right.Endpoint
	}
	return left.Capability < right.Capability
}

// probeLive 是"这个目标现在还是不是一个在挡路的条目"的读数。
type probeLive struct {
	generation  string
	expiry      time.Time
	remaining   time.Duration
	tripCount   int
	lastFailure time.Time
	writeSeq    uint64
	fromHold    bool
}

// liveProbeTarget 重新读一遍目标现在的状态：还挡不挡路、哪一段冷却、什么时候到期。
// 准入用它（要 remaining），结算不用它（结算用 probeIdentityUnchanged，不看剩余）。
func liveProbeTarget(now time.Time, target CircuitProbeTarget) (probeLive, bool) {
	key := target.key()
	if value, ok := globalBreaker.Load(key); ok {
		if entry, ok := value.(*circuitEntry); ok {
			entry.mu.Lock()
			state, tripCount, lastFailure, policy, writeSeq := entry.State, entry.TripCount, entry.LastFailureTime, entry.Policy, entry.writeSeq
			entry.mu.Unlock()
			if state == StateOpen {
				expiry := lastFailure.Add(getCooldownForPolicy(tripCount, policy))
				if remaining := expiry.Sub(now); remaining > 0 {
					return probeLive{
						generation:  fmt.Sprintf("open:%d", writeSeq),
						expiry:      expiry,
						remaining:   remaining,
						tripCount:   tripCount,
						lastFailure: lastFailure,
						writeSeq:    writeSeq,
					}, true
				}
			}
		}
	}
	if value, ok := globalHolds.Load(holdKey(target.ChannelID, target.KeyID, target.Model)); ok {
		entry, ok := value.(holdEntry)
		if ok && circuitScopedKey(target.ChannelID, target.KeyID, target.Model, entry.endpoint, entry.capability) == key {
			if remaining := entry.until.Sub(now); remaining > 0 {
				return probeLive{
					generation: fmt.Sprintf("hold:%d", entry.writeSeq),
					expiry:     entry.until,
					remaining:  remaining,
					writeSeq:   entry.writeSeq,
					fromHold:   true,
				}, true
			}
		}
	}
	return probeLive{}, false
}

// resolveProbeCandidates 在锁外问接线层：该用哪个上游端点探、渠道开关开着没有。
func resolveProbeCandidates(cfg CircuitProbeConfig, cb CircuitProbeCallbacks, candidates []probeCandidate) []probeResolved {
	resolved := make([]probeResolved, len(candidates))
	for i, candidate := range candidates {
		// 渠道级覆写在任何 continue 之前就解析好：决策/准入两侧都要拿到。
		if cb.ChannelProbeOverrides != nil {
			resolved[i].channel = cb.ChannelProbeOverrides(candidate.target.ChannelID)
		}
		kind := classifyProbeScope(candidate.target.Endpoint, candidate.target.Capability)
		resolved[i].kind = kind
		resolved[i].channelEnabled = true
		if cb.ChannelEnabled != nil {
			resolved[i].channelEnabled = cb.ChannelEnabled(candidate.target.ChannelID)
		}
		if kind == probeScopeMedia || kind == probeScopeUnknown {
			continue
		}
		if kind == probeScopeEmbedding && !cfg.EmbeddingsEnabled {
			continue
		}
		if cb.ResolveTarget == nil {
			continue
		}
		route, ok := cb.ResolveTarget(candidate.target)
		if !ok {
			continue
		}
		// 接线层给的上游端点若落到媒体/未知作用域，同样不探：宁可不探，也不拿一道
		// 算术题去打图片/视频端点。
		switch classifyProbeScope(route.Endpoint, route.Capability) {
		case probeScopeText:
			resolved[i].route = route
			resolved[i].routeOK = true
		case probeScopeEmbedding:
			if cfg.EmbeddingsEnabled {
				resolved[i].route = route
				resolved[i].routeOK = true
			}
		}
	}
	return resolved
}

// decideLocked 算"这个条目的探测现在该显示成什么状态、什么时候该探"。
func (s *circuitProbeScheduler) decideLocked(now time.Time, cfg CircuitProbeConfig, cb CircuitProbeCallbacks, candidate probeCandidate, resolved probeResolved, state *probeEntryState) probeDecision {
	switch resolved.kind {
	case probeScopeMedia, probeScopeUnknown:
		return probeDecision{state: CircuitProbeStateNotProbedMedia}
	case probeScopeEmbedding:
		if !cfg.EmbeddingsEnabled {
			return probeDecision{state: CircuitProbeStateNotProbedMedia}
		}
	}
	if s.run != nil && s.run.target.key() == candidate.target.key() {
		return probeDecision{state: CircuitProbeStateRunning}
	}
	if !resolved.routeOK {
		return probeDecision{state: CircuitProbeStateDisabled}
	}
	if !cfg.Enabled || !resolved.channelEnabled {
		return probeDecision{state: CircuitProbeStateDisabled}
	}
	if state.probed {
		// 这一段冷却已经领过（结论可能不改变冷却段，例如调用方取消）：不再重领。
		return probeDecision{state: CircuitProbeStateStopped}
	}
	if streak := probeStreakValue(candidate.target.tupleKey()); streak > 0 && streak >= cfg.maxStreakFor(resolved.channel) {
		return probeDecision{state: CircuitProbeStateStopped, skipReason: probeSkipStreak}
	}
	if wait, ok := s.waitLocked(now, candidate.target.ChannelID, candidate.target.KeyID, candidate.target.Model); ok {
		return probeDecision{state: CircuitProbeStateWaitUpstream, remaining: wait.until.Sub(now), skipReason: probeSkipWait}
	}
	if at, ok := probeRealTrafficAt(candidate.target.tupleKey()); ok && at.Add(probeTrafficSkipWindow).After(now) {
		return probeDecision{state: CircuitProbeStateSkippedTraffic, skipReason: probeSkipTraffic}
	}
	if s.cappedLocked(cfg, resolved.channel, candidate.target.ChannelID) {
		return probeDecision{state: CircuitProbeStateDisabled, skipReason: probeSkipCapped}
	}

	// 排期：只有冷却尾段才排。
	if candidate.remaining > probeJitterMax {
		// 还早：不算到期、不写下次时间；只报"窗口还要多久才开"。
		return probeDecision{state: CircuitProbeStateScheduled, remaining: candidate.remaining - probeJitterMax}
	}
	if !state.due.IsZero() {
		// 这一段已经挑过提前量：按那次算（哪怕现在剩余已 < 10s，晚到的 tick/claim 仍可领）。
		remaining := state.due.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		return probeDecision{state: CircuitProbeStateScheduled, remaining: remaining, due: state.due}
	}
	lead, feasible := circuitProbeJitter(cb, candidate.remaining)
	if !feasible {
		// 剩余 < 10s 且本段从没挑过：不再新排。
		return probeDecision{state: CircuitProbeStateStopped}
	}
	due := candidate.expiry.Add(-lead)
	if due.Before(now) {
		// lead 恰好等于剩余（如恰好 10s）时，本轮即到期。
		due = now
	}
	remaining := due.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	return probeDecision{state: CircuitProbeStateScheduled, remaining: remaining, due: due}
}

// tick 重算全部候选的排期与当日计数，并把"已到期"的候选排进 due 列表。
func (s *circuitProbeScheduler) tick(now time.Time) {
	cleanupCircuitProbeTraffic(now)
	candidates := probeCandidates(now)
	cfg, cb := s.configSnapshot()
	resolved := resolveProbeCandidates(cfg, cb, candidates)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rollDailyLocked(now)
	s.pruneWaitsLocked(now)
	s.due = s.due[:0]

	if s.run != nil && now.Sub(s.run.startedAt) > probeLaneTimeout {
		// 接线层没有结算那次探测（崩溃/分支丢了）：车道不能永久锁死。
		log.Warnf("circuit probe lane released after %v without a result: channel=%d key=%d model=%s",
			now.Sub(s.run.startedAt), s.run.target.ChannelID, s.run.target.KeyID, s.run.target.Model)
		s.run = nil
	}

	seen := make(map[string]struct{}, len(candidates))
	for i, candidate := range candidates {
		key := candidate.target.key()
		seen[key] = struct{}{}
		state := s.entries[key]
		if state == nil || state.generation != candidate.generation {
			// 这一段冷却是新的：提前量与 skip 记录全部重来。
			state = &probeEntryState{generation: candidate.generation}
			s.entries[key] = state
		}
		decision := s.decideLocked(now, cfg, cb, candidate, resolved[i], state)
		if decision.skipReason != "" {
			s.recordSkipLocked(now, candidate.target, state, decision.skipReason)
		}
		if decision.state != CircuitProbeStateScheduled {
			continue
		}
		if state.due.IsZero() && !decision.due.IsZero() {
			// 提前量只挑一次：这一段冷却挑过就按那次算。
			state.due = decision.due
		}
		if !state.due.IsZero() && !now.Before(state.due) && now.Before(candidate.expiry) {
			s.due = append(s.due, candidate)
		}
	}

	for key := range s.entries {
		if _, ok := seen[key]; ok {
			continue
		}
		if s.run != nil && s.run.target.key() == key {
			continue // 在飞的那一条要活到 complete
		}
		delete(s.entries, key)
	}

	// hold 行的探测专用 trip 计数只在这条 hold 还在时才有意义。
	for key := range s.trips {
		if _, ok := seen[key]; ok {
			continue
		}
		delete(s.trips, key)
	}
}

// admit 做一次准入；reserve 为真时占住车道并记一次 used。
//
// 外部回调（ResolveTarget / ChannelEnabled）在锁外跑：接线层的查询可能反过来摸本包，
// 拿着 s.mu 调它就是自找自锁。
func (s *circuitProbeScheduler) admit(now time.Time, cfg CircuitProbeConfig, cb CircuitProbeCallbacks, target CircuitProbeTarget, manual bool, expectedGeneration string, reserve bool) (CircuitProbeClaim, CircuitProbeRejection, bool) {
	reject := func(state string, remaining time.Duration) (CircuitProbeClaim, CircuitProbeRejection, bool) {
		rejection := CircuitProbeRejection{State: state}
		if remaining > 0 {
			rejection.RemainingSeconds = int((remaining + time.Second - 1) / time.Second)
		}
		return CircuitProbeClaim{}, rejection, false
	}

	if target.ChannelID <= 0 || target.KeyID <= 0 || strings.TrimSpace(target.Model) == "" {
		return reject(CircuitProbeStateDisabled, 0)
	}
	kind := classifyProbeScope(target.Endpoint, target.Capability)
	if kind == probeScopeMedia || kind == probeScopeUnknown {
		return reject(CircuitProbeStateNotProbedMedia, 0)
	}
	if kind == probeScopeEmbedding && !cfg.EmbeddingsEnabled {
		return reject(CircuitProbeStateNotProbedMedia, 0)
	}

	// 上游等待先于任何别的遮挡：手动也要拿到 wait_upstream + 剩余秒数。
	if wait, ok := s.currentWait(now, target.ChannelID, target.KeyID, target.Model); ok {
		return reject(CircuitProbeStateWaitUpstream, wait.until.Sub(now))
	}

	live, liveOK := liveProbeTarget(now, target)
	if !liveOK {
		// 不是"现在正在挡路的条目"：手动也不发。
		return reject(CircuitProbeStateDisabled, 0)
	}
	if expectedGeneration != "" && live.generation != expectedGeneration {
		// 排队期间条目变了（被重置、换了冷却段）：不做陈旧候选。
		return reject(CircuitProbeStateDisabled, 0)
	}
	if !now.Before(live.expiry) {
		// 到期之后一律不开跑（哪怕提前挑过 due、哪怕只是晚了一点）。
		return reject(CircuitProbeStateStopped, 0)
	}

	var route CircuitProbeRoute
	routeOK := false
	if cb.ResolveTarget != nil {
		if resolvedRoute, ok := cb.ResolveTarget(target); ok {
			switch classifyProbeScope(resolvedRoute.Endpoint, resolvedRoute.Capability) {
			case probeScopeText:
				route, routeOK = resolvedRoute, true
			case probeScopeEmbedding:
				if cfg.EmbeddingsEnabled {
					route, routeOK = resolvedRoute, true
				}
			}
		}
	}
	if !routeOK {
		return reject(CircuitProbeStateDisabled, 0)
	}

	var overrides CircuitProbeChannelOverrides
	if cb.ChannelProbeOverrides != nil {
		overrides = cb.ChannelProbeOverrides(target.ChannelID)
	}

	if !manual {
		if !cfg.Enabled {
			return reject(CircuitProbeStateDisabled, 0)
		}
		if cb.ChannelEnabled != nil && !cb.ChannelEnabled(target.ChannelID) {
			return reject(CircuitProbeStateDisabled, 0)
		}
		if streak := probeStreakValue(target.tupleKey()); streak > 0 && streak >= cfg.maxStreakFor(overrides) {
			return reject(CircuitProbeStateStopped, 0)
		}
		if at, ok := probeRealTrafficAt(target.tupleKey()); ok && at.Add(probeTrafficSkipWindow).After(now) {
			return reject(CircuitProbeStateSkippedTraffic, 0)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rollDailyLocked(now)
	if s.run != nil {
		return reject(CircuitProbeStateRunning, 0)
	}
	if wait, ok := s.waitLocked(now, target.ChannelID, target.KeyID, target.Model); ok {
		return reject(CircuitProbeStateWaitUpstream, wait.until.Sub(now))
	}
	if s.cappedLocked(cfg, overrides, target.ChannelID) {
		return reject(CircuitProbeStateDisabled, 0)
	}
	if !manual {
		// 自动车道的额外硬条件：这一段冷却必须已经挑过 due，而且 due 已经到点。
		state := s.entries[target.key()]
		if state == nil || state.generation != live.generation {
			return reject(CircuitProbeStateScheduled, 0)
		}
		if state.probed {
			return reject(CircuitProbeStateStopped, 0)
		}
		if state.due.IsZero() {
			// 本段还没挑过提前量：窗口没开就报"还要等多久窗口才开"，窗口已闭就报 stopped。
			if live.remaining < probeJitterMin {
				return reject(CircuitProbeStateStopped, 0)
			}
			return reject(CircuitProbeStateScheduled, live.remaining-probeJitterMax)
		}
		if now.Before(state.due) {
			return reject(CircuitProbeStateScheduled, state.due.Sub(now))
		}
	}
	if !reserve {
		return CircuitProbeClaim{}, CircuitProbeRejection{}, true
	}
	s.claimSeq++
	run := &probeRun{id: s.claimSeq, target: target, generation: live.generation, manual: manual, startedAt: now}
	s.run = run
	// 同代只领一次：领的瞬间就记下，结算结论改写不了冷却段也不会被重复领。
	if state := s.entries[target.key()]; state != nil && state.generation == live.generation {
		state.probed = true
	}
	s.countLocked(target.ChannelID, func(totals *CircuitProbeTotals) { totals.Used++ })
	var lastFailureUnixMs int64
	if !live.lastFailure.IsZero() {
		lastFailureUnixMs = live.lastFailure.UnixMilli()
	}
	return CircuitProbeClaim{
		ID:                run.id,
		Target:            target,
		Route:             route,
		Generation:        live.generation,
		FromHold:          live.fromHold,
		EntrySeq:          live.writeSeq,
		ExpiryUnixMs:      live.expiry.UnixMilli(),
		TripCount:         live.tripCount,
		LastFailureUnixMs: lastFailureUnixMs,
		Manual:            manual,
		StartedAt:         now,
	}, CircuitProbeRejection{}, true
}

// manualRequestLive 判断队里的手动请求是否已经陈旧（条目换了代/没了/过期）。
func (s *circuitProbeScheduler) manualRequestLive(now time.Time, request probeManualRequest) bool {
	live, ok := liveProbeTarget(now, request.target)
	if !ok {
		return false
	}
	return live.generation == request.generation && live.writeSeq == request.writeSeq && now.Before(live.expiry)
}

// claim 取一次可以开跑的探测。手动/审计探测排在自动探测前面，并且都受唯一车道约束。
func (s *circuitProbeScheduler) claim(now time.Time) (CircuitProbeClaim, bool) {
	s.tick(now)
	cfg, cb := s.configSnapshot()

	for _, request := range s.manualSnapshot() {
		if !s.manualRequestLive(now, request) {
			// 陈旧：绝不发出去，也绝不因此记 recovered。
			s.removeManual(request.target)
			log.Debugf("circuit probe manual target %s dropped: entry changed or expired", request.target.key())
			continue
		}
		claim, rejection, ok := s.admit(now, cfg, cb, request.target, true, request.generation, true)
		if ok {
			s.removeManual(request.target)
			return claim, true
		}
		if rejection.State == CircuitProbeStateRunning {
			// 车道被占：手动请求留在队里，下一轮仍然优先。
			return CircuitProbeClaim{}, false
		}
		// 等待/额度/开关这类暂时性拒绝：留在队里等下一轮（条目过期后会按陈旧出队）。
		log.Debugf("circuit probe manual target %s deferred: state=%s remaining=%ds", request.target.key(), rejection.State, rejection.RemainingSeconds)
	}

	if cb.AuditPending != nil && cb.AuditPending() {
		// 接线层那边有待办/到期的审计探测：唯一的车道让给它。
		return CircuitProbeClaim{}, false
	}

	for _, candidate := range s.dueSnapshot() {
		claim, _, ok := s.admit(now, cfg, cb, candidate.target, false, candidate.generation, true)
		if ok {
			return claim, true
		}
	}
	return CircuitProbeClaim{}, false
}

// complete 结算一次探测。
//
// 车道**到结算结束**才放开（见 defer releaseRun）：结算期间 tick/claim 只会看到
// running，不会有一条新探测在状态改到一半时插进来。只留一条全局发送车道，所以
// "占住到结算完"就等于不交叉。
func (s *circuitProbeScheduler) complete(claim CircuitProbeClaim, report CircuitProbeReport) {
	now := s.now()

	s.mu.Lock()
	run := s.run
	if run == nil || run.id != claim.ID || run.target.key() != claim.Target.key() {
		// 不是这一条（重复结算 / 已作废 / 对不上号）：什么都不改，也不碰车道。
		s.mu.Unlock()
		return
	}
	cfg, cb := s.cfg, s.cb
	s.mu.Unlock()
	defer s.releaseRun(claim)

	target := claim.Target
	tuple := target.tupleKey()

	// ① 上游等待先落，且盖在**原来这条路由**上：不管这条结论接下来会被判成
	//    stale / 取消 / 不归因 / 失败，Retry-After 与无头的 429 都先按上游口径记下。
	if report.RetryAfter > 0 || report.StatusCode == 429 {
		noteCircuitProbeUpstreamWait(target.ChannelID, target.KeyID, target.Model, report.StatusCode, report.RetryAfter)
	}

	// ② 身份复核：只认"准入时那一条、且现在还没被换过"的共享状态。
	//    这里刻意**不看**剩余冷却：探针在到期前发出、到期后才回来，只要条目还是同一条
	//    OPEN，结论依然作数。
	switch {
	case !probeIdentityUnchanged(claim):
		s.recordResult(now, claim, report, "stale")

	case report.Recovered:
		if !probeResetClaimed(claim) {
			s.recordResult(now, claim, report, "stale")
			return
		}
		resetProbeStreak(tuple)
		log.Infof("circuit probe recovered [%s] channel=%d key=%d model=%s", target.key(), target.ChannelID, target.KeyID, target.Model)
		s.countOutcome(now, target.ChannelID, func(totals *CircuitProbeTotals) { totals.Recovered++ })
		s.recordResult(now, claim, report, "recovered")

	case report.Attributable:
		escalated, tripCount, cooldown := s.applyProbeFailure(claim, report.StatusCode, now)
		if !escalated {
			s.recordResult(now, claim, report, "stale")
			return
		}
		log.Infof("circuit probe failed [%s] channel=%d key=%d model=%s trip=%d cooldown=%v",
			target.key(), target.ChannelID, target.KeyID, target.Model, tripCount, cooldown)
		channelOverrides := CircuitProbeChannelOverrides{}
		if cb.ChannelProbeOverrides != nil {
			channelOverrides = cb.ChannelProbeOverrides(target.ChannelID)
		}
		if streak := incProbeStreak(tuple); streak >= cfg.maxStreakFor(channelOverrides) {
			log.Warnf("circuit probe stopped for %s: %d consecutive probes without recovery", tuple, streak)
		}
		s.countOutcome(now, target.ChannelID, func(totals *CircuitProbeTotals) { totals.Failed++ })
		s.recordResult(now, claim, report, probeFailedResult(report.Outcome))

	default:
		// 不是渠道的锅（调用方取消 / 我们自己没发出去）：只落一条历史，不改熔断状态。
		s.countOutcome(now, target.ChannelID, func(totals *CircuitProbeTotals) { totals.Skipped++ })
		s.recordResult(now, claim, report, probeSkippedResult(report.Outcome))
	}
}

// releaseRun 只放开自己那一条车道占用（别人的在飞探测不动）。
func (s *circuitProbeScheduler) releaseRun(claim CircuitProbeClaim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != nil && s.run.id == claim.ID && s.run.target.key() == claim.Target.key() {
		s.run = nil
	}
}

// countOutcome 记一笔当日计数（渠道 + 全局）。
func (s *circuitProbeScheduler) countOutcome(now time.Time, channelID int, apply func(*CircuitProbeTotals)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDailyLocked(now)
	s.countLocked(channelID, apply)
}

// recordSkipLocked 记一次"决定不探"（每个理由每段冷却只记一次）。调用方持 s.mu。
func (s *circuitProbeScheduler) recordSkipLocked(now time.Time, target CircuitProbeTarget, state *probeEntryState, reason string) {
	if state.skipReasons == nil {
		state.skipReasons = make(map[string]struct{}, 2)
	}
	if _, done := state.skipReasons[reason]; done {
		return
	}
	state.skipReasons[reason] = struct{}{}
	s.countLocked(target.ChannelID, func(totals *CircuitProbeTotals) { totals.Skipped++ })
	s.appendHistoryLocked(now, target.ChannelID, target.KeyID, target.Model, "", "", 0, "skipped:"+reason)
}

// probeReasonToken 把 A 给的 outcome 收成可落盘的小写 token（只留 [a-z0-9_-]，限长）。
func probeReasonToken(outcome string) string {
	token := strings.Builder{}
	for _, r := range strings.ToLower(strings.TrimSpace(outcome)) {
		if token.Len() >= 32 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			token.WriteRune(r)
		}
	}
	return token.String()
}

// probeFailedResult 是"可归因失败"的历史结论串：保留 timeout / wrong_answer 这类可辨
// 的 outcome，方便运维分清"渠道不应答"和"渠道答错"。
func probeFailedResult(outcome string) string {
	if token := probeReasonToken(outcome); token != "" {
		return "failed:" + token
	}
	return "failed"
}

// probeSkippedResult 是"不归因"的历史结论串。
func probeSkippedResult(outcome string) string {
	if token := probeReasonToken(outcome); token != "" {
		return "skipped:" + token
	}
	return "skipped"
}

// recordResult 落一条探测结果历史（answer 只留安全数字、prompt 走白名单校验）。
func (s *circuitProbeScheduler) recordResult(now time.Time, claim CircuitProbeClaim, report CircuitProbeReport, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendHistoryLocked(now,
		claim.Target.ChannelID, claim.Target.KeyID, claim.Target.Model,
		probeSafePrompt(report.Prompt), probeSafeAnswer(report.Answer),
		report.Duration.Milliseconds(), result)
}

// appendHistoryLocked 往该渠道的历史环里塞一行（最新在前，最多 probeHistoryRingSize 行）。
// 落盘的只有：白名单校验过的题面、安全数字答案、有限长度的 reason token。
func (s *circuitProbeScheduler) appendHistoryLocked(now time.Time, channelID, keyID int, model, prompt, answer string, durationMS int64, result string) {
	row := CircuitProbeHistoryRow{
		Timestamp:  now.In(circuitProbeDayZone).Format(time.RFC3339),
		Model:      model,
		KeyID:      keyID,
		Prompt:     prompt,
		Answer:     answer,
		DurationMS: durationMS,
		Result:     result,
	}
	history := make([]CircuitProbeHistoryRow, 0, probeHistoryRingSize)
	history = append(history, row)
	for _, previous := range s.history[channelID] {
		if len(history) >= probeHistoryRingSize {
			break
		}
		history = append(history, previous)
	}
	s.history[channelID] = history
}

func (s *circuitProbeScheduler) manualSnapshot() []probeManualRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]probeManualRequest(nil), s.manual...)
}

func (s *circuitProbeScheduler) removeManual(target CircuitProbeTarget) {
	key := target.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.manual[:0]
	for _, queued := range s.manual {
		if queued.target.key() == key {
			continue
		}
		kept = append(kept, queued)
	}
	s.manual = kept
}

func (s *circuitProbeScheduler) dueSnapshot() []probeCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := make([]probeCandidate, len(s.due))
	copy(snapshot, s.due)
	return snapshot
}

// currentWait 取覆盖该 tuple 的上游等待（不持任何外部锁调用）。
func (s *circuitProbeScheduler) currentWait(now time.Time, channelID, keyID int, model string) (probeWait, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitLocked(now, channelID, keyID, model)
}

// snapshotChannel 返回一个渠道的探测视图（内部先 tick，保证和调度口径一致）。
func (s *circuitProbeScheduler) snapshotChannel(now time.Time, channelID int) CircuitProbeChannelView {
	s.tick(now)
	cfg, cb := s.configSnapshot()
	candidates := probeCandidates(now)
	resolved := resolveProbeCandidates(cfg, cb, candidates)

	s.mu.Lock()
	defer s.mu.Unlock()

	view := CircuitProbeChannelView{
		ChannelID:    channelID,
		Day:          s.day,
		Totals:       s.channelTotals[channelID],
		GlobalTotals: s.global,
		Entries:      make([]CircuitProbeEntryView, 0, len(candidates)),
		History:      append([]CircuitProbeHistoryRow(nil), s.history[channelID]...),
	}
	for i, candidate := range candidates {
		if candidate.target.ChannelID != channelID {
			continue
		}
		state := s.entries[candidate.target.key()]
		if state == nil {
			state = &probeEntryState{generation: candidate.generation}
		}
		decision := s.decideLocked(now, cfg, cb, candidate, resolved[i], state)
		view.Entries = append(view.Entries, CircuitProbeEntryView{
			ChannelID:  candidate.target.ChannelID,
			KeyID:      candidate.target.KeyID,
			Model:      candidate.target.Model,
			Endpoint:   candidate.target.Endpoint,
			Capability: candidate.target.Capability,
			Probe: CircuitProbeProbeView{
				State:         decision.state,
				NextInSeconds: int((decision.remaining + time.Second - 1) / time.Second),
			},
		})
	}
	return view
}

// snapshotGlobal 返回当日全局计数与每渠道计数。
func (s *circuitProbeScheduler) snapshotGlobal(now time.Time) CircuitProbeGlobalView {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDailyLocked(now)
	view := CircuitProbeGlobalView{Day: s.day, Totals: s.global, Channels: make([]CircuitProbeChannelTotals, 0, len(s.channelTotals))}
	channelIDs := make([]int, 0, len(s.channelTotals))
	for channelID := range s.channelTotals {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Ints(channelIDs)
	for _, channelID := range channelIDs {
		view.Channels = append(view.Channels, CircuitProbeChannelTotals{ChannelID: channelID, CircuitProbeTotals: s.channelTotals[channelID]})
	}
	return view
}

// ── 导出接口（接线层用这些）─────────────────────────────────────────────

// TickCircuitProbe 重算一遍探测排期与当日计数（不发探测）。接线层定时调它。
func TickCircuitProbe(now time.Time) {
	circuitProbe.tick(now)
}

// ClaimCircuitProbe 取一次可以开跑的探测（内部先 tick）。全局只有一条车道，所以同一
// 时刻最多一个 claim 成功；手动/审计探测排在自动探测前面。
func ClaimCircuitProbe(now time.Time) (CircuitProbeClaim, bool) {
	return circuitProbe.claim(now)
}

// CompleteCircuitProbe 结算一次探测：释放车道并按结论改熔断状态。
func CompleteCircuitProbe(claim CircuitProbeClaim, report CircuitProbeReport) {
	circuitProbe.complete(claim, report)
}

// CircuitProbeAdmission 只做准入检查、不占车道，也不记 used。接线层想问"手动探这一发
// 行不行"就用它：等待类拒绝带剩余秒数（state=wait_upstream），而且这个判断排在其它
// 遮挡条件之前。
func CircuitProbeAdmission(now time.Time, target CircuitProbeTarget, manual bool) (CircuitProbeRejection, bool) {
	s := circuitProbe
	cfg, cb := s.configSnapshot()
	_, rejection, ok := s.admit(now, cfg, cb, target, manual, "", false)
	return rejection, ok
}

// QueueManualCircuitProbe 把一个手动/审计探测排进优先队列（同一目标只排一次）。
// 只接受"确切的、现在正在挡路的、可探的条目"：不是活条目就直接拒绝（v 空目标不排）。
func QueueManualCircuitProbe(target CircuitProbeTarget) bool {
	if target.ChannelID <= 0 || target.KeyID <= 0 || strings.TrimSpace(target.Model) == "" {
		return false
	}
	s := circuitProbe
	now := s.now()
	cfg, cb := s.configSnapshot()

	kind := classifyProbeScope(target.Endpoint, target.Capability)
	if kind == probeScopeMedia || kind == probeScopeUnknown {
		return false
	}
	if kind == probeScopeEmbedding && !cfg.EmbeddingsEnabled {
		return false
	}
	if cb.ResolveTarget != nil {
		route, ok := cb.ResolveTarget(target)
		if !ok {
			return false
		}
		switch classifyProbeScope(route.Endpoint, route.Capability) {
		case probeScopeText:
		case probeScopeEmbedding:
			if !cfg.EmbeddingsEnabled {
				return false
			}
		default:
			return false
		}
	}
	live, ok := liveProbeTarget(now, target)
	if !ok || !now.Before(live.expiry) {
		return false
	}

	key := target.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, queued := range s.manual {
		if queued.target.key() == key {
			return true
		}
	}
	s.manual = append(s.manual, probeManualRequest{target: target, generation: live.generation, writeSeq: live.writeSeq})
	return true
}

// RunDueCircuitProbes 串行跑完当前到期的探测（claim → RunProbe → complete），返回实际跑
// 的条数。接线层可以不用它，自己按 tick/claim/complete 驱动一条串行车道。
func RunDueCircuitProbes(ctx context.Context) int {
	if ctx == nil {
		ctx = context.Background()
	}
	s := circuitProbe
	_, cb := s.configSnapshot()
	if cb.RunProbe == nil {
		return 0
	}
	ran := 0
	for {
		if ctx.Err() != nil {
			return ran
		}
		claim, ok := s.claim(s.now())
		if !ok {
			return ran
		}
		s.complete(claim, cb.RunProbe(ctx, claim))
		ran++
	}
}

// SnapshotCircuitProbe 返回一个渠道的探测视图：per-entry 的 state/next_in_seconds、
// 渠道与全局的当日计数、最近 20 条历史（最新在前）。
func SnapshotCircuitProbe(channelID int) CircuitProbeChannelView {
	s := circuitProbe
	return s.snapshotChannel(s.now(), channelID)
}

// SnapshotCircuitProbeGlobal 返回当日全局计数与每渠道计数。
func SnapshotCircuitProbeGlobal() CircuitProbeGlobalView {
	s := circuitProbe
	return s.snapshotGlobal(s.now())
}
