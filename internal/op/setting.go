package op

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
)

var settingCache = cache.New[model.SettingKey, string](16)

// settingWriteMu serializes every settings write against the settings-snapshot
// refresh. Without it, a refresh that read the pre-import snapshot can write its
// stale repair back OVER a value an admin saved while the refresh was in flight,
// and then publish that stale snapshot to the cache.
//
// Held by: SettingSetString / SettingSetInt, settingRefreshCache, and the whole
// settings part of an import (commit + snapshot publish). Never held by readers
// (SettingGet*) or by the relay hot path.
//
// ponytail: one process-wide lock. Octopus runs a single instance against one DB,
// so a finer-grained scheme would only add ways to get this wrong.
var settingWriteMu sync.Mutex

var settingLegacyDefaultUpgrades = map[model.SettingKey]map[string]string{
	model.SettingKeyClaudeHeaderUserAgent: {
		model.LegacyDefaultClaudeHeaderUserAgent2168:  model.DefaultClaudeHeaderUserAgent,
		model.LegacyDefaultClaudeHeaderUserAgent2126:  model.DefaultClaudeHeaderUserAgent,
		model.LegacyDefaultClaudeHeaderUserAgent2178:  model.DefaultClaudeHeaderUserAgent,
		model.LegacyDefaultClaudeHeaderUserAgent2198:  model.DefaultClaudeHeaderUserAgent,
		model.LegacyDefaultClaudeHeaderUserAgent21212: model.DefaultClaudeHeaderUserAgent,
	},
	model.SettingKeyClaudeHeaderPackage: {
		model.LegacyDefaultClaudeHeaderPackage0810: model.DefaultClaudeHeaderPackageVersion,
		model.LegacyDefaultClaudeHeaderPackage0940: model.DefaultClaudeHeaderPackageVersion,
	},
	model.SettingKeyClaudeHeaderRuntime: {
		model.LegacyDefaultClaudeHeaderRuntimeV2430: model.DefaultClaudeHeaderRuntimeVersion,
	},
	model.SettingKeyClaudeHeaderOS: {
		model.LegacyDefaultClaudeHeaderOSWindows: model.DefaultClaudeHeaderOS,
	},
	model.SettingKeyCodexHeaderUserAgent: {
		model.LegacyDefaultCodexHeaderUserAgent0133:        model.DefaultCodexHeaderUserAgent,
		model.LegacyDefaultCodexHeaderUserAgentCliRs0114:   model.DefaultCodexHeaderUserAgent,
		model.LegacyDefaultCodexHeaderUserAgent0132:        model.DefaultCodexHeaderUserAgent,
		model.LegacyDefaultCodexHeaderUserAgentExec0142Win: model.DefaultCodexHeaderUserAgent,
		model.LegacyDefaultCodexHeaderUserAgentCliRs0142:   model.DefaultCodexHeaderUserAgent,
		model.LegacyDefaultCodexHeaderUserAgentCliRs0144:   model.DefaultCodexHeaderUserAgent,
		model.LegacyDefaultCodexHeaderUserAgentCliRs0145:   model.DefaultCodexHeaderUserAgent,
	},
	model.SettingKeyCodexHeaderBetaFeatures: {
		model.LegacyDefaultCodexHeaderBetaFeaturesMultiAgent:           model.DefaultCodexHeaderBetaFeatures,
		model.LegacyDefaultCodexHeaderBetaFeaturesTerminalResizeReflow: model.DefaultCodexHeaderBetaFeatures,
	},
	model.SettingKeyRelayStreamDataTimeoutSec: {
		model.LegacyDefaultRelayStreamDataIntervalTimeoutSeconds: model.DefaultRelayStreamDataIntervalTimeoutSeconds,
	},
}

func SettingList(ctx context.Context) ([]model.Setting, error) {
	settings := make([]model.Setting, 0, settingCache.Len())
	for key, value := range settingCache.GetAll() {
		settings = append(settings, model.Setting{
			Key:   key,
			Value: value,
		})
	}
	return settings, nil
}

func SettingGetString(key model.SettingKey) (string, error) {
	setting, ok := settingCache.Get(key)
	if !ok {
		return "", fmt.Errorf("setting not found")
	}
	return setting, nil
}

func SettingSetString(key model.SettingKey, value string) error {
	settingWriteMu.Lock()
	defer settingWriteMu.Unlock()
	return settingSetStringLocked(key, value)
}

