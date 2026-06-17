package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/google/uuid"
)

// ensureClaudeMetadataUserID injects a Claude-Code-shaped metadata.user_id when the
// client did not send one. Relays such as AnyRouter route Anthropic requests to
// their serving pool only when they carry a genuine Claude Code fingerprint: a
// metadata.user_id holding a 64-hex device_id plus a session UUID, serialised as
// COMPACT JSON (a spaced form, or a missing/short id, is risk-rejected 429/503 before
// the business layer). Real Claude Code already supplies one, so it passes through
// untouched; minimal or non-CLI clients get a synthesised one (stable per user+key)
// so the same channel works for them too — paired with the agent-identity system
// block injected by the Anthropic outbound transformer.
func (ra *relayAttempt) ensureClaudeMetadataUserID() {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return
	}
	if ra.channel.Type != outbound.OutboundTypeAnthropic {
		return
	}
	if ra.internalRequest.Metadata == nil {
		ra.internalRequest.Metadata = map[string]string{}
	}
	if strings.TrimSpace(ra.internalRequest.Metadata["user_id"]) != "" {
		return
	}
	device := claudeFingerprintDeviceID(ra.userID, ra.apiKeyID)
	session := ra.claudeFingerprintSessionID()
	// Compact, no spaces — AnyRouter rejects the spaced json form.
	ra.internalRequest.Metadata["user_id"] = `{"device_id":"` + device + `","account_uuid":"","session_id":"` + session + `"}`
}

// claudeFingerprintDeviceID returns a stable 64-hex device id per user+api key,
// matching the shape a real Claude Code install reports.
func claudeFingerprintDeviceID(userID, apiKeyID int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("octopus:claude:device:%d:%d", userID, apiKeyID)))
	return hex.EncodeToString(sum[:])
}

// claudeFingerprintSessionID returns the session UUID shared by BOTH the
// X-Claude-Code-Session-Id header and the body metadata.user_id.session_id, the way
// real Claude Code emits one UUID in both places. It is deterministic for a sticky
// client session / prompt-cache key / api-key context so the header and body always
// agree (and multi-turn conversations keep one id), falling back to a fresh UUID only
// for fully anonymous requests.
func (ra *relayAttempt) claudeFingerprintSessionID() string {
	if ra != nil {
		if key := strings.TrimSpace(ra.clientSessionKey); key != "" {
			return claudeSessionUUID(key)
		}
		if ra.internalRequest != nil && ra.internalRequest.PromptCacheKey != nil {
			if value := strings.TrimSpace(*ra.internalRequest.PromptCacheKey); value != "" {
				return claudeSessionUUID("prompt-cache:" + value)
			}
		}
		if ra.apiKeyID > 0 || ra.userID > 0 {
			return claudeSessionUUID(fmt.Sprintf("octopus:%d:%d", ra.userID, ra.apiKeyID))
		}
	}
	return uuid.NewString()
}

// claudeSessionUUID derives a stable RFC-4122 UUID from a seed so the synthesized
// session id has the same shape a real Claude Code session id does.
func claudeSessionUUID(seed string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("octopus:claude:session:"+seed)).String()
}
