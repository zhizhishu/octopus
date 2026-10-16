package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// rescue_episode.go owns the per-message "machine rescue" eligibility gate.
//
// The relay may start at most ONE machine-driven automatic rescue per ACTIVE USER
// MESSAGE (episode). This gate answers two questions per inbound request:
//
//  1. May this request start a machine rescue for its episode? (lease.Allowed)
//  2. The (single) owner of a rescue reports its outcome through lease.Finish, and
//     ANY request for the same episode — including one that was bypassed and never
//     got to rescue — may report a complete success, which restores eligibility.
//
// Identity: an episode key is (apiKeyID, userID, endpoint, model, stableSession)
// plus the fingerprint of the request's FULL user-message Content sequence. A new
// user message changes that sequence (length or content), so it is a different,
// freshly-eligible episode; a byte-identical resend of the same history is the
// same episode and stays suppressed once exhausted.
//
// LIMITATION (standard protocols): a client re-sending an identical full history
// ("the user clicked retry") and a client auto-retrying the same turn are
// byte-identical on the wire, so this gate CANNOT tell them apart. It keys on the
// message content alone and does not claim to distinguish the two. Because the
// fingerprint covers only the ordered user Contents, tool/assistant growth never
// fakes a new user message, and only a real new user message reopens an episode.
//
// Only bounded digests are kept (never raw chat text): the store holds a hash, a
// suppression flag, an owner token, a success epoch and a timestamp per episode.
const (
	rescueEpisodeCapacity = 4096
	rescueEpisodeIdleTTL  = time.Hour
)

// rescueEpisodeScope is the isolation tuple. Trace-scoped ids are deliberately
// excluded upstream (see rescueStableSession) because they change per request.
type rescueEpisodeScope struct {
	apiKeyID int
	userID   int
	endpoint string
	model    string
	session  string
}

// rescueEpisodeKey is the full episode identity: the isolation tuple plus the
// ordered user-content fingerprint.
type rescueEpisodeKey struct {
	rescueEpisodeScope
	fingerprint string
}

// rescueEpisodeState is the bounded per-episode state. Suppression and the
// success epoch are what make "a late outcome must not overwrite a newer success"
// true; the owner token is the only thing a rescue lease may release.
type rescueEpisodeState struct {
	suppressed   bool
	owner        uint64 // 0 = no in-flight rescue owner; non-zero = the owner's token
	successEpoch uint64 // bumped on every complete success; clears suppression
	lastTouched  time.Time
}

type rescueEpisodeStore struct {
	mu             sync.Mutex
	capacity       int
	idleTTL        time.Duration
	now            func() time.Time
	states         map[rescueEpisodeKey]*rescueEpisodeState
	nextOwnerToken uint64
}

func newRescueEpisodeStore(capacity int, idleTTL time.Duration) *rescueEpisodeStore {
	return &rescueEpisodeStore{
		capacity: capacity,
		idleTTL:  idleTTL,
		now:      time.Now,
		states:   make(map[rescueEpisodeKey]*rescueEpisodeState),
	}
}

// rescueEpisodes is the process-wide gate. It is deliberately a plain bounded
// map behind one mutex (no goroutine, no background sweep): expiry and capacity
// are enforced lazily on every Acquire, which is enough to bound memory.
var rescueEpisodes = newRescueEpisodeStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL)

// rescueEpisodeLease is the result of Acquire. Allowed says whether THIS request
// may start a machine rescue; a request that is refused (a suppressed episode or a
// rescue already in flight) still receives a lease so it can report its outcome.
// A nil lease means the request had no usable identity — do not rescue — and a
// nil receiver Finish is a safe no-op, so callers may Finish unconditionally.
type rescueEpisodeLease struct {
	store        *rescueEpisodeStore
	key          rescueEpisodeKey
	entry        *rescueEpisodeState // identity guard against evict/recreate ABA
	ownerToken   uint64              // 0 when this lease does not own the rescue
	successEpoch uint64              // successEpoch observed at Acquire
	Allowed      bool
	finished     bool
}

// acquireRescueEpisode is the SINGLE entry point the relay wires to. It derives
// the episode identity and returns the lease. It returns nil only when the
// request identity is unknown (no user content AND no raw-input fallback), in
// which case machine rescue is not attempted.
func acquireRescueEpisode(apiKeyID, userID int, endpoint, model string, headers http.Header, req *model.InternalLLMRequest) *rescueEpisodeLease {
	key, ok := rescueEpisodeKeyFor(apiKeyID, userID, endpoint, model, headers, req)
	if !ok {
		return nil
	}
	return rescueEpisodes.acquire(key)
}

