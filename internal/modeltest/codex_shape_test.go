package modeltest

import (
	"testing"

	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	dbmodel "github.com/bestruirui/octopus/internal/model"
)

// resolvedFingerprint with no profile returns the global setting value for every getter,
// so a zero-value fp is the right shape for these unit tests (we do not exercise the
// fingerprint profile code paths here, only the codex reasoning normalization wiring).
func zeroFP() resolvedFingerprint { return resolvedFingerprint{} }

// prepareCodexModelTestRequest must run the SAME reasoning context + effort normalization
// the relay forward path runs, because the modeltest path also synthesizes the
// X-OpenAI-Internal-Codex-Responses-Lite header (via applyCodexHeaderDefaults in
// runOne -> applyHeaderDefaults), so the upstream applies the same body-field pairing
// rule. Without this the web "channel test" button 400s on the same
// `requires reasoning.context to be all_turns` / `level "minimal" not supported` the
// relay path used to 400 on.

func TestPrepareCodexModelTestDefaultsContextAndHighEffortForGPT56(t *testing.T) {
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected reasoning.context=all_turns on default gpt-5.6 modeltest probe, got %q", req.ReasoningContext)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("expected reasoning.effort=high (empty default for gpt-5.6), got %q", req.ReasoningEffort)
	}
}

func TestPrepareCodexModelTestRemapsMinimalEffortToLowForGPT56(t *testing.T) {
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "minimal",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected reasoning.effort=low (minimal remap for gpt-5.6), got %q", req.ReasoningEffort)
	}
	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected reasoning.context=all_turns, got %q", req.ReasoningContext)
	}
}

func TestPrepareCodexModelTestPreservesExplicitHighEffortForGPT56(t *testing.T) {
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "high",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningEffort != "high" {
		t.Fatalf("expected explicit reasoning.effort=high to pass through, got %q", req.ReasoningEffort)
	}
	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected reasoning.context=all_turns, got %q", req.ReasoningContext)
	}
}

func TestPrepareCodexModelTestContextStillInjectedForNon56Model(t *testing.T) {
	// A non-5.6 codex model: effort "minimal" is NOT remapped (5.5 does not have the
	// gpt-5.6 allow-set restriction), but context is STILL injected because the Lite
	// header is added to EVERY codex outbound, not just gpt-5.6.
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.5",
		ReasoningEffort:  "minimal",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningEffort != "minimal" {
		t.Fatalf("expected non-5.6 model to leave minimal effort untouched, got %q", req.ReasoningEffort)
	}
	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected reasoning.context=all_turns even on non-5.6 codex channel, got %q", req.ReasoningContext)
	}
}

func TestPrepareCodexModelTestPreservesExplicitClientContext(t *testing.T) {
	// A genuine codex CLI sends its own reasoning.context; echoing that back is what
	// keeps octopus faithful, so an explicit client value must NEVER be overwritten by
	// the codex shaper's default.
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "high",
		ReasoningContext: "client_chosen",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningContext != "client_chosen" {
		t.Fatalf("expected explicit client reasoning.context to survive, got %q", req.ReasoningContext)
	}
}

// Sanity-check the wiring through the channel type branch: a NON-OpenAIResponse channel
// type returns early from prepareCodexModelTestRequest after the metadata setup, so
// reasoning normalization must NOT have run. (The header apply is also gated on the
// channel type via applyHeaderDefaults, so this is consistent: no Lite header on a
// non-codex channel => no context/effort injection either.)
func TestPrepareCodexModelTestSkipsReasoningForNonOpenAIResponseChannel(t *testing.T) {
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "minimal",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIChat, zeroFP())
	if req.ReasoningContext != "" {
		t.Fatalf("expected no reasoning.context on a non-codex channel, got %q", req.ReasoningContext)
	}
	// effort also untouched on a non-codex channel: the normalization lives behind the
	// OpenAIResponse gate in prepareCodexModelTestShape.
	if req.ReasoningEffort != "minimal" {
		t.Fatalf("expected no effort remap on a non-codex channel, got %q", req.ReasoningEffort)
	}
}

// modelTestUsesCodexFingerprint gates whether runOne calls prepareCodexModelTestRequest
// (which sets reasoning.context=all_turns + normalizes effort). The gate MUST agree with
// applyHeaderDefaults about whether the Lite header will be added — otherwise the
// header goes out without its paired body field and the upstream 400s with the same
// `requires reasoning.context to be all_turns` the relay path used to 400 on.
//
// The web UI's "channel test" button defaults to the openai_chat endpoint, NOT
// openai_responses. applyHeaderDefaults adds the Lite header to a type=1 codex channel
// regardless of endpoint, so modelTestUsesCodexFingerprint must return true for a
// type=1 channel on every endpoint too. (Chat/CustomChat outbound types only get the
// Lite header on openai_responses, so they keep the endpoint gate.)

