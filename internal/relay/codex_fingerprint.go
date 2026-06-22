package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/google/uuid"
)

const (
	codexMetadataInstallationID = "x-codex-installation-id"
	codexMetadataWindowID       = "x-codex-window-id"
	codexMetadataTurnMetadata   = "x-codex-turn-metadata"
)

func (ra *relayAttempt) prepareCodexRequestFingerprint() {
	if ra == nil || ra.internalRequest == nil || !ra.shouldUseCodexFingerprint() {
		return
	}

	sessionID := codexSessionIDFromRequest(ra.internalRequest)
	if strings.HasPrefix(sessionID, autoPromptCacheKeyPrefix) {
		sessionID = stableCodexUUID("auto-prompt-cache", sessionID)
	}
	if sessionID == "" {
		sessionID = ra.defaultCodexSessionID()
	}
	if sessionID == "" {
		return
	}
	ra.internalRequest.PromptCacheKey = &sessionID
	ra.internalRequest.ClientMetadata = ensureCodexClientMetadata(ra.internalRequest.ClientMetadata, sessionID, ra.defaultCodexInstallationID())
}

func (ra *relayAttempt) shouldUseCodexFingerprint() bool {
	if ra == nil || ra.channel == nil || !shouldApplyChannelCloak(ra.channel.Cloak) {
		return false
	}
	switch ra.channel.Type {
	case outbound.OutboundTypeOpenAIResponse:
		return true
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		return ra.inboundType == inbound.InboundTypeOpenAIResponse
	default:
		return false
	}
}

func (ra *relayAttempt) inboundLooksLikeCodexClient() bool {
	if ra == nil || ra.c == nil || ra.c.Request == nil {
		return false
	}
	headers := ra.c.Request.Header
	if strings.EqualFold(strings.TrimSpace(headers.Get("Originator")), defaultCodexOriginator) {
		return true
	}
	if strings.TrimSpace(headers.Get("X-Codex-Beta-Features")) != "" ||
		strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")) != "" ||
		strings.TrimSpace(headers.Get("X-Codex-Window-Id")) != "" {
		return true
	}
	ua := strings.ToLower(headers.Get("User-Agent"))
	return strings.Contains(ua, "codex_exec") || strings.Contains(ua, "codex-cli")
}

func (ra *relayAttempt) defaultCodexSessionID() string {
	if ra == nil {
		return uuid.NewString()
	}
	parts := []string{
		"user=" + strconv.Itoa(ra.userID),
		"api_key=" + strconv.Itoa(ra.apiKeyID),
		"model=" + strings.ToLower(strings.TrimSpace(ra.requestModel)),
	}
	if sessionKey := strings.TrimSpace(ra.clientSessionKey); sessionKey != "" {
		parts = append(parts, "client_session="+sessionKey)
	} else {
		parts = append(parts, "request="+normalizeCacheHintJSON(ra.internalRequest))
	}
	seed := strings.Join(parts, "|")
	if strings.Trim(seed, "| =") == "" {
		return uuid.NewString()
	}
	return stableCodexUUID("session", seed)
}

func (ra *relayAttempt) defaultCodexInstallationID() string {
	// ONE uniform per-instance codex installation id for ALL codex traffic — an upstream
	// must not see a different install just because a different downstream user/api-key
	// relayed through octopus. Shared with the channel/model test path (op.CodexInstallationID).
	return op.CodexInstallationID()
}

func stableCodexUUID(kind, seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return uuid.NewString()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("octopus:codex:"+strings.TrimSpace(kind)+":"+seed)).String()
}

func applyCodexSessionHeaders(headers http.Header, req *transformerModel.InternalLLMRequest) {
	if headers == nil || req == nil {
		return
	}
	sessionID := codexSessionIDFromRequest(req)
	if sessionID == "" {
		return
	}
	setHeaderIfMissing(headers, "Session_id", sessionID)
	setHeaderIfMissing(headers, "Session-Id", sessionID)
	setHeaderIfMissing(headers, "X-Session-Id", sessionID)
	setHeaderIfMissing(headers, "Thread-Id", sessionID)
	setHeaderIfMissing(headers, "X-Client-Request-Id", sessionID)
	setHeaderIfMissing(headers, "X-Codex-Window-Id", sessionID+":0")
	if turnMetadata := codexTurnMetadataFromRequest(req, sessionID); turnMetadata != "" {
		setHeaderIfMissing(headers, "X-Codex-Turn-Metadata", turnMetadata)
	}
}

