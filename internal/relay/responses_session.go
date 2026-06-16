package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const defaultResponsesSessionTTL = time.Hour
const responsesSessionPruneInterval = time.Minute

type responsesSessionEntry struct {
	channelID    int
	channelKeyID int
	expiresAt    time.Time
}

type responsesSessionTranscriptEntry struct {
	messages  []transformerModel.Message
	expiresAt time.Time
}

var responsesSessionStore = struct {
	sync.Mutex
	items       map[string]responsesSessionEntry
	lastPruneAt time.Time
}{
	items: make(map[string]responsesSessionEntry),
}

var responsesSessionTranscriptStore = struct {
	sync.Mutex
	items       map[string]responsesSessionTranscriptEntry
	lastPruneAt time.Time
}{
	items: make(map[string]responsesSessionTranscriptEntry),
}

func recordResponsesSession(responseID string, channelID, channelKeyID int) {
	recordResponsesSessionWithContext(context.Background(), responseID, channelID, channelKeyID)
}

func recordResponsesSessionWithContext(ctx context.Context, responseID string, channelID, channelKeyID int) {
	_ = ctx // owner persistence must survive terminal-client disconnects; use a short background context below.
	responseID = strings.TrimSpace(responseID)
	if responseID == "" || channelID == 0 || channelKeyID == 0 {
		return
	}
	ttl := currentResponsesSessionTTL()
	now := time.Now()
	responsesSessionStore.Lock()
	maybePruneResponsesSessionsLocked(now)
	responsesSessionStore.items[responseID] = responsesSessionEntry{
		channelID:    channelID,
		channelKeyID: channelKeyID,
		expiresAt:    now.Add(ttl),
	}
	responsesSessionStore.Unlock()

	persistCtx, cancel := metricsPersistContext()
	defer cancel()
	if err := op.ResponseSessionBind(persistCtx, responseID, channelID, channelKeyID, ttl); err != nil {
		log.Warnf("failed to persist responses session owner: %v", err)
	}
}

func responsesSessionOwner(responseID string) (responsesSessionEntry, bool) {
	return responsesSessionOwnerWithContext(context.Background(), responseID)
}

func responsesSessionOwnerWithContext(ctx context.Context, responseID string) (responsesSessionEntry, bool) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return responsesSessionEntry{}, false
	}
	responsesSessionStore.Lock()
	now := time.Now()
	maybePruneResponsesSessionsLocked(now)
	entry, ok := responsesSessionStore.items[responseID]
	if !ok || now.After(entry.expiresAt) {
		delete(responsesSessionStore.items, responseID)
	} else {
		responsesSessionStore.Unlock()
		return entry, true
	}
	responsesSessionStore.Unlock()

	channelID, channelKeyID, expiresAt, ok, err := op.ResponseSessionOwner(ctx, responseID)
	if err != nil {
		log.Warnf("failed to load responses session owner: %v", err)
		return responsesSessionEntry{}, false
	}
	if !ok {
		return responsesSessionEntry{}, false
	}
	if expiresAt.IsZero() || now.After(expiresAt) {
		return responsesSessionEntry{}, false
	}
	entry = responsesSessionEntry{
		channelID:    channelID,
		channelKeyID: channelKeyID,
		expiresAt:    expiresAt,
	}
	responsesSessionStore.Lock()
	responsesSessionStore.items[responseID] = entry
	responsesSessionStore.Unlock()
	return entry, true
}

func clearResponsesSessionCacheForTest() {
	responsesSessionStore.Lock()
	responsesSessionStore.items = make(map[string]responsesSessionEntry)
	responsesSessionStore.lastPruneAt = time.Time{}
	responsesSessionStore.Unlock()

	responsesSessionTranscriptStore.Lock()
	responsesSessionTranscriptStore.items = make(map[string]responsesSessionTranscriptEntry)
	responsesSessionTranscriptStore.lastPruneAt = time.Time{}
	responsesSessionTranscriptStore.Unlock()
}

