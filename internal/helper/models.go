package helper

import (
	"strings"

	"github.com/bestruirui/octopus/internal/utils/xstrings"
)

// ReconcileSelectedModelsWithFetched keeps only models that were already selected.
// Fetching provider catalogs must never opt users into additional models.
func ReconcileSelectedModelsWithFetched(selectedModels []string, fetchedModels []string) ([]string, []string, bool) {
	cleanSelected := xstrings.TrimCompact(selectedModels)
	if len(cleanSelected) == 0 {
		return nil, nil, false
	}

	fetchedExact := make(map[string]string, len(fetchedModels))
	fetchedFolded := make(map[string]string, len(fetchedModels))
	for _, modelName := range xstrings.TrimCompact(fetchedModels) {
		if _, ok := fetchedExact[modelName]; !ok {
			fetchedExact[modelName] = modelName
		}
		folded := strings.ToLower(modelName)
		if _, ok := fetchedFolded[folded]; !ok {
			fetchedFolded[folded] = modelName
		}
	}

	retained := make([]string, 0, len(cleanSelected))
	deleted := make([]string, 0)
	seenRetained := make(map[string]struct{}, len(cleanSelected))
	for _, selected := range cleanSelected {
		modelName, ok := fetchedExact[selected]
		if !ok {
			modelName, ok = fetchedFolded[strings.ToLower(selected)]
		}
		if !ok {
			deleted = append(deleted, selected)
			continue
		}
		if _, exists := seenRetained[modelName]; exists {
			continue
		}
		seenRetained[modelName] = struct{}{}
		retained = append(retained, modelName)
	}

	return retained, deleted, !sameStringSlice(cleanSelected, retained)
}

func sameStringSlice(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
