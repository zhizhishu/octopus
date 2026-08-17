package op

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"github.com/bestruirui/octopus/internal/utils/diff"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/xstrings"
)

var channelCache = cache.New[int, model.Channel](16)
var channelKeyCache = cache.New[int, model.ChannelKey](16)
var channelKeyCacheNeedUpdate = make(map[int]struct{})
var channelKeyCacheNeedUpdateLock sync.Mutex

func ChannelList(ctx context.Context) ([]model.Channel, error) {
	channels := make([]model.Channel, 0, channelCache.Len())
	for _, channel := range channelCache.GetAll() {
		channels = append(channels, channel)
	}
	return channels, nil
}

func ChannelCreate(channel *model.Channel, ctx context.Context) error {
	normalizeChannelCreateModels(channel)
	if err := db.GetDB().WithContext(ctx).Create(channel).Error; err != nil {
		return err
	}
	channelCache.Set(channel.ID, *channel)
	for _, k := range channel.Keys {
		if k.ID != 0 {
			channelKeyCache.Set(k.ID, k)
		}
	}
	return nil
}

func normalizeChannelCreateModels(channel *model.Channel) {
	if channel == nil {
		return
	}
	rawSelectedModels := append([]string(nil), channel.SelectedModels...)
	rawLegacyModels := xstrings.SplitTrimCompact(",", channel.Model, channel.CustomModel)
	channel.DiscoveredModels = model.NormalizeChannelModelNames(channel.DiscoveredModels)
	if len(channel.SelectedModels) > 0 {
		channel.SelectedModels = model.NormalizeChannelModelNames(channel.SelectedModels)
		channel.Model = strings.Join(channel.SelectedModels, ",")
		channel.CustomModel = ""
	} else {
		channel.SelectedModels = model.ChannelSelectedModelNames(*channel)
	}
	if channel.Type == outbound.OutboundTypeAnthropic &&
		(model.ModelNamesWantAnthropicContext1M(rawSelectedModels) ||
			model.ModelNamesWantAnthropicContext1M(rawLegacyModels)) {
		channel.AnthropicContext1M = true
	}
}

func channelSelectionAfterLegacyUpdate(oldChannel model.Channel, req *model.ChannelUpdateRequest) []string {
	if req == nil {
		return model.ChannelSelectedModelNames(oldChannel)
	}
	nextModel := oldChannel.Model
	if req.Model != nil {
		nextModel = *req.Model
	}
	nextCustomModel := oldChannel.CustomModel
	if req.CustomModel != nil {
		nextCustomModel = *req.CustomModel
	}
	return model.SplitChannelModelCSV(nextModel, nextCustomModel)
}

// ChannelKeyUpdate 仅更新 ChannelKey 的内存缓存（不落库），并标记为需要在 SaveCache 时写入数据库。
func ChannelKeyUpdate(key model.ChannelKey) error {
	if key.ID == 0 || key.ChannelID == 0 {
		return fmt.Errorf("invalid channel key")
	}
	if err := channelKeyCacheSet(key); err != nil {
		return err
	}
	markChannelKeyDirty(key.ID)
	return nil
}

// ChannelKeyRecordUse records runtime key status and cost increments atomically.
func ChannelKeyRecordUse(key model.ChannelKey, statusCode int, usedAt int64, costDelta float64) error {
	if key.ID == 0 || key.ChannelID == 0 {
		return fmt.Errorf("invalid channel key")
	}
	updated, _ := channelKeyCache.Update(key.ID, func(current model.ChannelKey, exists bool) (model.ChannelKey, bool) {
		if !exists {
			current = key
		}
		current.StatusCode = statusCode
		current.LastUseTimeStamp = usedAt
		current.TotalCost += costDelta
		// Quarantine bookkeeping: a 401 means the key itself is bad (invalid/revoked),
		// so record why + when for operator visibility (and persistence across restart).
		// Any healthy 2xx clears it — the key self-heals after the auth re-probe.
		switch {
		case statusCode == 401:
			current.DisabledReason = "auth error 401 (key likely invalid/revoked)"
			current.DisabledAt = usedAt
		case statusCode >= 200 && statusCode < 300:
			current.DisabledReason = ""
			current.DisabledAt = 0
		}
		return current, true
	})
	if err := channelCacheSetKey(key.ChannelID, updated); err != nil {
		return err
	}
	markChannelKeyDirty(key.ID)
	return nil
}

