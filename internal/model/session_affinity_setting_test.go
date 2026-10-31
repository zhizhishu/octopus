package model

import "testing"

func TestSessionAffinitySettingDefaultsOnAndValidatesBool(t *testing.T) {
	found := false
	for _, s := range DefaultSettings() {
		if s.Key == SettingKeySessionAffinityEnabled {
			found = true
			if s.Value != "true" {
				t.Fatalf("%s default = %q, want true", s.Key, s.Value)
			}
		}
	}
	if !found {
		t.Fatalf("%s missing from DefaultSettings", SettingKeySessionAffinityEnabled)
	}
	for _, v := range []string{"true", "false"} {
		if err := (&Setting{Key: SettingKeySessionAffinityEnabled, Value: v}).Validate(); err != nil {
			t.Fatalf("value %q rejected: %v", v, err)
		}
	}
	for _, v := range []string{"", "1", "yes", "3600"} {
		if err := (&Setting{Key: SettingKeySessionAffinityEnabled, Value: v}).Validate(); err == nil {
			t.Fatalf("value %q accepted, want error", v)
		}
	}
}
