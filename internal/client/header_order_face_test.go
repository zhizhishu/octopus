package client

import (
	"context"
	"reflect"
	"testing"
)

// TestHeaderOrderFollowsFaceOverride locks the fix for the shape gap where a Chat channel
// dressed as codex/claude got the CLI ClientHello and CLI header SET but the Chrome header
// ORDER, because the ordering helpers only ever looked at the URL path. Header order is
// itself a recognisable tell, so an explicit face must win.
func TestHeaderOrderFollowsFaceOverride(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		face         string
		wantPseudo   []string
		wantCanonical []string
	}{
		{
			name:          "codex face on a chat path uses the codex pseudo and canonical order",
			path:          "/v1/chat/completions",
			face:          tlsFaceCodex,
			wantPseudo:    codexH2PseudoHeaderOrder,
			wantCanonical: codexCanonicalHeaderOrder,
		},
		{
			name:          "claude face on a chat path uses the claude canonical order",
			path:          "/v1/chat/completions",
			face:          tlsFaceClaude,
			wantPseudo:    chromePseudoHeaderOrder,
			wantCanonical: claudeCanonicalHeaderOrder,
		},
		{
			name:          "codex face on a responses path still uses the codex order",
			path:          "/v1/responses",
			face:          tlsFaceCodex,
			wantPseudo:    codexH2PseudoHeaderOrder,
			wantCanonical: codexCanonicalHeaderOrder,
		},
		{
			name:          "explicit chrome face suppresses the CLI canonical order",
			path:          "/v1/responses",
			face:          tlsFaceChrome,
			wantPseudo:    chromePseudoHeaderOrder,
			wantCanonical: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h2PseudoHeaderOrderForFace(tc.path, tc.face); !reflect.DeepEqual(got, tc.wantPseudo) {
				t.Fatalf("pseudo order = %v, want %v", got, tc.wantPseudo)
			}
			if got := canonicalHeaderOrderForFace(tc.path, tc.face); !reflect.DeepEqual(got, tc.wantCanonical) {
				t.Fatalf("canonical order = %v, want %v", got, tc.wantCanonical)
			}
		})
	}
}

// TestHeaderOrderWithoutFaceKeepsPathRule is the zero-regression half: with no face override
// (the uTLS/cloak switches off, i.e. every deployment that never opted into a shape) the
// helpers must return exactly what the path rule returned before, or this change would
// silently reorder outbound headers for everyone.
func TestHeaderOrderWithoutFaceKeepsPathRule(t *testing.T) {
	cases := []struct {
		path          string
		wantPseudo    []string
		wantCanonical []string
	}{
		{"/v1/responses", codexH2PseudoHeaderOrder, codexCanonicalHeaderOrder},
		{"/v1/messages", chromePseudoHeaderOrder, claudeCanonicalHeaderOrder},
		{"/v1/chat/completions", chromePseudoHeaderOrder, nil},
		{"/v1beta/models/gemini-pro:generateContent", chromePseudoHeaderOrder, nil},
	}

	for _, tc := range cases {
		if got := h2PseudoHeaderOrderForFace(tc.path, ""); !reflect.DeepEqual(got, tc.wantPseudo) {
			t.Fatalf("%s: pseudo order = %v, want %v", tc.path, got, tc.wantPseudo)
		}
		if got := canonicalHeaderOrderForFace(tc.path, ""); !reflect.DeepEqual(got, tc.wantCanonical) {
			t.Fatalf("%s: canonical order = %v, want %v", tc.path, got, tc.wantCanonical)
		}
	}
}

// TestTLSFaceOverrideReachesHeaderOrder closes the loop end to end: the face is carried on
// the request context by WithTLSFace, so the ordering helpers must observe it through the
// same context the ClientHello selection reads. This is the wiring the gap was missing.
func TestTLSFaceOverrideReachesHeaderOrder(t *testing.T) {
	ctx := WithTLSFace(context.Background(), tlsFaceCodex)
	if got := tlsFaceFromContext(ctx); got != tlsFaceCodex {
		t.Fatalf("face not carried on context: got %q, want %q", got, tlsFaceCodex)
	}

	// A codex face on a chat path must resolve to the codex order, which is the case that
	// previously fell through to Chrome.
	if got := canonicalHeaderOrderForFace("/v1/chat/completions", tlsFaceFromContext(ctx)); !reflect.DeepEqual(got, codexCanonicalHeaderOrder) {
		t.Fatalf("codex face did not reach the header order: got %v", got)
	}

	// An empty family clears the override, restoring the path rule.
	if got := tlsFaceFromContext(WithTLSFace(context.Background(), "")); got != "" {
		t.Fatalf("empty family must clear the override, got %q", got)
	}
}
