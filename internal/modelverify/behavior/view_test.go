package behavior

import (
	"errors"
	"strings"
	"testing"
)

func TestToViewRedactsProbeErrorsAndKeepsDisclaimer(t *testing.T) {
	report := Report{
		RequestedModel: "claude-sonnet-example",
		Verdict:        VerdictUnknown,
		Score:          0,
		Results: []Result{{
			ProbeID: ProbeLiveness,
			OK:      false,
			Err:     errors.New("upstream returned 401: Authorization: Bearer sk-secret-key"),
		}},
		Errors: []ProbeError{{
			Probe: ProbeLiveness,
			Error: "Authorization: Bearer sk-secret-key",
		}},
	}

	view := ToView(report)
	if view.Disclaimer == "" {
		t.Fatal("disclaimer must be present")
	}
	if view.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %q, want unknown", view.Verdict)
	}
	if len(view.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(view.Results))
	}
	if strings.Contains(view.Results[0].Error, "sk-secret-key") {
		t.Fatalf("result error leaked secret: %q", view.Results[0].Error)
	}
	if !strings.Contains(view.Results[0].Error, "[redacted]") {
		t.Fatalf("result error missing redaction marker: %q", view.Results[0].Error)
	}
	if len(view.Errors) != 1 {
		t.Fatalf("errors = %d, want 1", len(view.Errors))
	}
	if strings.Contains(view.Errors[0].Error, "sk-secret-key") {
		t.Fatalf("probe error leaked secret: %q", view.Errors[0].Error)
	}
}

func TestToViewSerializesGlitchCandidates(t *testing.T) {
	report := Report{
		RequestedModel: "claude-sonnet-example",
		Verdict:        VerdictNone,
		Results: []Result{{
			ProbeID: ProbeGlitch,
			OK:      true,
			Data: map[string]any{
				"candidates": []GlitchCandidate{{
					Family:     "claude",
					Exact:      true,
					Consistent: true,
					Coverage:   1,
				}},
			},
		}},
	}
	view := ToView(report)
	if view.Results[0].Data == nil {
		t.Fatal("expected serialized probe data")
	}
	if _, ok := view.Results[0].Data["candidates"]; !ok {
		t.Fatalf("candidates dropped: %#v", view.Results[0].Data)
	}
}
