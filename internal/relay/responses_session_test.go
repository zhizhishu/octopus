package relay

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

func TestPrepareResponsesSessionCursorKeepsKnownOwner(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_owner"
	recordResponsesSession(previous, 6, 7)

	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID == nil || *req.PreviousResponseID != previous {
		t.Fatalf("expected owner channel to keep previous_response_id, got %#v", req.PreviousResponseID)
	}
}

func TestPreviousResponsesOwnerKeyForChannel(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_owner_key"
	recordResponsesSession(previous, 6, 7)
	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}

	if got := previousResponsesOwnerKeyForChannel(context.Background(), req, 6); got != 7 {
		t.Fatalf("owner key for channel 6 = %d, want 7", got)
	}
	if got := previousResponsesOwnerKeyForChannel(context.Background(), req, 8); got != 0 {
		t.Fatalf("owner key for wrong channel = %d, want 0", got)
	}
}

// P1: a cursor owned by another tenant must be dropped even when this attempt is
// already pointed at the owner's channel+key (the exact shape that previously let
// a borrowed previous_response_id pass the channel/key check and replay history).
func TestPrepareResponsesSessionCursorDropsForeignTenant(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_foreign_tenant"
	recordResponsesSessionOwned(context.Background(), previous, 6, 7, 100, 0, "")

	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
			apiKeyID:        999, // different tenant than the owner (token 100)
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID != nil {
		t.Fatalf("foreign-tenant previous_response_id must be dropped, got %#v", *req.PreviousResponseID)
	}
}

// P1: the owning tenant keeps its cursor.
func TestPrepareResponsesSessionCursorKeepsMatchingTenant(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_same_tenant"
	recordResponsesSessionOwned(context.Background(), previous, 6, 7, 100, 0, "")

	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
			apiKeyID:        100, // matches the owner token
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID == nil || *req.PreviousResponseID != previous {
		t.Fatalf("owning tenant must keep previous_response_id, got %#v", req.PreviousResponseID)
	}
}

// P1: stored transcripts are never replayed into a foreign tenant's request.
func TestResponsesSessionTranscriptRejectsForeignTenant(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_transcript_owner"
	recordResponsesSessionTranscriptOwned(previous, []transformerModel.Message{
		{Role: "user", Content: textMessageContent("owner secret prompt")},
		{Role: "assistant", Content: textMessageContent("owner secret answer")},
	}, 100, 0)

	if _, ok := responsesSessionTranscript(previous, 999, 0); ok {
		t.Fatalf("foreign tenant must not read another tenant's transcript")
	}
	history, ok := responsesSessionTranscript(previous, 100, 0)
	if !ok || len(history) == 0 {
		t.Fatalf("owning tenant must read its own transcript, ok=%v len=%d", ok, len(history))
	}
}

// P1: the owner-key preference (raw-protocol sticky) is only honored for the owner.
func TestResponsesOwnerKeyForChannelEnforcesIdentity(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_owner_key_identity"
	recordResponsesSessionOwned(context.Background(), previous, 6, 7, 100, 0, "")

	if got := responsesOwnerKeyForChannel(context.Background(), previous, 6, 100, 0); got != 7 {
		t.Fatalf("owner must get preferred key, got %d want 7", got)
	}
	if got := responsesOwnerKeyForChannel(context.Background(), previous, 6, 999, 0); got != 0 {
		t.Fatalf("foreign tenant must not get owner key, got %d want 0", got)
	}
}

// P1: legacy rows written before the owner columns existed (owner 0/0) stay
// usable for backward compatibility until they expire within the session TTL.
func TestResponsesSessionOwnerMatchesLegacyUnrestricted(t *testing.T) {
	legacy := responsesSessionEntry{ownerTokenID: 0, ownerUserID: 0}
	if !responsesSessionOwnerMatches(legacy, 123, 456) {
		t.Fatalf("legacy owner-less record must be unrestricted")
	}
	owned := responsesSessionEntry{ownerTokenID: 100}
	if responsesSessionOwnerMatches(owned, 999, 0) {
		t.Fatalf("owned record must reject a mismatched token")
	}
	if !responsesSessionOwnerMatches(owned, 100, 0) {
		t.Fatalf("owned record must accept the matching token")
	}
}

