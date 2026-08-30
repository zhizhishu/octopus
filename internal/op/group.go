package op

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
)

// The model pool (groups/group_items tables, CRUD, and cache) has been removed in
// favour of the access-plan canvas. What remains here is the served-model listing
// (for /v1/models and friends) and the runtime fallback that resolves a request
// model to its enabled channels when no access-plan rule covers it.

func GroupListModelForAPIKey(apiKeyID int, ctx context.Context) ([]string, error) {
	models, seen := channelServedModelNames()
	routeModels, err := AccessPlanRouteModelsForAPIKey(apiKeyID, ctx)
	if err != nil {
		return nil, err
	}
	for _, modelName := range routeModels {
		modelName = model.CleanOneMillionCapabilityModelName(modelName)
		key := strings.ToLower(modelName)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, modelName)
	}
	sort.Strings(models)
	return models, nil
}

func GroupListModelForAPIKeyPlan(apiKeyID int, headerPlan string, ctx context.Context) ([]string, error) {
	if strings.TrimSpace(headerPlan) == "" {
		return GroupListModelForAPIKey(apiKeyID, ctx)
	}

	models, seen := channelServedModelNames()
	plan, err := AccessPlanSelect(apiKeyID, headerPlan, ctx)
	if err != nil {
		return nil, err
	}
	routeModels := AccessPlanRouteModelsForPlan(plan)
	for _, modelName := range routeModels {
		modelName = model.CleanOneMillionCapabilityModelName(modelName)
		key := strings.ToLower(modelName)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, modelName)
	}
	sort.Strings(models)
	return models, nil
}

func GroupGetEnabledMap(name string, ctx context.Context) (model.Group, error) {
	// With the pool gone every request model resolves through the channel-served
	// fallback below. Mode is left 0 so the fleet-wide route_mode_override (global
	// default) governs routing for un-planned models.
	return groupFallbackFromChannels(name)
}

// channelServedModelNames returns every client-facing model name currently served by
// an enabled channel: plainly selected models plus model_mapping aliases whose mapped
// upstream is itself selected. This is the served set the /v1/models listing starts
// from; callers layer access-plan route targets on top.
func channelServedModelNames() ([]string, map[string]struct{}) {
	models := []string{}
	seen := make(map[string]struct{})
	for _, ch := range channelCache.GetAll() {
		if !ch.Enabled {
			continue
		}
		for _, selected := range model.ChannelSelectedModelNames(ch) {
			name := model.CleanOneMillionCapabilityModelName(selected)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, name)
		}
		for clientName := range ch.ModelMapping {
			clean := model.CleanOneMillionCapabilityModelName(clientName)
			if clean == "" || !channelServesModel(ch, clean) {
				continue
			}
			key := strings.ToLower(clean)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, clean)
		}
	}
	return models, seen
}

// groupFallbackFromChannels builds the model-pool-free fallback Group for a request
// model that no access-plan rule covers: every currently-enabled channel that serves
// the model becomes a candidate, respecting the channel's own priority. Each item
// keeps the client-facing (alias) name so applyModelMapping still rewrites it to the
// upstream name on the wire — the same flow auto-created pool items used. Mode stays
// 0 so the fleet-wide route_mode_override (global default) governs routing, exactly
// like an auto-created pool group.
func groupFallbackFromChannels(name string) (model.Group, error) {
	clean := model.CleanOneMillionCapabilityModelName(name)
	if clean == "" {
		return model.Group{}, fmt.Errorf("model not found")
	}

	items := make([]model.GroupItem, 0)
	for _, ch := range channelCache.GetAll() {
		if !ch.Enabled || !channelServesModel(ch, clean) {
			continue
		}
		items = append(items, model.GroupItem{
			ChannelID:     ch.ID,
			ModelName:     clean,
			Priority:      ch.Priority,
			Weight:        1,
			RoutingWeight: 1,
		})
	}
	if len(items) == 0 {
		return model.Group{}, fmt.Errorf("model not found")
	}

	// Deterministic candidate order: channel priority asc, then channel ID asc. The
	// relay's enrich step re-reads ChannelPriority from the channel, so this only sets
	// the same value as the secondary key for fail-first ordering.
	sort.Slice(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		return items[i].ChannelID < items[j].ChannelID
	})

	return model.Group{Name: clean, Items: items}, nil
}

// channelServesModel reports whether a channel serves the cleaned, client-facing
// model name — either as a plainly-selected model, or via an explicit model_mapping
// alias whose mapped upstream is itself a selected model. This mirrors the served
// set AccessPlanSyncEnabledChannels computes so the listing and the canvas agree on
// who serves what.
func channelServesModel(ch model.Channel, clean string) bool {
	for _, selected := range model.ChannelSelectedModelNames(ch) {
		if strings.EqualFold(model.CleanOneMillionCapabilityModelName(selected), clean) {
			return true
		}
	}
	for clientName, upstreamName := range ch.ModelMapping {
		if !strings.EqualFold(model.CleanOneMillionCapabilityModelName(clientName), clean) {
			continue
		}
		upstreamClean := model.CleanOneMillionCapabilityModelName(upstreamName)
		if upstreamClean == "" {
			continue
		}
		for _, selected := range model.ChannelSelectedModelNames(ch) {
			if strings.EqualFold(model.CleanOneMillionCapabilityModelName(selected), upstreamClean) {
				return true
			}
		}
	}
	return false
}
