package relay

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	llmmodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

const autoPromptCacheKeyPrefix = "octo_pc_"

func openAIPromptCacheKeyChannel(channelType outbound.OutboundType) bool {
	return channelType == outbound.OutboundTypeOpenAIChat ||
		channelType == outbound.OutboundTypeOpenAIResponse ||
		channelType == outbound.OutboundTypeCustomOpenAIChat
}

func applyOpenAIAutoPromptCacheKey(req *llmmodel.InternalLLMRequest, channelType outbound.OutboundType, userID, apiKeyID, profileID int, requestModel string, enabled bool) {
	applyOpenAIAutoPromptCacheKeyWithSession(req, channelType, userID, apiKeyID, profileID, requestModel, "", enabled)
}

// applyOpenAIAutoPromptCacheKeyWithSession additionally anchors the derived key to
// a stable conversation-root hash. When convRootKey is non-empty (a responses turn
// that continues a prior, owned conversation) the key stays byte-for-byte stable
// across every turn of that conversation — so the upstream prompt cache is reused
// instead of missing on each turn's changing first-user message — while remaining
// isolated per tenant via the api-key/user namespace.
func applyOpenAIAutoPromptCacheKeyWithSession(req *llmmodel.InternalLLMRequest, channelType outbound.OutboundType, userID, apiKeyID, profileID int, requestModel, convRootKey string, enabled bool) {
	if !enabled || req == nil || !openAIPromptCacheKeyChannel(channelType) || !req.IsChatRequest() {
		return
	}
	if req.PromptCacheKey != nil && strings.TrimSpace(*req.PromptCacheKey) != "" {
		return
	}
	key := deriveOpenAIAutoPromptCacheKey(req, userID, apiKeyID, profileID, requestModel, convRootKey)
	if key == "" {
		return
	}
	req.PromptCacheKey = &key
}

func deriveOpenAIAutoPromptCacheKey(req *llmmodel.InternalLLMRequest, userID, apiKeyID, profileID int, requestModel, convRootKey string) string {
	if req == nil {
		return ""
	}

	modelName := strings.TrimSpace(requestModel)
	if modelName == "" {
		modelName = strings.TrimSpace(req.Model)
	}
	if modelName == "" {
		return ""
	}

	// A resolved conversation root is a fully stable per-conversation anchor: use it
	// alone (plus namespace + model) so every turn of the same conversation hashes to
	// the same key. Different conversations/tenants get different roots/namespaces and
	// therefore different keys.
	if strings.TrimSpace(convRootKey) != "" {
		parts := []string{
			"ns=" + cacheHintNamespace(userID, apiKeyID, profileID),
			"model=" + strings.ToLower(modelName),
			"conv_root=" + strings.TrimSpace(convRootKey),
		}
		sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
		return fmt.Sprintf("%s%x", autoPromptCacheKeyPrefix, sum[:16])
	}

	parts := []string{
		"ns=" + cacheHintNamespace(userID, apiKeyID, profileID),
		"model=" + strings.ToLower(modelName),
	}
	anchors := 0

	if value := strings.TrimSpace(req.ReasoningEffort); value != "" {
		parts = append(parts, "reasoning_effort="+value)
		anchors++
	}
	if appendCacheHintJSONSeed(&parts, "tool_choice", req.ToolChoice) {
		anchors++
	}
	if len(req.Tools) > 0 && appendCacheHintJSONSeed(&parts, "tools", req.Tools) {
		anchors++
	}
	if req.ResponseFormat != nil && appendCacheHintJSONSeed(&parts, "response_format", req.ResponseFormat) {
		anchors++
	}

	firstUserCaptured := false
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			if seed := messageContentCacheSeed(msg.Content); seed != "" {
				parts = append(parts, role+"="+seed)
				anchors++
			}
		case "user":
			if firstUserCaptured {
				continue
			}
			if seed := messageContentCacheSeed(msg.Content); seed != "" {
				parts = append(parts, "first_user="+seed)
				anchors++
				firstUserCaptured = true
			}
		}
	}

	if anchors == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return fmt.Sprintf("%s%x", autoPromptCacheKeyPrefix, sum[:16])
}

func cacheHintNamespace(userID, apiKeyID, profileID int) string {
	base := "anonymous"
	if userID > 0 {
		base = fmt.Sprintf("user:%d", userID)
	} else if apiKeyID > 0 {
		base = fmt.Sprintf("api_key:%d", apiKeyID)
	}
	// Fold the channel's fingerprint profile into the namespace so two channels that
	// present DIFFERENT device identities (different ProfileID) never share an auto
	// prompt_cache_key. Otherwise a strict upstream seeing one cache key used by two
	// "devices" could correlate them as the same real client — a fingerprint tell.
	// ProfileID 0 (the global-default device) leaves the namespace byte-for-byte as
	// before, so existing keys/behaviour are unchanged.
	if profileID > 0 {
		base += fmt.Sprintf("|fp:%d", profileID)
	}
	return base
}

func appendCacheHintJSONSeed(parts *[]string, label string, value any) bool {
	if value == nil {
		return false
	}
	seed := normalizeCacheHintJSON(value)
	if strings.TrimSpace(seed) == "" || seed == "null" {
		return false
	}
	*parts = append(*parts, label+"="+seed)
	return true
}

func messageContentCacheSeed(content llmmodel.MessageContent) string {
	if content.Content != nil {
		return strings.TrimSpace(*content.Content)
	}
	if len(content.MultipleContent) == 0 {
		return ""
	}
	return normalizeCacheHintJSON(content.MultipleContent)
}

func normalizeCacheHintJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return strings.TrimSpace(string(raw))
	}
	out, err := json.Marshal(normalized)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return strings.TrimSpace(string(out))
}