func TestPrepareResponsesSessionCursorDropsCrossChannelOwner(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_cross_channel"
	recordResponsesSession(previous, 6, 7)

	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{ID: 8, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 9},
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID != nil {
		t.Fatalf("expected cross-channel previous_response_id to be dropped, got %#v", *req.PreviousResponseID)
	}
}

func TestPrepareResponsesSessionCursorDropsUnknownNonStickyCursor(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_unknown"
	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
			iter:            balancer.NewIterator(dbmodel.Group{}, 1, "gpt-5.5"),
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID != nil {
		t.Fatalf("expected unknown non-sticky previous_response_id to be dropped, got %#v", *req.PreviousResponseID)
	}
}

func TestPrepareResponsesSessionCursorDropsUnknownLegacyStickyKeyCursor(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_unknown_sticky"
	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
			iter:            balancer.NewIterator(group, 1, "gpt-5.5"),
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}
	if !ra.iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID != nil {
		t.Fatalf("legacy sticky key without explicit client session must drop unknown previous_response_id, got %#v", *req.PreviousResponseID)
	}
}

func TestPrepareResponsesSessionCursorKeepsUnknownStickyKeyCursorWithTrustedClientSession(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_unknown_sticky_trusted"
	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:         inbound.InboundTypeOpenAIResponse,
			internalRequest:     req,
			iter:                balancer.NewIterator(group, 1, "gpt-5.5"),
			clientSessionSource: "header:Session_id",
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}
	if !ra.iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID == nil || *req.PreviousResponseID != previous {
		t.Fatalf("expected trusted sticky channel key to keep unknown previous_response_id, got %#v", req.PreviousResponseID)
	}
}

func TestPrepareResponsesSessionCursorDropsUnknownStickyCursorFromPreviousResponseFallback(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_unknown_previous_fallback"
	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:         inbound.InboundTypeOpenAIResponse,
			internalRequest:     req,
			iter:                balancer.NewIterator(group, 1, "gpt-5.5"),
			clientSessionSource: "body:previous_response_id",
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}
	if !ra.iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID != nil {
		t.Fatalf("previous_response_id fallback alone must not prove cursor ownership, got %#v", *req.PreviousResponseID)
	}
}

func TestPrepareResponsesSessionCursorDropsUnknownStickyChannelWrongKey(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_unknown_wrong_key"
	req := &transformerModel.InternalLLMRequest{PreviousResponseID: &previous}
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
			iter:            balancer.NewIterator(group, 1, "gpt-5.5"),
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 8},
	}
	if !ra.iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID != nil {
		t.Fatalf("expected wrong sticky key previous_response_id to be dropped, got %#v", *req.PreviousResponseID)
	}
}

func TestResponsesSessionOwnerFallsBackToPersistentStore(t *testing.T) {
	setupResponsesSessionDB(t)
	clearResponsesSessionCacheForTest()

	previous := "resp_persisted_owner"
	recordResponsesSessionWithContext(context.Background(), previous, 11, 12)
	clearResponsesSessionCacheForTest()

	owner, ok := responsesSessionOwner(previous)
	if !ok {
		t.Fatalf("expected persistent response owner")
	}
	if owner.channelID != 11 || owner.channelKeyID != 12 {
		t.Fatalf("owner = (%d,%d), want (11,12)", owner.channelID, owner.channelKeyID)
	}
}

