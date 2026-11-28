package errorjudge

import (
	"net/http"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

func TestContentPolicyTerminalNoRescueNoBreaker(t *testing.T) {
	d := Judge(Evidence{
		ErrorText:        "failed to transform stream event: responses stream failed: This content was flagged, code: cyber_policy, type: invalid_request",
		Code:             "cyber_policy",
		Type:             "invalid_request",
		StreamError:      true,
		UpstreamAnswered: true,
	}, nil)
	if d.Category != CategoryContentPolicy || d.Retry != RetryNone || d.Rescue || d.CountFailure || d.KeyEffect != EffectNone || d.ChannelEffect != EffectNone {
		t.Fatalf("content policy decision = %+v", d)
	}
}

func TestBareSafetyTypeIsTerminal(t *testing.T) {
	d := Judge(Evidence{StatusCode: http.StatusBadRequest, Type: "safety", Message: "blocked"}, nil)
	if d.Category != CategoryContentPolicy || d.Rescue || d.CountFailure {
		t.Fatalf("safety decision = %+v", d)
	}
}

func TestStrongAuthDisablesKeyImmediatelyAndDoesNotRescue(t *testing.T) {
	d := Judge(Evidence{
		StatusCode: http.StatusUnauthorized,
		Message:    "User account is not active",
	}, nil)
	if d.Category != CategoryStrongAuth || d.KeyEffect != EffectDisableKey || d.Rescue || d.CountFailure {
		t.Fatalf("strong 401 decision = %+v", d)
	}
	if d.Retry != RetryNextKey {
		t.Fatalf("strong 401 may fail over to another key in the same pass, got %+v", d)
	}
}

func TestAmbiguousAuthStillRescues(t *testing.T) {
	d := Judge(Evidence{StatusCode: http.StatusUnauthorized, Message: "authentication failed"}, nil)
	if d.Category != CategoryAuth || !d.Rescue || d.KeyEffect != EffectCooldownKey || !d.CountFailure {
		t.Fatalf("ambiguous 401 decision = %+v", d)
	}
}

func TestCircuitOpenFastFailsWithoutRescue(t *testing.T) {
	d := Judge(Evidence{LocalRoute: true, CircuitOpen: true, ErrorText: "octopus_channel_circuit_open"}, nil)
	if d.Category != CategoryLocalUnavailable || d.Rescue || d.Retry != RetryNone || d.CountFailure {
		t.Fatalf("circuit decision = %+v", d)
	}
}

func TestCircuitRescueWaitSwitchKeepsDefaultFastFailAndOptInRescue(t *testing.T) {
	ev := Evidence{LocalRoute: true, CircuitOpen: true, RetryAfter: 3 * time.Second, ErrorText: "octopus_channel_circuit_open"}
	fast := Judge(ev, nil)
	if fast.Rescue || fast.RuleID != "local.fast_fail" || fast.Cooldown != 3*time.Second {
		t.Fatalf("default circuit decision = %+v", fast)
	}
	ev.AllowCircuitRescue = true
	wait := Judge(ev, nil)
	if !wait.Rescue || wait.RuleID != "local.wait_for_recovery" || wait.Cooldown != 3*time.Second {
		t.Fatalf("opt-in circuit decision = %+v", wait)
	}
}

func TestOperatorRuleOverridesAndReplaySafetyTightens(t *testing.T) {
	rules := []model.CircuitErrorRule{{
		Name: "retry-429", StatusCodes: []int{429}, Retry: "next_channel", Effect: "cooldown_channel", CooldownSeconds: 30,
	}}
	d := Judge(Evidence{StatusCode: 429, ErrorText: "rate limited", UpstreamAnswered: true}, rules)
	if d.Retry != RetryNone || d.Rescue || d.RuleID != "safety.replay_unknown" {
		t.Fatalf("replay safety must be last, got %+v", d)
	}
}

func TestRateLimitOrTransientCanRescue(t *testing.T) {
	d := Judge(Evidence{StatusCode: 429, ErrorText: "rate limited"}, nil)
	if !d.Rescue || !d.CountFailure || d.Retry != RetryNextChannel {
		t.Fatalf("429 decision = %+v", d)
	}
}