func channelKeyCacheSet(key model.ChannelKey) error {
	if err := channelCacheSetKey(key.ChannelID, key); err != nil {
		return err
	}
	channelKeyCache.Set(key.ID, key)
	return nil
}

func channelCacheSetKey(channelID int, key model.ChannelKey) error {
	_, ok := channelCache.Update(channelID, func(ch model.Channel, exists bool) (model.Channel, bool) {
		if !exists {
			return ch, false
		}
		keys := make([]model.ChannelKey, len(ch.Keys))
		copy(keys, ch.Keys)
		found := false
		for i := range keys {
			if keys[i].ID == key.ID {
				keys[i] = key
				found = true
				break
			}
		}
		if !found {
			keys = append(keys, key)
		}
		ch.Keys = keys
		return ch, true
	})
	if !ok {
		return fmt.Errorf("channel not found")
	}
	return nil
}

func markChannelKeyDirty(id int) {
	channelKeyCacheNeedUpdateLock.Lock()
	channelKeyCacheNeedUpdate[id] = struct{}{}
	channelKeyCacheNeedUpdateLock.Unlock()
}

func ChannelBaseUrlUpdate(channelID int, baseUrl []model.BaseUrl) error {
	ch, ok := channelCache.Get(channelID)
	if !ok {
		return fmt.Errorf("channel not found")
	}
	// Copy to decouple callers from internal cache storage.
	if baseUrl == nil {
		ch.BaseUrls = nil
	} else {
		cp := make([]model.BaseUrl, len(baseUrl))
		copy(cp, baseUrl)
		ch.BaseUrls = cp
	}
	channelCache.Set(channelID, ch)
	return nil
}

// ChannelKeySaveDB 将运行时更新过的 ChannelKey 缓存写入数据库。
func ChannelKeySaveDB(ctx context.Context) error {
	channelKeyCacheNeedUpdateLock.Lock()
	keyIDs := make([]int, 0, len(channelKeyCacheNeedUpdate))
	for id := range channelKeyCacheNeedUpdate {
		keyIDs = append(keyIDs, id)
	}
	channelKeyCacheNeedUpdate = make(map[int]struct{})
	channelKeyCacheNeedUpdateLock.Unlock()

	if len(keyIDs) == 0 {
		return nil
	}

	dbConn := db.GetDB().WithContext(ctx)
	for _, id := range keyIDs {
		k, ok := channelKeyCache.Get(id)
		if !ok {
			continue
		}
		if err := dbConn.Save(&k).Error; err != nil {
			remarkChannelKeysDirty(keyIDs)
			return err
		}
	}
	return nil
}

func remarkChannelKeysDirty(ids []int) {
	channelKeyCacheNeedUpdateLock.Lock()
	defer channelKeyCacheNeedUpdateLock.Unlock()
	for _, id := range ids {
		channelKeyCacheNeedUpdate[id] = struct{}{}
	}
}