func currentResponsesSessionTTL() time.Duration {
	value, err := op.SettingGetInt(dbmodel.SettingKeyResponsesSessionTTL)
	if err != nil {
		return defaultResponsesSessionTTL
	}
	if value <= 0 {
		return defaultResponsesSessionTTL
	}
	return time.Duration(value) * time.Second
}

func pruneResponsesSessionsLocked(now time.Time) {
	for responseID, entry := range responsesSessionStore.items {
		if now.After(entry.expiresAt) {
			delete(responsesSessionStore.items, responseID)
		}
	}
	responsesSessionStore.lastPruneAt = now
}

func maybePruneResponsesSessionsLocked(now time.Time) {
	if !responsesSessionStore.lastPruneAt.IsZero() && now.Sub(responsesSessionStore.lastPruneAt) < responsesSessionPruneInterval {
		return
	}
	pruneResponsesSessionsLocked(now)
}

func recordResponsesSessionTranscript(responseID string, messages []transformerModel.Message) {
	responseID = strings.TrimSpace(responseID)
	messages = trimResponsesSessionTranscript(messages)
	if responseID == "" || len(messages) == 0 {
		return
	}
	now := time.Now()
	responsesSessionTranscriptStore.Lock()
	maybePruneResponsesSessionTranscriptsLocked(now)
	responsesSessionTranscriptStore.items[responseID] = responsesSessionTranscriptEntry{
		messages:  cloneResponsesSessionMessages(messages),
		expiresAt: now.Add(currentResponsesSessionTTL()),
	}
	responsesSessionTranscriptStore.Unlock()
}

func responsesSessionTranscript(responseID string) ([]transformerModel.Message, bool) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil, false
	}
	now := time.Now()
	responsesSessionTranscriptStore.Lock()
	maybePruneResponsesSessionTranscriptsLocked(now)
	entry, ok := responsesSessionTranscriptStore.items[responseID]
	if !ok || now.After(entry.expiresAt) {
		delete(responsesSessionTranscriptStore.items, responseID)
		responsesSessionTranscriptStore.Unlock()
		return nil, false
	}
	out := cloneResponsesSessionMessages(entry.messages)
	responsesSessionTranscriptStore.Unlock()
	return out, len(out) > 0
}

func pruneResponsesSessionTranscriptsLocked(now time.Time) {
	for responseID, entry := range responsesSessionTranscriptStore.items {
		if now.After(entry.expiresAt) {
			delete(responsesSessionTranscriptStore.items, responseID)
		}
	}
	responsesSessionTranscriptStore.lastPruneAt = now
}

func maybePruneResponsesSessionTranscriptsLocked(now time.Time) {
	if !responsesSessionTranscriptStore.lastPruneAt.IsZero() && now.Sub(responsesSessionTranscriptStore.lastPruneAt) < responsesSessionPruneInterval {
		return
	}
	pruneResponsesSessionTranscriptsLocked(now)
}

const (
	maxResponsesSessionTranscriptMessages = 80
	maxResponsesSessionTranscriptChars    = 120000
)

func trimResponsesSessionTranscript(messages []transformerModel.Message) []transformerModel.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]transformerModel.Message, 0, len(messages))
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" || strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
			continue
		}
		if messageHasNoReplayableContent(msg) {
			continue
		}
		out = append(out, msg)
	}
	for len(out) > maxResponsesSessionTranscriptMessages {
		out = out[1:]
	}
	for transcriptCharLen(out) > maxResponsesSessionTranscriptChars && len(out) > 1 {
		out = out[1:]
	}
	return out
}

func messageHasNoReplayableContent(msg transformerModel.Message) bool {
	if strings.TrimSpace(messageTextContent(msg.Content)) != "" {
		return false
	}
	return len(msg.ToolCalls) == 0 && msg.ToolCallID == nil
}

