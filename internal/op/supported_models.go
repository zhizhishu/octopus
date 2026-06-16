package op

import (
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
)

func SupportedModelsList(value string) []string {
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, part := range strings.Split(value, ",") {
		name := dbmodel.CleanOneMillionCapabilityModelName(part)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, name)
	}
	return models
}

func IsModelSupported(supportedModels string, modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if strings.TrimSpace(supportedModels) == "" {
		return true
	}
	if modelName == "" {
		return false
	}
	for _, allowed := range SupportedModelsList(supportedModels) {
		if supportedModelMatches(allowed, modelName) {
			return true
		}
	}
	return false
}

func supportedModelMatches(allowed string, requested string) bool {
	allowed = strings.TrimSpace(allowed)
	requested = dbmodel.CleanOneMillionCapabilityModelName(requested)
	allowed = dbmodel.CleanOneMillionCapabilityModelName(allowed)
	if strings.EqualFold(allowed, requested) {
		return true
	}

	return aliasSetIntersects(modelSupportAliases(allowed), modelSupportAliases(requested))
}

func modelSupportAliases(modelName string) []string {
	seen := make(map[string]struct{})
	aliases := make([]string, 0, 4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		aliases = append(aliases, value)
	}

	add(modelName)
	add(transformermodel.NormalizeAnthropicModelAlias(modelName))
	for _, candidate := range transformermodel.AnthropicModelAliasCandidates(modelName) {
		add(candidate)
	}
	return aliases
}

func aliasSetIntersects(left []string, right []string) bool {
	for _, l := range left {
		for _, r := range right {
			if strings.EqualFold(l, r) {
				return true
			}
		}
	}
	return false
}
