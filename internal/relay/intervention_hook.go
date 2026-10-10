package relay

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

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
		// 429 stays transient (rate limit). 400/422 are only rescueable when the body
		// carries generic transient-capacity evidence (isTransientCapacityUpstreamError,
		// which itself loses to the deterministic request-invalid / context-window
		// exclusions already applied above). Every other 4xx (401/403/404/405/409/413/415
		// ...) is a deterministic client error and fast-fails.
		sc := upErr.StatusCode()
		if sc == http.StatusTooManyRequests {
			return true
		}
		if sc == http.StatusBadRequest || sc == http.StatusUnprocessableEntity {
			return isTransientCapacityUpstreamError(err)
		}
		if sc >= 400 && sc < 500 {
			return false
		}
		return true
	}
	// Network errors, timeouts, transient stream endings, etc.
	return true
}

// isRescueableHeldRequest evaluates whether a request whose automatic attempts/fallbacks
// failed may enter the automatic-recovery hold: an eligible (transient) upstream failure
// with nothing committed downstream yet. The client's stream preference is deliberately
// NOT part of this test — a non-stream caller's request is every bit as rescuable as a
// streaming one (it just gets no SSE heartbeats while it waits), so gating on it silently
// turned off the whole rescue chain for plain HTTP clients.
func isRescueableHeldRequest(req *relayRequest, fatalClientErr, finalErr error) bool {
	if req == nil || req.internalRequest == nil {
		return false
	}
	if req.wroteBusinessData {
		return false
	}
	if fatalClientErr != nil {
		return false
	}
	return isEligibleForInterventionRescue(finalErr)
}

func shouldHoldForOperator(req *relayRequest, fatalClientErr, finalErr error) bool {
	return intervention.Enabled() && isRescueableHeldRequest(req, fatalClientErr, finalErr)
}

// waitRescueRound paces one automatic-rescue round. A request that is also registered for
// an operator decision waits through intervention.WaitRound so a human click preempts the
// sleep; an unregistered automatic rescue has no registry entry to watch, so it just waits
// out the backoff under the rescue deadline context.
func waitRescueRound(ctx context.Context, id string, registered bool, backoff time.Duration) (intervention.Resolution, bool, error) {
	if registered {
		return intervention.WaitRound(ctx, id, backoff)
	}
	if backoff <= 0 {
		return intervention.Resolution{}, false, nil
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return intervention.Resolution{}, false, ctx.Err()
	case <-timer.C:
		return intervention.Resolution{}, false, nil
	}
}

// relayStop reports client abort vs rescue/operator abort. parent is the original
// client context; current may already be the rescue deadline context. rescueFired marks
// whether a rescue context cancel came from the automatic-recovery deadline.
func relayStop(parent, current, rescueCtx context.Context, rescueFired *atomic.Bool) (clientGone bool, stopErr error) {
	if parent != nil && parent.Err() != nil {
		return true, parent.Err()
	}
	if current != nil && current.Err() != nil {
		return false, rescueStopError(rescueCtx, parent, rescueFired)
	}
	return false, nil
}

// rescueStopError distinguishes the automatic-recovery deadline from every other cancel
// (operator abort, client lifetime). The deadline is now a releasable time.AfterFunc (not
// a context deadline), so its fire flag — not DeadlineExceeded — decides
// "rescue_timeout" vs "rescue_stopped". Semantics are unchanged.
func rescueStopError(rescueCtx, clientCtx context.Context, rescueFired *atomic.Bool) error {
	if rescueCtx == nil || rescueCtx.Err() == nil || clientCtx.Err() != nil {
		return nil
	}
	if rescueFired != nil && rescueFired.Load() {
		return &localRelayError{status: http.StatusGatewayTimeout, code: "octopus_rescue_timeout", strategy: "rescue_timeout", message: "automatic rescue timed out"}
	}
	return &localRelayError{status: http.StatusServiceUnavailable, code: "octopus_rescue_stopped", strategy: "rescue_stopped", message: "automatic rescue stopped"}
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
