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
