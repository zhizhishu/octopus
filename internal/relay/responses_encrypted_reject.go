package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// Upstreams reject encrypted reasoning content with invalid_encrypted_content when a
// ciphertext was minted by a different account/key (or a key that has since rotated).
// A relay that only reacts per-request sends the same poisoned ciphertext again on the
// next turn of the same conversation, so the user sees the same failure every round.
//
// Reference behaviour (sub2api openai_encrypted_content_lineage.go): remember the
// digests an upstream rejected, keyed per conversation, and strip exactly those on
// subsequent outbound requests instead of dropping every reasoning signature
// (stripResponsesEncryptedContent is the blunt all-or-nothing variant and costs the
// whole reasoning context each turn).
//
// This store is deliberately scoped per conversation, not per request: the whole point
// is to survive across turns. Entries expire with the same TTL as responses sessions.
const (
	// Cap per conversation so a pathological upstream cannot grow the set without bound.
	responsesEncryptedRejectMaxPerSession = 256
	// Hard cap across all conversations; eviction is oldest-expiry-first via prune.
	responsesEncryptedRejectMaxEntries = 8192
	responsesEncryptedRejectPruneMin   = time.Minute
)

type responsesEncryptedRejectEntry struct {
	digests   map[string]struct{}
	expiresAt time.Time
}

var responsesEncryptedRejectStore = struct {
	sync.Mutex
	sessions    map[string]*responsesEncryptedRejectEntry
	lastPruneAt time.Time
}{
	sessions: make(map[string]*responsesEncryptedRejectEntry),
}

// responsesEncryptedDigest normalises a ciphertext so the same value is recognised
// whether it arrives via ReasoningSignature or inside ResponsesInputRaw. Hashing keeps
// the store from retaining plaintext ciphertext in memory beyond what is already held.
func responsesEncryptedDigest(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

// collectResponsesEncryptedDigests gathers digests for every encrypted reasoning item
// present on the request. Used when an upstream rejects the request: whatever was in
// the payload is a candidate, and the caller cannot tell which item the upstream
// objected to.
func collectResponsesEncryptedDigests(req *transformerModel.InternalLLMRequest) []string {
	if req == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 8)
	add := func(raw string) {
		digest := responsesEncryptedDigest(raw)
		if digest == "" {
			return
		}
		if _, ok := seen[digest]; ok {
			return
		}
		seen[digest] = struct{}{}
		out = append(out, digest)
	}
	for _, message := range req.Messages {
		if message.ReasoningSignature == nil {
			continue
		}
		add(*message.ReasoningSignature)
	}
	for _, raw := range collectEncryptedContentStringsFromRaw(req.ResponsesInputRaw) {
		add(raw)
	}
	return out
}

// collectEncryptedContentStringsFromRaw walks a raw Responses input payload and returns
// every encrypted_content string it finds. Mirrors removeEncryptedContentFields' traversal
// so the digest set and the strip pass agree on what counts as encrypted content.
func collectEncryptedContentStringsFromRaw(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	var out []string
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			if strings.TrimSpace(stringMapValue(typed, "type")) == "reasoning" {
				if text, ok := typed["encrypted_content"].(string); ok {
					out = append(out, text)
				}
			}
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	return out
}

// markResponsesEncryptedRejected records digests rejected by an upstream for a session.
func markResponsesEncryptedRejected(sessionKey string, digests []string) {
	if sessionKey == "" || len(digests) == 0 {
		return
	}
	now := time.Now()
	ttl := currentResponsesSessionTTL()

	responsesEncryptedRejectStore.Lock()
	defer responsesEncryptedRejectStore.Unlock()

	maybePruneResponsesEncryptedRejectsLocked(now)
	if len(responsesEncryptedRejectStore.sessions) >= responsesEncryptedRejectMaxEntries {
		// Pressure valve: drop expired first, then give up on remembering rather than
		// growing without bound. Losing memory degrades to current behaviour, never worse.
		pruneResponsesEncryptedRejectsLocked(now)
		if len(responsesEncryptedRejectStore.sessions) >= responsesEncryptedRejectMaxEntries {
			return
		}
	}
	entry, ok := responsesEncryptedRejectStore.sessions[sessionKey]
	if !ok {
		entry = &responsesEncryptedRejectEntry{digests: make(map[string]struct{})}
		responsesEncryptedRejectStore.sessions[sessionKey] = entry
	}
	entry.expiresAt = now.Add(ttl)
	for _, digest := range digests {
		if digest == "" {
			continue
		}
		if len(entry.digests) >= responsesEncryptedRejectMaxPerSession {
			break
		}
		entry.digests[digest] = struct{}{}
	}
}

// knownRejectedResponsesEncryptedDigests returns a copy of the digests remembered for a
// session, or nil when nothing is known. Returning a copy keeps callers from mutating the
// store outside the lock.
func knownRejectedResponsesEncryptedDigests(sessionKey string) map[string]struct{} {
	if sessionKey == "" {
		return nil
	}
	now := time.Now()

	responsesEncryptedRejectStore.Lock()
	defer responsesEncryptedRejectStore.Unlock()

	entry, ok := responsesEncryptedRejectStore.sessions[sessionKey]
	if !ok || now.After(entry.expiresAt) {
		return nil
	}
	if len(entry.digests) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(entry.digests))
	for digest := range entry.digests {
		out[digest] = struct{}{}
	}
	return out
}

func pruneResponsesEncryptedRejectsLocked(now time.Time) {
	for key, entry := range responsesEncryptedRejectStore.sessions {
		if now.After(entry.expiresAt) {
			delete(responsesEncryptedRejectStore.sessions, key)
		}
	}
	responsesEncryptedRejectStore.lastPruneAt = now
}