func transcriptCharLen(messages []transformerModel.Message) int {
	total := 0
	for _, msg := range messages {
		total += len(messageTextContent(msg.Content))
		for _, tc := range msg.ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		if msg.ToolCallID != nil {
			total += len(*msg.ToolCallID)
		}
	}
	return total
}

func cloneResponsesSessionMessages(messages []transformerModel.Message) []transformerModel.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]transformerModel.Message, len(messages))
	for i := range messages {
		out[i] = cloneResponseSessionMessage(messages[i])
	}
	return out
}

func cloneResponseSessionMessage(msg transformerModel.Message) transformerModel.Message {
	clone := msg
	clone.Content = cloneResponseSessionMessageContent(msg.Content)
	clone.ToolCalls = append([]transformerModel.ToolCall(nil), msg.ToolCalls...)
	if msg.ToolCallID != nil {
		value := *msg.ToolCallID
		clone.ToolCallID = &value
	}
	if msg.ReasoningSignature != nil {
		value := *msg.ReasoningSignature
		clone.ReasoningSignature = &value
	}
	if msg.ReasoningContent != nil {
		value := *msg.ReasoningContent
		clone.ReasoningContent = &value
	}
	return clone
}

func cloneResponseSessionMessageContent(content transformerModel.MessageContent) transformerModel.MessageContent {
	clone := content
	if content.Content != nil {
		value := *content.Content
		clone.Content = &value
	}
	clone.MultipleContent = append([]transformerModel.MessageContentPart(nil), content.MultipleContent...)
	for i := range clone.MultipleContent {
		if content.MultipleContent[i].Text != nil {
			value := *content.MultipleContent[i].Text
			clone.MultipleContent[i].Text = &value
		}
		if content.MultipleContent[i].ImageURL != nil {
			image := *content.MultipleContent[i].ImageURL
			if content.MultipleContent[i].ImageURL.Detail != nil {
				value := *content.MultipleContent[i].ImageURL.Detail
				image.Detail = &value
			}
			clone.MultipleContent[i].ImageURL = &image
		}
	}
	return clone
}

func prioritizeResponsesSessionOwner(ctx context.Context, iter *balancer.Iterator, req *transformerModel.InternalLLMRequest) {
	if iter == nil || req == nil || req.PreviousResponseID == nil {
		return
	}
	owner, ok := responsesSessionOwnerWithContext(ctx, *req.PreviousResponseID)
	if !ok {
		return
	}
	iter.PrioritizeChannels(map[int]bool{owner.channelID: true})
}

func responsesOwnerKeyForChannel(ctx context.Context, previousResponseID string, channelID int) int {
	if strings.TrimSpace(previousResponseID) == "" || channelID == 0 {
		return 0
	}
	owner, ok := responsesSessionOwnerWithContext(ctx, previousResponseID)
	if !ok || owner.channelID != channelID {
		return 0
	}
	return owner.channelKeyID
}