func ChannelUpdate(req *model.ChannelUpdateRequest, ctx context.Context) (*model.Channel, error) {
	if req == nil {
		return nil, fmt.Errorf("channel update request is nil")
	}
	oldChannel, ok := channelCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}
	deletedModelNames := channelDeletedModelNames(oldChannel, req)
	resetRuntimeStats := channelRuntimeIdentityChanged(oldChannel, req)

	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var selectFields []string
	updates := model.Channel{ID: req.ID}

	if req.Name != nil {
		selectFields = append(selectFields, "name")
		updates.Name = *req.Name
	}
	if req.Type != nil {
		selectFields = append(selectFields, "type")
		updates.Type = *req.Type
	}
	if req.Enabled != nil {
		selectFields = append(selectFields, "enabled")
		updates.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		selectFields = append(selectFields, "priority")
		updates.Priority = *req.Priority
	}
	if req.BaseUrls != nil {
		selectFields = append(selectFields, "base_urls")
		updates.BaseUrls = *req.BaseUrls
	}
	nextType := oldChannel.Type
	if req.Type != nil {
		nextType = *req.Type
	}
	rawSelectedModels := []string(nil)
	if req.Model != nil && req.SelectedModels == nil {
		selectFields = append(selectFields, "model")
		updates.Model = *req.Model
		rawSelectedModels = append(rawSelectedModels, xstrings.SplitTrimCompact(",", *req.Model)...)
	}
	if req.CustomModel != nil && req.SelectedModels == nil {
		selectFields = append(selectFields, "custom_model")
		updates.CustomModel = *req.CustomModel
		rawSelectedModels = append(rawSelectedModels, xstrings.SplitTrimCompact(",", *req.CustomModel)...)
	}
	if req.SelectedModels != nil {
		rawSelectedModels = append(rawSelectedModels, *req.SelectedModels...)
		selected := model.NormalizeChannelModelNames(*req.SelectedModels)
		selectFields = append(selectFields, "selected_models", "model", "custom_model")
		updates.SelectedModels = selected
		updates.Model = strings.Join(selected, ",")
		updates.CustomModel = ""
	} else if req.Model != nil || req.CustomModel != nil {
		selected := channelSelectionAfterLegacyUpdate(oldChannel, req)
		selectFields = append(selectFields, "selected_models")
		updates.SelectedModels = selected
	}
	if req.DiscoveredModels != nil {
		selectFields = append(selectFields, "discovered_models")
		updates.DiscoveredModels = model.NormalizeChannelModelNames(*req.DiscoveredModels)
	}
	if req.AnthropicContext1M != nil {
		selectFields = append(selectFields, "anthropic_context_1m")
		updates.AnthropicContext1M = *req.AnthropicContext1M
	} else if nextType == outbound.OutboundTypeAnthropic && model.ModelNamesWantAnthropicContext1M(rawSelectedModels) {
		selectFields = append(selectFields, "anthropic_context_1m")
		updates.AnthropicContext1M = true
	}
	if req.ThinkingToContent != nil {
		selectFields = append(selectFields, "thinking_to_content")
		updates.ThinkingToContent = *req.ThinkingToContent
	}
	if req.MaxConcurrent != nil {
		selectFields = append(selectFields, "max_concurrent")
		updates.MaxConcurrent = *req.MaxConcurrent
	}
	if req.RPMLimit != nil {
		selectFields = append(selectFields, "rpm_limit")
		updates.RPMLimit = *req.RPMLimit
	}
	if req.KeySelectStrategy != nil {
		selectFields = append(selectFields, "key_select_strategy")
		updates.KeySelectStrategy = *req.KeySelectStrategy
	}
	if req.DisableCircuitBreaker != nil {
		selectFields = append(selectFields, "disable_circuit_breaker")
		updates.DisableCircuitBreaker = *req.DisableCircuitBreaker
	}
	if req.Proxy != nil {
		selectFields = append(selectFields, "proxy")
		updates.Proxy = *req.Proxy
	}
	if req.AutoSync != nil {
		selectFields = append(selectFields, "auto_sync")
		updates.AutoSync = *req.AutoSync
	}
	if req.AutoGroup != nil {
		selectFields = append(selectFields, "auto_group")
		updates.AutoGroup = *req.AutoGroup
	}
	if req.CustomHeader != nil {
		selectFields = append(selectFields, "custom_header")
		updates.CustomHeader = *req.CustomHeader
	}
	if req.Cloak != nil {
		selectFields = append(selectFields, "cloak")
		updates.Cloak = *req.Cloak
	}
	if req.ChannelProxy != nil {
		selectFields = append(selectFields, "channel_proxy")
		updates.ChannelProxy = req.ChannelProxy
	}
	if req.ParamOverride != nil {
		selectFields = append(selectFields, "param_override")
		updates.ParamOverride = req.ParamOverride
	}
	if req.SystemPromptOverride != nil {
		selectFields = append(selectFields, "system_prompt_override")
		updates.SystemPromptOverride = *req.SystemPromptOverride
	}
	if req.PromptOverrideMode != nil {
		selectFields = append(selectFields, "prompt_override_mode")
		updates.PromptOverrideMode = *req.PromptOverrideMode
	}
	if req.MatchRegex != nil {
		selectFields = append(selectFields, "match_regex")
		updates.MatchRegex = req.MatchRegex
	}
	if req.ModelMapping != nil {
		selectFields = append(selectFields, "model_mapping")
		updates.ModelMapping = *req.ModelMapping
	}
	if req.OpenAIChatPath != nil {
		selectFields = append(selectFields, "open_ai_chat_path")
		updates.OpenAIChatPath = *req.OpenAIChatPath
	}
	if req.OpenAIModelsPath != nil {
		selectFields = append(selectFields, "open_ai_models_path")
		updates.OpenAIModelsPath = *req.OpenAIModelsPath
	}

	// 只有当有字段需要更新时才执行 UPDATE
	if len(selectFields) > 0 {
		if err := tx.Model(&model.Channel{}).Where("id = ?", req.ID).Select(selectFields).Updates(&updates).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to update channel: %w", err)
		}
	}

	// 删除 keys
	if len(req.KeysToDelete) > 0 {
		if err := tx.Where("id IN ? AND channel_id = ?", req.KeysToDelete, req.ID).Delete(&model.ChannelKey{}).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to delete channel keys: %w", err)
		}
	}

	// 更新 keys（逐条，只更新提供的字段）
	if len(req.KeysToUpdate) > 0 {
		for _, ku := range req.KeysToUpdate {
			updates := map[string]interface{}{}
			if ku.Enabled != nil {
				updates["enabled"] = *ku.Enabled
			}
			if ku.ChannelKey != nil {
				updates["channel_key"] = *ku.ChannelKey
			}
			if ku.Remark != nil {
				updates["remark"] = *ku.Remark
			}
			if len(updates) == 0 {
				continue
			}
			if err := tx.Model(&model.ChannelKey{}).
				Where("id = ? AND channel_id = ?", ku.ID, req.ID).
				Updates(updates).Error; err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("failed to update channel key %d: %w", ku.ID, err)
			}
		}
	}

	// 新增 keys
	if len(req.KeysToAdd) > 0 {
		newKeys := make([]model.ChannelKey, 0, len(req.KeysToAdd))
		for _, ka := range req.KeysToAdd {
			newKeys = append(newKeys, model.ChannelKey{
				ChannelID:  req.ID,
				Enabled:    ka.Enabled,
				ChannelKey: ka.ChannelKey,
				Remark:     ka.Remark,
			})
		}
		if err := tx.Create(&newKeys).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to create channel keys: %w", err)
		}
	}

	if resetRuntimeStats {
		if err := tx.Model(&model.ChannelKey{}).
			Where("channel_id = ?", req.ID).
			Updates(map[string]interface{}{
				"status_code":         0,
				"last_use_time_stamp": 0,
				"total_cost":          0,
			}).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to reset channel key runtime stats: %w", err)
		}
		if err := tx.Where("channel_id = ?", req.ID).Delete(&model.StatsChannel{}).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to reset channel stats: %w", err)
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	if resetRuntimeStats {
		resetStatsChannelCache(req.ID)
	}

	if len(deletedModelNames) > 0 {
		keys := make([]model.GroupIDAndLLMName, 0, len(deletedModelNames))
		for _, modelName := range deletedModelNames {
			keys = append(keys, model.GroupIDAndLLMName{
				ChannelID: req.ID,
				ModelName: modelName,
			})
		}
		if err := GroupItemBatchDelByChannelAndModels(keys, ctx); err != nil {
			log.Warnf("failed to delete group items for removed channel models (channel=%d): %v", req.ID, err)
		}
	}

	// 刷新缓存并返回最新数据
	if err := channelRefreshCacheByID(req.ID, ctx); err != nil {
		return nil, err
	}

	channel, _ := channelCache.Get(req.ID)
	return &channel, nil
}

