package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

type routeSessionCandidate struct {
	scope string
	keys  []string
}

type clientSessionInfo struct {
	Key    string
	Source string
}

var routeHeaderSessionCandidates = []routeSessionCandidate{
	{scope: "thread", keys: []string{"AH-Thread-Id", "X-Amp-Thread-Id"}},
	{scope: "codex-session", keys: []string{"Session_id", "Session-Id", "X-Session-Id"}},
	{scope: "conversation", keys: []string{"Conversation_id", "Conversation-Id", "X-Conversation-Id"}},
	{scope: "trace", keys: []string{"AH-Trace-Id", "Trace-Id", "X-Trace-Id", "X-Client-Request-Id"}},
}

var routeHeaderThreadCandidate = routeSessionCandidate{scope: "thread", keys: []string{"AH-Thread-Id", "X-Amp-Thread-Id"}}
var routeHeaderTraceCandidate = routeSessionCandidate{scope: "trace", keys: []string{"AH-Trace-Id", "Trace-Id", "X-Trace-Id", "X-Client-Request-Id"}}

var routeMetadataSessionCandidates = []routeSessionCandidate{
	{scope: "thread", keys: []string{"thread_id", "threadId", "ah_thread_id", "amp_thread_id"}},
	{scope: "codex-session", keys: []string{"session_id", "sessionId"}},
	{scope: "conversation", keys: []string{"conversation_id", "conversationId"}},
	{scope: "trace", keys: []string{"trace_id", "traceId", "ah_trace_id", "client_request_id"}},
	{scope: "user", keys: []string{"user_id", "userId"}},
}

var routeMetadataThreadCandidate = routeSessionCandidate{scope: "thread", keys: []string{"thread_id", "threadId", "ah_thread_id", "amp_thread_id"}}
var routeMetadataTraceCandidate = routeSessionCandidate{scope: "trace", keys: []string{"trace_id", "traceId", "ah_trace_id", "client_request_id"}}

func deriveClientSessionKey(headers http.Header, req *model.InternalLLMRequest) string {
	return deriveClientSessionInfo(headers, req).Key
}

func deriveClientSessionInfo(headers http.Header, req *model.InternalLLMRequest) clientSessionInfo {
	if key := deriveClientSessionKeyFromHeaders(headers); key != "" {
		return clientSessionInfo{Key: key, Source: deriveClientSessionSourceFromHeaders(headers)}
	}
	if req == nil {
		return clientSessionInfo{}
	}
	if key := deriveClientSessionKeyFromMetadata(req.Metadata); key != "" {
		return clientSessionInfo{Key: key, Source: deriveClientSessionSourceFromMetadata(req.Metadata)}
	}
	if info := deriveClientSessionInfoFromCodexClientMetadata(req.ClientMetadata); info.Key != "" {
		return info
	}
	return clientSessionInfo{}
}

func deriveManagedClientSessionInfo(headers http.Header, req *model.InternalLLMRequest) clientSessionInfo {
	if info := deriveClientSessionInfo(headers, req); info.Key != "" {
		return info
	}
	return deriveOctopusManagedClientSessionInfo(req)
}

func deriveOctopusManagedClientSessionInfo(req *model.InternalLLMRequest) clientSessionInfo {
	if req == nil || (!req.IsChatRequest() && !req.IsImageGenerationRequest()) {
		return clientSessionInfo{}
	}
	if req.PromptCacheKey != nil {
		if value := normalizeRouteSessionValue(*req.PromptCacheKey); value != "" {
			return clientSessionInfo{
				Key:    hashRouteSessionKey("prompt-cache", value),
				Source: "body:prompt_cache_key",
			}
		}
	}
	if req.PreviousResponseID != nil {
		if value := normalizeRouteSessionValue(*req.PreviousResponseID); value != "" {
			return clientSessionInfo{
				Key:    hashRouteSessionKey("previous-response", value),
				Source: "body:previous_response_id",
			}
		}
	}
	if req.User != nil {
		if value := normalizeRouteSessionValue(*req.User); value != "" {
			return clientSessionInfo{
				Key:    hashRouteSessionKey("user", value),
				Source: "body:user",
			}
		}
	}
	if req.SafetyIdentifier != nil {
		if value := normalizeRouteSessionValue(*req.SafetyIdentifier); value != "" {
			return clientSessionInfo{
				Key:    hashRouteSessionKey("safety-identifier", value),
				Source: "body:safety_identifier",
			}
		}
	}
	if len(req.RawRequest) > 0 {
		sum := sha256.Sum256(bytes.TrimSpace(req.RawRequest))
		return clientSessionInfo{
			Key:    hashRouteSessionKey("octopus-request", hex.EncodeToString(sum[:16])),
			Source: "octopus:request_fingerprint",
		}
	}
	return clientSessionInfo{}
}

func deriveClientSessionKeyFromHeaders(headers http.Header) string {
	return deriveClientSessionInfoFromHeaders(headers).Key
}

func deriveClientSessionSourceFromHeaders(headers http.Header) string {
	return deriveClientSessionInfoFromHeaders(headers).Source
}

func deriveClientSessionInfoFromHeaders(headers http.Header) clientSessionInfo {
	if len(headers) == 0 {
		return clientSessionInfo{}
	}
	if hasRouteSessionHeaderCandidate(headers, routeHeaderThreadCandidate) && hasRouteSessionHeaderCandidate(headers, routeHeaderTraceCandidate) {
		if info := deriveClientSessionInfoFromHeaderCandidate(headers, routeHeaderTraceCandidate); info.Key != "" {
			return info
		}
	}
	for _, candidate := range routeHeaderSessionCandidates {
		if info := deriveClientSessionInfoFromHeaderCandidate(headers, candidate); info.Key != "" {
			return info
		}
	}
	return clientSessionInfo{}
}

