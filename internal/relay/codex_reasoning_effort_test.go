package relay

import (
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
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