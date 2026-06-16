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

// claudeFingerprintSessionID is stable for a sticky client session and otherwise a
// fresh UUID, so multi-turn conversations keep one session id.
func (ra *relayAttempt) claudeFingerprintSessionID() string {
	if ra != nil && strings.TrimSpace(ra.clientSessionKey) != "" {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("octopus:claude:session:"+ra.clientSessionKey)).String()
	}
	return uuid.NewString()
}
