package modelcatalog

import "testing"

func TestNewModelFamiliesAndStrictSnapshots(t *testing.T) {
	for _, tc := range []struct{ id, family string }{
		{"gpt-6.2-astra", "openai"},
		{"gpt-6.2-sol", "openai"},
		{"openai/gpt-6.2-sol-2026-11-17", "openai"},
		{"gemini-3.8-flash", "google"},
	} {
		m, ok := Lookup(tc.id)
		if !ok || m.Family != tc.family || !m.Reasoning {
			t.Errorf("%s = %#v,%v", tc.id, m, ok)
		}
	}
	for _, bad := range []string{"gpt-6.2", "gpt-6.2-foo", "gpt-6.2-astra-preview", "gpt-6.2-astra-2026-99-99", "gpt-6.2-solids"} {
		if IsGPT62Model(bad) {
			t.Errorf("unlisted model matched %q", bad)
		}
	}
}
