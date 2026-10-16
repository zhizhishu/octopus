package relay

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// rescueTestClock is an injectable clock so TTL/capacity tests exercise the real
// store logic without real wall-clock waits.
type rescueTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newRescueTestClock() *rescueTestClock {
	return &rescueTestClock{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *rescueTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *rescueTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newRescueTestStore(capacity int, ttl time.Duration, clock *rescueTestClock) *rescueEpisodeStore {
	store := newRescueEpisodeStore(capacity, ttl)
	store.now = clock.Now
	return store
}

func rescueUserMessage(text string) model.Message {
	t := text
	return model.Message{Role: "user", Content: model.MessageContent{Content: &t}}
}

func rescueToolMessage(text string) model.Message {
	t := text
	return model.Message{Role: "tool", Content: model.MessageContent{Content: &t}}
}

func rescueAssistantMessage(text string) model.Message {
	t := text
	return model.Message{Role: "assistant", Content: model.MessageContent{Content: &t}}
}

func rescueRequest(modelName string, messages ...model.Message) *model.InternalLLMRequest {
	return &model.InternalLLMRequest{Model: modelName, Messages: messages}
}

func rescueKey(apiKeyID, userID int, endpoint, modelName, session, fingerprint string) rescueEpisodeKey {
	return rescueEpisodeKey{
		rescueEpisodeScope: rescueEpisodeScope{
			apiKeyID: apiKeyID,
			userID:   userID,
			endpoint: endpoint,
			model:    modelName,
			session:  session,
		},
		fingerprint: fingerprint,
	}
}

// TestRescueEpisodeConcurrentOnlyOneAllowed: a burst of concurrent Acquire calls
// for one episode must grant the rescue to exactly one of them.
func TestRescueEpisodeConcurrentOnlyOneAllowed(t *testing.T) {
	store := newRescueTestStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL, newRescueTestClock())
	key := rescueKey(1, 2, "/v1/messages", "claude", "sess-1", "fp-1")

	const workers = 64
	var allowed int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if store.acquire(key).Allowed {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if allowed != 1 {
		t.Fatalf("exactly one concurrent Acquire may be Allowed, got %d", allowed)
	}
}

// TestRescueEpisodeSuppressedBypassSuccessRestores: once an episode is exhausted
// it is suppressed; a request that is refused (bypassed) and then completes
// successfully must lift the suppression and re-open the episode.
func TestRescueEpisodeSuppressedBypassSuccessRestores(t *testing.T) {
	store := newRescueTestStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL, newRescueTestClock())
	key := rescueKey(1, 2, "/v1/messages", "claude", "sess", "fp")

	owner := store.acquire(key)
	if !owner.Allowed {
		t.Fatal("the first Acquire must be Allowed")
	}
	owner.Finish(false, true) // exhausted

	if entry := store.states[key]; entry == nil || !entry.suppressed {
		t.Fatalf("an exhausted owner must suppress the episode, state=%+v", entry)
	}

	bypass := store.acquire(key)
	if bypass == nil {
		t.Fatal("a suppressed episode must still return an observable lease")
	}
	if bypass.Allowed {
		t.Fatal("a suppressed episode must not be rescued again")
	}
	bypass.Finish(true, false) // normal-context full success

	if entry := store.states[key]; entry == nil || entry.suppressed {
		t.Fatalf("a bypass full success must lift suppression, state=%+v", entry)
	}
	if again := store.acquire(key); !again.Allowed {
		t.Fatal("after a bypass success the episode must be eligible again")
	}
}

// TestRescueEpisodeLateOwnerExhaustedDoesNotOverwriteBypassSuccess: the rescue
// owner exhausts AFTER a bypass success has already restored the episode; the late
// exhaustion must not re-suppress it.
func TestRescueEpisodeLateOwnerExhaustedDoesNotOverwriteBypassSuccess(t *testing.T) {
	store := newRescueTestStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL, newRescueTestClock())
	key := rescueKey(1, 2, "/v1/messages", "claude", "sess", "fp")

	owner := store.acquire(key)
	if !owner.Allowed {
		t.Fatal("the first Acquire must be Allowed")
	}
	bypass := store.acquire(key)
	if bypass.Allowed {
		t.Fatal("a rescue is already in flight, a duplicate must not be Allowed")
	}
	bypass.Finish(true, false) // bypass success bumps the epoch -> clears suppression

	owner.Finish(false, true) // late exhaustion from the stale owner

	entry := store.states[key]
	if entry == nil || entry.suppressed {
		t.Fatalf("a late owner exhaustion must not suppress after a bypass success, state=%+v", entry)
	}
	if entry.owner != 0 {
		t.Fatalf("the owner token must be released on Finish, owner=%d", entry.owner)
	}
	if next := store.acquire(key); !next.Allowed {
		t.Fatal("the episode must stay eligible after a bypass success")
	}
}