func (s *rescueEpisodeStore) acquire(key rescueEpisodeKey) *rescueEpisodeLease {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(now)

	entry := s.states[key]
	if entry == nil {
		if s.capacity > 0 && len(s.states) >= s.capacity {
			// Capacity full: reclaim one idle entry, but never evict an in-flight
			// owner (a running lease must outlive TTL and capacity pressure). If
			// every entry is in flight, neither add nor rescue.
			if !s.evictOldestIdleLocked() {
				return &rescueEpisodeLease{store: s, key: key}
			}
		}
		entry = &rescueEpisodeState{lastTouched: now}
		s.states[key] = entry
	}
	entry.lastTouched = now

	lease := &rescueEpisodeLease{
		store:        s,
		key:          key,
		entry:        entry,
		successEpoch: entry.successEpoch,
	}
	if !entry.suppressed && entry.owner == 0 {
		s.nextOwnerToken++
		entry.owner = s.nextOwnerToken
		lease.ownerToken = s.nextOwnerToken
		lease.Allowed = true
	}
	return lease
}

// Finish reports the episode outcome and releases this lease. It is idempotent
// (guarded by the store mutex) and nil-safe.
//
//   - success   == true: a complete success re-opens the episode (bump the epoch,
//     clear suppression). This applies to ANY lease, including a bypassed one that
//     never got to rescue, so a normal-context success restores eligibility.
//   - exhausted == true: only the OWNER may suppress, and only while no newer
//     success happened since it was granted (the epoch is unchanged), so a late
//     exhausted cannot overwrite a bypass success. "Client cancel" is neither.
func (l *rescueEpisodeLease) Finish(success bool, exhausted bool) {
	if l == nil || l.store == nil {
		return
	}

	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotent under the store mutex: a second Finish never mutates state.
	if l.finished {
		return
	}
	l.finished = true

	// ABA guard: the entry must still be the exact object this lease was issued
	// for. If it was swept and recreated, leave the new entry untouched.
	if l.entry == nil || s.states[l.key] != l.entry {
		return
	}
	entry := l.entry

	// The owner token is released only by the lease that holds it.
	if l.ownerToken != 0 && entry.owner == l.ownerToken {
		entry.owner = 0
	}

	switch {
	case success:
		entry.successEpoch++
		entry.suppressed = false
		entry.lastTouched = s.now()
	case exhausted:
		if l.ownerToken != 0 && entry.successEpoch == l.successEpoch {
			entry.suppressed = true
		}
		entry.lastTouched = s.now()
	default:
		// Client cancellation: release only the owner (done above); never suppress.
		entry.lastTouched = s.now()
	}
}

// sweepLocked drops idle entries whose last touch is older than the TTL. An
// in-flight entry (owner != 0) is never swept.
func (s *rescueEpisodeStore) sweepLocked(now time.Time) {
	if s.idleTTL <= 0 {
		return
	}
	for key, entry := range s.states {
		if entry.owner == 0 && now.Sub(entry.lastTouched) > s.idleTTL {
			delete(s.states, key)
		}
	}
}

// evictOldestIdleLocked removes the least-recently-touched IDLE entry and reports
// whether one was removed. An in-flight entry (owner != 0) is never evicted. A
// single map scan keeps the store from carrying an unbounded order list.
func (s *rescueEpisodeStore) evictOldestIdleLocked() bool {
	var oldestKey rescueEpisodeKey
	var oldestAt time.Time
	found := false
	for key, entry := range s.states {
		if entry.owner != 0 {
			continue
		}
		if !found || entry.lastTouched.Before(oldestAt) {
			oldestKey, oldestAt, found = key, entry.lastTouched, true
		}
	}
	if !found {
		return false
	}
	delete(s.states, oldestKey)
	return true
}

// rescueEpisodeKeyFor derives the full episode identity. ok is false when the
// request has no usable identity: no user content AND no raw-input fallback. Such
// a request is never collapsed onto one global key.
func rescueEpisodeKeyFor(apiKeyID, userID int, endpoint, model string, headers http.Header, req *model.InternalLLMRequest) (rescueEpisodeKey, bool) {
	fingerprint := rescueEpisodeFingerprint(req)
	if fingerprint == "" {
		return rescueEpisodeKey{}, false
	}
	return rescueEpisodeKey{
		rescueEpisodeScope: rescueEpisodeScope{
			apiKeyID: apiKeyID,
			userID:   userID,
			endpoint: endpoint,
			model:    model,
			session:  rescueStableSession(headers, req),
		},
		fingerprint: fingerprint,
	}, true
}