func maybePruneResponsesEncryptedRejectsLocked(now time.Time) {
	if !responsesEncryptedRejectStore.lastPruneAt.IsZero() &&
		now.Sub(responsesEncryptedRejectStore.lastPruneAt) < responsesEncryptedRejectPruneMin {
		return
	}
	pruneResponsesEncryptedRejectsLocked(now)
}

// PruneResponsesEncryptedRejectsExpired drops expired entries. Called from the same
// periodic task as PruneResponsesSessionsExpired so quiet periods reclaim memory.
func PruneResponsesEncryptedRejectsExpired() {
	now := time.Now()
	responsesEncryptedRejectStore.Lock()
	pruneResponsesEncryptedRejectsLocked(now)
	responsesEncryptedRejectStore.Unlock()
}

// resetResponsesEncryptedRejects clears the store. Test-only helper, kept unexported so
// production code cannot accidentally discard live reject memory.
func resetResponsesEncryptedRejects() {
	responsesEncryptedRejectStore.Lock()
	responsesEncryptedRejectStore.sessions = make(map[string]*responsesEncryptedRejectEntry)
	responsesEncryptedRejectStore.lastPruneAt = time.Time{}
	responsesEncryptedRejectStore.Unlock()
}

// stripResponsesEncryptedContentMatching removes only the encrypted items whose digest is
// in the rejected set, leaving everything else (including unrelated reasoning signatures)
// intact. Returns true when it removed at least one item.
func stripResponsesEncryptedContentMatching(req *transformerModel.InternalLLMRequest, rejected map[string]struct{}) bool {
	if req == nil || len(rejected) == 0 {
		return false
	}
	removed := false
	for idx := range req.Messages {
		signature := req.Messages[idx].ReasoningSignature
		if signature == nil {
			continue
		}
		if _, hit := rejected[responsesEncryptedDigest(*signature)]; hit {
			req.Messages[idx].ReasoningSignature = nil
			removed = true
		}
	}
	if len(req.ResponsesInputRaw) > 0 {
		if sanitized, ok := stripResponsesEncryptedContentMatchingRaw(req.ResponsesInputRaw, rejected); ok {
			req.ResponsesInputRaw = sanitized
			removed = true
		}
	}
	return removed
}

func stripResponsesEncryptedContentMatchingRaw(raw json.RawMessage, rejected map[string]struct{}) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	_, changed := removeRejectedEncryptedContentFields(value, rejected)
	if !changed {
		return raw, false
	}
	sanitized, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return sanitized, true
}

// removeRejectedEncryptedContentFields deletes encrypted_content only when its digest is in
// the rejected set. Reports whether anything was removed and whether the tree was mutated.
func removeRejectedEncryptedContentFields(value any, rejected map[string]struct{}) (removed bool, changed bool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			r, c := removeRejectedEncryptedContentFields(item, rejected)
			removed = removed || r
			changed = changed || c
		}
	case map[string]any:
		if strings.TrimSpace(stringMapValue(typed, "type")) == "reasoning" {
			if text, ok := typed["encrypted_content"].(string); ok {
				if _, hit := rejected[responsesEncryptedDigest(text)]; hit {
					delete(typed, "encrypted_content")
					removed = true
					changed = true
				}
			}
		}
		for _, item := range typed {
			r, c := removeRejectedEncryptedContentFields(item, rejected)
			removed = removed || r
			changed = changed || c
		}
	}
	return removed, changed
}

// encryptedRejectSessionKey scopes the reject store to one client conversation. The
// client session key already drives sticky routing and is the same identity the relay
// treats as "this conversation" everywhere else; userID/apiKeyID are mixed in so two
// tenants that happen to present the same client key never share reject memory (which
// would leak one tenant's traffic shape into another's request).
func (ra *relayAttempt) encryptedRejectSessionKey() string {
	if ra == nil || ra.relayRequest == nil {
		return ""
	}
	base := strings.TrimSpace(ra.clientSessionKey)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s|%d|%d", base, ra.userID, ra.apiKeyID)
}

// isResponsesEncryptedContentError reports whether an upstream message is an
// encrypted-reasoning refusal. Upstreams do not agree on the code, so the message
// fingerprints matter as much as the code: the same refusal has been observed as
// invalid_encrypted_content and as thinking_signature_invalid carrying the
// "could not be verified"/"could not be decrypted or parsed" wording. Matching on
// the code alone would silently miss half the cases.
func isResponsesEncryptedContentError(message string) bool {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "invalid_encrypted_content") {
		return true
	}
	// Anthropic-shaped upstreams report the same poisoned-thinking condition without ever
	// mentioning "encrypted content", so this wording has to be matched on its own.
	if strings.Contains(lower, "invalid signature in thinking block") {
		return true
	}
	encrypted := strings.Contains(lower, "encrypted content") || strings.Contains(lower, "encrypted_content")
	if !encrypted {
		return false
	}
	return strings.Contains(lower, "could not be verified") ||
		strings.Contains(lower, "could not be decrypted") ||
		strings.Contains(lower, "could not be parsed")
}

// rememberRejectedResponsesEncryptedContent records the ciphertexts currently on the
// request as rejected by this upstream. Called at the moment of refusal, before any
// strip discards them, because after the strip the evidence is gone.
func (ra *relayAttempt) rememberRejectedResponsesEncryptedContent() {
	if ra == nil || ra.internalRequest == nil {
		return
	}
	key := ra.encryptedRejectSessionKey()
	if key == "" {
		// No conversation identity (first turn, or a client that sends no session key):
		// there is nowhere safe to remember this, so the next turn starts clean rather
		// than sharing reject memory across unrelated callers.
		return
	}
	markResponsesEncryptedRejected(key, collectResponsesEncryptedDigests(ra.internalRequest))
}