// TestRescueEpisodeCapacityAllInFlightDoesNotGrow: when the store is at capacity
// and every entry is in flight, a new episode is neither added nor rescued, and
// the store never grows past the cap.
func TestRescueEpisodeCapacityAllInFlightDoesNotGrow(t *testing.T) {
	store := newRescueTestStore(2, time.Hour, newRescueTestClock())
	a := store.acquire(rescueKey(1, 0, "/e", "m", "s-a", "fp-a"))
	b := store.acquire(rescueKey(1, 0, "/e", "m", "s-b", "fp-b"))
	if !a.Allowed || !b.Allowed {
		t.Fatal("setup: both in-flight leases must be Allowed")
	}
	if len(store.states) != 2 {
		t.Fatalf("setup size=%d, want 2", len(store.states))
	}

	c := store.acquire(rescueKey(1, 0, "/e", "m", "s-c", "fp-c"))
	if c == nil {
		t.Fatal("a recognizable request must still return a lease")
	}
	if c.Allowed {
		t.Fatal("capacity full and all in flight must not grant rescue")
	}
	if len(store.states) != 2 {
		t.Fatalf("capacity must not grow past 2, size=%d", len(store.states))
	}
	c.Finish(true, false) // must be a harmless no-op (no entry was created)
	if len(store.states) != 2 {
		t.Fatalf("a no-entry Finish must not add an entry, size=%d", len(store.states))
	}

	// Keep a and b alive so their owners remain in flight for the whole case.
	if _, ok := store.states[a.key]; !ok {
		t.Fatal("in-flight entry a must survive")
	}
	if _, ok := store.states[b.key]; !ok {
		t.Fatal("in-flight entry b must survive")
	}
}

// TestRescueEpisodeExpiredEvictionRecreateABA: a lease issued for an entry that
// was later swept and recreated (same key) must not mutate the new entry.
func TestRescueEpisodeExpiredEvictionRecreateABA(t *testing.T) {
	clock := newRescueTestClock()
	store := newRescueTestStore(rescueEpisodeCapacity, time.Hour, clock)
	key := rescueKey(1, 0, "/e", "m", "s", "fp")

	owner := store.acquire(key)
	if !owner.Allowed {
		t.Fatal("the first Acquire must be Allowed")
	}
	observer := store.acquire(key) // observing lease bound to the SAME entry
	if observer.Allowed {
		t.Fatal("the second Acquire must be Allowed=false")
	}

	owner.Finish(false, false) // release the owner -> entry becomes idle

	// Expire the idle entry, then a request for another episode triggers the sweep.
	clock.Advance(time.Hour + time.Minute)
	store.acquire(rescueKey(9, 9, "/e2", "m2", "s2", "fp2"))

	if _, ok := store.states[key]; ok {
		t.Fatal("an idle expired entry must have been swept")
	}

	recreated := store.acquire(key)
	if !recreated.Allowed {
		t.Fatal("the recreated episode must be fresh and eligible")
	}
	if recreated.entry.successEpoch != 0 {
		t.Fatalf("a fresh entry must start at epoch 0, got %d", recreated.entry.successEpoch)
	}

	observer.Finish(true, false) // stale lease from the evicted entry

	if recreated.entry.successEpoch != 0 {
		t.Fatalf("a stale lease must not mutate the recreated entry, epoch=%d", recreated.entry.successEpoch)
	}
	if recreated.entry.suppressed {
		t.Fatal("a stale lease must not suppress the recreated entry")
	}
}

// TestRescueEpisodeTenantIsolation: different tenants/keys are independent
// episodes.
func TestRescueEpisodeTenantIsolation(t *testing.T) {
	store := newRescueTestStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL, newRescueTestClock())
	keyA := rescueKey(1, 100, "/e", "m", "s", "fp")
	keyB := rescueKey(2, 200, "/e", "m", "s", "fp")

	ownerA := store.acquire(keyA)
	if !ownerA.Allowed {
		t.Fatal("A must be Allowed first")
	}
	ownerA.Finish(false, true) // suppress A

	if store.acquire(keyA).Allowed {
		t.Fatal("A must stay suppressed")
	}
	if !store.acquire(keyB).Allowed {
		t.Fatal("a different tenant/key must be an independent, eligible episode")
	}
}

