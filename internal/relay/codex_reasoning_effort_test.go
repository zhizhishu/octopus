package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// normalizeCodexReasoningEffort remaps an unsupported reasoning.effort on the GPT-5.6 family
// to "low" (the lowest legal value the upstream accepts) instead of letting the request 400.
// The upstream's own 400 message is the authority: "level \"minimal\" not supported, valid
// levels: low, medium, high, xhigh, max". "minimal"/"none" are real codex CLI effort levels
// elsewhere, but the gpt-5.6 codex upstream rejects them.

func TestNormalizeCodexReasoningEffortEmptyGPT56(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "high" {
		t.Fatalf("expected empty effort on gpt-5.6 to default to high, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortMinimalGPT56RemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-terra",
		ReasoningEffort: "minimal",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected minimal effort on gpt-5.6 to remap to low, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNoneGPT56RemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "none",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected none effort on gpt-5.6 to remap to low, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortLowGPT56Preserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "low",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected low effort on gpt-5.6 to be preserved, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortMediumGPT56Preserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "medium",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "medium" {
		t.Fatalf("expected medium effort on gpt-5.6 to be preserved, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortHighGPT56Preserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "high",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "high" {
		t.Fatalf("expected high effort on gpt-5.6 to be preserved, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortXhighGPT56Preserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "xhigh",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "xhigh" {
		t.Fatalf("expected xhigh effort on gpt-5.6 to be preserved, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortMaxGPT56Preserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "max",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "max" {
		t.Fatalf("expected max effort on gpt-5.6 to be preserved (5.6 accepts max), got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortBareGPT56MaxPreserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6",
		ReasoningEffort: "max",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "max" {
		t.Fatalf("expected max effort on bare gpt-5.6 to be preserved, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortPrefixedDatedGPT56MaxPreserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "openai/gpt-5.6-terra-2026-07-09",
		ReasoningEffort: "max",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "max" {
		t.Fatalf("expected max effort on prefixed/dated gpt-5.6 to be preserved, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNon56MaxBecomesXhigh(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.5",
		ReasoningEffort: "max",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "xhigh" {
		t.Fatalf("expected max effort on gpt-5.5 to remap to xhigh, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNon56PassThrough(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.5",
		ReasoningEffort: "high",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "high" {
		t.Fatalf("expected high effort on gpt-5.5 to pass through, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortUnknownEffortGPT56RemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-luna",
		ReasoningEffort: "ultra",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected unknown effort on gpt-5.6 to remap to low, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNilNoop(t *testing.T) {
	normalizeCodexReasoningEffort(nil)
}

// gpt-6 family (production evidence: 400 "Unsupported value: 'none' is not supported
// with the 'gpt-6-astra' model. Supported values are: 'low', 'medium', 'high', 'xhigh',
// and 'max'." from the real upstream via a cpA-gpt Responses/codex channel). A non-empty
// effort outside the 5-value set (none/minimal/unknown) is clamped to "low"; an empty
// effort is left untouched (only the explicit 'none' rejection is proven, no default is
// invented); legal values — including "max", which gpt-6 explicitly supports — pass through.

func TestNormalizeCodexReasoningEffortNoneGPT6RemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6-astra",
		ReasoningEffort: "none",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected none effort on gpt-6-astra to remap to low, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortMinimalGPT6RemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6-astra",
		ReasoningEffort: "minimal",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected minimal effort on gpt-6-astra to remap to low, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortUnknownGPT6RemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6-astra",
		ReasoningEffort: "ultra",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected unknown effort on gpt-6-astra to remap to low, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortHighGPT6Preserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6-astra",
		ReasoningEffort: "high",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "high" {
		t.Fatalf("expected high effort on gpt-6-astra to be preserved, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortMaxGPT6Preserved(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6-astra",
		ReasoningEffort: "max",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "max" {
		t.Fatalf("expected max effort on gpt-6-astra to be preserved (upstream supports max on gpt-6), got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortEmptyGPT6Untouched(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6-astra",
		ReasoningEffort: "",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "" {
		t.Fatalf("expected empty effort on gpt-6-astra to stay untouched (no default injection), got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNoneGPT6CCFormatRemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6-astra-turbo",
		ReasoningEffort: "none",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected none effort on gpt-6-astra-turbo to remap to low, got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNonePrefixedGPT6RemapsToLow(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "openai/gpt-6-astra",
		ReasoningEffort: "none",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "low" {
		t.Fatalf("expected none effort on openai/gpt-6-astra to remap to low, got %q", req.ReasoningEffort)
	}
}

// Counter-examples: no evidence these families reject "none", so nothing is rewritten.

func TestNormalizeCodexReasoningEffortNoneGPT61SolUntouched(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-6.1-sol",
		ReasoningEffort: "none",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "none" {
		t.Fatalf("expected none effort on gpt-6.1-sol to stay untouched (not gpt-6 family), got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNoneGPT55Untouched(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.5",
		ReasoningEffort: "none",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "none" {
		t.Fatalf("expected none effort on gpt-5.5 to stay untouched (not gpt-6 family), got %q", req.ReasoningEffort)
	}
}

func TestNormalizeCodexReasoningEffortNoneGPT60Untouched(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-60",
		ReasoningEffort: "none",
	}
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "none" {
		t.Fatalf("expected none effort on gpt-60 to stay untouched (adjacent name, not gpt-6 family), got %q", req.ReasoningEffort)
	}
}

func TestPrepareCodexRequestShapeMinimalReasoningEffortGPT56RemapsToLow(t *testing.T) {
	content := "hello"
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		RawAPIFormat:    transformerModel.APIFormatOpenAIResponse,
		ReasoningEffort: "minimal",
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.ReasoningEffort != "low" {
		t.Fatalf("expected reasoning.effort on gpt-5.6 to remap to low, got %q", req.ReasoningEffort)
	}
	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected reasoning.context to default to all_turns, got %q", req.ReasoningContext)
	}
}