func channelRuntimeIdentityChanged(oldChannel model.Channel, req *model.ChannelUpdateRequest) bool {
	if req == nil {
		return false
	}
	if req.Type != nil && *req.Type != oldChannel.Type {
		return true
	}
	if req.BaseUrls != nil && !reflect.DeepEqual(normalizeBaseUrlsForCompare(*req.BaseUrls), normalizeBaseUrlsForCompare(oldChannel.BaseUrls)) {
		return true
	}
	if req.OpenAIChatPath != nil && strings.TrimSpace(*req.OpenAIChatPath) != strings.TrimSpace(oldChannel.OpenAIChatPath) {
		return true
	}
	if req.OpenAIModelsPath != nil && strings.TrimSpace(*req.OpenAIModelsPath) != strings.TrimSpace(oldChannel.OpenAIModelsPath) {
		return true
	}
	if req.CustomHeader != nil && !reflect.DeepEqual(normalizeHeadersForCompare(*req.CustomHeader), normalizeHeadersForCompare(oldChannel.CustomHeader)) {
		return true
	}
	if len(req.KeysToAdd) > 0 || len(req.KeysToDelete) > 0 {
		return true
	}
	if len(req.KeysToUpdate) > 0 {
		oldKeysByID := make(map[int]string, len(oldChannel.Keys))
		for _, key := range oldChannel.Keys {
			oldKeysByID[key.ID] = strings.TrimSpace(key.ChannelKey)
		}
		for _, keyUpdate := range req.KeysToUpdate {
			if keyUpdate.ChannelKey == nil {
				continue
			}
			if strings.TrimSpace(*keyUpdate.ChannelKey) != oldKeysByID[keyUpdate.ID] {
				return true
			}
		}
	}
	return false
}