// settingSetStringLocked assumes settingWriteMu is held.
func settingSetStringLocked(key model.SettingKey, value string) error {
	if key == model.SettingKeyRouteModeOverride {
		if err := (&model.Setting{Key: key, Value: value}).Validate(); err != nil {
			return err
		}
		value = model.NormalizeRouteModeOverride(value)
	}
	if _, ok := settingCache.Get(key); !ok {
		return fmt.Errorf("setting not found")
	}
	// Always write through to the DB, even when the cache already holds this value.
	// A failed import can leave the DB and the cache disagreeing (DB imported, cache
	// refresh failed), and a cache-equality short circuit would then report success
	// while the DB kept the imported value.
	result := db.GetDB().Model(&model.Setting{Key: key}).Update("Value", value)
	if result.Error != nil {
		return fmt.Errorf("failed to set setting: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		// Some drivers report 0 rows for an UPDATE that stores the existing value, so a
		// missing row is the only real error: confirm before claiming the key is gone.
		var count int64
		if err := db.GetDB().Model(&model.Setting{}).Where("key = ?", key).Count(&count).Error; err != nil {
			return fmt.Errorf("failed to set setting: %w", err)
		}
		if count == 0 {
			return fmt.Errorf("failed to set setting, key not found")
		}
	}
	settingCache.Set(key, value)
	return nil
}

func SettingGetInt(key model.SettingKey) (int, error) {
	setting, ok := settingCache.Get(key)
	if !ok {
		return 0, fmt.Errorf("setting not found")
	}
	return strconv.Atoi(setting)
}

func SettingGetBool(key model.SettingKey) (bool, error) {
	setting, ok := settingCache.Get(key)
	if !ok {
		return false, fmt.Errorf("setting not found")
	}
	return strconv.ParseBool(setting)
}

func SettingSetInt(key model.SettingKey, value int) error {
	settingWriteMu.Lock()
	defer settingWriteMu.Unlock()
	return settingSetIntLocked(key, value)
}

// settingSetIntLocked assumes settingWriteMu is held.
func settingSetIntLocked(key model.SettingKey, value int) error {
	valueCache, ok := settingCache.Get(key)
	if !ok {
		return fmt.Errorf("setting not found")
	}
	// Keep the type guard: SettingSetInt on a key whose stored value is not an integer
	// must fail rather than quietly reinterpret it.
	if _, err := strconv.Atoi(valueCache); err != nil {
		return fmt.Errorf("failed to set setting: %w", err)
	}
	stored := strconv.Itoa(value)
	result := db.GetDB().Model(&model.Setting{Key: key}).Update("Value", stored)
	if result.Error != nil {
		return fmt.Errorf("failed to set setting: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := db.GetDB().Model(&model.Setting{}).Where("key = ?", key).Count(&count).Error; err != nil {
			return fmt.Errorf("failed to set setting: %w", err)
		}
		if count == 0 {
			return fmt.Errorf("failed to set setting, key not found")
		}
	}
	settingCache.Set(key, stored)
	return nil
}

func settingRefreshCache(ctx context.Context) error {
	settingWriteMu.Lock()
	defer settingWriteMu.Unlock()
	return settingRefreshCacheLocked(ctx)
}

// settingRefreshCacheLocked assumes settingWriteMu is held. It must stay that way:
// the SELECT, the legacy-value repair, and the ReplaceAll publish are one atomic
// step, otherwise a concurrent save is overwritten and the stale snapshot wins.
func settingRefreshCacheLocked(ctx context.Context) error {
	db := db.GetDB().WithContext(ctx)

	var settings []model.Setting
	if err := db.Find(&settings).Error; err != nil {
		return fmt.Errorf("failed to get settings: %w", err)
	}

	existingKeys := make(map[model.SettingKey]bool)
	for _, setting := range settings {
		existingKeys[setting.Key] = true
	}

	defaultSettings := model.DefaultSettings()
	missingSettings := make([]model.Setting, 0, len(defaultSettings))

	for _, defaultSetting := range defaultSettings {
		if !existingKeys[defaultSetting.Key] {
			missingSettings = append(missingSettings, defaultSetting)
		}
	}

	if len(missingSettings) > 0 {
		if err := db.CreateInBatches(missingSettings, len(missingSettings)).Error; err != nil {
			return fmt.Errorf("failed to create missing settings: %w", err)
		}
		settings = append(settings, missingSettings...)
	}

	for i := range settings {
		if settings[i].Key == model.SettingKeyRouteModeOverride {
			nextValue := model.NormalizeRouteModeOverride(settings[i].Value)
			if nextValue != settings[i].Value {
				result := db.Model(&model.Setting{Key: settings[i].Key}).Update("Value", nextValue)
				if result.Error != nil {
					return fmt.Errorf("failed to normalize route mode setting: %w", result.Error)
				}
				if result.RowsAffected == 0 {
					return fmt.Errorf("failed to normalize route mode setting: key not found")
				}
				settings[i].Value = nextValue
			}
		}
		replacements := settingLegacyDefaultUpgrades[settings[i].Key]
		if len(replacements) == 0 {
			continue
		}
		nextValue, ok := replacements[settings[i].Value]
		if !ok {
			continue
		}
		if err := db.Model(&model.Setting{Key: settings[i].Key}).Update("Value", nextValue).Error; err != nil {
			return fmt.Errorf("failed to upgrade legacy default setting %s: %w", settings[i].Key, err)
		}
		settings[i].Value = nextValue
	}

	snapshot := make(map[model.SettingKey]string, len(settings))
	for _, setting := range settings {
		snapshot[setting.Key] = setting.Value
	}
	settingCache.ReplaceAll(snapshot)
	return nil
}
