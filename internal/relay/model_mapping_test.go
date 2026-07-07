package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// TestApplyModelMapping verifies that applyModelMapping remaps internalRequest.Model
// to the upstream name when a matching entry exists in channel.ModelMapping, and
// that ra.requestModel (the client-visible name) is never modified.
func TestApplyModelMapping(t *testing.T) {
	t.Run("mapped model is replaced with upstream name", func(t *testing.T) {
		ch := &dbmodel.Channel{
			ModelMapping: map[string]string{"glm-5.2": "z-ai/glm-5.2"},
		}
		req := &transformerModel.InternalLLMRequest{Model: "glm-5.2"}
		ra := &relayAttempt{
			relayRequest: &relayRequest{requestModel: "glm-5.2", internalRequest: req},
			channel:      ch,
		}

		ra.applyModelMapping()

		if ra.internalRequest.Model != "z-ai/glm-5.2" {
			t.Errorf("want internalRequest.Model=z-ai/glm-5.2, got %s", ra.internalRequest.Model)
		}
		if !ra.modelMapped {
			t.Error("want modelMapped=true after mapping applied")
		}
		// client-visible name must never change
		if ra.requestModel != "glm-5.2" {
			t.Errorf("requestModel must not change, got %s", ra.requestModel)
		}
	})

	t.Run("model not in mapping is unchanged", func(t *testing.T) {
		ch := &dbmodel.Channel{
			ModelMapping: map[string]string{"glm-5.2": "z-ai/glm-5.2"},
		}
		req := &transformerModel.InternalLLMRequest{Model: "gpt-4o"}
		ra := &relayAttempt{
			relayRequest: &relayRequest{requestModel: "gpt-4o", internalRequest: req},
			channel:      ch,
		}

		ra.applyModelMapping()

		if ra.internalRequest.Model != "gpt-4o" {
			t.Errorf("unmapped model must stay gpt-4o, got %s", ra.internalRequest.Model)
		}
		if ra.modelMapped {
			t.Error("want modelMapped=false for model not in mapping")
		}
	})

	t.Run("nil/empty mapping is a no-op", func(t *testing.T) {
		ch := &dbmodel.Channel{} // ModelMapping is nil
		req := &transformerModel.InternalLLMRequest{Model: "glm-5.2"}
		ra := &relayAttempt{
			relayRequest: &relayRequest{requestModel: "glm-5.2", internalRequest: req},
			channel:      ch,
		}

		ra.applyModelMapping()

		if ra.internalRequest.Model != "glm-5.2" {
			t.Errorf("nil mapping: model must stay glm-5.2, got %s", ra.internalRequest.Model)
		}
		if ra.modelMapped {
			t.Error("want modelMapped=false for nil mapping")
		}
	})

	t.Run("empty-string upstream value is treated as absent", func(t *testing.T) {
		ch := &dbmodel.Channel{
			ModelMapping: map[string]string{"glm-5.2": ""},
		}
		req := &transformerModel.InternalLLMRequest{Model: "glm-5.2"}
		ra := &relayAttempt{
			relayRequest: &relayRequest{requestModel: "glm-5.2", internalRequest: req},
			channel:      ch,
		}

		ra.applyModelMapping()

		if ra.internalRequest.Model != "glm-5.2" {
			t.Errorf("empty upstream value: model must stay glm-5.2, got %s", ra.internalRequest.Model)
		}
		if ra.modelMapped {
			t.Error("want modelMapped=false when upstream value is empty string")
		}
	})
}
