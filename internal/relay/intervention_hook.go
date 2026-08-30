package relay

import (
	"context"
	"errors"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/bestruirui/octopus/internal/utils/log"
)

// shouldHoldForOperator evaluates whether a request whose automatic attempts/fallbacks failed
// is eligible to be held for manual operator intervention.
// Conditions:
// 1. Intervention feature is enabled.
// 2. Stream is preferred/requested (non-stream clients cannot safely receive continuous keepalive heartbeats).
// 3. Haven't exceeded max intervention rounds.
// 4. No meaningful business data has been written to client (SSE heartbeat/blank-line alone is fine, but content commits the stream).
// 5. Error is not client cancellation/abort (e.g. context canceled / client disconnected).
// 6. Error is not context window exceeded (deterministic oversized input).
// 7. Error is not parsing/request invalid (deterministic invalid request body/parameters).
// 8. Error is not unsupported endpoint/model.
func shouldHoldForOperator(
	req *relayRequest,
	interventionRounds int,
	contextWindowErr error,
	finalErr error,
) bool {
	if !intervention.Enabled() {
		return false
	}
	if req == nil || req.internalRequest == nil {
		return false
	}
	if !internalRequestPrefersStream(req.internalRequest) {
		return false
	}
	if interventionRounds >= maxRelayInterventionRounds {
		return false
	}
	if req.wroteBusinessData {
		return false
	}
	if contextWindowErr != nil || isContextWindowError(finalErr) {
		return false
	}
	if isClientAbortError(finalErr) {
		return false
	}
	if isRequestInvalidUpstreamError(finalErr) {
		return false
	}
	var upErr *upstreamError
	if errors.As(finalErr, &upErr) && upErr != nil {
		if isOpenAIResponsesEndpointUnsupportedError(upErr.StatusCode(), upErr.Body()) {
			return false
		}
	}
	return true
}

// holdForOperator registers a failed request into the intervention registry and blocks
// waiting for an operator decision, timeout, or context cancellation.
func holdForOperator(ctx context.Context, endpoint string, requestModel string, attempts []model.ChannelAttempt, lastErr error) (intervention.Resolution, bool) {
	if !intervention.Enabled() {
		return intervention.Resolution{}, false
	}

	lastErrStr := ""
	if lastErr != nil {
		lastErrStr = lastErr.Error()
	}

	pending := &intervention.Pending{
		RequestModel: requestModel,
		Endpoint:     endpoint,
		Attempts:     attempts,
		LastError:    lastErrStr,
	}

	id, err := intervention.Register(pending)
	if err != nil {
		log.Infof("Intervention registration skipped endpoint=%s model=%s err=%v", endpoint, requestModel, err)
		return intervention.Resolution{}, false
	}
	log.Infof("Intervention held request id=%s endpoint=%s model=%s", id, endpoint, requestModel)

	resolution, err := intervention.Wait(ctx, id, intervention.Timeout())
	if err != nil {
		log.Infof("Intervention finished id=%s err=%v", id, err)
		return intervention.Resolution{}, false
	}

	if resolution.Action == intervention.ActionAbort {
		return intervention.Resolution{}, false
	}

	return resolution, true
}

// singleChannelGroup packages an operator-selected channel and model into a single-item Group.
func singleChannelGroup(resolution intervention.Resolution, requestModel string) model.Group {
	modelName := resolution.ModelName
	if modelName == "" {
		modelName = requestModel
	}

	return model.Group{
		Name: modelName,
		Mode: 0,
		Items: []model.GroupItem{
			{
				ChannelID:     resolution.ChannelID,
				ModelName:     modelName,
				Priority:      1,
				Weight:        1,
				RoutingWeight: 1,
			},
		},
	}
}
