package relay

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay/intervention"
)

// isEligibleForInterventionRescue evaluates whether a final error is eligible for machine-first
// automatic rescue / manual intervention (e.g. transient 429 / 5xx / network hiccups).
// Deterministic 4xx (except 429 and 401), context-window overflow, malformed request, and client
// abort are not eligible; 401 is rescued because flaky gateways throw spurious ones during bad
// windows (see the StatusUnauthorized branch below).
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
		// exclusions already applied above). Every other 4xx (403/404/405/409/413/415
		// ...) is a deterministic client error and fast-fails.
		sc := upErr.StatusCode()
		if sc == http.StatusTooManyRequests {
			return true
		}
		// 401 也救（用户定 2026-10-27）。
		//
		// 生产实证：聚合型上游抽风时会把 401 混在一串 500 里甩出来（key 其实是活的，
		// 下一分钟同一个 key 又能跑通）。把这类 401 直接透传给客户端，客户端只会把
		// 它读成"你的密钥无效"——坏的是上游，挨骂的是用户自己的 key。
		//
		// 救的代价有界：401 的那把 key 已经被 ChannelKeyAuthErrorCooldown 关 15 分钟，
		// 救援轮次只会打到**其它** key/渠道；没有替代时走"不全黑"节流探针（1 次/秒），
		// 401 是秒回的，不烧 token。真死的 key 最坏情况是客户端在救援预算内多等一会儿，
		// 之后照常拿到错误——这笔等待正是用户愿意付的。
		// 403 仍然不救：权限拒绝（区域封锁/模型无权）比 401 更像真确定性，先不动。
		if sc == http.StatusUnauthorized {
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