func TestResponsesSessionOwnerIgnoresExpiredPersistentStore(t *testing.T) {
	setupResponsesSessionDB(t)
	clearResponsesSessionCacheForTest()

	previous := "resp_expired_owner"
	if err := op.ResponseSessionBind(context.Background(), previous, 11, 12, time.Second); err != nil {
		t.Fatalf("bind response session: %v", err)
	}
	hash := op.ResponseSessionIDHash(previous)
	if err := db.GetDB().Model(&dbmodel.ResponseSession{}).
		Where("response_id_hash = ?", hash).
		Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatalf("expire response session: %v", err)
	}

	if owner, ok := responsesSessionOwner(previous); ok {
		t.Fatalf("expected expired owner miss, got %#v", owner)
	}
}

func TestRecordResponsesSessionPersistsWithCanceledRequestContext(t *testing.T) {
	setupResponsesSessionDB(t)
	clearResponsesSessionCacheForTest()

	previous := "resp_canceled_context_owner"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recordResponsesSessionWithContext(ctx, previous, 21, 22)
	clearResponsesSessionCacheForTest()

	owner, ok := responsesSessionOwner(previous)
	if !ok {
		t.Fatalf("expected owner to persist despite canceled request context")
	}
	if owner.channelID != 21 || owner.channelKeyID != 22 {
		t.Fatalf("owner = (%d,%d), want (21,22)", owner.channelID, owner.channelKeyID)
	}
}

func TestShouldRecoverOpenAIResponsesPreviousResponseNotFound(t *testing.T) {
	previous := "resp_missing"
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType: inbound.InboundTypeOpenAIResponse,
			internalRequest: &transformerModel.InternalLLMRequest{
				PreviousResponseID: &previous,
				Messages: []transformerModel.Message{{
					Role: "user",
				}},
			},
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
	}
	err := newUpstreamError(400, []byte(`{"error":{"code":"previous_response_not_found","message":"previous response not found"}}`))
	if !ra.shouldRecoverOpenAIResponsesPreviousResponseNotFound(400, err) {
		t.Fatalf("expected previous_response_not_found recovery")
	}
}

func TestShouldNotRecoverOpenAIResponsesToolOutputCursor(t *testing.T) {
	previous := "resp_missing"
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType: inbound.InboundTypeOpenAIResponse,
			internalRequest: &transformerModel.InternalLLMRequest{
				PreviousResponseID: &previous,
				Messages: []transformerModel.Message{{
					Role: "tool",
				}},
			},
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
	}
	err := newUpstreamError(400, []byte(`{"error":{"code":"previous_response_not_found","message":"previous response not found"}}`))
	if ra.shouldRecoverOpenAIResponsesPreviousResponseNotFound(400, err) {
		t.Fatalf("tool output should not retry without previous_response_id")
	}
}

func TestShouldRecoverOpenAIResponsesInvalidEncryptedContent(t *testing.T) {
	encrypted := "gAAAAABstale"
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType: inbound.InboundTypeOpenAIResponse,
			internalRequest: &transformerModel.InternalLLMRequest{
				Messages: []transformerModel.Message{{
					Role:               "assistant",
					ReasoningSignature: &encrypted,
				}},
			},
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
	}
	err := newUpstreamError(400, []byte(`{"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted or parsed"}}`))
	if !ra.shouldRecoverOpenAIResponsesInvalidEncryptedContent(400, err) {
		t.Fatalf("expected invalid encrypted content recovery")
	}
}

func TestShouldNotRecoverInvalidEncryptedContentWithoutReasoning(t *testing.T) {
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType: inbound.InboundTypeOpenAIResponse,
			internalRequest: &transformerModel.InternalLLMRequest{
				Messages: []transformerModel.Message{{Role: "user"}},
			},
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
	}
	err := newUpstreamError(400, []byte(`{"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted or parsed"}}`))
	if ra.shouldRecoverOpenAIResponsesInvalidEncryptedContent(400, err) {
		t.Fatalf("plain requests should not retry encrypted-content recovery")
	}
}