func normalizeBaseUrlsForCompare(values []model.BaseUrl) []model.BaseUrl {
	if values == nil {
		return nil
	}
	normalized := make([]model.BaseUrl, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, model.BaseUrl{
			URL:   strings.TrimRight(strings.TrimSpace(value.URL), "/"),
			Delay: value.Delay,
		})
	}
	return normalized
}

func normalizeHeadersForCompare(values []model.CustomHeader) []model.CustomHeader {
	if values == nil {
		return nil
	}
	normalized := make([]model.CustomHeader, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, model.CustomHeader{
			HeaderKey:   strings.TrimSpace(value.HeaderKey),
			HeaderValue: strings.TrimSpace(value.HeaderValue),
		})
	}
	return normalized
}

func channelDeletedModelNames(oldChannel model.Channel, req *model.ChannelUpdateRequest) []string {
	if req == nil || (req.Model == nil && req.CustomModel == nil && req.SelectedModels == nil && req.ModelMapping == nil) {
		return nil
	}

	// Reconcile the FULL set of routable pool names a channel registers — its selected
	// models PLUS every model_mapping alias KEY — so removing a mapping alias (or the
	// selected model it mapped from) evicts the now-stale pool item. This mirrors
	// helper.ChannelEnsureModelGroups, which registers both; without the mapping side a
	// removed alias would linger as a pool member that still routes to this channel yet
	// no longer gets rewritten to the upstream name (a stale route). Also react to a
	// mapping-only update (req.ModelMapping != nil) which the old guard ignored.
	oldModels := channelRoutablePoolNames(model.ChannelSelectedModelNames(oldChannel), oldChannel.ModelMapping)

	newSelected := channelSelectionAfterLegacyUpdate(oldChannel, req)
	if req.SelectedModels != nil {
		newSelected = model.NormalizeChannelModelNames(*req.SelectedModels)
	}
	newMapping := oldChannel.ModelMapping
	if req.ModelMapping != nil {
		newMapping = *req.ModelMapping
	}
	newModels := channelRoutablePoolNames(newSelected, newMapping)

	deletedModels, _ := diff.Diff(oldModels, newModels)
	return deletedModels
}

// channelRoutablePoolNames returns the pool names a channel registers: its selected
// models plus every model_mapping alias KEY (cleaned + case-insensitively deduped),
// matching helper.ChannelEnsureModelGroups so the add and remove sides stay symmetric.
func channelRoutablePoolNames(selected []string, mapping map[string]string) []string {
	names := make([]string, 0, len(selected)+len(mapping))
	names = append(names, selected...)
	for clientName := range mapping {
		if clean := model.CleanOneMillionCapabilityModelName(clientName); clean != "" {
			names = append(names, clean)
		}
	}
	return model.NormalizeChannelModelNames(names)
}

func ChannelEnabled(id int, enabled bool, ctx context.Context) error {
	oldChannel, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.Channel{}).Where("id = ?", id).Update("enabled", enabled).Error; err != nil {
		return err
	}
	oldChannel.Enabled = enabled
	channelCache.Set(id, oldChannel)
	return nil
}

