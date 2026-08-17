package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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

// Session source tags: a chat-minted id is never a real OpenAI Responses store
// id and must not be forwarded as previous_response_id to a stateful upstream.
// Cursor (and similar clients) can still reuse that id across /v1/chat and
// /v1/responses; octopus rebuilds history from the stored transcript instead.
const (
	responseSessionSourceResponses = "responses"
	responseSessionSourceChat      = "chat"
)

type responsesSessionEntry struct {
	channelID    int
	channelKeyID int
	ownerTokenID int
	ownerUserID  int
	rootHash     string
	source       string
	expiresAt    time.Time
}

type responsesSessionTranscriptEntry struct {
	messages     []transformerModel.Message
	tools        []transformerModel.Tool
	ownerTokenID int
	ownerUserID  int
	expiresAt    time.Time
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

// recordResponsesSessionWithContext keeps the pre-isolation signature (no owner
// identity / conversation root) so existing callers and tests are unaffected;
// records written this way carry owner 0/0 and are treated as unrestricted.
func recordResponsesSessionWithContext(ctx context.Context, responseID string, channelID, channelKeyID int) {
	recordResponsesSessionOwned(ctx, responseID, channelID, channelKeyID, 0, 0, "", responseSessionSourceResponses)
}

func recordResponsesSessionOwned(ctx context.Context, responseID string, channelID, channelKeyID, ownerTokenID, ownerUserID int, rootHash, source string) {
	_ = ctx // owner persistence must survive terminal-client disconnects; use a short background context below.
	responseID = strings.TrimSpace(responseID)
	if responseID == "" || channelID == 0 || channelKeyID == 0 {
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = responseSessionSourceResponses
	}
	ttl := currentResponsesSessionTTL()
	now := time.Now()
	responsesSessionStore.Lock()
	maybePruneResponsesSessionsLocked(now)
	responsesSessionStore.items[responseID] = responsesSessionEntry{
		channelID:    channelID,
		channelKeyID: channelKeyID,
		ownerTokenID: ownerTokenID,
		ownerUserID:  ownerUserID,
		rootHash:     rootHash,
		source:       source,
		expiresAt:    now.Add(ttl),
	}
	responsesSessionStore.Unlock()

	persistCtx, cancel := metricsPersistContext()
	defer cancel()
	if err := op.ResponseSessionBindOwned(persistCtx, responseID, channelID, channelKeyID, ownerTokenID, ownerUserID, rootHash, source, ttl); err != nil {
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

	row, ok, err := op.ResponseSessionOwner(ctx, responseID)
	if err != nil {
		log.Warnf("failed to load responses session owner: %v", err)
		return responsesSessionEntry{}, false
	}
	if !ok {
		return responsesSessionEntry{}, false
	}
	if row.ExpiresAt.IsZero() || now.After(row.ExpiresAt) {
		return responsesSessionEntry{}, false
	}
	entry = responsesSessionEntry{
		channelID:    row.ChannelID,
		channelKeyID: row.ChannelKeyID,
		ownerTokenID: row.OwnerTokenID,
		ownerUserID:  row.OwnerUserID,
		rootHash:     row.RootHash,
		source:       row.Source,
		expiresAt:    row.ExpiresAt,
	}
	responsesSessionStore.Lock()
	responsesSessionStore.items[responseID] = entry
	responsesSessionStore.Unlock()
	return entry, true
}

// responsesSessionOwnerMatches reports whether reqTokenID/reqUserID may use a
// response id owned by entry. Fail-closed: an owner with a recorded identity is
// only matched by the same identity (token first, else user). An owner with no
// recorded identity (0/0 — legacy rows written before isolation, or genuinely
// tokenless traffic) is treated as unrestricted for backward compatibility;
// such rows expire within the session TTL, after which every live row is bound.
func responsesSessionOwnerMatches(entry responsesSessionEntry, reqTokenID, reqUserID int) bool {
	if entry.ownerTokenID == 0 && entry.ownerUserID == 0 {
		return true
	}
	if entry.ownerTokenID > 0 {
		return entry.ownerTokenID == reqTokenID
	}
	return entry.ownerUserID == reqUserID
}

// responsesConversationRootForRequest returns the stable conversation-root hash
// for a request that continues a prior response id, or "" when there is no
// resolvable/owned prior turn. The root is identical for every turn of the same
// conversation, giving a stable upstream prompt-cache anchor that is still
// isolated per tenant (the cache key also folds in the api-key/user namespace).
func responsesConversationRootForRequest(ctx context.Context, previousResponseID string, reqTokenID, reqUserID int) string {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if previousResponseID == "" {
		return ""
	}
	entry, ok := responsesSessionOwnerWithContext(ctx, previousResponseID)
	if !ok || !responsesSessionOwnerMatches(entry, reqTokenID, reqUserID) {
		return ""
	}
	if entry.rootHash != "" {
		return entry.rootHash
	}
	return op.ResponseSessionIDHash(previousResponseID)
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

// PruneResponsesSessionsExpired drops expired session + transcript entries. Exposed for a
// periodic task so entries are reclaimed during quiet periods too: the in-line maybePrune*
// only fires on a record call, so a burst of codex traffic followed by silence would keep
// expired entries (each holding up to ~120KB of transcript) resident until the next request.
// Only past-TTL entries are removed — live transcripts a continuation still needs are kept —
// so this is memory hygiene with no behaviour change.
func PruneResponsesSessionsExpired() {
	now := time.Now()
	responsesSessionStore.Lock()
	pruneResponsesSessionsLocked(now)
	responsesSessionStore.Unlock()

	responsesSessionTranscriptStore.Lock()
	for responseID, entry := range responsesSessionTranscriptStore.items {
		if now.After(entry.expiresAt) {
			delete(responsesSessionTranscriptStore.items, responseID)
		}
	}
	responsesSessionTranscriptStore.lastPruneAt = now
	responsesSessionTranscriptStore.Unlock()
}

// recordResponsesSessionTranscript keeps the pre-isolation signature (no owner);
// transcripts written this way carry owner 0/0 and are readable by any requester
// for backward compatibility.
func recordResponsesSessionTranscript(responseID string, messages []transformerModel.Message) {
	recordResponsesSessionTranscriptOwned(responseID, messages, nil, 0, 0)
}

func recordResponsesSessionTranscriptOwned(responseID string, messages []transformerModel.Message, tools []transformerModel.Tool, ownerTokenID, ownerUserID int) {
	responseID = strings.TrimSpace(responseID)
	messages = trimResponsesSessionTranscript(messages)
	if responseID == "" || len(messages) == 0 {
		return
	}
	now := time.Now()
	responsesSessionTranscriptStore.Lock()
	maybePruneResponsesSessionTranscriptsLocked(now)
	responsesSessionTranscriptStore.items[responseID] = responsesSessionTranscriptEntry{
		messages:     cloneResponsesSessionMessages(messages),
		tools:        cloneResponsesSessionTools(tools),
		ownerTokenID: ownerTokenID,
		ownerUserID:  ownerUserID,
		expiresAt:    now.Add(currentResponsesSessionTTL()),
	}
	responsesSessionTranscriptStore.Unlock()

	persistCtx, cancel := metricsPersistContext()
	defer cancel()
	if err := op.ResponseSessionBindTranscript(
		persistCtx,
		responseID,
		messages,
		tools,
		ownerTokenID,
		ownerUserID,
		currentResponsesSessionTTL(),
	); err != nil {
		log.Warnf("failed to persist responses session transcript: %v", err)
	}
}

func cloneResponsesSessionTools(tools []transformerModel.Tool) []transformerModel.Tool {
	if len(tools) == 0 {
		return nil
	}
	return append([]transformerModel.Tool(nil), tools...)
}

// responsesSessionTranscriptTools returns the tools stored with a transcript,
// owner-checked exactly like responsesSessionTranscript. It lets a codex client
// that continues via previous_response_id against a STATELESS Anthropic upstream
// recover the real tool set it declared on the turn that created that id — the
// upstream never saw those tools and codex omits them on continuation.
func responsesSessionTranscriptTools(responseID string, reqTokenID, reqUserID int) ([]transformerModel.Tool, bool) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil, false
	}
	now := time.Now()
	responsesSessionTranscriptStore.Lock()
	entry, ok := responsesSessionTranscriptStore.items[responseID]
	if !ok || now.After(entry.expiresAt) {
		responsesSessionTranscriptStore.Unlock()
		entry, ok = loadResponsesSessionTranscriptFromPersistentStore(responseID, reqTokenID, reqUserID)
		if !ok {
			return nil, false
		}
		return cloneResponsesSessionTools(entry.tools), len(entry.tools) > 0
	}
	if !responsesSessionOwnerMatches(responsesSessionEntry{ownerTokenID: entry.ownerTokenID, ownerUserID: entry.ownerUserID}, reqTokenID, reqUserID) {
		responsesSessionTranscriptStore.Unlock()
		return nil, false
	}
	out := cloneResponsesSessionTools(entry.tools)
	responsesSessionTranscriptStore.Unlock()
	return out, len(out) > 0
}

// responsesSessionTranscript returns a stored transcript only when reqTokenID/
// reqUserID own it (fail-closed via responsesSessionOwnerMatches). A transcript
// owned by another tenant is never replayed into this request.
func responsesSessionTranscript(responseID string, reqTokenID, reqUserID int) ([]transformerModel.Message, bool) {
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
		entry, ok = loadResponsesSessionTranscriptFromPersistentStore(responseID, reqTokenID, reqUserID)
		if !ok {
			return nil, false
		}
		return cloneResponsesSessionMessages(entry.messages), len(entry.messages) > 0
	}
	if !responsesSessionOwnerMatches(responsesSessionEntry{ownerTokenID: entry.ownerTokenID, ownerUserID: entry.ownerUserID}, reqTokenID, reqUserID) {
		responsesSessionTranscriptStore.Unlock()
		return nil, false
	}
	out := cloneResponsesSessionMessages(entry.messages)
	responsesSessionTranscriptStore.Unlock()
	return out, len(out) > 0
}

