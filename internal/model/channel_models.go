package model

import (
	"strings"

	"github.com/bestruirui/octopus/internal/utils/xstrings"
)

// CleanOneMillionCapabilityModelName removes old UI-facing 1M suffix aliases.
// 1M is a channel capability (AnthropicContext1M), not part of the public model
// name that is shown in model pools, access plans, or channel selections.
func CleanOneMillionCapabilityModelName(modelName string) string {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "opus[1m]", "claude-opus-4-7[1m]", "claude-opus-4.7[1m]", "claude-opus-4-8[1m]", "claude-opus-4.8[1m]":
		return "claude-opus-4-8"
	case "fable[1m]", "claude-fable-5[1m]":
		return "claude-fable-5"
	}
	if strings.HasSuffix(lower, "[1m]") {
		return strings.TrimSpace(trimmed[:len(trimmed)-len("[1m]")])
	}
	return trimmed
}

func ModelNameWantsAnthropicContext1M(modelName string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	return strings.Contains(lower, "[1m]") && (strings.Contains(lower, "claude") || strings.Contains(lower, "opus") || strings.Contains(lower, "fable"))
}

func ModelNamesWantAnthropicContext1M(modelNames []string) bool {
	for _, modelName := range modelNames {
		if ModelNameWantsAnthropicContext1M(modelName) {
			return true
		}
	}
	return false
}

// NormalizeChannelModelNames trims and de-duplicates model names while
// preserving the administrator's ordering. Model selection is an authorization
// surface, so callers should never merge discovered provider catalogs here.
func NormalizeChannelModelNames(modelNames []string) []string {
	seen := make(map[string]struct{}, len(modelNames))
	result := make([]string, 0, len(modelNames))
	for _, modelName := range modelNames {
		name := CleanOneMillionCapabilityModelName(modelName)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, name)
	}
	return result
}

// ChannelSelectedModelNames returns the models explicitly enabled by an
// administrator. The legacy Model/CustomModel CSV fields remain as the
// compatibility view for old databases and older UI payloads.
func ChannelSelectedModelNames(channel Channel) []string {
	if len(channel.SelectedModels) > 0 {
		return NormalizeChannelModelNames(channel.SelectedModels)
	}
	return NormalizeChannelModelNames(xstrings.SplitTrimCompact(",", channel.Model, channel.CustomModel))
}

// ChannelDiscoveredModelNames returns synced upstream catalog candidates. These
// names are intentionally not callable until copied into SelectedModels.
func ChannelDiscoveredModelNames(channel Channel) []string {
	return NormalizeChannelModelNames(channel.DiscoveredModels)
}

func ChannelSelectedModelCSV(channel Channel) string {
	return strings.Join(ChannelSelectedModelNames(channel), ",")
}

func SplitChannelModelCSV(values ...string) []string {
	return NormalizeChannelModelNames(xstrings.SplitTrimCompact(",", values...))
}
