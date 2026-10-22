package relay

import (
	"context"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// maxGroupRaceChannels caps how many channels may join one cross-channel race.
const maxGroupRaceChannels = 5

// groupRaceCandidate is one channel that will contribute contending keys. Its template is
// the parent's untouched request cloned, model-pinned and prompt-applied on the MAIN
// goroutine; runtimeModel/capabilityKey are derived from that same template so the breaker
// scope used to pick keys matches the scope failures are recorded under.
type groupRaceCandidate struct {
	item           dbmodel.GroupItem
	channel        *dbmodel.Channel
	keys           []dbmodel.ChannelKey
	template       *model.InternalLLMRequest
	promptSnapshot promptOverrideSnapshot
	runtimeModel   string
	capabilityKey  string
}

// buildGroupRaceContenders builds the (MAIN-THREAD ONLY) contender list for a
// cross-channel race — it never races and owns no scheduler.
//
// It groups the current candidate plus the CONTIGUOUS same-tier following candidates,
// stopping at the first serial / non-opt-in / disabled / protocol-incompatible / keyless
// one (the rest, if any, are left for the normal candidate loop). For each channel it
// clones the parent's ORIGINAL request (main has already restored the original template),
// sets that item's model name, and applies prompt overrides on the main goroutine — the
// builder never re-applies the parent template's prompt. Keys are filtered by breaker
// scope (or taken wholesale for no-breaker channels) using the existing health / owner /
// sticky preference. Each channel's keys are interleaved round-robin into one global
// sequence so the first wave yields one key per channel before filling.
//
// endpoint is the resolved request endpoint (the same string attempt spans record under);
// it is threaded into the breaker scope so the key filter matches what the racers record.
//
// additionalChannels counts only the contiguous candidates that actually joined, not
// counting the current one.
func buildGroupRaceContenders(
	ctx context.Context,
	req *relayRequest,
	currentItem dbmodel.GroupItem,
	currentChannel *dbmodel.Channel,
	currentKeys []dbmodel.ChannelKey,
	route routeGroupResult,
	endpoint string,
) (contenders []raceContender, additionalChannels int) {
	if req == nil || req.internalRequest == nil || currentChannel == nil {
		return nil, 0
	}
	// Manual intervention pins a single key/channel: never race.
	if req.interventionKeyID > 0 {
		return nil, 0
	}
	// An existing Responses owner cursor must not be re-routed to another channel — the
	// cursor is only forwardable to the exact channel+key that minted it.
	if req.internalRequest.PreviousResponseID != nil && *req.internalRequest.PreviousResponseID != "" {
		return nil, 0
	}
	// Protocol limits: never race image / video / embed requests across channels.
	if isVideoRequest(req) {
		return nil, 0
	}
	if req.internalRequest.IsEmbeddingRequest() || req.internalRequest.IsImageGenerationRequest() {
		return nil, 0
	}

	candidates := make([]groupRaceCandidate, 0, maxGroupRaceChannels)

	current, ok := buildGroupRaceCandidate(req, currentItem, currentChannel, route, endpoint, currentKeys)
	if !ok {
		return nil, 0
	}
	candidates = append(candidates, current)

	for _, item := range req.iter.RemainingTierItems() {
		if len(candidates) >= maxGroupRaceChannels {
			break
		}
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil || channel == nil || !channel.Enabled {
			break
		}
		if !groupRaceChannelCompatible(req.internalRequest, channel) {
			break
		}
		next, ok := buildGroupRaceCandidate(req, item, channel, route, endpoint, nil)
		if !ok {
			break
		}
		candidates = append(candidates, next)
		additionalChannels++
	}

	return interleaveGroupRaceContenders(candidates), additionalChannels
}

// buildGroupRaceCandidate builds one channel's contribution: it clones the parent's
// untouched request, pins the item's model name, applies the cross-channel prompt layers on
// the MAIN goroutine, then derives the runtime model and routing capability key from that
// SAME template. It then selects the contending keys with the breaker filter scoped by
// exactly the (runtime model, endpoint, capability) tuple failures are RECORDED under, so a
// tripped scope is honoured instead of a phantom (raw-model, empty-endpoint) one.
func buildGroupRaceCandidate(
	req *relayRequest,
	item dbmodel.GroupItem,
	channel *dbmodel.Channel,
	route routeGroupResult,
	endpoint string,
	presetKeys []dbmodel.ChannelKey,
) (groupRaceCandidate, bool) {
	if channel == nil {
		return groupRaceCandidate{}, false
	}
	template := cloneInternalRequestForRetry(req.internalRequest)
	if template == nil {
		return groupRaceCandidate{}, false
	}
	template.Model = item.ModelName
	snapshot := applyPromptOverrides(template, route.AccessPlan, route.AccessRouteRule, channel)
	if len(snapshot.Sources) > 0 {
		clearResponsesRawPromptShape(template)
	}
	runtimeModel := runtimeModelForChannel(channel, template)
	capabilityKey := routingCapabilityKey(template, channel)

	keys := raceKeysForChannel(req, item, channel, endpoint, runtimeModel, capabilityKey, presetKeys)
	if len(keys) == 0 {
		return groupRaceCandidate{}, false
	}
	return groupRaceCandidate{
		item:           item,
		channel:        channel,
		keys:           keys,
		template:       template,
		promptSnapshot: snapshot,
		runtimeModel:   runtimeModel,
		capabilityKey:  capabilityKey,
	}, true
}

// raceKeysForChannel selects this channel's contending keys: every enabled key for a
// no-breaker channel (never benched — matches the relay hot path), otherwise the available
// set filtered by the breaker scope that matches the RECORD tuple (runtime model, endpoint,
// capability). When preset is non-empty it is used as the base set but is still
// circuit-filtered (no-breaker channels are never filtered). Owner-key (Responses cursor)
// then sticky preference wins, then health ordering — all via the existing helpers.
func raceKeysForChannel(
	req *relayRequest,
	item dbmodel.GroupItem,
	channel *dbmodel.Channel,
	endpoint string,
	runtimeModel string,
	capabilityKey string,
	preset []dbmodel.ChannelKey,
) []dbmodel.ChannelKey {
	if channel == nil {
		return nil
	}
	base := preset
	if len(base) == 0 {
		if channel.DisableCircuitBreaker {
			base = channel.GetAllEnabledChannelKeys()
		} else {
			base = channel.GetAvailableChannelKeys()
		}
	}
	if !channel.DisableCircuitBreaker {
		filtered := make([]dbmodel.ChannelKey, 0, len(base))
		for _, key := range base {
			if tripped, _ := balancer.IsTrippedScoped(channel.ID, key.ID, runtimeModel, endpoint, capabilityKey); tripped {
				continue
			}
			filtered = append(filtered, key)
		}
		base = filtered
	}
	if len(base) == 0 {
		return nil
	}

	preferredKeyID := 0
	if req != nil && req.c != nil && req.c.Request != nil {
		preferredKeyID = previousResponsesOwnerKeyForChannelOwned(req.c.Request.Context(), req.internalRequest, channel.ID, req.apiKeyID, req.userID)
	}
	if preferredKeyID == 0 && req != nil && req.iter != nil {
		preferredKeyID = req.iter.StickyKeyIDForCurrentChannel(channel.ID)
	}
	return balancer.PrioritizeChannelKeysByHealth(base, channel.ID, item.ModelName, preferredKeyID)
}

// groupRaceChannelCompatible reports whether a channel may join the race for this request
// (enabled + opt-in + protocol/type compatibility). Serial (non-RaceMode) channels end the
// contiguous group.
func groupRaceChannelCompatible(req *model.InternalLLMRequest, channel *dbmodel.Channel) bool {
	if channel == nil || !channel.Enabled || !channel.RaceMode {
		return false
	}
	if req == nil {
		return false
	}
	if req.IsImageGenerationRequest() && !isImageGenerationRequestCompatibleChannelType(channel.Type) {
		return false
	}
	// 能力位判定：空 Capabilities 的存量渠道退回按渠道类型判定，行为不变。
	if req.IsEmbeddingRequest() && !channel.SupportsCapability(dbmodel.CapabilityEmbeddings) {
		return false
	}
	if req.IsChatRequest() && !outbound.IsChatChannelType(channel.Type) {
		return false
	}
	return true
}

// interleaveGroupRaceContenders flattens channels into one global sequence, round-robin by
// key index (each channel's key 1, then key 2, ...), cloning an independent request
// template per contender from the channel's already prompt-applied template.
func interleaveGroupRaceContenders(candidates []groupRaceCandidate) []raceContender {
	if len(candidates) == 0 {
		return nil
	}
	maxLen := 0
	for _, c := range candidates {
		if len(c.keys) > maxLen {
			maxLen = len(c.keys)
		}
	}
	out := make([]raceContender, 0, len(candidates)*maxLen)
	for round := 0; round < maxLen; round++ {
		for _, c := range candidates {
			if round >= len(c.keys) {
				continue
			}
			template := cloneInternalRequestForRetry(c.template)
			if template == nil {
				continue
			}
			out = append(out, raceContender{
				channel:                c.channel,
				key:                    c.keys[round],
				internalRequest:        template,
				promptSnapshot:         c.promptSnapshot,
				runtimeModel:           c.runtimeModel,
				capabilityKey:          c.capabilityKey,
				promptOverridesApplied: true,
			})
		}
	}
	return out
}
