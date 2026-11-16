package model

import "testing"

func TestPromptAudienceDefaultsAndValidation(t *testing.T) {
	for _, audience := range []PromptAudience{"", PromptAudienceAll, PromptAudienceAdminOnly} {
		if !ValidPromptAudience(audience) {
			t.Fatalf("valid audience %q rejected", audience)
		}
	}
	if ValidPromptAudience("admin") || ValidPromptAudience("svip") {
		t.Fatal("untrusted role or plan must not become a prompt audience")
	}
	for _, item := range []struct {
		audience PromptAudience
		admin    bool
		allow    bool
	}{
		{"", false, true}, {PromptAudienceAll, false, true},
		{PromptAudienceAll, true, true}, {PromptAudienceAdminOnly, false, false},
		{PromptAudienceAdminOnly, true, true}, {"typo", true, false},
	} {
		if got := PromptAudienceAllows(item.audience, item.admin); got != item.allow {
			t.Errorf("audience=%q admin=%v: got %v want %v", item.audience, item.admin, got, item.allow)
		}
	}
}

func TestPromptAudienceSettingValidation(t *testing.T) {
	for _, v := range []string{"all", "admin_only"} {
		if err := (&Setting{Key: SettingKeyPromptAudience, Value: v}).Validate(); err != nil {
			t.Errorf("%q should be valid: %v", v, err)
		}
	}
	for _, v := range []string{"", "admin", "guest_key", "svip"} {
		if err := (&Setting{Key: SettingKeyPromptAudience, Value: v}).Validate(); err == nil {
			t.Errorf("invalid setting audience %q accepted", v)
		}
	}
}
