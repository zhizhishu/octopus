package relay

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/relay/errorjudge"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// judgeFailureEvidence is the relay-side adapter into the unified failure
// referee. The ordering (built-in classification -> operator rules -> replay
// safety) is borrowed from gpt-load internal/health/execution_judge.go.
func judgeFailureEvidence(err error, statusCode int, upstreamAnswered bool, downstreamCommitted bool) errorjudge.Decision {
	return errorjudge.Judge(failureEvidenceFromError(err, statusCode, upstreamAnswered, downstreamCommitted), balancer.CurrentCircuitRules())
}

func failureEvidenceFromError(err error, statusCode int, upstreamAnswered bool, downstreamCommitted bool) errorjudge.Evidence {
	ev := errorjudge.Evidence{
		StatusCode:              statusCode,
		ErrorText:               errSafeMessage(err),
		UpstreamAnswered:        upstreamAnswered,
		DownstreamCommitted:     downstreamCommitted,
		RetryAfter:              upstreamRetryAfter(err),
		Now:                     time.Now(),
		ContentPolicyTerms:      configuredContentPolicyTerms(),
		ExplicitClientRejection: isContextWindowError(err) || isRequestInvalidUpstreamError(err) || isDeterministicClientRejection(err),
		TransientCapacity:       isTransientCapacityUpstreamError(err),
		NonRescuable:            isClientAbortError(err),
		AllowCircuitRescue:      circuitRescueWaitEnabled(),
		// 「首字超时后立即换渠道」(relay_first_token_timeout_switch_channel, 默认开).
		// 关掉时这里保持 false，首字超时回到改动前的通用判定（fallback.ambiguous → 不重试本渠道
		// 剩余 Key），候选循环/救援循环照旧跑，也就是「维持现状」。
		FirstTokenTimeout: isFirstTokenTimeoutError(err) && firstTokenTimeoutSwitchChannelEnabled(),
	}
	if ev.RetryAfter == 0 {
		ev.RetryAfter, _ = retryAfterFromError(err)
	}
	var upErr *upstreamError
	if errors.As(err, &upErr) && upErr != nil {
		ev.StatusCode = upErr.StatusCode()
		body := upErr.Body()
		ev.Code = normalizeErrorCode(extractUpstreamErrorCode([]byte(body)))
		ev.Message = upstreamBodySummary(body)
		if ev.ErrorText == "" {
			ev.ErrorText = upErr.DetailedMessage()
		}
		// A clear HTTP error response is evidence about the request/credential; it
		// is not the ambiguous "stream started then execution outcome is unknown"
		// case. Replay tightening below therefore keys on stream/committed state,
		// not merely on having received a status line.
		ev.UpstreamAnswered = upstreamAnswered && ev.StatusCode == 0
		if isOpenAIResponsesEndpointUnsupportedError(upErr.StatusCode(), upErr.Body()) || isExhaustedQuotaWithoutRetryWindow(upErr) {
			ev.NonRescuable = true
		}
	}
	if strings.Contains(ev.ErrorText, "responses stream failed:") || strings.Contains(ev.ErrorText, "stream_transform_error") {
		ev.StreamError = true
		if ev.StatusCode == 0 || ev.StatusCode >= 500 {
			ev.StatusCode = 0
		}
		ev.Code = firstNonEmpty(extractStreamErrorField(ev.ErrorText, "code"), ev.Code)
		ev.Type = extractStreamErrorField(ev.ErrorText, "type")
		// An upstream error event is an explicit rejection, not proof that generation
		// started; replay safety still comes from meaningful downstream commitment.
	}
	var localErr *localRelayError
	if errors.As(err, &localErr) && localErr != nil {
		ev.ErrorText = firstNonEmpty(ev.ErrorText, localErr.Error())
		switch localErr.code {
		case "octopus_channel_circuit_open":
			ev.LocalRoute = true
			ev.CircuitOpen = true
		case "octopus_no_available_channel":
			ev.LocalRoute = true
			ev.NoCandidates = true
		}
		if localErr.retryAfter > ev.RetryAfter {
			ev.RetryAfter = localErr.retryAfter
		}
	}
	lower := strings.ToLower(ev.ErrorText)
	if strings.Contains(lower, "circuit_break") || strings.Contains(lower, "circuit open") || strings.Contains(lower, "no available channel") {
		ev.LocalRoute = true
	}
	return ev
}

func circuitRescueWaitEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyRelayCircuitRescueWait)
	return err == nil && enabled
}

// firstTokenTimeoutSwitchChannelEnabled reports the operator switch for 「首字超时后
// 立即换渠道」. Default ON: an unreadable/missing setting must keep the new behaviour
// (switch candidates on a pre-commit first-token timeout) instead of silently
// reverting to the old one on a fresh/partially-migrated deployment.
func firstTokenTimeoutSwitchChannelEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyRelayFirstTokenTimeoutSwitchChannel)
	if err != nil {
		return true
	}
	return enabled
}

func configuredContentPolicyTerms() []string {
	raw, err := op.SettingGetString(model.SettingKeyRelayContentPolicyKeywords)
	if err != nil {
		return nil
	}
	parts := strings.Split(raw, ",")
	terms := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			terms = append(terms, part)
		}
	}
	return terms
}

func extractStreamErrorField(text, field string) string {
	lower := strings.ToLower(text)
	needle := strings.ToLower(field) + ": "
	idx := strings.Index(lower, needle)
	if idx < 0 {
		return ""
	}
	value := text[idx+len(needle):]
	if cut := strings.IndexAny(value, ",;\n"); cut >= 0 {
		value = value[:cut]
	}
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// applyErrorDecisionEffects performs the runtime side effects selected by the
// unified judge. Cooldowns reuse the existing breaker hold machinery; hard key /
// channel disable follows axonhub channel_auto_disable.go's immediate-cache-update
// shape instead of waiting for the next operator action.
func applyErrorDecisionEffects(decision errorjudge.Decision, channelID, keyID int, modelName, endpoint, capability string, key model.ChannelKey) {
	customRule := len(decision.RuleID) >= len("custom.") && decision.RuleID[:len("custom.")] == "custom."
	if customRule {
		log.Infof("error judge rule hit channel=%d key=%d rule=%s retry=%s key_effect=%s channel_effect=%s", channelID, keyID, decision.RuleID, decision.Retry, decision.KeyEffect, decision.ChannelEffect)
	}
	// Built-in cooldown effects are enforced by the existing key quarantine /
	// runtime telemetry paths. Only operator rules create holds here; otherwise a
	// built-in 429 would double-cool the key and, because hold keys are channel-ID
	// scoped process memory, poison later unrelated traffic in tests and runtime.
	if customRule && decision.Cooldown > 0 {
		scope := ""
		switch {
		case decision.KeyEffect == errorjudge.EffectCooldownKey:
			scope = model.CircuitRuleScopeKey
		case decision.ChannelEffect == errorjudge.EffectCooldownModel:
			scope = model.CircuitRuleScopeModel
		case decision.ChannelEffect == errorjudge.EffectCooldownChannel:
			scope = model.CircuitRuleScopeChannel
		}
		if scope != "" {
			balancer.ApplyHoldScoped(channelID, keyID, modelName, scope, decision.Cooldown, decision.RuleID, endpoint, capability)
		}
	}
	if decision.KeyEffect == errorjudge.EffectDisableKey {
		if err := op.ChannelKeyHardDisable(key, decision.Reason, time.Now().Unix()); err != nil {
			log.Warnf("error judge hard-disable key channel=%d key=%d failed: %v", channelID, keyID, err)
		} else {
			log.Warnf("error judge hard-disabled key channel=%d key=%d rule=%s reason=%s", channelID, keyID, decision.RuleID, decision.Reason)
		}
	}
	if decision.ChannelEffect == errorjudge.EffectDisableChannel {
		if err := op.ChannelEnabled(channelID, false, context.Background()); err != nil {
			log.Warnf("error judge disable channel=%d failed: %v", channelID, err)
		} else {
			log.Warnf("error judge disabled channel=%d rule=%s reason=%s", channelID, decision.RuleID, decision.Reason)
		}
	}
}
