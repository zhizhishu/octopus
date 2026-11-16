package redteamlab

import (
	"strings"
	"testing"
)

func validCase() Case {
	return Case{ID: "marker_case", Name: "角色优先级", Prompt: "Please describe how system and user instructions differ.", Expected: "marker", MaxTokens: 160}
}

func TestConfigDefaultsOffAndCasesNonNil(t *testing.T) {
	c, err := Parse("")
	if err != nil || c.Enabled || len(c.Cases) != 0 {
		t.Fatalf("empty: %+v err=%v", c, err)
	}
	c, err = Parse(`{"enabled":false,"cases":null}`)
	if err != nil || c.Cases == nil {
		t.Fatalf("null cases: %+v err=%v", c, err)
	}
}
func TestConfigValidationCasesAndLimits(t *testing.T) {
	a := validCase()
	if err := (Config{Enabled: true, Cases: []Case{a}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Case)
	}{
		{"invalid id", func(c *Case) { c.ID = "id/../abc" }},
		{"empty prompt", func(c *Case) { c.Prompt = "" }},
		{"long prompt", func(c *Case) { c.Prompt = strings.Repeat("测", 3001) }},
		{"over token budget", func(c *Case) { c.MaxTokens = 513 }},
		{"low token budget", func(c *Case) { c.MaxTokens = 0 }},
		{"long marker", func(c *Case) { c.Expected = strings.Repeat("a", 161) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := a
			tc.change(&c)
			if err := (Config{Cases: []Case{c}}).Validate(); err == nil {
				t.Fatalf("accepted invalid %+v", c)
			}
		})
	}
	if err := (Config{Cases: []Case{a, a}}).Validate(); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	huge := make([]Case, 9)
	for i := range huge {
		huge[i] = a
	}
	if err := (Config{Cases: huge}).Validate(); err == nil {
		t.Fatal("too many cases accepted")
	}
}
func TestLiteralMarkerIsNotJailbreakVerdict(t *testing.T) {
	if Score("needle", "needle") != "matched" || Score("needle", "Needle") != "not_matched" {
		t.Fatal("literal comparison drift")
	}
	if Score("", "anything") != "unscored" {
		t.Fatal("empty reference must not score")
	}
	s, clipped := ClipResponse(strings.Repeat("界", 4001))
	if !clipped || len([]rune(s)) != 4000 {
		t.Fatalf("clipping %d %v", len([]rune(s)), clipped)
	}
	_, clipped = ClipResponse("short")
	if clipped {
		t.Fatal("short text clipped")
	}
}
