package redteamlab

import (
	"strings"
	"testing"
)

func validV2Case() V2CaseSpec {
	return V2CaseSpec{
		Schema: V2Schema, ID: "case-v2", Name: "授权评估", Category: "prompt_injection.direct",
		Prompts:  []string{"Explain the applicable instruction order clearly."},
		Expected: "BOUNDARY_MARKER", MaxTokens: 96, SourceKind: "manual", SourceLicense: "original",
	}
}

func TestV2CaseSchemaValidationAndDigest(t *testing.T) {
	item := validV2Case()
	if err := item.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, hash, err := item.Canonical()
	if err != nil || !strings.Contains(raw, "case-v2") || len(hash) != 64 {
		t.Fatalf("canonical %v %s", err, hash)
	}
	if item.Score("BOUNDARY_MARKER") != "matched" || item.Score("not present") != "not_matched" {
		t.Fatal("marker drift: not a jailbreak success claim")
	}
	for _, tc := range []struct {
		name   string
		change func(*V2CaseSpec)
	}{
		{"schema", func(c *V2CaseSpec) { c.Schema = "unknown" }},
		{"traversal", func(c *V2CaseSpec) { c.ID = "../escaped" }},
		{"category", func(c *V2CaseSpec) { c.Category = "Untrusted Script" }},
		{"excess turns", func(c *V2CaseSpec) { c.Prompts = make([]string, 5) }},
		{"empty prompt", func(c *V2CaseSpec) { c.Prompts = []string{""} }},
		{"large input", func(c *V2CaseSpec) {
			c.Prompts = []string{strings.Repeat("x", 3000), strings.Repeat("x", 3000), strings.Repeat("x", 3000)}
		}},
		{"big tokens", func(c *V2CaseSpec) { c.MaxTokens = 20000 }},
		{"source spoofing", func(c *V2CaseSpec) {
			c.SourceKind = "imported"
			c.SourceLicense = "unknown"
			c.SourceURL = "http://example.com"
		}},
		{"import missing license", func(c *V2CaseSpec) {
			c.SourceKind = "imported"
			c.SourceLicense = ""
			c.SourceURL = "https://example.com"
		}},
		{"import URL invalid", func(c *V2CaseSpec) {
			c.SourceKind = "imported"
			c.SourceLicense = "MIT"
			c.SourceURL = "file:///etc/passwd"
		}},
		{"tag overlimit", func(c *V2CaseSpec) { c.Tags = make([]string, 9) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := validV2Case()
			tc.change(&spec)
			if err := spec.Validate(); err == nil {
				t.Fatalf("accepted bad spec %+v", spec)
			}
		})
	}
	item.SourceKind = "imported"
	item.SourceLicense = "MIT"
	item.SourceURL = "https://github.com/example/project/blob/a1/testcases.json"
	if err := item.Validate(); err != nil {
		t.Fatal(err)
	}
}
