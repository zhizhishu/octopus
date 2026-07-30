package model

import "strings"

// Reasoning signatures are opaque, provider-SPECIFIC blobs carried across turns on
// the single Message.ReasoningSignature field. Four protocols feed it:
//
//   - Anthropic thinking signature: a bare base64 value (no tag).
//   - Anthropic redacted_thinking data: tagged with ReasoningSigRedactedPrefix.
//   - Gemini thoughtSignature: tagged with reasoningSigGeminiPrefix.
//   - OpenAI Responses reasoning.encrypted_content: tagged with reasoningSigOpenAIEncPrefix.
//
// On a CROSS-protocol replay (e.g. an Anthropic history routed to a Gemini or OpenAI
// upstream) the wrong provider's blob must not be emitted as the target's signature,
// or the upstream rejects it. Tagging the two NEW cross-emitted sources (Gemini,
// OpenAI encrypted_content) lets each outbound emit only its own kind and fall back
// safely for a foreign blob. The Gemini/OpenAI tags are prefixed with a control
// character (\x00) that cannot appear in a real base64 signature, so a tag can never
// collide with a bare same-family value.
const (
	// ReasoningSigRedactedPrefix marks an Anthropic redacted_thinking payload. KEEP
	// this exact value: existing Anthropic redacted tests and on-the-wire histories
	// depend on it.
	ReasoningSigRedactedPrefix = "redacted_thinking:"

	reasoningSigGeminiPrefix    = "\x00rs-gemini:"
	reasoningSigOpenAIEncPrefix = "\x00rs-openai-enc:"
)

// EncodeRedactedThinkingSignature packs Anthropic redacted_thinking data into the
// ReasoningSignature field with a clear marker so it is distinguishable from a real
// thinking.signature on the outbound path.
func EncodeRedactedThinkingSignature(data string) string {
	return ReasoningSigRedactedPrefix + data
}

// DecodeRedactedThinkingSignature returns the redacted payload when sig was produced
// by EncodeRedactedThinkingSignature.
func DecodeRedactedThinkingSignature(sig string) (string, bool) {
	if !strings.HasPrefix(sig, ReasoningSigRedactedPrefix) {
		return "", false
	}
	return strings.TrimPrefix(sig, ReasoningSigRedactedPrefix), true
}

// IsRedactedThinkingSignature reports whether sig carries redacted_thinking data.
func IsRedactedThinkingSignature(sig *string) bool {
	return sig != nil && strings.HasPrefix(*sig, ReasoningSigRedactedPrefix)
}

// TagGeminiThoughtSignature marks a raw Gemini thoughtSignature so only the Gemini
// outbound re-emits it; every other outbound treats it as a foreign blob.
func TagGeminiThoughtSignature(raw string) string {
	return reasoningSigGeminiPrefix + raw
}

// GeminiThoughtSignature returns (raw, true) iff sig is a Gemini-tagged signature.
func GeminiThoughtSignature(sig *string) (string, bool) {
	if sig == nil || !strings.HasPrefix(*sig, reasoningSigGeminiPrefix) {
		return "", false
	}
	return strings.TrimPrefix(*sig, reasoningSigGeminiPrefix), true
}

// TagOpenAIEncryptedContent marks a raw OpenAI Responses reasoning.encrypted_content
// blob so only the OpenAI Responses/codex outbound re-emits it.
func TagOpenAIEncryptedContent(raw string) string {
	return reasoningSigOpenAIEncPrefix + raw
}

// OpenAIEncryptedContent returns (raw, true) iff sig is an OpenAI-encrypted-tagged
// signature.
func OpenAIEncryptedContent(sig *string) (string, bool) {
	if sig == nil || !strings.HasPrefix(*sig, reasoningSigOpenAIEncPrefix) {
		return "", false
	}
	return strings.TrimPrefix(*sig, reasoningSigOpenAIEncPrefix), true
}

// HasProviderReasoningTag reports whether sig carries ANY provider tag (redacted,
// gemini, or openai-enc) — i.e. it is NOT a bare same-family signature.
func HasProviderReasoningTag(sig *string) bool {
	if sig == nil {
		return false
	}
	return strings.HasPrefix(*sig, ReasoningSigRedactedPrefix) ||
		strings.HasPrefix(*sig, reasoningSigGeminiPrefix) ||
		strings.HasPrefix(*sig, reasoningSigOpenAIEncPrefix)
}