func (ra *relayAttempt) prepareResponsesSessionCursor(outAdapter transformerModel.Outbound) {
	if ra == nil || ra.internalRequest == nil || ra.internalRequest.PreviousResponseID == nil {
		return
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse {
		ra.internalRequest.PreviousResponseID = nil
		return
	}
	if !outboundSupportsResponsesSessionCursor(outAdapter) {
		ra.internalRequest.PreviousResponseID = nil
		return
	}
	owner, ok := responsesSessionOwnerWithContext(ra.context(), *ra.internalRequest.PreviousResponseID)
	if !ok {
		// An unresolvable cursor under store=false (codex shape) can only yield an
		// empty turn: the upstream never persisted it, so octopus bridges the prior
		// history into messages instead and drops the cursor. A cursor that DOES
		// resolve to this channel+key is still forwarded below so the existing
		// retry-without-cursor-on-400 path keeps working. Otherwise fall back to the
		// sticky-routing heuristic for unknown cursors.
		storeDisabled := ra.internalRequest.Store != nil && !*ra.internalRequest.Store
		if storeDisabled || !ra.canTrustStickyForUnknownResponsesCursor() {
			ra.internalRequest.PreviousResponseID = nil
		}
		return
	}
	if ra.channel == nil || ra.usedKey.ID != owner.channelKeyID || ra.channel.ID != owner.channelID {
		ra.internalRequest.PreviousResponseID = nil
	}
}

func (ra *relayAttempt) prepareResponsesEncryptedContent(outAdapter transformerModel.Outbound) {
	if ra == nil || ra.internalRequest == nil || !requestHasResponsesEncryptedContent(ra.internalRequest) {
		return
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse || !outboundSupportsResponsesSessionCursor(outAdapter) {
		stripResponsesEncryptedContent(ra.internalRequest)
		return
	}
	if ra.responsesEncryptedContentOwnerMatchesCurrentAttempt() {
		return
	}
	stripResponsesEncryptedContent(ra.internalRequest)
}

func (ra *relayAttempt) responsesEncryptedContentOwnerMatchesCurrentAttempt() bool {
	if ra == nil || ra.internalRequest == nil || ra.internalRequest.PreviousResponseID == nil {
		return false
	}
	owner, ok := responsesSessionOwnerWithContext(ra.context(), *ra.internalRequest.PreviousResponseID)
	if !ok {
		return false
	}
	return ra.channel != nil && ra.channel.ID == owner.channelID && ra.usedKey.ID == owner.channelKeyID
}

func (ra *relayAttempt) canTrustStickyForUnknownResponsesCursor() bool {
	if ra == nil || ra.iter == nil || ra.channel == nil || ra.usedKey.ID == 0 {
		return false
	}
	if !canTrustSessionSourceForUnknownResponsesCursor(ra.clientSessionSource) {
		return false
	}
	return ra.iter.IsStickyChannelKey(ra.channel.ID, ra.usedKey.ID)
}

func canTrustSessionSourceForUnknownResponsesCursor(source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return false
	}
	if strings.HasPrefix(source, "octopus:") {
		return false
	}
	return source != "body:previous_response_id"
}

func requestHasResponsesEncryptedContent(req *transformerModel.InternalLLMRequest) bool {
	if req == nil {
		return false
	}
	for _, message := range req.Messages {
		if message.ReasoningSignature != nil && strings.TrimSpace(*message.ReasoningSignature) != "" {
			return true
		}
	}
	if bytes.Contains(req.ResponsesInputRaw, []byte(`"encrypted_content"`)) {
		return true
	}
	return false
}

func stripResponsesEncryptedContent(req *transformerModel.InternalLLMRequest) {
	if req == nil {
		return
	}
	for idx := range req.Messages {
		req.Messages[idx].ReasoningSignature = nil
	}
	if len(req.ResponsesInputRaw) > 0 {
		if sanitized, ok := stripResponsesEncryptedContentRaw(req.ResponsesInputRaw); ok {
			req.ResponsesInputRaw = sanitized
		} else {
			req.ResponsesInputRaw = nil
		}
	}
}

func stripResponsesEncryptedContentRaw(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, true
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	removeEncryptedContentFields(value)
	sanitized, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return sanitized, true
}

func removeEncryptedContentFields(value any) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			removeEncryptedContentFields(item)
		}
	case map[string]any:
		if strings.TrimSpace(stringMapValue(typed, "type")) == "reasoning" {
			delete(typed, "encrypted_content")
		}
		for _, item := range typed {
			removeEncryptedContentFields(item)
		}
	}
}

func stringMapValue(value map[string]any, key string) string {
	raw, ok := value[key]
	if !ok {
		return ""
	}
	text, ok := raw.(string)
	if !ok {
		return ""
	}
	return text
}

