package openai

import (
	"testing"
)

func TestNormalizeToolArgumentsRecoversConjoinedJSON(t *testing.T) {
	conjoined := `{"command": "Get-ChildItem -Path skills"}{"command": "Get-ChildItem -Path hooks"}`

	recovered := normalizeToolArguments(conjoined)

	if recovered != `{"command": "Get-ChildItem -Path hooks"}` {
		t.Fatalf("expected last valid json recovered, got %q", recovered)
	}

	validSingle := `{"command": "Get-Process"}`
	if got := normalizeToolArguments(validSingle); got != validSingle {
		t.Fatalf("expected valid json untouched, got %q", got)
	}

	empty := normalizeToolArguments("")
	if empty != "{}" {
		t.Fatalf("expected empty normalized to {}, got %q", empty)
	}
}