func loadResponsesSessionTranscriptFromPersistentStore(responseID string, reqTokenID, reqUserID int) (responsesSessionTranscriptEntry, bool) {
	persisted, ok, err := op.ResponseSessionTranscriptOwned(context.Background(), responseID, reqTokenID, reqUserID)
	if err != nil {
		log.Warnf("failed to load responses session transcript: %v", err)
		return responsesSessionTranscriptEntry{}, false
	}
	if !ok || len(persisted.Messages) == 0 {
		return responsesSessionTranscriptEntry{}, false
	}
	entry := responsesSessionTranscriptEntry{
		messages:     trimResponsesSessionTranscript(persisted.Messages),
		tools:        cloneResponsesSessionTools(persisted.Tools),
		ownerTokenID: reqTokenID,
		ownerUserID:  reqUserID,
		expiresAt:    time.Now().Add(currentResponsesSessionTTL()),
	}
	if len(entry.messages) == 0 {
		return responsesSessionTranscriptEntry{}, false
	}
	responsesSessionTranscriptStore.Lock()
	responsesSessionTranscriptStore.items[responseID] = entry
	responsesSessionTranscriptStore.Unlock()
	return entry, true
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

func prioritizeResponsesSessionOwner(ctx context.Context, iter *balancer.Iterator, req *transformerModel.InternalLLMRequest, reqTokenID, reqUserID int) {
	if iter == nil || req == nil || req.PreviousResponseID == nil {
		return
	}
	owner, ok := responsesSessionOwnerWithContext(ctx, *req.PreviousResponseID)
	if !ok {
		return
	}
	// Never steer routing toward another tenant's channel on the strength of a
	// borrowed previous_response_id — that is what let a cross-tenant cursor pass
	// the channel+key check downstream and replay the owner's history.
	if !responsesSessionOwnerMatches(owner, reqTokenID, reqUserID) {
		return
	}
	iter.PrioritizeChannels(map[int]bool{owner.channelID: true})
}

func responsesOwnerKeyForChannel(ctx context.Context, previousResponseID string, channelID, reqTokenID, reqUserID int) int {
	if strings.TrimSpace(previousResponseID) == "" || channelID == 0 {
		return 0
	}
	owner, ok := responsesSessionOwnerWithContext(ctx, previousResponseID)
	if !ok || owner.channelID != channelID {
		return 0
	}
	// Only prefer the owner's key when the caller actually owns the cursor.
	if !responsesSessionOwnerMatches(owner, reqTokenID, reqUserID) {
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
	// Chat-minted ids are local conversation cursors only. Rebuild the prior
	// history into messages (if we have a transcript) and never forward the id
	// to a stateful responses upstream — it is not a real store id.
	if ra.rebuildHistoryForChatSourcedPreviousResponseID() {
		return
	}
	if !outboundSupportsResponsesSessionCursor(outAdapter) {
		ra.internalRequest.PreviousResponseID = nil
		return
	}
	owner, ok := responsesSessionOwnerWithContext(ra.context(), *ra.internalRequest.PreviousResponseID)
	if ok && !responsesSessionOwnerMatches(owner, ra.apiKeyID, ra.userID) {
		// Foreign cursor: this response id belongs to another tenant. Fail closed —
		// drop it outright and do NOT fall back to the sticky heuristic, so a
		// borrowed previous_response_id can neither be forwarded upstream nor steer
		// this request onto the owner's channel.
		ra.internalRequest.PreviousResponseID = nil
		return
	}
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
	// Under store=false the encrypted reasoning item is the official channel for carrying
	// reasoning context across turns (matches how sub2api / CLIProxyAPI handle it): a
	// reasoning model needs the prior turn's encrypted reasoning replayed to continue a
	// tool loop. Preserve it by default so the reasoning→tool-call loop keeps going instead
	// of the model restarting with a plain-text answer (the "stopped calling tools"
	// symptom). Only strip when we can PROVE the encrypted reasoning belongs to a different
	// tenant/channel/key than this attempt; an unprovable mismatch is still caught by the
	// upstream "invalid encrypted content" 400, which drives the strip-and-retry recovery.
	if ra.responsesEncryptedContentOwnerIsForeign() {
		stripResponsesEncryptedContent(ra.internalRequest)
	}
}

// responsesEncryptedContentOwnerIsForeign reports whether the request's
// previous_response_id maps to a RECORDED owner that is a different tenant, channel, or
// key than the current attempt. Unknown ownership — no previous_response_id, or an
// unrecorded id (e.g. codex under store=false, which never sends one) — is treated as NOT
// foreign, so the encrypted reasoning is preserved by default. A real mismatch we cannot
// prove up front is still caught by the upstream invalid-encrypted-content 400 recovery.
func (ra *relayAttempt) responsesEncryptedContentOwnerIsForeign() bool {
	if ra == nil || ra.internalRequest == nil || ra.internalRequest.PreviousResponseID == nil {
		return false
	}
	owner, ok := responsesSessionOwnerWithContext(ra.context(), *ra.internalRequest.PreviousResponseID)
	if !ok {
		return false
	}
	if !responsesSessionOwnerMatches(owner, ra.apiKeyID, ra.userID) {
		return true
	}
	return ra.channel == nil || ra.channel.ID != owner.channelID || ra.usedKey.ID != owner.channelKeyID
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
	if ra == nil || resp == nil {
		return
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse {
		// Chat inbound is handled separately so a later /v1/responses turn can
		// continue the same conversation without a real Responses store id.
		ra.recordChatSessionFromInbound(resp)
		return
	}
	if ra.channel == nil || ra.usedKey.ID == 0 {
		return
	}
	// Conversation root: inherit the prior turn's root when this request continues
	// one the caller actually owns, otherwise this response id seeds a fresh root.
	// The root is a per-conversation, per-tenant stable prompt-cache anchor.
	rootHash := ""
	if ra.internalRequest != nil {
		// Prefer the live previous_response_id; fall back to the id the chat history bridge
		// cleared (it strips it from the chat wire) so a rebuilt chat turn still inherits the
		// prior turn's conversation-root instead of minting a fresh prompt-cache anchor.
		prevForRoot := ra.internalRequest.PreviousResponseID
		if prevForRoot == nil {
			prevForRoot = ra.chatHistoryRebuiltPreviousResponseID
		}
		if prevForRoot != nil {
			rootHash = responsesConversationRootForRequest(ra.context(), *prevForRoot, ra.apiKeyID, ra.userID)
		}
	}
	if rootHash == "" {
		rootHash = op.ResponseSessionIDHash(strings.TrimSpace(resp.ID))
	}
	recordResponsesSessionOwned(ra.context(), resp.ID, ra.channel.ID, ra.usedKey.ID, ra.apiKeyID, ra.userID, rootHash, responseSessionSourceResponses)
	if ra.shouldBridgeResponsesHistory() {
		recordResponsesSessionTranscriptOwned(resp.ID, ra.responsesSessionTranscriptFromResponse(resp), cloneResponsesSessionTools(ra.internalRequest.Tools), ra.apiKeyID, ra.userID)
	}
}

// recordChatSessionFromInbound persists a /v1/chat/completions completion id as a
// local conversation cursor (source=chat) plus the full request+assistant transcript.
// Cursor may later send that id as previous_response_id on /v1/responses; octopus
// must rebuild history from the transcript and MUST NOT forward the id upstream
// (it is not a real Responses store id).
func (ra *relayAttempt) recordChatSessionFromInbound(resp *transformerModel.InternalLLMResponse) {
	if ra == nil || resp == nil || ra.inboundType != inbound.InboundTypeOpenAIChat {
		return
	}
	if ra.channel == nil || ra.usedKey.ID == 0 {
		return
	}
	responseID := strings.TrimSpace(resp.ID)
	if responseID == "" {
		return
	}
	rootHash := op.ResponseSessionIDHash(responseID)
	recordResponsesSessionOwned(ra.context(), responseID, ra.channel.ID, ra.usedKey.ID, ra.apiKeyID, ra.userID, rootHash, responseSessionSourceChat)
	recordResponsesSessionTranscriptOwned(
		responseID,
		ra.responsesSessionTranscriptFromResponse(resp),
		cloneResponsesSessionTools(ra.internalRequest.Tools),
		ra.apiKeyID,
		ra.userID,
	)
}

// rebuildHistoryForChatSourcedPreviousResponseID detects a previous_response_id
// that was minted by /v1/chat/completions (source=chat). Those ids are never real
// OpenAI Responses store ids, so:
//  1. rebuild the full prior history from the local transcript into messages
//  2. drop previous_response_id so it is not forwarded upstream
// Returns true when the cursor was chat-sourced (handled, caller must not keep it).
func (ra *relayAttempt) rebuildHistoryForChatSourcedPreviousResponseID() bool {
	if ra == nil || ra.internalRequest == nil || ra.internalRequest.PreviousResponseID == nil {
		return false
	}
	previousResponseID := strings.TrimSpace(*ra.internalRequest.PreviousResponseID)
	if previousResponseID == "" {
		return false
	}
	owner, ok := responsesSessionOwnerWithContext(ra.context(), previousResponseID)
	if !ok || !responsesSessionOwnerMatches(owner, ra.apiKeyID, ra.userID) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(owner.source), responseSessionSourceChat) {
		return false
	}
	req := ra.internalRequest
	if !responsesMessagesAlreadyCarryAssistantContext(req.Messages) {
		if history, hasHistory := responsesSessionTranscript(previousResponseID, ra.apiKeyID, ra.userID); hasHistory && len(history) > 0 {
			req.Messages = appendPlainResponsesHistory(history, req.Messages)
			req.Messages = dropUnpairedToolItems(req.Messages)
			// Stash so the re-recorded session inherits the conversation root and
			// forward() can normalize tool-call pairing on the wire copy only.
			stashedPrevID := previousResponseID
			ra.chatHistoryRebuiltPreviousResponseID = &stashedPrevID
			ra.chatHistoryRebuilt = true
		}
	}
	// Always drop the chat-minted cursor: even without a transcript, forwarding it
	// to a stateful responses upstream yields a hard 400 (not a real store id).
	req.PreviousResponseID = nil
	req.ResponsesInputRaw = nil
	return true
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
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat:
		// Chat/completions upstreams keep no server-side response state, so record
		// the transcript locally: the next previous_response_id turn is rebuilt from
		// it by bridgeResponsesHistoryForChat (mirrors the Anthropic bridge above).
		return true
	case outbound.OutboundTypeOpenAIResponse:
		// Store the transcript for any plain responses client (NOT the codex CLI, which
		// replays its own history against a stateful codex upstream). It feeds the codex
		// history bridge and — crucially — lets bridgeResponsesHistoryForChat rebuild the
		// conversation if this responses upstream flakes and the request is downgraded to
		// chat/completions (which keeps no server-side state). The codex-fingerprint-only
		// store (shouldBridgePlainResponsesCodexHistory) was mutually exclusive with
		// fallback-eligibility, so a downgraded turn could never find a transcript.
		return !ra.inboundLooksLikeCodexClient()
	default:
		return false
	}
}

// bridgeResponsesHistoryForAnthropic replays a prior responses turn's history
// into the outgoing messages when a plain responses client (e.g. Cursor) targets
// an Anthropic channel. Anthropic has no previous_response_id, so without this
// the next turn would lose all prior context.
//
// Tool-output continuations ARE rebuilt (a codex-style agent sends only the
// function_call_output increment after its first tool call). The stored transcript retains the
// assistant turn that issued the matching tool_call, so the rebuilt
// [..., assistant(tool_call), tool(output)] converts to a paired tool_use + tool_result for the
// Anthropic upstream — mirroring bridgePlainResponsesCodexHistory and bridgeResponsesHistoryForChat.
// Without the rebuild, a stateless Anthropic upstream (which has no server-side response state at
// all) would receive only the bare tool result, losing all prior context and orphaning the result.
// applyPlainResponsesCodexHistoryForPreviousResponseID leaves the request untouched when the current
// turn already carries assistant context or the transcript is unavailable (no worse than before).
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
	ra.applyPlainResponsesCodexHistoryForPreviousResponseID(*req.PreviousResponseID)
}