// TestRescueEpisodeEndpointModelIsolation: the same tenant but a different
// endpoint/model is a different episode.
func TestRescueEpisodeEndpointModelIsolation(t *testing.T) {
	store := newRescueTestStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL, newRescueTestClock())
	keyA := rescueKey(1, 1, "/v1/messages", "claude-a", "s", "fp")
	keyB := rescueKey(1, 1, "/v1/messages", "claude-b", "s", "fp")
	keyC := rescueKey(1, 1, "/v1/responses", "claude-a", "s", "fp")

	ownerA := store.acquire(keyA)
	ownerA.Finish(false, true)

	if store.acquire(keyA).Allowed {
		t.Fatal("A must stay suppressed")
	}
	if !store.acquire(keyB).Allowed {
		t.Fatal("a different model must be independent")
	}
	if !store.acquire(keyC).Allowed {
		t.Fatal("a different endpoint must be independent")
	}
}

// TestRescueEpisodeToolOutputDoesNotChangeFingerprint: tool/assistant messages are
// not part of the user sequence, so more tool output keeps the same episode.
func TestRescueEpisodeToolOutputDoesNotChangeFingerprint(t *testing.T) {
	base := rescueRequest("m", rescueUserMessage("hello"))
	withTool := rescueRequest("m",
		rescueUserMessage("hello"),
		rescueAssistantMessage("assistant"),
		rescueToolMessage("tool output"),
		rescueToolMessage("more tool output"),
	)

	if baseFp, toolFp := rescueEpisodeFingerprint(base), rescueEpisodeFingerprint(withTool); baseFp != toolFp {
		t.Fatalf("tool/assistant messages must not change the user fingerprint: %s vs %s", baseFp, toolFp)
	}
}

// TestRescueEpisodeNewUserMessageReopens: appending a user message (even with
// identical text) changes the sequence and so the fingerprint.
func TestRescueEpisodeNewUserMessageReopens(t *testing.T) {
	first := rescueRequest("m", rescueUserMessage("hello"))
	second := rescueRequest("m", rescueUserMessage("hello"), rescueUserMessage("hello"))

	if firstFp, secondFp := rescueEpisodeFingerprint(first), rescueEpisodeFingerprint(second); firstFp == secondFp {
		t.Fatal("appending a user message must change the fingerprint (count and sequence both differ)")
	}
}

// TestRescueEpisodeNoSessionUsesPromptAnchorDistinctPerConversation: with no
// session header/metadata, the prompt-anchor fallback must still isolate distinct
// conversations.
func TestRescueEpisodeNoSessionUsesPromptAnchorDistinctPerConversation(t *testing.T) {
	a := rescueRequest("m", rescueUserMessage("conversation A opener"))
	b := rescueRequest("m", rescueUserMessage("conversation B opener"))

	sa := rescueStableSession(http.Header{}, a)
	sb := rescueStableSession(http.Header{}, b)
	if sa == "" || sb == "" {
		t.Fatalf("the prompt-anchor fallback must yield a session: %q, %q", sa, sb)
	}
	if sa == sb {
		t.Fatalf("different first user messages must yield different anchors: %q", sa)
	}
}

// TestRescueEpisodeTraceNotUsedAsSession: with BOTH a stable thread id and a
// volatile trace id present, the thread id must win (the trace id is removed from
// the derived copy, not the whole result discarded).
func TestRescueEpisodeTraceNotUsedAsSession(t *testing.T) {
	headers := http.Header{}
	headers.Set("AH-Thread-Id", "thread-stable")
	headers.Set("X-Client-Request-Id", "trace-volatile")

	req := rescueRequest("m", rescueUserMessage("hello"))
	got := rescueStableSession(headers, req)

	if want := hashRouteSessionKey("thread", "thread-stable"); got != want {
		t.Fatalf("the stable thread id must win over a volatile trace id, got %q want %q", got, want)
	}
	if got == hashRouteSessionKey("trace", "trace-volatile") {
		t.Fatal("a volatile trace id must never become the session identity")
	}
}

// TestRescueEpisodeClaudeSessionHeaderFallback: X-Claude-Code-Session-Id is a
// read-only session fallback when nothing else yields a stable session.
func TestRescueEpisodeClaudeSessionHeaderFallback(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Claude-Code-Session-Id", "11111111-1111-1111-1111-111111111111")

	req := rescueRequest("m", rescueUserMessage("hello"))
	got := rescueStableSession(headers, req)

	if want := hashRouteSessionKey("claude-session", "11111111-1111-1111-1111-111111111111"); got != want {
		t.Fatalf("X-Claude-Code-Session-Id must be a session fallback, got %q want %q", got, want)
	}
}

// TestRescueEpisodeUnknownIdentityReturnsNilLease: a request with no user content
// and no raw input has no usable identity -> nil lease, and nil Finish is safe.
func TestRescueEpisodeUnknownIdentityReturnsNilLease(t *testing.T) {
	req := &model.InternalLLMRequest{Model: "m"}

	if lease := acquireRescueEpisode(1, 2, "/e", "m", http.Header{}, req); lease != nil {
		t.Fatalf("unknown identity must return a nil lease, got %+v", lease)
	}
	if _, ok := rescueEpisodeKeyFor(1, 2, "/e", "m", http.Header{}, req); ok {
		t.Fatal("no user content and no raw input must yield an unknown identity")
	}

	var nilLease *rescueEpisodeLease
	nilLease.Finish(true, false) // must not panic
}

