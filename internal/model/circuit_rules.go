package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// SettingKeyCircuitBreakerErrorRules holds operator rules that turn a recognisable upstream
// failure (status code and/or a keyword in the error text) into an immediate hold on the
// right scope, instead of waiting for the consecutive-failure breaker. JSON array of
// CircuitErrorRule; "[]" (the default) changes nothing.
const SettingKeyCircuitBreakerErrorRules SettingKey = "circuit_breaker_error_rules"

const (
	CircuitRuleScopeModel   = "model"   // this model on this key
	CircuitRuleScopeKey     = "key"     // every model on this key
	CircuitRuleScopeChannel = "channel" // every key and model of the channel

	maxCircuitErrorRules           = 100
	maxCircuitRuleCooldownSeconds  = 86400
	maxCircuitRuleKeywords         = 20
	maxCircuitRuleKeywordLength    = 200
	maxCircuitRuleStatusCodesCount = 20
)

// CircuitErrorRule matches when the failure's status is in StatusCodes (empty = any status)
// AND the error text contains any of Keywords (empty = no text condition, case-insensitive).
// At least one of the two conditions must be set. The first matching rule wins.
type CircuitErrorRule struct {
	Name        string   `json:"name"`
	StatusCodes []int    `json:"status_codes,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	// Scope + CooldownSeconds are the legacy hold-only form. Retry/Effect extend
	// the same rule into the unified judge vocabulary, following gpt-load
	// internal/health/error_rules.go:18-24. Empty Retry/Effect preserve legacy behavior.
	Scope           string `json:"scope,omitempty"`
	CooldownSeconds int    `json:"cooldown_seconds,omitempty"`
	Retry           string `json:"retry,omitempty"`
	Effect          string `json:"effect,omitempty"`
	Disabled        bool   `json:"disabled,omitempty"`
}

// ParseCircuitErrorRules strictly validates and normalises the rule list; an empty or
// blank value is an empty list.
func ParseCircuitErrorRules(raw string) ([]CircuitErrorRule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var rules []CircuitErrorRule
	if err := decoder.Decode(&rules); err != nil {
		return nil, fmt.Errorf("circuit error rules must be a JSON array of rules: %w", err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("circuit error rules: trailing data after the JSON array")
	}
	if len(rules) > maxCircuitErrorRules {
		return nil, fmt.Errorf("circuit error rules: at most %d rules", maxCircuitErrorRules)
	}
	for i := range rules {
		rule := &rules[i]
		rule.Name = strings.TrimSpace(rule.Name)
		if rule.Name == "" {
			rule.Name = fmt.Sprintf("rule-%d", i+1)
		}
		if rule.Scope != "" {
			switch rule.Scope {
			case CircuitRuleScopeModel, CircuitRuleScopeKey, CircuitRuleScopeChannel:
			default:
				return nil, fmt.Errorf("circuit error rule %q: scope must be model, key or channel", rule.Name)
			}
		}
		rule.Retry = strings.ToLower(strings.TrimSpace(rule.Retry))
		rule.Effect = strings.ToLower(strings.TrimSpace(rule.Effect))
		if rule.Retry != "" && !validCircuitRuleRetry(rule.Retry) {
			return nil, fmt.Errorf("circuit error rule %q: unknown retry action %q", rule.Name, rule.Retry)
		}
		if rule.Effect != "" && !validCircuitRuleEffect(rule.Effect) {
			return nil, fmt.Errorf("circuit error rule %q: unknown effect %q", rule.Name, rule.Effect)
		}
		legacyHold := rule.Retry == "" && rule.Effect == ""
		effectNeedsCooldown := legacyHold || rule.Effect == "cooldown_key" || rule.Effect == "cooldown_credential" || rule.Effect == "cooldown_model" || rule.Effect == "cooldown_channel" || rule.Effect == "skip_group"
		if legacyHold && rule.Scope == "" {
			return nil, fmt.Errorf("circuit error rule %q: legacy hold rules need scope", rule.Name)
		}
		if effectNeedsCooldown && (rule.CooldownSeconds < 1 || rule.CooldownSeconds > maxCircuitRuleCooldownSeconds) {
			return nil, fmt.Errorf("circuit error rule %q: cooldown_seconds must be 1..%d", rule.Name, maxCircuitRuleCooldownSeconds)
		}
		if !effectNeedsCooldown && rule.CooldownSeconds < 0 {
			return nil, fmt.Errorf("circuit error rule %q: cooldown_seconds must be non-negative", rule.Name)
		}
		if len(rule.StatusCodes) > maxCircuitRuleStatusCodesCount {
			return nil, fmt.Errorf("circuit error rule %q: at most %d status codes", rule.Name, maxCircuitRuleStatusCodesCount)
		}
		for _, code := range rule.StatusCodes {
			if code < 100 || code > 599 {
				return nil, fmt.Errorf("circuit error rule %q: status code %d out of range", rule.Name, code)
			}
		}
		if len(rule.Keywords) > maxCircuitRuleKeywords {
			return nil, fmt.Errorf("circuit error rule %q: at most %d keywords", rule.Name, maxCircuitRuleKeywords)
		}
		keywords := make([]string, 0, len(rule.Keywords))
		for _, keyword := range rule.Keywords {
			keyword = strings.ToLower(strings.TrimSpace(keyword))
			if keyword == "" {
				continue
			}
			if len(keyword) > maxCircuitRuleKeywordLength {
				return nil, fmt.Errorf("circuit error rule %q: keyword longer than %d bytes", rule.Name, maxCircuitRuleKeywordLength)
			}
			keywords = append(keywords, keyword)
		}
		rule.Keywords = keywords
		if len(rule.StatusCodes) == 0 && len(rule.Keywords) == 0 {
			return nil, fmt.Errorf("circuit error rule %q: needs status_codes or keywords", rule.Name)
		}
	}
	return rules, nil
}

// Match reports whether the rule applies to a failure with this status and lower-cased text.
func (r CircuitErrorRule) Match(statusCode int, lowerText string) bool {
	if r.Disabled {
		return false
	}
	if len(r.StatusCodes) > 0 {
		found := false
		for _, code := range r.StatusCodes {
			if code == statusCode {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(r.Keywords) == 0 {
		return true
	}
	for _, keyword := range r.Keywords {
		if strings.Contains(lowerText, keyword) {
			return true
		}
	}
	return false
}

func validCircuitRuleRetry(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "no_retry", "stop", "same_key", "same_credential", "next_key", "refresh_credential", "next_credential", "next_channel", "next_candidate", "retry":
		return true
	}
	return false
}

func validCircuitRuleEffect(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "cooldown_key", "cooldown_credential", "cooldown_model", "cooldown_channel", "skip_group", "record_failure", "record_credential_failure", "disable_key", "disable_credential", "disable_channel":
		return true
	}
	return false
}
