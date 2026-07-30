package model

import "testing"

// TestReasoningSignatureTagRoundTrip pins the source-tagging scheme: each provider
// tag round-trips to its raw value, the three tag kinds are mutually exclusive, and
// HasProviderReasoningTag distinguishes any tagged blob from a bare same-family one.
func TestReasoningSignatureTagRoundTrip(t *testing.T) {
	// Gemini tag/untag.
	geminiTagged := TagGeminiThoughtSignature("gemini-raw")
	if raw, ok := GeminiThoughtSignature(&geminiTagged); !ok || raw != "gemini-raw" {
		t.Fatalf("gemini round-trip failed: raw=%q ok=%v", raw, ok)
	}
	if _, ok := OpenAIEncryptedContent(&geminiTagged); ok {
		t.Fatalf("gemini-tagged must not decode as openai-enc")
	}
	if IsRedactedThinkingSignature(&geminiTagged) {
		t.Fatalf("gemini-tagged must not be redacted")
	}

	// OpenAI encrypted_content tag/untag.
	openaiTagged := TagOpenAIEncryptedContent("enc-raw")
	if raw, ok := OpenAIEncryptedContent(&openaiTagged); !ok || raw != "enc-raw" {
		t.Fatalf("openai-enc round-trip failed: raw=%q ok=%v", raw, ok)
	}
	if _, ok := GeminiThoughtSignature(&openaiTagged); ok {
		t.Fatalf("openai-tagged must not decode as gemini")
	}

	// Redacted encode/decode.
	redacted := EncodeRedactedThinkingSignature("REDACTED")
	if redacted != ReasoningSigRedactedPrefix+"REDACTED" {
		t.Fatalf("redacted encode = %q, want %q", redacted, ReasoningSigRedactedPrefix+"REDACTED")
	}
	if data, ok := DecodeRedactedThinkingSignature(redacted); !ok || data != "REDACTED" {
		t.Fatalf("redacted decode failed: data=%q ok=%v", data, ok)
	}
	if !IsRedactedThinkingSignature(&redacted) {
		t.Fatalf("IsRedactedThinkingSignature(redacted) = false, want true")
	}

	// HasProviderReasoningTag: true for all three tags.
	for _, sig := range []string{geminiTagged, openaiTagged, redacted} {
		s := sig
		if !HasProviderReasoningTag(&s) {
			t.Fatalf("HasProviderReasoningTag(%q) = false, want true", s)
		}
	}

	// A bare value is not tagged and decodes as none of the kinds.
	bare := "abc"
	if HasProviderReasoningTag(&bare) {
		t.Fatalf("HasProviderReasoningTag(bare) = true, want false")
	}
	if HasProviderReasoningTag(nil) {
		t.Fatalf("HasProviderReasoningTag(nil) = true, want false")
	}
	if _, ok := GeminiThoughtSignature(&bare); ok {
		t.Fatalf("bare must not decode as gemini")
	}
	if _, ok := OpenAIEncryptedContent(&bare); ok {
		t.Fatalf("bare must not decode as openai-enc")
	}
	if IsRedactedThinkingSignature(&bare) {
		t.Fatalf("bare must not be redacted")
	}
}