func ChannelDel(id int, ctx context.Context) error {
	ch, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}

	// 开启事务
	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 获取所有受影响的 GroupID，用于刷新缓存
	var affectedGroupIDs []int
	if err := tx.Model(&model.GroupItem{}).
		Where("channel_id = ?", id).
		Pluck("group_id", &affectedGroupIDs).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to get affected groups: %w", err)
	}

	// 删除所有引用该渠道的 GroupItem
	if err := tx.Where("channel_id = ?", id).Delete(&model.GroupItem{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete group items: %w", err)
	}
	if err := deleteEmptyAutoCreatedGroups(tx, affectedGroupIDs, ctx); err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete empty auto-created groups: %w", err)
	}

	// 删除渠道 keys
	if err := tx.Where("channel_id = ?", id).Delete(&model.ChannelKey{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel keys: %w", err)
	}

	// 删除统计数据
	if err := tx.Where("channel_id = ?", id).Delete(&model.StatsChannel{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel stats: %w", err)
	}

	// 删除渠道
	if err := tx.Delete(&model.Channel{}, id).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// 删除缓存
	channelCache.Del(id)
	for _, k := range ch.Keys {
		if k.ID != 0 {
			channelKeyCache.Del(k.ID)
		}
	}
	StatsChannelDel(id)

	if len(affectedGroupIDs) > 0 {
		if err := groupRefreshCache(ctx); err != nil {
			log.Warnf("failed to refresh group cache after channel delete: %v", err)
		}
	}

	return nil
}

func ChannelLLMList(ctx context.Context) ([]model.LLMChannel, error) {
	models := []model.LLMChannel{}
	for _, channel := range channelCache.GetAll() {
		modelNames := model.ChannelSelectedModelNames(channel)
		for _, modelName := range modelNames {
			if modelName == "" {
				continue
			}
			models = append(models, model.LLMChannel{
				Name:        modelName,
				Enabled:     channel.Enabled,
				ChannelID:   channel.ID,
				ChannelName: channel.Name,
				ChannelType: channel.Type,
			})
		}
	}
	return models, nil
}

func ChannelGet(id int, ctx context.Context) (*model.Channel, error) {
	channel, ok := channelCache.Get(id)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}
	return &channel, nil
}

func channelRefreshCache(ctx context.Context) error {
	channels := []model.Channel{}
	if err := db.GetDB().WithContext(ctx).
		Preload("Keys").
		Preload("Stats").
		Find(&channels).Error; err != nil {
		log.Warnf("failed to get channels: %v", err)
		return err
	}
	channelSnapshot := make(map[int]model.Channel, len(channels))
	keySnapshot := make(map[int]model.ChannelKey)
	for _, channel := range channels {
		normalizeChannelRuntimeModels(&channel)
		channelSnapshot[channel.ID] = channel
		for _, k := range channel.Keys {
			if k.ID != 0 {
				keySnapshot[k.ID] = k
			}
		}
	}
	channelCache.ReplaceAll(channelSnapshot)
	channelKeyCache.ReplaceAll(keySnapshot)
	channelKeyCacheNeedUpdateLock.Lock()
	channelKeyCacheNeedUpdate = make(map[int]struct{})
	channelKeyCacheNeedUpdateLock.Unlock()
	return nil
}

func channelRefreshCacheByID(id int, ctx context.Context) error {
	if old, ok := channelCache.Get(id); ok {
		for _, k := range old.Keys {
			if k.ID != 0 {
				channelKeyCache.Del(k.ID)
			}
		}
	}
	var channel model.Channel
	if err := db.GetDB().WithContext(ctx).
		Preload("Keys").
		Preload("Stats").
		First(&channel, id).Error; err != nil {
		return err
	}
	normalizeChannelRuntimeModels(&channel)
	channelCache.Set(channel.ID, channel)
	for _, k := range channel.Keys {
		if k.ID != 0 {
			channelKeyCache.Set(k.ID, k)
		}
	}
	return nil
}

func normalizeChannelRuntimeModels(channel *model.Channel) {
	if channel == nil {
		return
	}
	rawSelected := append([]string(nil), channel.SelectedModels...)
	rawLegacy := xstrings.SplitTrimCompact(",", channel.Model, channel.CustomModel)
	channel.DiscoveredModels = model.NormalizeChannelModelNames(channel.DiscoveredModels)
	if len(channel.SelectedModels) > 0 {
		channel.SelectedModels = model.NormalizeChannelModelNames(channel.SelectedModels)
	} else {
		channel.SelectedModels = model.ChannelSelectedModelNames(*channel)
	}
	channel.Model = strings.Join(channel.SelectedModels, ",")
	channel.CustomModel = ""
	if channel.Type == outbound.OutboundTypeAnthropic &&
		(model.ModelNamesWantAnthropicContext1M(rawSelected) || model.ModelNamesWantAnthropicContext1M(rawLegacy)) {
		channel.AnthropicContext1M = true
	}
}