func TestModelTestUsesCodexFingerprintCodexChannelOnAnyEndpoint(t *testing.T) {
	// ch1=GPT-CPA-生图 (type=1, cloak mode="" treated as auto) reproducibly 400ed on
	// the web "channel test" button because the UI defaulted to openai_chat and the
	// gate returned false, so reasoning.context was never set even though the Lite
	// header was added. This test pins the fix: type=1 + cloak auto + openai_chat =>
	// true (and the same for openai_responses / anthropic_messages / empty).
	cases := []struct {
		name     string
		endpoint string
	}{
		{"openai_chat_default", "openai_chat"},
		{"openai_responses", "openai_responses"},
		{"anthropic_messages", "anthropic_messages"},
		{"empty_endpoint", ""},
	}
	ch := &dbmodel.Channel{
		Type:  outbound.OutboundTypeOpenAIResponse,
		Cloak: dbmodel.ChannelCloak{Mode: "", ProfileID: 1}, // mirrors production ch1
	}
	for _, c := range cases {
		if got := modelTestUsesCodexFingerprint(ch, c.endpoint); !got {
			t.Fatalf("type=1 codex channel must use codex fingerprint on endpoint %q, got false", c.endpoint)
		}
	}
}

func TestModelTestUsesCodexFingerprintChatChannelOnlyOnResponsesEndpoint(t *testing.T) {
	// A type=0/Custom OpenAIChat channel only gets the Lite header on the
	// openai_responses endpoint (applyHeaderDefaults gates it that way), so the gate
	// must agree: true on openai_responses, false on openai_chat / anthropic_messages.
	ch := &dbmodel.Channel{
		Type:  outbound.OutboundTypeOpenAIChat,
		Cloak: dbmodel.ChannelCloak{Mode: "auto"},
	}
	if got := modelTestUsesCodexFingerprint(ch, "openai_responses"); !got {
		t.Fatalf("type=0 chat channel on openai_responses endpoint must use codex fingerprint, got false")
	}
	if got := modelTestUsesCodexFingerprint(ch, "openai_chat"); got {
		t.Fatalf("type=0 chat channel on openai_chat endpoint must NOT use codex fingerprint, got true")
	}
	if got := modelTestUsesCodexFingerprint(ch, "anthropic_messages"); got {
		t.Fatalf("type=0 chat channel on anthropic_messages endpoint must NOT use codex fingerprint, got true")
	}
}

func TestModelTestUsesCodexFingerprintSkipsWhenCloakNever(t *testing.T) {
	// cloak=never means applyHeaderDefaults returns early without adding the Lite
	// header, so the gate MUST also return false on every endpoint to avoid sending
	// reasoning.context without the paired header (the inverse mismatch).
	ch := &dbmodel.Channel{
		Type:  outbound.OutboundTypeOpenAIResponse,
		Cloak: dbmodel.ChannelCloak{Mode: "never"},
	}
	if got := modelTestUsesCodexFingerprint(ch, "openai_responses"); got {
		t.Fatalf("cloak=never codex channel must NOT use codex fingerprint on openai_responses, got true")
	}
	if got := modelTestUsesCodexFingerprint(ch, "openai_chat"); got {
		t.Fatalf("cloak=never codex channel must NOT use codex fingerprint on openai_chat, got true")
	}
}

func TestModelTestUsesCodexFingerprintSkipsNonCodexChannelTypes(t *testing.T) {
	// A Gemini or Anthropic channel never gets the codex Lite header, so the gate
	// must return false on every endpoint regardless of cloak mode.
	for _, chType := range []outbound.OutboundType{
		outbound.OutboundTypeAnthropic,
		outbound.OutboundTypeGemini,
	} {
		ch := &dbmodel.Channel{Type: chType, Cloak: dbmodel.ChannelCloak{Mode: "auto"}}
		if got := modelTestUsesCodexFingerprint(ch, "openai_responses"); got {
			t.Fatalf("channel type %d must NOT use codex fingerprint on openai_responses, got true", chType)
		}
		if got := modelTestUsesCodexFingerprint(ch, "openai_chat"); got {
			t.Fatalf("channel type %d must NOT use codex fingerprint on openai_chat, got true", chType)
		}
	}
}