func hasRouteSessionHeader(headers http.Header, keys ...string) bool {
	for _, key := range keys {
		if normalizeRouteSessionValue(headers.Get(key)) != "" {
			return true
		}
	}
	return false
}

func hasRouteSessionHeaderCandidate(headers http.Header, candidate routeSessionCandidate) bool {
	return hasRouteSessionHeader(headers, candidate.keys...)
}

func deriveClientSessionInfoFromHeaderCandidate(headers http.Header, candidate routeSessionCandidate) clientSessionInfo {
	for _, key := range candidate.keys {
		if value := normalizeRouteSessionValue(headers.Get(key)); value != "" {
			return clientSessionInfo{
				Key:    hashRouteSessionKey(candidate.scope, value),
				Source: "header:" + key,
			}
		}
	}
	return clientSessionInfo{}
}

func deriveClientSessionKeyFromMetadata(metadata map[string]string) string {
	return deriveClientSessionInfoFromMetadata(metadata).Key
}

func deriveClientSessionSourceFromMetadata(metadata map[string]string) string {
	return deriveClientSessionInfoFromMetadata(metadata).Source
}

func deriveClientSessionInfoFromMetadata(metadata map[string]string) clientSessionInfo {
	if len(metadata) == 0 {
		return clientSessionInfo{}
	}
	if hasRouteSessionMetadataCandidate(metadata, routeMetadataThreadCandidate) && hasRouteSessionMetadataCandidate(metadata, routeMetadataTraceCandidate) {
		if info := deriveClientSessionInfoFromMetadataCandidate(metadata, routeMetadataTraceCandidate); info.Key != "" {
			return info
		}
	}
	for _, candidate := range routeMetadataSessionCandidates {
		if info := deriveClientSessionInfoFromMetadataCandidate(metadata, candidate); info.Key != "" {
			return info
		}
	}
	return clientSessionInfo{}
}

func deriveClientSessionInfoFromCodexClientMetadata(raw json.RawMessage) clientSessionInfo {
	metadata := decodeCodexClientMetadata(raw)
	if len(metadata) == 0 {
		return clientSessionInfo{}
	}
	if value := normalizeCodexWindowSessionValue(metadataStringValue(metadata, codexMetadataWindowID)); value != "" {
		return clientSessionInfo{
			Key:    hashRouteSessionKey("codex-session", value),
			Source: "client_metadata:" + codexMetadataWindowID,
		}
	}
	if turnMetadata := metadataStringValue(metadata, codexMetadataTurnMetadata); turnMetadata != "" {
		turn := decodeCodexTurnMetadata(turnMetadata)
		for _, key := range []string{"session_id", "thread_id", "prompt_cache_key"} {
			if value := normalizeRouteSessionValue(metadataStringValue(turn, key)); value != "" {
				return clientSessionInfo{
					Key:    hashRouteSessionKey("codex-session", value),
					Source: "client_metadata:" + codexMetadataTurnMetadata + ":" + key,
				}
			}
		}
	}
	if value := normalizeRouteSessionValue(metadataStringValue(metadata, codexMetadataInstallationID)); value != "" {
		return clientSessionInfo{
			Key:    hashRouteSessionKey("codex-installation", value),
			Source: "client_metadata:" + codexMetadataInstallationID,
		}
	}
	return clientSessionInfo{}
}

func normalizeCodexWindowSessionValue(value string) string {
	value = normalizeRouteSessionValue(value)
	if value == "" {
		return ""
	}
	if base, _, ok := strings.Cut(value, ":"); ok && normalizeRouteSessionValue(base) != "" {
		return normalizeRouteSessionValue(base)
	}
	return value
}

func hasRouteSessionMetadataCandidate(metadata map[string]string, candidate routeSessionCandidate) bool {
	for _, key := range candidate.keys {
		if normalizeRouteSessionValue(metadata[key]) != "" {
			return true
		}
	}
	return false
}

func deriveClientSessionInfoFromMetadataCandidate(metadata map[string]string, candidate routeSessionCandidate) clientSessionInfo {
	for _, key := range candidate.keys {
		if value := normalizeRouteSessionValue(metadata[key]); value != "" {
			if candidate.scope == "user" {
				if sessionID := parseClaudeCodeMetadataSessionID(value); sessionID != "" {
					return clientSessionInfo{
						Key:    hashRouteSessionKey("anthropic-session", sessionID),
						Source: "metadata:" + key + ":claude-session",
					}
				}
			}
			return clientSessionInfo{
				Key:    hashRouteSessionKey(candidate.scope, value),
				Source: "metadata:" + key,
			}
		}
	}
	return clientSessionInfo{}
}

var claudeLegacySessionPattern = regexp.MustCompile(`(?i)(?:^|_)session_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:$|_)`)

func parseClaudeCodeMetadataSessionID(value string) string {
	value = normalizeRouteSessionValue(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "{") {
		var payload struct {
			SessionID string `json:"session_id"`
			SessionId string `json:"sessionId"`
		}
		if err := json.Unmarshal([]byte(value), &payload); err == nil {
			if sessionID := normalizeRouteSessionValue(payload.SessionID); sessionID != "" {
				return sessionID
			}
			if sessionID := normalizeRouteSessionValue(payload.SessionId); sessionID != "" {
				return sessionID
			}
		}
	}
	if match := claudeLegacySessionPattern.FindStringSubmatch(value); len(match) == 2 {
		return match[1]
	}
	return ""
}

func normalizeRouteSessionValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

func hashRouteSessionKey(scope, value string) string {
	sum := sha256.Sum256([]byte(scope + ":" + value))
	return scope + ":" + hex.EncodeToString(sum[:16])
}