// rescueStableSession derives the per-conversation session component of the scope.
//
// Volatile trace ids must not become the session identity, but a request can carry
// BOTH a real thread/session id and a trace id. So we DERIVE FROM A COPY with the
// trace candidates removed (rather than deriving and discarding the whole result):
// the real thread/session survives the copy while the trace id is gone.
//
// Fallbacks in order: trace-stripped deriveClientSessionInfo -> the read-only
// X-Claude-Code-Session-Id header -> the prompt-anchor digest. The header fallback
// is READ ONLY: no outbound header/identity is ever modified.
func rescueStableSession(headers http.Header, req *model.InternalLLMRequest) string {
	if info := deriveClientSessionInfo(rescueHeadersWithoutTrace(headers), rescueRequestWithoutTraceMetadata(req)); info.Key != "" {
		return info.Key
	}
	if headers != nil {
		if value := normalizeRouteSessionValue(headers.Get("X-Claude-Code-Session-Id")); value != "" {
			return hashRouteSessionKey("claude-session", value)
		}
	}
	if info := deriveOctopusManagedPromptAnchorSessionInfo(req); info.Key != "" {
		return info.Key
	}
	return ""
}

func rescueHeadersWithoutTrace(headers http.Header) http.Header {
	if len(headers) == 0 {
		return headers
	}
	clone := headers.Clone()
	for _, key := range routeHeaderTraceCandidate.keys {
		clone.Del(key)
	}
	return clone
}

func rescueRequestWithoutTraceMetadata(req *model.InternalLLMRequest) *model.InternalLLMRequest {
	if req == nil || len(req.Metadata) == 0 {
		return req
	}
	clone := *req
	metadata := make(map[string]string, len(req.Metadata))
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	for _, key := range routeMetadataTraceCandidate.keys {
		delete(metadata, key)
	}
	clone.Metadata = metadata
	return &clone
}

// rescueEpisodeFingerprint is the ordered digest of every message whose Role is
// exactly "user", plus the user-message count. tool / assistant / system messages
// are NOT counted, so growing tool output never fakes a new user message, while a
// genuinely new user message (even with identical text) changes the sequence.
//
// When there is no user message at all (a bare Responses tool-continuation), it
// degrades to a stable hash of the raw Responses input. That fallback is a
// per-request identity only; it is never used to pause a whole api key.
func rescueEpisodeFingerprint(req *model.InternalLLMRequest) string {
	if digest := rescueUserContentDigest(req); digest != "" {
		return digest
	}
	return rescueRawInputDigest(req)
}

// rescueUserSequence frames the ordered user contents (and the count) with the
// JSON encoder so the digest is unambiguous — no hand-rolled separators a content
// string could collide with. Only the Content field is encoded: cache_control is a
// help field (json:"-") and never reaches the digest, and no raw text is stored —
// only the resulting hash.
type rescueUserSequence struct {
	Count    int                    `json:"count"`
	Contents []model.MessageContent `json:"contents"`
}

func rescueUserContentDigest(req *model.InternalLLMRequest) string {
	if req == nil {
		return ""
	}
	contents := make([]model.MessageContent, 0, len(req.Messages))
	for i := range req.Messages {
		if req.Messages[i].Role != "user" {
			continue
		}
		contents = append(contents, req.Messages[i].Content)
	}
	if len(contents) == 0 {
		return ""
	}
	hasher := sha256.New()
	if err := json.NewEncoder(hasher).Encode(rescueUserSequence{Count: len(contents), Contents: contents}); err != nil {
		// The internal content model cannot normally fail to encode; if it ever did,
		// return no digest rather than emit a half-written hash.
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func rescueRawInputDigest(req *model.InternalLLMRequest) string {
	if req == nil || len(req.ResponsesInputRaw) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(req.ResponsesInputRaw, &decoded); err != nil {
		sum := sha256.Sum256(req.ResponsesInputRaw)
		return "raw:" + hex.EncodeToString(sum[:])
	}
	hasher := sha256.New()
	if err := json.NewEncoder(hasher).Encode(decoded); err != nil {
		sum := sha256.Sum256(req.ResponsesInputRaw)
		return "raw:" + hex.EncodeToString(sum[:])
	}
	return "raw:" + hex.EncodeToString(hasher.Sum(nil))
}
