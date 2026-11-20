package modeltest

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// resolvedFingerprint with no profile returns the global setting value for every getter,
// so a zero-value fp is the right shape for these unit tests (we do not exercise the
// fingerprint profile code paths here, only the codex reasoning normalization wiring).
func zeroFP() resolvedFingerprint { return resolvedFingerprint{} }

// prepareCodexModelTestRequest runs the SAME codex effort normalization the relay forward path
// runs. The real Codex CLI 0.163 sends no X-OpenAI-Internal-Codex-Responses-Lite header and oct
// no longer synthesizes one, so there is no header/body pairing rule (reasoning.context /
// parallel_tool_calls) to satisfy; only the effort remap remains, so the web "channel test"
// button never 400s with `level "minimal" not supported`.

func TestPrepareCodexModelTestDefaultsContextAndHighEffortForGPT56(t *testing.T) {
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningContext != "" {
		t.Fatalf("expected no reasoning.context injection on the modeltest probe (oct no longer forces all_turns), got %q", req.ReasoningContext)
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
	if req.ReasoningContext != "" {
		t.Fatalf("expected no reasoning.context injection, got %q", req.ReasoningContext)
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
	if req.ReasoningContext != "" {
		t.Fatalf("expected no reasoning.context injection, got %q", req.ReasoningContext)
	}
}

func TestPrepareCodexModelTestContextStillInjectedForNon56Model(t *testing.T) {
	// A non-5.6 codex model: effort "minimal" is NOT remapped (only gpt-5.6 has the
	// allow-set restriction), and no reasoning.context is injected (the pairing rule is gone).
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-4.1-mini",
		ReasoningEffort:  "minimal",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningEffort != "minimal" {
		t.Fatalf("expected non-5.6 model to leave minimal effort untouched, got %q", req.ReasoningEffort)
	}
}

func TestPrepareCodexModelTestSkipsPairingFieldsForDeniedModel(t *testing.T) {
	// gpt-5.5 is Lite-denied (no Lite header on the wire). Octopus no longer injects
	// reasoning.context for ANY model, so it stays empty here; its explicit minimal effort
	// also passes through untouched.
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.5",
		ReasoningEffort:  "minimal",
		ReasoningContext: "",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningEffort != "minimal" {
		t.Fatalf("expected denied model to leave minimal effort untouched, got %q", req.ReasoningEffort)
	}
	if req.ReasoningContext != "" {
		t.Fatalf("expected denied model to skip reasoning.context injection, got %q", req.ReasoningContext)
	}
}

func TestPrepareCodexModelTestPreservesExplicitReasoningContext(t *testing.T) {
	// Octopus no longer forces reasoning.context to all_turns, so the downstream client's
	// explicit choice is forwarded verbatim.
	req := &transformermodel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "high",
		ReasoningContext: "client_chosen",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ReasoningContext != "client_chosen" {
		t.Fatalf("expected explicit reasoning.context preserved, got %q", req.ReasoningContext)
	}
}

// Sanity-check the wiring through the channel type branch: a NON-OpenAIResponse channel
// type returns early from prepareCodexModelTestRequest after the metadata setup, so the
// codex effort normalization must NOT have run on it.
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
// (which now only normalizes reasoning.effort). The gate decides whether the codex
// fingerprint path is applied for a given channel type / endpoint.
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

// Octopus no longer force-sets parallel_tool_calls to pair with the
// X-OpenAI-Internal-Codex-Responses-Lite header (the header itself is no longer synthesized).
// The modeltest shaper leaves an absent field absent and preserves an explicit client true —
// faithful forwarding of whatever the caller sent.

func TestPrepareCodexModelTestDoesNotForceParallelToolCalls(t *testing.T) {
	req := &transformermodel.InternalLLMRequest{
		Model: "gpt-5.6-sol",
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ParallelToolCalls != nil {
		t.Fatalf("expected parallel_tool_calls to stay absent (oct no longer forces false), got %#v", req.ParallelToolCalls)
	}
}

// An explicit parallel_tool_calls=true is preserved — oct no longer coerces it to false (the
// Lite-header pairing rule is gone).
func TestPrepareCodexModelTestPreservesExplicitTrueParallelToolCalls(t *testing.T) {
	trueVal := true
	req := &transformermodel.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		ParallelToolCalls: &trueVal,
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
		t.Fatalf("expected explicit true preserved, got %#v", req.ParallelToolCalls)
	}
}

// A genuine codex CLI already sends parallel_tool_calls=false; echoing that back is what
// keeps octopus faithful, so an explicit client false MUST survive untouched.
func TestPrepareCodexModelTestPreservesExplicitFalseParallelToolCalls(t *testing.T) {
	falseVal := false
	req := &transformermodel.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		ParallelToolCalls: &falseVal,
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIResponse, zeroFP())
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("expected explicit false to survive, got %#v", req.ParallelToolCalls)
	}
}

// Sanity-check the wiring through the channel type branch: a NON-OpenAIResponse channel
// type returns early from prepareCodexModelTestRequest after the metadata setup, so the
// codex shaper (and any parallel_tool_calls handling) must NOT have run on it.
func TestPrepareCodexModelTestSkipsParallelToolCallsForNonOpenAIResponseChannel(t *testing.T) {
	trueVal := true
	req := &transformermodel.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		ParallelToolCalls: &trueVal,
	}
	prepareCodexModelTestRequest(req, outbound.OutboundTypeOpenAIChat, zeroFP())
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != true {
		t.Fatalf("expected explicit true to survive on a non-codex channel, got %#v", req.ParallelToolCalls)
	}
}
