package relay

import (
	"strconv"
	"testing"
	"time"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// The reject store is process-wide and keyed by conversation, so every test that touches
// it must start clean or ordering between tests changes the outcome.
func newRejectTestAttempt(sessionKey string, userID, apiKeyID int) *relayAttempt {
	return &relayAttempt{relayRequest: &relayRequest{
		clientSessionKey: sessionKey,
		userID:           userID,
		apiKeyID:         apiKeyID,
	}}
}

func pointerString(v string) *string { return &v }

func TestResponsesEncryptedRejectKeyScopesPerConversationAndTenant(t *testing.T) {
	resetResponsesEncryptedRejects()
	t.Cleanup(resetResponsesEncryptedRejects)

	base := newRejectTestAttempt("session-a", 1, 2).encryptedRejectSessionKey()
	if base == "" {
		t.Fatal("expected a non-empty key when the client provides a session key")
	}
	// Same conversation, different tenant: must not share reject memory, or one tenant's
	// refused ciphertexts would silently strip another tenant's request.
	otherTenant := newRejectTestAttempt("session-a", 1, 99).encryptedRejectSessionKey()
	if otherTenant == base {
		t.Fatalf("api key must be part of the scope, got identical key %q", base)
	}
	// No session identity at all: remembering would leak across unrelated callers.
	if got := newRejectTestAttempt("", 1, 2).encryptedRejectSessionKey(); got != "" {
		t.Fatalf("empty session key must yield an empty scope, got %q", got)
	}
	if got := (*relayAttempt)(nil).encryptedRejectSessionKey(); got != "" {
		t.Fatalf("nil attempt must yield an empty scope, got %q", got)
	}
}

func TestStripResponsesEncryptedContentMatchingRemovesOnlyRejected(t *testing.T) {
	poisoned := "ciphertext-rejected"
	healthy := "ciphertext-still-valid"
	req := &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{
			{Role: "assistant", ReasoningSignature: pointerString(poisoned)},
			{Role: "assistant", ReasoningSignature: pointerString(healthy)},
			{Role: "user"},
		},
	}
	rejected := map[string]struct{}{responsesEncryptedDigest(poisoned): {}}

	if !stripResponsesEncryptedContentMatching(req, rejected) {
		t.Fatal("expected the rejected ciphertext to be stripped")
	}
	if req.Messages[0].ReasoningSignature != nil {
		t.Error("rejected ciphertext must be removed")
	}
	// This is the whole point of precise stripping: an unrelated reasoning signature
	// survives, so the continuation keeps its reasoning context.
	if req.Messages[1].ReasoningSignature == nil || *req.Messages[1].ReasoningSignature != healthy {
		t.Error("unrelated ciphertext must be preserved")
	}
	if req.Messages[2].ReasoningSignature != nil {
		t.Error("message without a signature must stay untouched")
	}
}

func TestStripResponsesEncryptedContentMatchingRawTargetsRejectedOnly(t *testing.T) {
	poisoned := "raw-rejected"
	healthy := "raw-valid"
	req := &transformerModel.InternalLLMRequest{
		ResponsesInputRaw: []byte(`{"input":[
			{"type":"reasoning","encrypted_content":"` + poisoned + `"},
			{"type":"reasoning","encrypted_content":"` + healthy + `"},
			{"type":"message","role":"user","content":"hi"}
		]}`),
	}
	rejected := map[string]struct{}{responsesEncryptedDigest(poisoned): {}}

	if !stripResponsesEncryptedContentMatching(req, rejected) {
		t.Fatal("expected raw payload to be modified")
	}
	body := string(req.ResponsesInputRaw)
	if contains(body, poisoned) {
		t.Error("rejected ciphertext must be gone from the raw payload")
	}
	if !contains(body, healthy) {
		t.Error("unrelated ciphertext must survive in the raw payload")
	}
	if !contains(body, `"role":"user"`) {
		t.Error("non-reasoning items must be preserved verbatim")
	}
}

func TestStripResponsesEncryptedContentMatchingNoMatchLeavesRequestAlone(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{
			{Role: "assistant", ReasoningSignature: pointerString("untouched")},
		},
	}
	rejected := map[string]struct{}{responsesEncryptedDigest("something-else"): {}}
	if stripResponsesEncryptedContentMatching(req, rejected) {
		t.Fatal("no-op expected when nothing matches")
	}
	if req.Messages[0].ReasoningSignature == nil {
		t.Error("non-matching signature must survive")
	}
	// Guards against the old all-or-nothing behaviour creeping back in via a nil set.
	if stripResponsesEncryptedContentMatching(req, nil) {
		t.Error("nil reject set must be a no-op")
	}
}