// TestRescueEpisodeStoresOnlyDigestNotRawText: the identity and state hold only a
// digest, never the raw prompt text.
func TestRescueEpisodeStoresOnlyDigestNotRawText(t *testing.T) {
	store := newRescueTestStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL, newRescueTestClock())
	const secret = "super-secret-prompt-text"

	req := rescueRequest("m", rescueUserMessage(secret))
	key, ok := rescueEpisodeKeyFor(1, 2, "/v1/messages", "m", http.Header{}, req)
	if !ok {
		t.Fatal("identity must be known")
	}
	if len(key.fingerprint) != 64 {
		t.Fatalf("the fingerprint must be a sha256 hex digest, got %q", key.fingerprint)
	}
	if strings.Contains(key.fingerprint, secret) {
		t.Fatalf("the fingerprint must not embed raw text: %q", key.fingerprint)
	}

	store.acquire(key)
	entry := store.states[key]
	if entry == nil {
		t.Fatal("the episode entry must exist")
	}
	if rendered := fmt.Sprintf("%+v", *entry); strings.Contains(rendered, secret) {
		t.Fatalf("the episode state must not hold raw text: %s", rendered)
	}
	if renderedKey := fmt.Sprintf("%+v", key); strings.Contains(renderedKey, secret) {
		t.Fatalf("the episode key must not hold raw text: %s", renderedKey)
	}
}

// TestRescueEpisodeIdleTTLSweep: an idle episode older than the TTL is swept on
// the next Acquire; a fresh one survives.
func TestRescueEpisodeIdleTTLSweep(t *testing.T) {
	clock := newRescueTestClock()
	store := newRescueTestStore(rescueEpisodeCapacity, time.Hour, clock)
	stale := rescueKey(1, 0, "/e", "m", "s-stale", "fp-stale")
	fresh := rescueKey(1, 0, "/e", "m", "s-fresh", "fp-fresh")

	staleLease := store.acquire(stale)
	if !staleLease.Allowed {
		t.Fatal("setup: stale must be Allowed")
	}
	staleLease.Finish(false, false) // idle

	clock.Advance(2 * time.Hour)
	store.acquire(fresh) // triggers the sweep

	if _, ok := store.states[stale]; ok {
		t.Fatal("an idle episode older than the TTL must be swept")
	}
	if _, ok := store.states[fresh]; !ok {
		t.Fatal("the fresh episode must remain")
	}
}

// TestRescueEpisodeCapacityEvictsOldestIdle: at capacity the least-recently-touched
// idle entry is evicted to make room.
func TestRescueEpisodeCapacityEvictsOldestIdle(t *testing.T) {
	clock := newRescueTestClock()
	store := newRescueTestStore(2, 24*time.Hour, clock)
	k1 := rescueKey(1, 0, "/e", "m", "s1", "f1")
	k2 := rescueKey(1, 0, "/e", "m", "s2", "f2")
	k3 := rescueKey(1, 0, "/e", "m", "s3", "f3")

	l1 := store.acquire(k1)
	l1.Finish(false, false)
	clock.Advance(time.Minute)
	l2 := store.acquire(k2)
	l2.Finish(false, false)
	clock.Advance(time.Minute)

	store.acquire(k3) // at capacity: must evict the oldest idle (k1)

	if _, ok := store.states[k1]; ok {
		t.Fatal("the oldest idle entry must be evicted at capacity")
	}
	if _, ok := store.states[k2]; !ok {
		t.Fatal("the newer idle entry must remain")
	}
	if _, ok := store.states[k3]; !ok {
		t.Fatal("the new entry must be added")
	}
}

// TestRescueEpisodeFinishIdempotentAndNilSafe: Finish is once-only and nil-safe.
func TestRescueEpisodeFinishIdempotentAndNilSafe(t *testing.T) {
	store := newRescueTestStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL, newRescueTestClock())
	key := rescueKey(1, 0, "/e", "m", "s", "fp")

	lease := store.acquire(key)
	if !lease.Allowed {
		t.Fatal("the first Acquire must be Allowed")
	}
	lease.Finish(true, false) // success
	lease.Finish(false, true) // second call must be a no-op

	entry := store.states[key]
	if entry == nil || entry.suppressed {
		t.Fatalf("the second Finish must be a no-op after success, state=%+v", entry)
	}

	var nilLease *rescueEpisodeLease
	nilLease.Finish(false, true) // must not panic
}
