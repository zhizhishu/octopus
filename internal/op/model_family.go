package op

import (
	"context"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func FilterModelNamesForEndpointFamily(ctx context.Context, names []string, family model.APIKeyEndpointFamily) []string {
	normalizedFamily, ok := model.NormalizeAPIKeyEndpointFamily(family)
	if !ok || len(names) == 0 {
		return names
	}

	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			allowed[strings.ToLower(trimmed)] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nil
	}

	visible := make(map[string]struct{}, len(names))
	for _, group := range groupCache.GetAll() {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(group.Name))]; !ok {
			continue
		}
		for _, item := range group.Items {
			channel, ok := channelCache.Get(item.ChannelID)
			if !ok || !channel.Enabled {
				continue
			}
			if !channelTypeMatchesEndpointFamily(channel.Type, normalizedFamily) {
				continue
			}
			visible[strings.ToLower(strings.TrimSpace(group.Name))] = struct{}{}
			break
		}
	}

	// Access-plan route targets can make a request model callable even when
	// there is no same-name group fallback. Respect that, but still require an
	// enabled target on a selected model in the matching endpoint family.
	if err := ensureAccessPlanCache(ctx); err == nil {
		for _, plan := range accessPlanCache.GetAll() {
			if !plan.Enabled || plan.RouteProfile == nil {
				continue
			}
			for _, rule := range plan.RouteProfile.Rules {
				requestName := strings.ToLower(strings.TrimSpace(rule.RequestModel))
				if _, ok := allowed[requestName]; !ok {
					continue
				}
				for _, target := range rule.Targets {
					channel, ok := channelCache.Get(target.ChannelID)
					if !ok || !channelTypeMatchesEndpointFamily(channel.Type, normalizedFamily) {
						continue
					}
					if accessRouteTargetAvailable(target) {
						visible[requestName] = struct{}{}
						break
					}
				}
			}
		}
	}

	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := visible[strings.ToLower(strings.TrimSpace(name))]; ok {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

func channelTypeMatchesEndpointFamily(channelType outbound.OutboundType, family model.APIKeyEndpointFamily) bool {
	switch family {
	case model.APIKeyEndpointFamilyAnthropic:
		return channelType == outbound.OutboundTypeAnthropic
	case model.APIKeyEndpointFamilyGemini:
		return channelType == outbound.OutboundTypeGemini
	case model.APIKeyEndpointFamilyOpenAICompatible:
		switch channelType {
		case outbound.OutboundTypeOpenAIChat,
			outbound.OutboundTypeOpenAIResponse,
			outbound.OutboundTypeOpenAIEmbedding,
			outbound.OutboundTypeCustomOpenAIChat,
			outbound.OutboundTypeVolcengine:
			return true
		default:
			return false
		}
	default:
		return true
	}
}