func TestRememberRejectedResponsesEncryptedContentRoundTrip(t *testing.T) {
	resetResponsesEncryptedRejects()
	t.Cleanup(resetResponsesEncryptedRejects)

	ra := newRejectTestAttempt("session-roundtrip", 7, 8)
	poisoned := "poisoned-ciphertext"
	ra.internalRequest = &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{
			{Role: "assistant", ReasoningSignature: pointerString(poisoned)},
		},
	}

	key := ra.encryptedRejectSessionKey()
	if got := knownRejectedResponsesEncryptedDigests(key); got != nil {
		t.Fatal("nothing should be remembered before the first rejection")
	}

	ra.rememberRejectedResponsesEncryptedContent()

	known := knownRejectedResponsesEncryptedDigests(key)
	if _, ok := known[responsesEncryptedDigest(poisoned)]; !ok {
		t.Fatal("rejected ciphertext must be remembered for the conversation")
	}

	// A later turn of the same conversation must strip it without being told again.
	nextReq := &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{
			{Role: "assistant", ReasoningSignature: pointerString(poisoned)},
			{Role: "assistant", ReasoningSignature: pointerString("fresh-ciphertext")},
		},
	}
	if !stripResponsesEncryptedContentMatching(nextReq, known) {
		t.Fatal("the remembered ciphertext must be stripped on the next turn")
	}
	if nextReq.Messages[0].ReasoningSignature != nil {
		t.Error("remembered ciphertext must be gone")
	}
	if nextReq.Messages[1].ReasoningSignature == nil {
		t.Error("a ciphertext never rejected must survive")
	}
}

func TestRememberRejectedResponsesEncryptedContentSkipsWithoutSession(t *testing.T) {
	resetResponsesEncryptedRejects()
	t.Cleanup(resetResponsesEncryptedRejects)

	ra := newRejectTestAttempt("", 1, 2)
	ra.internalRequest = &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{
			{Role: "assistant", ReasoningSignature: pointerString("x")},
		},
	}
	ra.rememberRejectedResponsesEncryptedContent()
	if len(responsesEncryptedRejectStore.sessions) != 0 {
		t.Fatal("no session identity means nothing may be stored")
	}
}

func TestKnownRejectedResponsesEncryptedDigestsDropsExpired(t *testing.T) {
	resetResponsesEncryptedRejects()
	t.Cleanup(resetResponsesEncryptedRejects)

	key := "expiring-session"
	markResponsesEncryptedRejected(key, []string{responsesEncryptedDigest("old")})
	if knownRejectedResponsesEncryptedDigests(key) == nil {
		t.Fatal("freshly marked entry should be readable")
	}

	responsesEncryptedRejectStore.Lock()
	responsesEncryptedRejectStore.sessions[key].expiresAt = time.Now().Add(-time.Second)
	responsesEncryptedRejectStore.Unlock()

	if got := knownRejectedResponsesEncryptedDigests(key); got != nil {
		t.Fatal("an expired entry must not be returned")
	}
}

func TestMarkResponsesEncryptedRejectedCapsPerSession(t *testing.T) {
	resetResponsesEncryptedRejects()
	t.Cleanup(resetResponsesEncryptedRejects)

	key := "capped-session"
	digests := make([]string, 0, responsesEncryptedRejectMaxPerSession+50)
	for i := 0; i < responsesEncryptedRejectMaxPerSession+50; i++ {
		digests = append(digests, responsesEncryptedDigest(strconv.Itoa(i)))
	}
	markResponsesEncryptedRejected(key, digests)

	responsesEncryptedRejectStore.Lock()
	entries := len(responsesEncryptedRejectStore.sessions[key].digests)
	responsesEncryptedRejectStore.Unlock()

	if entries != responsesEncryptedRejectMaxPerSession {
		t.Fatalf("per-session cap not enforced: got %d, want %d", entries, responsesEncryptedRejectMaxPerSession)
	}
}

func TestIsResponsesEncryptedContentErrorRecognisesObservedWordings(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "observed upstream verbatim",
			msg:  "responses stream failed: The encrypted content abc could not be verified. Reason: Encrypted content could not be decrypted or parsed., code: invalid_encrypted_content, type: invalid_request_error",
			want: true,
		},
		{
			name: "code only",
			msg:  "upstream said invalid_encrypted_content",
			want: true,
		},
		{
			name: "thinking signature wording",
			msg:  "Invalid signature in thinking block",
			want: true,
		},
		{
			name: "encrypted but unrelated failure",
			msg:  "encrypted content field is required",
			want: false,
		},
		{
			name: "unrelated stream error",
			msg:  "upstream stream ended without internal response",
			want: false,
		},
		{name: "empty", msg: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResponsesEncryptedContentError(tc.msg); got != tc.want {
				t.Fatalf("isResponsesEncryptedContentError(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsRetryableResponsesStreamRejectionKeepsOriginalChannel(t *testing.T) {
	if isRetryableResponsesStreamRejection(nil) {
		t.Fatal("nil error must not be retryable")
	}
	if isRetryableResponsesStreamRejection(errString("upstream stream ended without internal response")) {
		t.Fatal("the original stream-empty error is handled by isRetryableUpstreamStreamError, not here")
	}
	if !isRetryableResponsesStreamRejection(errString("... could not be decrypted or parsed., code: invalid_encrypted_content")) {
		t.Fatal("an encrypted-content refusal must widen the retry set")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
