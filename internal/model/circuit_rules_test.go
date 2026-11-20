package model

import "testing"

func TestParseCircuitErrorRulesValidatesAndNormalises(t *testing.T) {
	rules, err := ParseCircuitErrorRules(`[{"name":" quota ","status_codes":[429,403],"keywords":[" Insufficient_Quota ",""],"scope":"key","cooldown_seconds":1800}]`)
	if err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "quota" || len(rules[0].Keywords) != 1 || rules[0].Keywords[0] != "insufficient_quota" {
		t.Fatalf("normalised rules = %+v", rules)
	}
	if !rules[0].Match(429, "error: insufficient_quota for this key") {
		t.Fatal("status + keyword must match")
	}
	if rules[0].Match(500, "insufficient_quota") {
		t.Fatal("status outside the list must not match")
	}
	if rules[0].Match(429, "rate limited per minute") {
		t.Fatal("missing keyword must not match")
	}

	for name, raw := range map[string]string{
		"bad scope":     `[{"keywords":["x"],"scope":"group","cooldown_seconds":10}]`,
		"no condition":  `[{"scope":"model","cooldown_seconds":10}]`,
		"zero cooldown": `[{"keywords":["x"],"scope":"model","cooldown_seconds":0}]`,
		"bad status":    `[{"status_codes":[42],"scope":"model","cooldown_seconds":10}]`,
		"unknown field": `[{"keywords":["x"],"scope":"model","cooldown_seconds":10,"groups":[1]}]`,
		"not array":     `{"keywords":["x"]}`,
	} {
		if _, err := ParseCircuitErrorRules(raw); err == nil {
			t.Fatalf("%s: expected rejection for %s", name, raw)
		}
	}
	if rules, err := ParseCircuitErrorRules(" "); err != nil || rules != nil {
		t.Fatalf("blank must be an empty list, got %v %v", rules, err)
	}
	if err := (&Setting{Key: SettingKeyCircuitBreakerErrorRules, Value: `[{"scope":"x"}]`}).Validate(); err == nil {
		t.Fatal("setting validation must reject invalid rules")
	}
}
