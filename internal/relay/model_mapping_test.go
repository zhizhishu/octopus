package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay/balancer"
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

	t.Run("resolveRuntimeModel returns mapped model and falls back to original", func(t *testing.T) {
		ch := &dbmodel.Channel{
			ModelMapping: map[string]string{"alias-model": "upstream-model"},
		}
		req := &transformerModel.InternalLLMRequest{Model: "alias-model"}
		ra := &relayAttempt{
			relayRequest: &relayRequest{requestModel: "alias-model", internalRequest: req},
			channel:      ch,
		}

		if got := ra.resolveRuntimeModel(); got != "upstream-model" {
			t.Fatalf("want resolveRuntimeModel=upstream-model, got %s", got)
		}

		// unmapped
		reqUnmapped := &transformerModel.InternalLLMRequest{Model: "other-model"}
		raUnmapped := &relayAttempt{
			relayRequest: &relayRequest{requestModel: "other-model", internalRequest: reqUnmapped},
			channel:      ch,
		}
		if got := raUnmapped.resolveRuntimeModel(); got != "other-model" {
			t.Fatalf("want resolveRuntimeModel=other-model, got %s", got)
		}
	})
}

// TestTelemetryReservationModelMappingAlignment verifies that when channel.ModelMapping
// maps an alias to an upstream model, BeginRuntimeAttempt and finish/RecordRuntimeSuccess/Failure
// use the same mapped upstream model key, leaving no residual reservation on the alias.
func TestTelemetryReservationModelMappingAlignment(t *testing.T) {
	t.Run("alias to upstream aligns begin and finish under upstream key", func(t *testing.T) {
		balancer.ResetRuntimeTelemetry()

		channelID := 101
		keyID := 202
		aliasModel := "client-alias"
		upstreamModel := "mapped-upstream"

		ch := &dbmodel.Channel{
			ID:           channelID,
			ModelMapping: map[string]string{aliasModel: upstreamModel},
		}
		req := &transformerModel.InternalLLMRequest{Model: aliasModel}
		ra := &relayAttempt{
			relayRequest: &relayRequest{requestModel: aliasModel, internalRequest: req},
			channel:      ch,
			usedKey:      dbmodel.ChannelKey{ID: keyID},
		}

		runtimeModel := ra.resolveRuntimeModel()
		if runtimeModel != upstreamModel {
			t.Fatalf("want resolveRuntimeModel=%s, got %s", upstreamModel, runtimeModel)
		}

		// Begin attempt
		finishAttempt := balancer.BeginRuntimeAttempt(ra.channel.ID, ra.usedKey.ID, runtimeModel)

		// Check snapshot under upstream key
		snapUpstream := balancer.SnapshotKeyRuntime(channelID, keyID, upstreamModel)
		if snapUpstream.InFlight != 1 {
			t.Fatalf("expected 1 in-flight under upstream key %s, got %d", upstreamModel, snapUpstream.InFlight)
		}

		// Check snapshot under alias key - must be 0
		snapAlias := balancer.SnapshotKeyRuntime(channelID, keyID, aliasModel)
		if snapAlias.InFlight != 0 {
			t.Fatalf("expected 0 in-flight under alias key %s, got %d", aliasModel, snapAlias.InFlight)
		}

		// Finish attempt
		finishAttempt()
		snapUpstreamAfter := balancer.SnapshotKeyRuntime(channelID, keyID, upstreamModel)
		if snapUpstreamAfter.InFlight != 0 {
			t.Fatalf("expected 0 in-flight under upstream key %s after finish, got %d", upstreamModel, snapUpstreamAfter.InFlight)
		}

		// RecordRuntimeSuccess under runtimeModel
		balancer.RecordRuntimeSuccess(ra.channel.ID, ra.usedKey.ID, runtimeModel, balancer.AttemptRuntimeMetrics{})
		snapUpstreamSuccess := balancer.SnapshotKeyRuntime(channelID, keyID, upstreamModel)
		if snapUpstreamSuccess.RequestSuccess != 1 {
			t.Fatalf("expected RequestSuccess=1 under upstream key, got %d", snapUpstreamSuccess.RequestSuccess)
		}
		snapAliasSuccess := balancer.SnapshotKeyRuntime(channelID, keyID, aliasModel)
		if snapAliasSuccess.RequestSuccess != 0 {
			t.Fatalf("expected RequestSuccess=0 under alias key, got %d", snapAliasSuccess.RequestSuccess)
		}
	})

	t.Run("unmapped model keeps original key for runtime telemetry", func(t *testing.T) {
		balancer.ResetRuntimeTelemetry()

		channelID := 102
		keyID := 203
		rawModel := "gpt-4o"

		ch := &dbmodel.Channel{
			ID:           channelID,
			ModelMapping: map[string]string{"other-alias": "other-upstream"},
		}
		req := &transformerModel.InternalLLMRequest{Model: rawModel}
		ra := &relayAttempt{
			relayRequest: &relayRequest{requestModel: rawModel, internalRequest: req},
			channel:      ch,
			usedKey:      dbmodel.ChannelKey{ID: keyID},
		}

		runtimeModel := ra.resolveRuntimeModel()
		if runtimeModel != rawModel {
			t.Fatalf("want resolveRuntimeModel=%s, got %s", rawModel, runtimeModel)
		}

		finishAttempt := balancer.BeginRuntimeAttempt(ra.channel.ID, ra.usedKey.ID, runtimeModel)
		snap := balancer.SnapshotKeyRuntime(channelID, keyID, rawModel)
		if snap.InFlight != 1 {
			t.Fatalf("expected 1 in-flight under raw key %s, got %d", rawModel, snap.InFlight)
		}
		finishAttempt()
		snapAfter := balancer.SnapshotKeyRuntime(channelID, keyID, rawModel)
		if snapAfter.InFlight != 0 {
			t.Fatalf("expected 0 in-flight under raw key %s after finish, got %d", rawModel, snapAfter.InFlight)
		}
	})
}
