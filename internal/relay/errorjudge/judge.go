// Package errorjudge is the single failure referee for relay attempts.
//
// Shape borrowed from gpt-load internal/health/decision.go:21-80 and
// internal/health/execution_judge.go:299-360,363-490: one Decision carries the
// retry direction separately from the runtime effect, and user rules are applied
// after built-in classification but before replay-safety tightening.
package errorjudge

import (
	"net/http"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

type RetryDirection string

const (
	RetryNone        RetryDirection = "none"
	RetrySameKey     RetryDirection = "same_key"
	RetryNextKey     RetryDirection = "next_key"
	RetryNextChannel RetryDirection = "next_channel"
)

type Effect string

const (
	EffectNone            Effect = "none"
	EffectCooldownKey     Effect = "cooldown_key"
	EffectCooldownModel   Effect = "cooldown_model"
	EffectCooldownChannel Effect = "cooldown_channel"
	EffectRecordFailure   Effect = "record_failure"
	EffectDisableKey      Effect = "disable_key"
	EffectDisableChannel  Effect = "disable_channel"
)

type Category string

const (
	CategoryNone             Category = "none"
	CategoryContentPolicy    Category = "content_policy"
	CategoryStrongAuth       Category = "strong_auth_failure"
	CategoryAuth             Category = "auth_failure"
	CategoryRateLimited      Category = "rate_limited"
	CategoryClientError      Category = "client_error"
	CategoryUpstreamError    Category = "upstream_error"
	CategoryLocalUnavailable Category = "local_unavailable"
	CategoryAmbiguous        Category = "ambiguous"
)

type Evidence struct {
	StatusCode              int
	Type                    string
	Code                    string
	Message                 string
	ErrorText               string
	StreamError             bool
	LocalRoute              bool
	CircuitOpen             bool
	NoCandidates            bool
	UpstreamAnswered        bool
	DownstreamCommitted     bool
	RetryAfter              time.Duration
	ExplicitClientRejection bool
	TransientCapacity       bool
	NonRescuable            bool
	AllowCircuitRescue      bool
	Now                     time.Time
	ContentPolicyTerms      []string
}

type Decision struct {
	Category      Category
	Retry         RetryDirection
	KeyEffect     Effect
	ChannelEffect Effect
	Rescue        bool
	CountFailure  bool
	Cooldown      time.Duration
	RuleID        string
	Reason        string
}

func (d Decision) ShouldRetry() bool { return d.Retry != "" && d.Retry != RetryNone }

// Terminal request-fault codes borrowed from CLIProxyAPI
// internal/clienterror/client_error.go requestFaultCodes/requestFaultTypes.
// These are payload faults: resending the same bytes to another credential or
// channel can only fail the same way, so they must neither rescue nor poison
// channel health.
var defaultContentPolicyTerms = []string{
	"cyber_policy",
	"cyber_policy_violation",
	"bio_policy",
	"misalignment_policy_violation",
	"content_policy_violation",
	"image_content_policy_violation",
	"content_policy",
	"safety_policy",
	"context_length_exceeded",
	"message_too_big",
	"string_above_max_length",
	"invalid_prompt",
	"invalid_value",
	"unsupported_value",
	"invalid_request_error",
	"previous_response_not_found",
}

func baseDecision(ev Evidence) Decision {
	now := ev.Now
	if now.IsZero() {
		now = time.Now()
	}
	_ = now
	text := strings.ToLower(strings.Join([]string{ev.Type, ev.Code, ev.Message, ev.ErrorText}, " "))
	code := strings.ToLower(strings.TrimSpace(ev.Code))
	typ := strings.ToLower(strings.TrimSpace(ev.Type))

	if ev.NonRescuable {
		return Decision{Category: CategoryClientError, Retry: RetryNone, KeyEffect: EffectNone, ChannelEffect: EffectNone, Rescue: false, CountFailure: false, RuleID: "request.non_rescuable", Reason: "explicitly non-rescuable failure"}
	}

	if ev.LocalRoute || ev.CircuitOpen || ev.NoCandidates {
		if ev.AllowCircuitRescue && ev.RetryAfter > 0 {
			return Decision{Category: CategoryLocalUnavailable, Retry: RetryNone, KeyEffect: EffectNone, ChannelEffect: EffectNone, Rescue: true, CountFailure: false, Cooldown: ev.RetryAfter, RuleID: "local.wait_for_recovery", Reason: "local route/circuit state may recover inside the rescue window"}
		}
		return Decision{Category: CategoryLocalUnavailable, Retry: RetryNone, KeyEffect: EffectNone, ChannelEffect: EffectNone, Rescue: false, CountFailure: false, Cooldown: ev.RetryAfter, RuleID: "local.fast_fail", Reason: "local route/circuit state cannot be fixed by immediate rescue"}
	}

	terms := append([]string(nil), defaultContentPolicyTerms...)
	for _, term := range ev.ContentPolicyTerms {
		if term = strings.ToLower(strings.TrimSpace(term)); term != "" {
			terms = append(terms, term)
		}
	}
	for _, term := range terms {
		if term == "" {
			continue
		}
		if code == term || typ == term || (term != "safety" && strings.Contains(text, term)) {
			return Decision{Category: CategoryContentPolicy, Retry: RetryNone, KeyEffect: EffectNone, ChannelEffect: EffectNone, Rescue: false, CountFailure: false, RuleID: "request.content_policy", Reason: "content-policy rejection is terminal for this payload"}
		}
	}

	// Explicit request rejections borrowed from gpt-load
	// internal/execution/request_rejection.go:5-15, with cyber_policy added above.
	if ev.ExplicitClientRejection || containsAny(text, "context_length_exceeded", "invalid_parameter", "prompt is too long") {
		return Decision{Category: CategoryClientError, Retry: RetryNone, KeyEffect: EffectNone, ChannelEffect: EffectNone, Rescue: false, CountFailure: false, RuleID: "request.explicit_rejection", Reason: "deterministic request rejection"}
	}

	if ev.TransientCapacity && (ev.StatusCode == http.StatusBadRequest || ev.StatusCode == http.StatusUnprocessableEntity) {
		return Decision{Category: CategoryRateLimited, Retry: RetryNextChannel, KeyEffect: EffectCooldownKey, ChannelEffect: EffectRecordFailure, Rescue: true, CountFailure: true, Cooldown: maxDuration(5*time.Second, ev.RetryAfter), RuleID: "upstream.transient_capacity", Reason: "400/422 body carries generic transient-capacity evidence"}
	}

	if ev.StatusCode == http.StatusUnauthorized {
		if containsAny(text,
			"user account is not active",
			"account is not active",
			"account suspended",
			"account deactivated",
			"account disabled",
			"key revoked",
			"api key revoked",
			"invalid_api_key",
		) {
			return Decision{Category: CategoryStrongAuth, Retry: RetryNextKey, KeyEffect: EffectDisableKey, ChannelEffect: EffectNone, Rescue: true, CountFailure: false, RuleID: "auth.strong_disable_key", Reason: "upstream says the credential/account is dead"}
		}
		return Decision{Category: CategoryAuth, Retry: RetryNextKey, KeyEffect: EffectCooldownKey, ChannelEffect: EffectRecordFailure, Rescue: true, CountFailure: true, Cooldown: 15 * time.Minute, RuleID: "auth.ambiguous_credential", Reason: "ambiguous 401 may be a flaky gateway"}
	}

	if ev.StatusCode == http.StatusTooManyRequests || containsAny(text, "rate_limit", "rate limit", "too_many_requests", "throttl") {
		return Decision{Category: CategoryRateLimited, Retry: RetryNextChannel, KeyEffect: EffectCooldownKey, ChannelEffect: EffectRecordFailure, Rescue: true, CountFailure: true, Cooldown: maxDuration(5*time.Second, ev.RetryAfter), RuleID: "upstream.rate_limited", Reason: "temporary capacity/rate-limit rejection"}
	}

	if ev.StatusCode >= 400 && ev.StatusCode < 500 && ev.StatusCode != 0 {
		return Decision{Category: CategoryClientError, Retry: RetryNone, KeyEffect: EffectNone, ChannelEffect: EffectNone, Rescue: false, CountFailure: false, RuleID: "upstream.client_error", Reason: "deterministic 4xx"}
	}

	if ev.StatusCode >= 500 || ev.StreamError || ev.StatusCode == 0 {
		return Decision{Category: CategoryUpstreamError, Retry: RetryNextChannel, KeyEffect: EffectNone, ChannelEffect: EffectRecordFailure, Rescue: true, CountFailure: true, Cooldown: ev.RetryAfter, RuleID: "upstream.ambiguous_failure", Reason: "transient or ambiguous upstream failure"}
	}

	return Decision{Category: CategoryAmbiguous, Retry: RetryNone, KeyEffect: EffectNone, ChannelEffect: EffectNone, Rescue: false, CountFailure: false, RuleID: "fallback.ambiguous", Reason: "unclassified failure"}
}

// Judge applies built-in classification, then operator rules, then replay safety.
func Judge(ev Evidence, rules []model.CircuitErrorRule) Decision {
	d := baseDecision(ev)
	for i, rule := range rules {
		if !rule.Match(ev.StatusCode, strings.ToLower(strings.Join([]string{ev.Type, ev.Code, ev.Message, ev.ErrorText}, " "))) {
			continue
		}
		if retry := NormalizeRetry(rule.Retry); retry != "" {
			d.Retry = retry
		}
		if effect := NormalizeEffect(rule.Effect); effect != "" {
			applyEffect(&d, effect)
		} else if rule.Scope != "" {
			// Legacy rules only meant "hold this scope".
			switch rule.Scope {
			case model.CircuitRuleScopeKey:
				applyEffect(&d, EffectCooldownKey)
			case model.CircuitRuleScopeModel:
				applyEffect(&d, EffectCooldownModel)
			case model.CircuitRuleScopeChannel:
				applyEffect(&d, EffectCooldownChannel)
			}
		}
		if rule.CooldownSeconds > 0 {
			d.Cooldown = time.Duration(rule.CooldownSeconds) * time.Second
		}
		d.RuleID = "custom." + rule.Name
		if strings.TrimSpace(d.RuleID) == "custom." {
			d.RuleID = "custom.rule"
		}
		d.CountFailure = d.KeyEffect == EffectRecordFailure || d.ChannelEffect == EffectRecordFailure || d.CountFailure
		if d.Retry == RetryNone {
			d.Rescue = false
		}
		_ = i
		break
	}
	// Replay safety is always last. Once upstream has started answering, or once
	// meaningful bytes reached the downstream, a retry would risk duplicate execution
	// or duplicate visible output. Terminal deterministic decisions are unaffected
	// because they already carry RetryNone.
	if (ev.UpstreamAnswered || ev.DownstreamCommitted) && d.Retry != RetryNone {
		d.Retry = RetryNone
		d.Rescue = false
		d.RuleID = "safety.replay_unknown"
		d.Reason = "upstream/downstream already started; replay is not safe"
	}
	return d
}

func applyEffect(d *Decision, effect Effect) {
	switch effect {
	case EffectCooldownKey, EffectDisableKey:
		d.KeyEffect = effect
	case EffectCooldownModel, EffectCooldownChannel, EffectDisableChannel, EffectRecordFailure:
		d.ChannelEffect = effect
		if effect == EffectRecordFailure {
			d.CountFailure = true
		}
	}
}

func NormalizeRetry(value string) RetryDirection {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "no_retry", "stop":
		return RetryNone
	case "same_key", "same_credential":
		return RetrySameKey
	case "next_key", "refresh_credential", "next_credential":
		return RetryNextKey
	case "next_channel", "next_candidate", "retry":
		return RetryNextChannel
	}
	return ""
}

func NormalizeEffect(value string) Effect {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none":
		return EffectNone
	case "cooldown_key", "cooldown_credential":
		return EffectCooldownKey
	case "cooldown_model":
		return EffectCooldownModel
	case "cooldown_channel", "skip_group":
		return EffectCooldownChannel
	case "record_failure", "record_credential_failure":
		return EffectRecordFailure
	case "disable_key", "disable_credential":
		return EffectDisableKey
	case "disable_channel":
		return EffectDisableChannel
	}
	return ""
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
