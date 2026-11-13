package behavior

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/modelverify/fingerprint"
)

// The bank uses claude/gpt/gemini; the audit classifier uses vendor names.
// These fixtures reproduce the old false cross-family findings before the fix.
func TestCatalogReferenceFamilyBoundary(t *testing.T) {
	for _, tc := range []struct{ declared, predicted, family, name string }{
		{"claude-opus-4-6", "claude-opus-4.6", "claude", "Claude"},
		{"gpt-5.3-codex", "gpt-5.3-codex", "gpt", "GPT"},
		{"gemini-3.1-pro-preview", "gemini-3.1-pro-preview", "gemini", "Gemini"},
		{"gemini-4-argon", "gemini-4-argon", "gemini", "Gemini"},
		{"gemini-4-carbon", "gemini-4-carbon", "gemini", "Gemini"},
		{"gemini-4-hydrogen", "gemini-4-hydrogen", "gemini", "Gemini"},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			res := fingerprintResult("scorable", tc.predicted, tc.family, tc.name, nil,
				fingerprint.Candidate{Model: tc.predicted, Family: tc.family, Score: 4.8},
				fingerprint.Candidate{Model: "other", Family: tc.family, Score: 3.0})
			findings := fingerprintFindings(res, tc.declared)
			if len(findings) != 1 || findings[0].Score != 0 || !strings.Contains(findings[0].Title, "不能作为正品证明") {
				t.Fatalf("same model must not be a cross-family accusation: %+v", findings)
			}
			partial := fingerprintResult("partial", tc.predicted, tc.family, tc.name, nil)
			for _, finding := range fingerprintFindings(partial, tc.declared) {
				if finding.Score != 0 || strings.Contains(finding.Title, "不同族") {
					t.Fatalf("partial same-family evidence is mislabeled: %+v", finding)
				}
			}
		})
	}
}

func TestCatalogCrossFamilyStillFlags(t *testing.T) {
	for _, tc := range []struct{ declared, predicted, family string }{
		{"claude-opus-4-6", "gpt-5.5", "gpt"},
		{"gpt-5.3-codex", "gemini-4-carbon", "gemini"},
		{"gemini-3.1-pro-preview", "claude-opus-4.6", "claude"},
	} {
		res := fingerprintResult("scorable", tc.predicted, tc.family, tc.family, nil,
			fingerprint.Candidate{Model: tc.predicted, Family: tc.family, Score: 4.8},
			fingerprint.Candidate{Model: "other", Family: tc.family, Score: 3.0})
		findings := fingerprintFindings(res, tc.declared)
		if len(findings) != 1 || findings[0].Score != fingerprintMismatchScore {
			t.Fatalf("%s vs %s: real cross-family mismatch was suppressed: %+v", tc.declared, tc.predicted, findings)
		}
	}
}

func TestCatalogGeminiVariantsRemainDistinctModels(t *testing.T) {
	for _, declared := range []string{"gemini-4-argon-high", "gemini-4-carbon-high", "gemini-4-hydrogen-xhigh-20261112"} {
		if !sameFamily("gemini", declared) {
			t.Errorf("full variant ID lost its family: %s", declared)
		}
	}
	res := fingerprintResult("scorable", "gemini-4-carbon", "gemini", "Gemini", nil,
		fingerprint.Candidate{Model: "gemini-4-carbon", Family: "gemini", Score: 4.8},
		fingerprint.Candidate{Model: "gemini-4-argon", Family: "gemini", Score: 3.0})
	findings := fingerprintFindings(res, "gemini-4-argon")
	if len(findings) != 1 || findings[0].Score != fingerprintDowngradeScore {
		t.Fatalf("different Gemini models must retain the existing same-family rule: %+v", findings)
	}
}

// Replays public reference answers without a Sender or any upstream request.
func TestCatalogReferenceReplay(t *testing.T) {
	path := os.Getenv("CATALOG_REFERENCE_INPUTS")
	if path == "" {
		path = "../fingerprint/testdata/g_full.json"
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Source        string   `json:"source"`
		Texts         []string `json:"texts"`
		ExpectedCount []int    `json:"expected_count"`
	}
	if err := json.Unmarshal(blob, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		outputs := make([]fingerprint.Output, len(tc.Texts))
		for i, text := range tc.Texts {
			outputs[i] = fingerprint.Output{Text: text, ExpectedCount: tc.ExpectedCount[i]}
		}
		analysis, err := fingerprint.Analyze(outputs)
		if err != nil {
			t.Fatalf("%s: %v", tc.Source, err)
		}
		t.Logf("reference source=%s decision=%s prediction=%s family=%s", tc.Source, analysis.Decision, analysis.Prediction, analysis.Family)
		if strings.HasPrefix(tc.Source, "gemini-4-") && os.Getenv("CATALOG_REFERENCE_INPUTS") == "" {
			if analysis.Decision != fingerprint.DecisionScorable || analysis.Family != "gemini" {
				t.Fatalf("new Gemini reference failed: %+v", analysis)
			}
		}
	}
}
