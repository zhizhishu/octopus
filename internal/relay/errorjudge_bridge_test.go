package relay

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/zhizhishu/octopus/internal/relay/errorjudge"
)

func TestJudgeBridgeCyberPolicyIsTerminal(t *testing.T) {
	err := fmt.Errorf("failed to transform stream event: responses stream failed: This content was flagged, code: cyber_policy, type: invalid_request")
	d := judgeFailureEvidence(err, http.StatusBadGateway, true, false)
	if d.Category != errorjudge.CategoryContentPolicy || d.Rescue || d.CountFailure || d.Retry != errorjudge.RetryNone {
		t.Fatalf("cyber_policy decision = %+v", d)
	}
}

func TestJudgeBridgeStrongAuthDisablesKey(t *testing.T) {
	err := newUpstreamError(http.StatusUnauthorized, []byte(`{"error":{"code":"invalid_api_key","message":"User account is not active"}}`))
	d := judgeFailureEvidence(err, http.StatusUnauthorized, true, false)
	if d.Category != errorjudge.CategoryStrongAuth || d.KeyEffect != errorjudge.EffectDisableKey || !d.Rescue {
		t.Fatalf("strong auth decision = %+v", d)
	}
}

func TestJudgeBridgeCircuitOpenDoesNotRescue(t *testing.T) {
	err := routeSelectionErrorFromAttempts(nil)
	if !errors.Is(err, err) { // keep errors imported in this package's test build obvious
		t.Fatal(err)
	}
	d := judgeFailureEvidence(err, http.StatusServiceUnavailable, false, false)
	if d.Category != errorjudge.CategoryLocalUnavailable || d.Rescue || d.Retry != errorjudge.RetryNone {
		t.Fatalf("no-candidate decision = %+v", d)
	}
}

func TestJudgeBridgeAmbiguous429StillRescues(t *testing.T) {
	err := newUpstreamError(http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limited, retry soon"}}`))
	d := judgeFailureEvidence(err, http.StatusTooManyRequests, true, false)
	if !d.Rescue || !d.CountFailure || d.Retry != errorjudge.RetryNextChannel {
		t.Fatalf("429 decision = %+v", d)
	}
}
