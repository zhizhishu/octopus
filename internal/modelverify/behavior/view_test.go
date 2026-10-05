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

func TestToViewKeepsAuthRateLimitFindingTitle(t *testing.T) {
	report := Report{
		RequestedModel: "claude-sonnet-4-5",
		Verdict:        VerdictUnknown,
		Score:          0,
		Findings: []Finding{{
			Probe:    ProbeSignature,
			Severity: SeverityLow,
			Score:    0,
			Title:    "签名回放被鉴权/限流拒绝（不可判）",
			Evidence: map[string]any{
				"replay_reject_status": 403,
				"replay_reject_class":  "auth_rate_limit",
			},
			Recommendation: "回放的 thinking 签名被上游 4xx 拒绝：无法区分限流/鉴权/请求构造与签名问题，不作为伪造证据，必要时人工复核。",
		}},
	}
	view := ToView(report)
	if len(view.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(view.Findings))
	}
	if view.Findings[0].Title != report.Findings[0].Title {
		t.Fatalf("card title %q != raw finding %q", view.Findings[0].Title, report.Findings[0].Title)
	}
	if view.Findings[0].Score != 0 || view.Findings[0].Severity != SeverityLow {
		t.Fatalf("card must keep 0/low: %+v", view.Findings[0])
	}
	if got := view.Findings[0].Evidence["replay_reject_status"]; got != float64(403) && got != 403 {
		t.Fatalf("card evidence status = %#v, want 403", got)
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