// openAIChatOutboundChannel reports whether the channel down-converts to the OpenAI
// /v1/chat/completions wire protocol (plain or custom). Such upstreams keep no
// server-side response state and the wire format has no previous_response_id field.
func openAIChatOutboundChannel(channelType outbound.OutboundType) bool {
	return channelType == outbound.OutboundTypeOpenAIChat ||
		channelType == outbound.OutboundTypeCustomOpenAIChat
}

// bridgeResponsesHistoryForChat replays a prior responses turn's history into the
// outgoing messages when a plain responses client (relying on previous_response_id)
// targets a CHAT channel. A chat/completions upstream keeps no server-side state and
// the wire protocol has no previous_response_id field, so without this the next turn
// would reach the upstream carrying ONLY the incremental input and silently lose all
// prior context — the upstream would still answer 200, masking the loss.
//
// When the transcript is unavailable (unknown/expired id we never stored, and the
// client did not carry the history itself), the request is refused with a
// deterministic invalid_request_error instead of forwarding a context-stripped turn
// — mirroring new-api's stateful-field rejection and CLIProxyAPI's
// previous_response_not_found classification. That 400 is a request-shape rejection,
// so shouldRecordBreakerFailure never charges it to the channel breaker, and the
// caller's own error handling reacts instead of receiving a false success.
//
// Tool-output continuations ARE rebuilt here (unlike bridgeResponsesHistoryForAnthropic):
// a codex-style agent on a chat channel sends only the function_call_output increment
// (role "tool") on every turn after its first tool call and relies on previous_response_id
// for the rest, so skipping these turns would silently forward a bare tool result with no
// context — total history loss behind a 200. The stored transcript retains the assistant
// message that issued the matching tool_call (messageHasNoReplayableContent keeps tool-call
// -only messages, cloneResponseSessionMessage preserves ToolCalls/ToolCallID, and the trim
// drops from the front so the prior turn's trailing assistant tool_call survives), so the
// rebuilt [..., assistant(tool_call), tool(output)] sequence stays coherent and call_ids line up.
func (ra *relayAttempt) bridgeResponsesHistoryForChat() error {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return nil
	}
	ra.chatHistoryRebuilt = false
	ra.chatHistoryRebuiltPreviousResponseID = nil
	// Runs for a native chat channel, or for a responses channel that the compatibility
	// fallback downgraded onto the chat/completions wire (responsesDowngradedToChat).
	if ra.inboundType != inbound.InboundTypeOpenAIResponse ||
		(!openAIChatOutboundChannel(ra.channel.Type) && !ra.responsesDowngradedToChat) {
		return nil
	}
	req := ra.internalRequest
	if req.PreviousResponseID == nil || strings.TrimSpace(*req.PreviousResponseID) == "" {
		// No stateful chaining requested (e.g. full-replay tier1 clients that carry the
		// whole conversation every turn): nothing to rebuild and nothing at risk.
		return nil
	}
	if responsesMessagesAlreadyCarryAssistantContext(req.Messages) {
		// The client already sent the assistant history in this request (full-replay
		// clients, including ones that inline their own tool-call/tool-result turns), so
		// there is nothing to restore and no context is lost. Pure sticky increments —
		// including a lone function_call_output (role "tool") — carry no assistant turn
		// and fall through to the transcript rebuild below.
		return nil
	}
	previousResponseID := strings.TrimSpace(*req.PreviousResponseID)
	history, ok := responsesSessionTranscript(previousResponseID, ra.apiKeyID, ra.userID)
	if !ok || len(history) == 0 {
		return newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"type":"invalid_request_error","message":"previous_response_id cannot be continued on this channel: no server-side response state is available and no stored transcript matched. Resend the full conversation in input instead of relying on previous_response_id."}}`))
	}
	// Rebuild the FULL prior history and store it un-normalized: keeping every announced
	// tool_call lets a later turn that answers a still-pending parallel call pair it. The
	// chat tool-call pairing invariant is enforced only on the wire copy at send time
	// (forward -> normalizeChatToolCallPairing), never on the stored transcript.
	req.Messages = appendPlainResponsesHistory(history, req.Messages)
	// Preserve the conversation-root anchor across the previous_response_id clear below so the
	// re-recorded session inherits the prior turn's prompt-cache root instead of minting a
	// fresh one every turn (see recordResponsesSessionFromInbound).
	stashedPrevID := previousResponseID
	ra.chatHistoryRebuiltPreviousResponseID = &stashedPrevID
	ra.chatHistoryRebuilt = true
	req.PreviousResponseID = nil
	req.ResponsesInputRaw = nil
	return nil
}

// restoreCodexToolsForAnthropic re-attaches the codex tool set when a codex CLI
// continuation reaches an Anthropic channel with no tools. The codex CLI omits the
// `tools` array on continuation turns (it relies on previous_response_id, which a
// real codex upstream would remember); Anthropic is stateless, so without this the
// mapped Claude model loses its tools mid-conversation and stops calling them — it
// narrates ("let me look at ...") instead of acting and the agent stalls. This
// mirrors ensureCodexAgentContext's tool restoration on the codex→codex path
// (prepareCodexRequestShape, which never runs for a non-Responses upstream), but is
// scoped to codex clients only so a non-codex responses client (e.g. Cursor)
// targeting Anthropic is left untouched — its tools differ from the codex set.
func (ra *relayAttempt) restoreCodexToolsForAnthropic() {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil {
		return
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse || ra.channel.Type != outbound.OutboundTypeAnthropic {
		return
	}
	if !ra.inboundLooksLikeCodexClient() {
		return
	}
	req := ra.internalRequest
	if len(req.Tools) > 0 || len(req.ResponsesToolsRaw) > 0 {
		return
	}
	if req.PreviousResponseID == nil || strings.TrimSpace(*req.PreviousResponseID) == "" {
		return
	}
	// Restore the client's REAL tools stored on the turn that created
	// previous_response_id — NOT a hardcoded default set, whose names (e.g.
	// shell_command) may not match this codex version's real tool (shell /
	// exec_command / local_shell). If the session is gone, leave tools empty
	// rather than inject a wrong-named default the client's model cannot map.
	restored, ok := responsesSessionTranscriptTools(strings.TrimSpace(*req.PreviousResponseID), ra.apiKeyID, ra.userID)
	if !ok || len(restored) == 0 {
		return
	}
	req.Tools = restored
	if req.ToolChoice == nil {
		choice := "auto"
		req.ToolChoice = &transformerModel.ToolChoice{ToolChoice: &choice}
	}
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
