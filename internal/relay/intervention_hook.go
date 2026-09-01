package relay

import (
	"errors"
	"net/http"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay/intervention"
)

// isEligibleForInterventionRescue evaluates whether a final error is eligible for machine-first
// automatic rescue / manual intervention (e.g. transient 429 / 5xx / network hiccups).
// Deterministic 4xx (except 429), context-window overflow, malformed request, and client abort are not eligible.
func isEligibleForInterventionRescue(err error) bool {
	if err == nil {
		return false
	}
	if isClientAbortError(err) {
		return false
	}
	if isContextWindowError(err) {
		return false
	}
	if isRequestInvalidUpstreamError(err) {
		return false
	}
	var upErr *upstreamError
	if errors.As(err, &upErr) && upErr != nil {
		if isOpenAIResponsesEndpointUnsupportedError(upErr.StatusCode(), upErr.Body()) {
			return false
		}
		// Deterministic client 4xx status codes (except 429 rate limit) should not be rescued.
		sc := upErr.StatusCode()
		if sc >= 400 && sc < 500 && sc != http.StatusTooManyRequests {
			return false
		}
		return true
	}
	// Network errors, timeouts, transient stream endings, etc.
	return true
}

// shouldHoldForOperator evaluates whether a request whose automatic attempts/fallbacks failed
// is eligible to be held for automatic rescue and operator intervention.
func isRescueableHeldRequest(req *relayRequest, contextWindowErr, finalErr error) bool {
	if req == nil || req.internalRequest == nil {
		return false
	}
	if !internalRequestPrefersStream(req.internalRequest) {
		return false
	}
	if req.wroteBusinessData {
		return false
	}
	if contextWindowErr != nil {
		return false
	}
	return isEligibleForInterventionRescue(finalErr)
}

func shouldHoldForOperator(req *relayRequest, contextWindowErr, finalErr error) bool {
	return intervention.Enabled() && isRescueableHeldRequest(req, contextWindowErr, finalErr)
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