func outboundSupportsResponsesSessionCursor(outAdapter transformerModel.Outbound) bool {
	switch outAdapter.(type) {
	case *openaiOutbound.ResponseOutbound:
		return true
	default:
		return false
	}
}

func (ra *relayAttempt) recordResponsesSessionFromInbound(resp *transformerModel.InternalLLMResponse) {
	if ra == nil || resp == nil || ra.inboundType != inbound.InboundTypeOpenAIResponse {
		return
	}
	if ra.channel == nil || ra.usedKey.ID == 0 {
		return
	}
	recordResponsesSessionWithContext(ra.context(), resp.ID, ra.channel.ID, ra.usedKey.ID)
	if ra.shouldBridgeResponsesHistory() {
		recordResponsesSessionTranscript(resp.ID, ra.responsesSessionTranscriptFromResponse(resp))
	}
}

// shouldBridgeResponsesHistory reports whether octopus itself must persist and
// replay conversation history because the chosen upstream cannot continue a
// responses session through previous_response_id: Anthropic channels have no
// responses cursor at all, and codex-shaped responses channels are forced to
// store=false. Applies only to plain responses clients (e.g. Cursor), not the
// codex CLI which manages its own history.
func (ra *relayAttempt) shouldBridgeResponsesHistory() bool {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return false
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse {
		return false
	}
	switch ra.channel.Type {
	case outbound.OutboundTypeAnthropic:
		return true
	case outbound.OutboundTypeOpenAIResponse:
		return ra.shouldBridgePlainResponsesCodexHistory()
	default:
		return false
	}
}

// bridgeResponsesHistoryForAnthropic replays a prior responses turn's history
// into the outgoing messages when a plain responses client (e.g. Cursor) targets
// an Anthropic channel. Anthropic has no previous_response_id, so without this
// the next turn would lose all prior context.
func (ra *relayAttempt) bridgeResponsesHistoryForAnthropic() {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse || ra.channel.Type != outbound.OutboundTypeAnthropic {
		return
	}
	req := ra.internalRequest
	if req.PreviousResponseID == nil || strings.TrimSpace(*req.PreviousResponseID) == "" {
		return
	}
	if responsesMessagesContainToolOutput(req.Messages) {
		return
	}
	ra.applyPlainResponsesCodexHistoryForPreviousResponseID(*req.PreviousResponseID)
}

func (ra *relayAttempt) shouldBridgePlainResponsesCodexHistory() bool {
	return ra != nil &&
		ra.internalRequest != nil &&
		ra.channel != nil &&
		ra.inboundType == inbound.InboundTypeOpenAIResponse &&
		ra.channel.Type == outbound.OutboundTypeOpenAIResponse &&
		ra.shouldUseCodexFingerprint() &&
		!ra.inboundLooksLikeCodexClient()
}

func (ra *relayAttempt) responsesSessionTranscriptFromResponse(resp *transformerModel.InternalLLMResponse) []transformerModel.Message {
	if ra == nil || ra.internalRequest == nil || resp == nil {
		return nil
	}
	messages := cloneResponsesSessionMessages(ra.internalRequest.Messages)
	if assistant := assistantMessageFromResponsesSession(resp); assistant != nil {
		messages = append(messages, *assistant)
	}
	return trimResponsesSessionTranscript(messages)
}

func assistantMessageFromResponsesSession(resp *transformerModel.InternalLLMResponse) *transformerModel.Message {
	if resp == nil {
		return nil
	}
	for _, choice := range resp.Choices {
		if choice.Message != nil {
			msg := cloneResponseSessionMessage(*choice.Message)
			if strings.TrimSpace(msg.Role) == "" {
				msg.Role = "assistant"
			}
			if messageHasNoReplayableContent(msg) {
				return nil
			}
			return &msg
		}
	}
	return nil
}

func (ra *relayAttempt) context() context.Context {
	if ra != nil && ra.c != nil && ra.c.Request != nil {
		return ra.c.Request.Context()
	}
	return context.Background()
}
