package balancer

import (
	"net/http"
	"sync"
	"testing"
)

func withRules(t *testing.T, raw string) {
	t.Helper()
	previous := circuitRulesSource
	circuitRulesSource = func() (string, error) { return raw, nil }
	circuitRulesCache = sync.Map{}
	t.Cleanup(func() {
		circuitRulesSource = previous
		circuitRulesCache = sync.Map{}
	})
}

func resetHoldState() {
	globalBreaker = sync.Map{}
	globalHolds = sync.Map{}
	ResetRuntimeTelemetry()
}

func TestErrorRuleModelScopeHoldsOnlyThatModelOnThatKey(t *testing.T) {
	resetHoldState()
	withRules(t, `[{"name":"not-found","status_codes":[404],"keywords":["model_not_found"],"scope":"model","cooldown_seconds":600}]`)

	if got := ApplyErrorRules(51, 1, "gpt-x", http.StatusNotFound, `{"error":{"code":"model_not_found"}}`); got != "not-found" {
		t.Fatalf("matched rule = %q, want not-found", got)
	}
	tripped, remaining := IsTrippedScoped(51, 1, "gpt-x", "chat", "")
	if !tripped || remaining <= 0 {
		t.Fatalf("model hold must block every endpoint scope of that model, got %v %v", tripped, remaining)
	}
	if tripped, _ := IsTripped(51, 1, "gpt-y"); tripped {
		t.Fatal("another model on the same key must stay routable")
	}
	if tripped, _ := IsTripped(51, 2, "gpt-x"); tripped {
		t.Fatal("the same model on another key must stay routable")
	}
	if got := ApplyErrorRules(51, 1, "gpt-x", http.StatusTooManyRequests, "model_not_found"); got != "" {
		t.Fatalf("status outside the rule must not match, got %q", got)
	}
	views := ListChannelCircuits(51)
	if len(views) != 1 || views[0].State != "held" || views[0].Reason != "not-found" || views[0].Model != "gpt-x" {
		t.Fatalf("admin list = %+v", views)
	}
}

func TestErrorRuleKeyAndChannelScopes(t *testing.T) {
	resetHoldState()
	withRules(t, `[
		{"name":"quota","keywords":["insufficient_quota"],"scope":"key","cooldown_seconds":1800},
		{"name":"suspended","keywords":["account suspended"],"scope":"channel","cooldown_seconds":3600}
	]`)

	ApplyErrorRules(52, 3, "m1", http.StatusTooManyRequests, "insufficient_quota")
	if tripped, _ := IsTripped(52, 3, "m2"); !tripped {
		t.Fatal("key hold must cover every model on the key")
	}
	if tripped, _ := IsTripped(52, 4, "m1"); tripped {
		t.Fatal("key hold must not cover sibling keys")
	}

	ApplyErrorRules(52, 4, "m1", http.StatusForbidden, "Account Suspended by provider")
	if tripped, _ := IsTripped(52, 9, "m9"); !tripped {
		t.Fatal("channel hold must cover every key and model")
	}

	// A key reset keeps the wider channel hold; a channel reset clears everything.
	ResetScoped(52, 3, "")
	if tripped, _ := IsTripped(52, 3, "m2"); !tripped {
		t.Fatal("channel hold must survive a single-key reset")
	}
	ResetChannel(52)
	if tripped, _ := IsTripped(52, 3, "m2"); tripped {
		t.Fatal("channel reset must clear every hold")
	}
	if got := len(ListChannelCircuits(52)); got != 0 {
		t.Fatalf("entries after channel reset = %d", got)
	}
}

func TestHoldOnlyExtendsAndInvalidRulesAreIgnored(t *testing.T) {
	resetHoldState()
	ApplyHold(53, 1, "m", "model", 600e9, "long")
	ApplyHold(53, 1, "m", "model", 5e9, "short")
	if remaining, rule := holdRemaining(53, 1, "m"); remaining < 500e9 || rule != "long" {
		t.Fatalf("a shorter hold must not shorten a longer one: %v %q", remaining, rule)
	}

	withRules(t, `[{"scope":"nope"}]`)
	if got := ApplyErrorRules(53, 1, "m", 500, "anything"); got != "" {
		t.Fatalf("invalid rules must be ignored, matched %q", got)
	}
}
