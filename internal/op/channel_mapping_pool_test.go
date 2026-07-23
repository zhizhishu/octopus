package op

import (
	"slices"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// TestChannelDeletedModelNamesIncludesRemovedMappingAlias locks the fix where a
// model_mapping alias KEY is a routable pool (ChannelEnsureModelGroups registers it),
// so removing the alias must evict its now-stale pool item. Before the fix,
// channelDeletedModelNames only diffed selected_models, leaving a removed alias as a
// stale pool member that routes to the channel yet no longer gets rewritten upstream.
func TestChannelDeletedModelNamesIncludesRemovedMappingAlias(t *testing.T) {
	old := model.Channel{
		SelectedModels: []string{"z-ai/glm-5.2"},
		ModelMapping:   map[string]string{"glm-5.2": "z-ai/glm-5.2"},
	}
	newSelected := []string{"z-ai/glm-5.2"}
	newMapping := map[string]string{} // alias removed, selected model kept
	req := &model.ChannelUpdateRequest{
		ID:             1,
		SelectedModels: &newSelected,
		ModelMapping:   &newMapping,
	}

	deleted := channelDeletedModelNames(old, req)

	if !slices.Contains(deleted, "glm-5.2") {
		t.Fatalf("removed mapping alias %q must be evicted, got %v", "glm-5.2", deleted)
	}
	if slices.Contains(deleted, "z-ai/glm-5.2") {
		t.Fatalf("still-selected model %q must NOT be evicted, got %v", "z-ai/glm-5.2", deleted)
	}
}

// TestChannelDeletedModelNamesKeepsAliasStillMapped ensures a mapping alias that is
// still configured is NOT evicted on an unrelated update.
func TestChannelDeletedModelNamesKeepsAliasStillMapped(t *testing.T) {
	old := model.Channel{
		SelectedModels: []string{"z-ai/glm-5.2"},
		ModelMapping:   map[string]string{"glm-5.2": "z-ai/glm-5.2"},
	}
	sameSelected := []string{"z-ai/glm-5.2"}
	sameMapping := map[string]string{"glm-5.2": "z-ai/glm-5.2"}
	req := &model.ChannelUpdateRequest{
		ID:             1,
		SelectedModels: &sameSelected,
		ModelMapping:   &sameMapping,
	}

	if deleted := channelDeletedModelNames(old, req); len(deleted) != 0 {
		t.Fatalf("no models changed, expected zero deletions, got %v", deleted)
	}
}
