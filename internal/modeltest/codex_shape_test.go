package modeltest

import (
	"testing"

	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
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