func TestPrepareResponsesEncryptedContentKeepsKnownOwner(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_owner_encrypted"
	encrypted := "gAAAAABowner"
	recordResponsesSession(previous, 6, 7)

	req := &transformerModel.InternalLLMRequest{
		PreviousResponseID: &previous,
		Messages: []transformerModel.Message{{
			Role:               "assistant",
			ReasoningSignature: &encrypted,
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}

	ra.prepareResponsesEncryptedContent(&openaiOutbound.ResponseOutbound{})

	if req.Messages[0].ReasoningSignature == nil || *req.Messages[0].ReasoningSignature != encrypted {
		t.Fatalf("expected owner encrypted content to be preserved, got %#v", req.Messages[0].ReasoningSignature)
	}
}

func TestPrepareResponsesEncryptedContentStripsCrossChannel(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_cross_encrypted"
	encrypted := "gAAAAABcross"
	recordResponsesSession(previous, 6, 7)

	req := &transformerModel.InternalLLMRequest{
		PreviousResponseID: &previous,
		Messages: []transformerModel.Message{{
			Role:               "assistant",
			ReasoningSignature: &encrypted,
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{ID: 8, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 9},
	}

	ra.prepareResponsesEncryptedContent(&openaiOutbound.ResponseOutbound{})

	if req.Messages[0].ReasoningSignature != nil {
		t.Fatalf("expected cross-channel encrypted content to be stripped, got %#v", req.Messages[0].ReasoningSignature)
	}
}

// Option B (default-preserve): an unknown-owner encrypted reasoning item under store=false
// is the multi-turn continuity channel and is PRESERVED by default; only a proven foreign
// owner is stripped. (Was TestPrepareResponsesEncryptedContentStripsUnknownStickyKey, which
// asserted the old strip-when-unsure default.)
func TestPrepareResponsesEncryptedContentPreservesUnknownOwner(t *testing.T) {
	clearResponsesSessionCacheForTest()
	encrypted := "gAAAAABsticky"
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	req := &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{{
			Role:               "assistant",
			ReasoningSignature: &encrypted,
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
			iter:            balancer.NewIterator(group, 1, "gpt-5.5"),
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}
	if !ra.iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	ra.prepareResponsesEncryptedContent(&openaiOutbound.ResponseOutbound{})

	if req.Messages[0].ReasoningSignature == nil || *req.Messages[0].ReasoningSignature != encrypted {
		t.Fatalf("expected unknown-owner encrypted content to be preserved (store=false continuity), got %#v", req.Messages[0].ReasoningSignature)
	}
}

// Codex-shaped clients run with store=false and never send a previous_response_id, so
// the owner-match check can never succeed for them. When the turn is pinned to the same
// channel+key through a correctness-critical sticky source, the encrypted reasoning was
// produced by that very account and must be preserved so the reasoning→tool-call loop can
// continue instead of restarting with a plain-text answer (the "stopped calling tools"
// symptom). This mirrors TestPrepareResponsesSessionCursorKeepsUnknownStickyKeyCursorWithTrustedClientSession.
func TestPrepareResponsesEncryptedContentKeepsUnknownStickyKeyWithTrustedClientSession(t *testing.T) {
	clearResponsesSessionCacheForTest()
	encrypted := "gAAAAABtrustedsticky"
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	req := &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{{
			Role:               "assistant",
			ReasoningSignature: &encrypted,
		}},
		ResponsesInputRaw: []byte(`[{"type":"reasoning","encrypted_content":"gAAAAABtrustedsticky"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]`),
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:         inbound.InboundTypeOpenAIResponse,
			internalRequest:     req,
			iter:                balancer.NewIterator(group, 1, "gpt-5.5"),
			clientSessionSource: "header:Session_id",
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}
	if !ra.iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	ra.prepareResponsesEncryptedContent(&openaiOutbound.ResponseOutbound{})

	if req.Messages[0].ReasoningSignature == nil || *req.Messages[0].ReasoningSignature != encrypted {
		t.Fatalf("expected trusted sticky channel key to preserve codex encrypted reasoning, got %#v", req.Messages[0].ReasoningSignature)
	}
	if !strings.Contains(string(req.ResponsesInputRaw), `gAAAAABtrustedsticky`) {
		t.Fatalf("expected trusted sticky raw encrypted content to be preserved, got %s", string(req.ResponsesInputRaw))
	}
}

// Option B: the unknown-owner cursor is still dropped, but the encrypted reasoning is now
// PRESERVED by default (previously stripped). A real account mismatch is caught by the
// upstream invalid-encrypted-content 400 recovery.
func TestPrepareResponsesSessionDropsOldStickyCursorButPreservesEncryptedRawInput(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_old_codex_unknown_owner"
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	req := &transformerModel.InternalLLMRequest{
		PreviousResponseID: &previous,
		ResponsesInputRaw:  []byte(`[{"type":"reasoning","encrypted_content":"gAAAAABold"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue old conversation"}]}]`),
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
			iter:            balancer.NewIterator(group, 1, "gpt-5.5"),
		},
		channel: &dbmodel.Channel{ID: 6, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 7},
	}
	if !ra.iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	ra.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})
	ra.prepareResponsesEncryptedContent(&openaiOutbound.ResponseOutbound{})

	if req.PreviousResponseID != nil {
		t.Fatalf("expected old sticky cursor without explicit client session to be dropped, got %#v", *req.PreviousResponseID)
	}
	if !strings.Contains(string(req.ResponsesInputRaw), `gAAAAABold`) {
		t.Fatalf("expected unknown-owner encrypted content to be preserved (store=false continuity), got %s", string(req.ResponsesInputRaw))
	}
	if !strings.Contains(string(req.ResponsesInputRaw), `"continue old conversation"`) {
		t.Fatalf("expected native responses input shape to be preserved, got %s", string(req.ResponsesInputRaw))
	}
}

// Option B still strips when the prior response is a PROVEN foreign owner (recorded on a
// different channel/key). This keeps coverage of the raw-input strip mechanism: the reasoning
// item's encrypted_content is removed while message content and non-reasoning fields survive.
func TestPrepareResponsesEncryptedContentStripsForeignOwnerRawInput(t *testing.T) {
	clearResponsesSessionCacheForTest()
	encrypted := "gAAAAABraw"
	previous := "resp_foreign_raw"
	recordResponsesSession(previous, 6, 7) // owner = channel 6 / key 7
	req := &transformerModel.InternalLLMRequest{
		PreviousResponseID: &previous,
		ResponsesInputRaw:  []byte(`[{"type":"reasoning","encrypted_content":"gAAAAABraw"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}],"encrypted_content":"leave-message-alone"}]`),
		Messages: []transformerModel.Message{{
			Role:               "assistant",
			ReasoningSignature: &encrypted,
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{ID: 8, Type: outbound.OutboundTypeOpenAIResponse}, // different channel → foreign owner
		usedKey: dbmodel.ChannelKey{ID: 9},
	}

	ra.prepareResponsesEncryptedContent(&openaiOutbound.ResponseOutbound{})

	if req.Messages[0].ReasoningSignature != nil {
		t.Fatalf("expected message encrypted content to be stripped, got %#v", req.Messages[0].ReasoningSignature)
	}
	if req.ResponsesInputRaw == nil {
		t.Fatalf("expected raw responses input to be preserved without encrypted content")
	}
	if strings.Contains(string(req.ResponsesInputRaw), `gAAAAABraw`) {
		t.Fatalf("expected raw responses reasoning encrypted content to be stripped, got %s", string(req.ResponsesInputRaw))
	}
	if !strings.Contains(string(req.ResponsesInputRaw), `"leave-message-alone"`) {
		t.Fatalf("expected non-reasoning encrypted content to be preserved, got %s", string(req.ResponsesInputRaw))
	}
	if !strings.Contains(string(req.ResponsesInputRaw), `"continue"`) {
		t.Fatalf("expected raw responses input content to be preserved, got %s", string(req.ResponsesInputRaw))
	}
}

func setupResponsesSessionDB(t *testing.T) {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		clearResponsesSessionCacheForTest()
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
}