func codexSessionIDFromRequest(req *transformerModel.InternalLLMRequest) string {
	if req == nil {
		return ""
	}
	if req.PromptCacheKey != nil {
		if value := strings.TrimSpace(*req.PromptCacheKey); value != "" {
			return value
		}
	}
	metadata := decodeCodexClientMetadata(req.ClientMetadata)
	if value := metadataStringValue(metadata, codexMetadataWindowID); value != "" {
		if base, _, ok := strings.Cut(value, ":"); ok && strings.TrimSpace(base) != "" {
			return strings.TrimSpace(base)
		}
		return value
	}
	if turnMetadata := metadataStringValue(metadata, codexMetadataTurnMetadata); turnMetadata != "" {
		turn := decodeCodexTurnMetadata(turnMetadata)
		if value := metadataStringValue(turn, "prompt_cache_key"); value != "" {
			return value
		}
		if value := metadataStringValue(turn, "session_id"); value != "" {
			return value
		}
		if value := metadataStringValue(turn, "thread_id"); value != "" {
			return value
		}
	}
	return ""
}

func codexTurnMetadataFromRequest(req *transformerModel.InternalLLMRequest, sessionID string) string {
	metadata := decodeCodexClientMetadata(req.ClientMetadata)
	if value := metadataStringValue(metadata, codexMetadataTurnMetadata); value != "" {
		return ensureCodexTurnMetadata(value, sessionID)
	}
	return synthesizeCodexTurnMetadata(sessionID)
}

func ensureCodexClientMetadata(raw json.RawMessage, sessionID, installationID string) json.RawMessage {
	metadata := decodeCodexClientMetadata(raw)
	if metadata == nil {
		metadata = map[string]any{}
	}
	if metadataStringValue(metadata, codexMetadataInstallationID) == "" {
		metadata[codexMetadataInstallationID] = installationID
	}
	if metadataStringValue(metadata, codexMetadataWindowID) == "" {
		metadata[codexMetadataWindowID] = sessionID + ":0"
	}
	metadata[codexMetadataTurnMetadata] = ensureCodexTurnMetadata(metadataStringValue(metadata, codexMetadataTurnMetadata), sessionID)

	out, err := json.Marshal(metadata)
	if err != nil {
		return raw
	}
	return out
}

func ensureCodexTurnMetadata(raw string, sessionID string) string {
	if strings.TrimSpace(raw) == "" {
		return synthesizeCodexTurnMetadata(sessionID)
	}
	turn := decodeCodexTurnMetadata(raw)
	if turn == nil {
		return synthesizeCodexTurnMetadata(sessionID)
	}
	if metadataStringValue(turn, "session_id") == "" {
		turn["session_id"] = sessionID
	}
	if metadataStringValue(turn, "thread_id") == "" {
		turn["thread_id"] = sessionID
	}
	if metadataStringValue(turn, "thread_source") == "" {
		turn["thread_source"] = "user"
	}
	if metadataStringValue(turn, "turn_id") == "" {
		turn["turn_id"] = uuid.NewString()
	}
	if _, ok := turn["workspaces"]; !ok {
		turn["workspaces"] = map[string]any{}
	}
	if metadataStringValue(turn, "sandbox") == "" {
		turn["sandbox"] = "none"
	}
	if _, ok := turn["turn_started_at_unix_ms"]; !ok {
		turn["turn_started_at_unix_ms"] = time.Now().UnixMilli()
	}
	out, err := json.Marshal(turn)
	if err != nil {
		return raw
	}
	return string(out)
}

func synthesizeCodexTurnMetadata(sessionID string) string {
	turn := map[string]any{
		"session_id":              sessionID,
		"thread_id":               sessionID,
		"thread_source":           "user",
		"turn_id":                 uuid.NewString(),
		"workspaces":              map[string]any{},
		"sandbox":                 "none",
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	}
	out, _ := json.Marshal(turn)
	return string(out)
}

func decodeCodexClientMetadata(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil
	}
	return metadata
}

func decodeCodexTurnMetadata(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil
	}
	return metadata
}

func metadataStringValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}
