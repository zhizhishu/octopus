package op

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var groupCache = cache.New[int, model.Group](16)
var groupMap = cache.New[string, model.Group](16)

func GroupList(ctx context.Context) ([]model.Group, error) {
	groups := make([]model.Group, 0, groupCache.Len())
	for _, group := range groupCache.GetAll() {
		groups = append(groups, group)
	}
	return groups, nil
}

func GroupListModel(ctx context.Context) ([]string, error) {
	models, seen := enabledGroupModelNames()
	routeModels, err := AccessPlanRouteModels(ctx)
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

func GroupListModelForAPIKey(apiKeyID int, ctx context.Context) ([]string, error) {
	models, seen := enabledGroupModelNames()
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

	models, seen := enabledGroupModelNames()
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

func enabledGroupModelNames() ([]string, map[string]struct{}) {
	models := []string{}
	seen := make(map[string]struct{})
	for _, group := range groupCache.GetAll() {
		if !groupHasEnabledItems(group) {
			continue
		}
		name := model.CleanOneMillionCapabilityModelName(group.Name)
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
	return models, seen
}

func groupHasEnabledItems(group model.Group) bool {
	for _, item := range group.Items {
		channel, ok := channelCache.Get(item.ChannelID)
		if ok && channel.Enabled {
			return true
		}
	}
	return false
}

func normalizeGroupForRuntime(group *model.Group) {
	if group == nil {
		return
	}
	group.Name = model.CleanOneMillionCapabilityModelName(group.Name)
	for i := range group.Items {
		group.Items[i].ModelName = model.CleanOneMillionCapabilityModelName(group.Items[i].ModelName)
	}
}

func GroupGet(id int, ctx context.Context) (*model.Group, error) {
	group, ok := groupCache.Get(id)
	if !ok {
		return nil, fmt.Errorf("group not found")
	}
	return &group, nil
}

func GroupGetEnabledMap(name string, ctx context.Context) (model.Group, error) {
	group, ok := groupMap.Get(name)
	if !ok {
		cleanName := model.CleanOneMillionCapabilityModelName(name)
		if cleanName != "" && cleanName != name {
			group, ok = groupMap.Get(cleanName)
		}
		if !ok {
			for _, candidate := range groupCache.GetAll() {
				if strings.EqualFold(model.CleanOneMillionCapabilityModelName(candidate.Name), cleanName) {
					group, ok = candidate, true
					break
				}
			}
		}
	}
	if !ok {
		return model.Group{}, fmt.Errorf("group not found")
	}
	normalizeGroupForRuntime(&group)
	if len(group.Items) == 0 {
		group.Items = nil
		return group, nil
	}

	enabledItems := make([]model.GroupItem, 0, len(group.Items))
	for _, item := range group.Items {
		channel, ok := channelCache.Get(item.ChannelID)
		if !ok || !channel.Enabled {
			continue
		}
		enabledItems = append(enabledItems, item)
	}
	group.Items = enabledItems
	return group, nil
}

func GroupCreate(group *model.Group, ctx context.Context) error {
	group.Name = model.CleanOneMillionCapabilityModelName(group.Name)
	if err := db.GetDB().WithContext(ctx).Create(group).Error; err != nil {
		return err
	}
	groupCache.Set(group.ID, *group)
	groupMap.Set(group.Name, *group)
	return nil
}

func GroupUpdate(req *model.GroupUpdateRequest, ctx context.Context) (*model.Group, error) {
	oldGroup, ok := groupCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("group not found")
	}
	oldName := oldGroup.Name

	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var selectFields []string
	updates := model.Group{ID: req.ID}

	if req.Name != nil {
		selectFields = append(selectFields, "name")
		updates.Name = model.CleanOneMillionCapabilityModelName(*req.Name)
	}
	if req.Mode != nil {
		selectFields = append(selectFields, "mode")
		updates.Mode = *req.Mode
	}
	if req.MatchRegex != nil {
		selectFields = append(selectFields, "match_regex")
		updates.MatchRegex = *req.MatchRegex
	}
	if req.FirstTokenTimeOut != nil {
		selectFields = append(selectFields, "first_token_time_out")
		updates.FirstTokenTimeOut = *req.FirstTokenTimeOut
	}
	if req.SessionKeepTime != nil {
		selectFields = append(selectFields, "session_keep_time")
		updates.SessionKeepTime = *req.SessionKeepTime
	}
	if req.MaxConcurrent != nil {
		selectFields = append(selectFields, "max_concurrent")
		updates.MaxConcurrent = *req.MaxConcurrent
	}
	if req.RPMLimit != nil {
		selectFields = append(selectFields, "rpm_limit")
		updates.RPMLimit = *req.RPMLimit
	}
	if oldGroup.AutoCreated {
		selectFields = append(selectFields, "auto_created")
		updates.AutoCreated = false
	}

	if len(selectFields) > 0 {
		if err := tx.Model(&model.Group{}).Where("id = ?", req.ID).Select(selectFields).Updates(&updates).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to update group: %w", err)
		}
	}

	// 删除 items
	if len(req.ItemsToDelete) > 0 {
		if err := tx.Where("id IN ? AND group_id = ?", req.ItemsToDelete, req.ID).Delete(&model.GroupItem{}).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to delete items: %w", err)
		}
	}

	// 批量更新 items
	if len(req.ItemsToUpdate) > 0 {
		ids := make([]int, len(req.ItemsToUpdate))
		priorityCase := "CASE id"
		weightCase := "CASE id"
		for i, item := range req.ItemsToUpdate {
			ids[i] = item.ID
			priorityCase += fmt.Sprintf(" WHEN %d THEN %d", item.ID, item.Priority)
			weightCase += fmt.Sprintf(" WHEN %d THEN %d", item.ID, item.Weight)
		}
		priorityCase += " END"
		weightCase += " END"

		if err := tx.Model(&model.GroupItem{}).
			Where("id IN ? AND group_id = ?", ids, req.ID).
			Updates(map[string]interface{}{
				"priority": gorm.Expr(priorityCase),
				"weight":   gorm.Expr(weightCase),
			}).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to update items: %w", err)
		}
	}

	// 批量新增 items
	if len(req.ItemsToAdd) > 0 {
		newItems := make([]model.GroupItem, len(req.ItemsToAdd))
		for i, item := range req.ItemsToAdd {
			newItems[i] = model.GroupItem{
				GroupID:   req.ID,
				ChannelID: item.ChannelID,
				ModelName: model.CleanOneMillionCapabilityModelName(item.ModelName),
				Priority:  item.Priority,
				Weight:    item.Weight,
			}
		}
		if err := tx.Create(&newItems).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to create items: %w", err)
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	// 刷新缓存并返回最新数据
	if err := groupRefreshCacheByID(req.ID, ctx); err != nil {
		return nil, err
	}

	group, _ := groupCache.Get(req.ID)
	if oldName != "" && oldName != group.Name {
		groupMap.Del(oldName)
	}
	return &group, nil
}

func GroupDel(id int, ctx context.Context) error {
	group, ok := groupCache.Get(id)
	if !ok {
		return fmt.Errorf("group not found")
	}

	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err := tx.Where("group_id = ?", id).Delete(&model.GroupItem{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete group items: %w", err)
	}

	if err := tx.Delete(&model.Group{}, id).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete group: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	groupCache.Del(id)
	groupMap.Del(group.Name)
	return nil
}

func GroupItemAdd(item *model.GroupItem, ctx context.Context) error {
	if _, ok := groupCache.Get(item.GroupID); !ok {
		return fmt.Errorf("group not found")
	}
	item.ModelName = model.CleanOneMillionCapabilityModelName(item.ModelName)

	if err := db.GetDB().WithContext(ctx).Create(item).Error; err != nil {
		return err
	}

	return groupRefreshCacheByID(item.GroupID, ctx)
}

func GroupItemBatchAdd(groupID int, items []model.GroupIDAndLLMName, ctx context.Context) error {
	if len(items) == 0 {
		return nil
	}

	group, ok := groupCache.Get(groupID)
	if !ok {
		return fmt.Errorf("group not found")
	}

	seen := make(map[string]struct{}, len(items))
	uniq := make([]model.GroupIDAndLLMName, 0, len(items))
	for _, it := range items {
		it.ModelName = model.CleanOneMillionCapabilityModelName(it.ModelName)
		if it.ChannelID == 0 || it.ModelName == "" {
			continue
		}
		k := fmt.Sprintf("%d|%s", it.ChannelID, it.ModelName)
		if _, exists := seen[k]; exists {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, it)
	}
	if len(uniq) == 0 {
		return nil
	}

	nextPriority := 1
	for _, gi := range group.Items {
		if gi.Priority >= nextPriority {
			nextPriority = gi.Priority + 1
		}
	}

	newItems := make([]model.GroupItem, 0, len(uniq))
	for _, it := range uniq {
		newItems = append(newItems, model.GroupItem{
			GroupID:   groupID,
			ChannelID: it.ChannelID,
			ModelName: it.ModelName,
			Priority:  nextPriority,
			Weight:    1,
		})
		nextPriority++
	}

	if err := db.GetDB().WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "group_id"}, {Name: "channel_id"}, {Name: "model_name"}},
			DoNothing: true,
		}).
		Create(&newItems).Error; err != nil {
		return fmt.Errorf("failed to create group items: %w", err)
	}

	return groupRefreshCacheByID(groupID, ctx)
}

func GroupEnsureChannelModels(channelID int, modelNames []string, ctx context.Context) error {
	if channelID == 0 {
		return fmt.Errorf("invalid channel id")
	}

	names := normalizeGroupModelNames(modelNames)
	if len(names) == 0 {
		return nil
	}

	for _, name := range names {
		group, err := groupGetOrCreateAuto(name, ctx)
		if err != nil {
			return err
		}
		if err := GroupItemBatchAdd(group.ID, []model.GroupIDAndLLMName{{
			ChannelID: channelID,
			ModelName: name,
		}}, ctx); err != nil {
			return err
		}
	}
	return nil
}

func normalizeGroupModelNames(modelNames []string) []string {
	return model.NormalizeChannelModelNames(modelNames)
}

func groupGetOrCreateAuto(name string, ctx context.Context) (model.Group, error) {
	name = model.CleanOneMillionCapabilityModelName(name)
	if group, ok := groupMap.Get(name); ok {
		return group, nil
	}

	group := model.Group{
		Name:        name,
		Mode:        model.GroupModeRoundRobin,
		AutoCreated: true,
	}
	if err := db.GetDB().WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&group).Error; err != nil {
		return model.Group{}, fmt.Errorf("failed to create model group %q: %w", name, err)
	}
	if group.ID != 0 {
		groupCache.Set(group.ID, group)
		groupMap.Set(group.Name, group)
		return group, nil
	}

	var existing model.Group
	if err := db.GetDB().WithContext(ctx).
		Preload("Items").
		Where("name = ?", name).
		First(&existing).Error; err != nil {
		return model.Group{}, fmt.Errorf("failed to load model group %q: %w", name, err)
	}
	groupCache.Set(existing.ID, existing)
	groupMap.Set(existing.Name, existing)
	return existing, nil
}

func GroupItemUpdate(item *model.GroupItem, ctx context.Context) error {
	item.ModelName = model.CleanOneMillionCapabilityModelName(item.ModelName)
	if err := db.GetDB().WithContext(ctx).Model(item).
		Select("ModelName", "Priority", "Weight").
		Updates(item).Error; err != nil {
		return err
	}

	return groupRefreshCacheByID(item.GroupID, ctx)
}

func GroupItemDel(id int, ctx context.Context) error {
	var item model.GroupItem
	if err := db.GetDB().WithContext(ctx).First(&item, id).Error; err != nil {
		return fmt.Errorf("group item not found")
	}

	if err := db.GetDB().WithContext(ctx).Delete(&item).Error; err != nil {
		return err
	}
	if err := deleteEmptyAutoCreatedGroups(db.GetDB().WithContext(ctx), []int{item.GroupID}, ctx); err != nil {
		return fmt.Errorf("failed to delete empty auto-created groups: %w", err)
	}

	return groupRefreshCache(ctx)
}

// GroupItemBatchDelByChannelAndModels 根据渠道ID和模型名称批量删除分组项
func GroupItemBatchDelByChannelAndModels(keys []model.GroupIDAndLLMName, ctx context.Context) error {
	if len(keys) == 0 {
		return nil
	}

	conditions := make([][]interface{}, len(keys))
	for i, key := range keys {
		conditions[i] = []interface{}{key.ChannelID, key.ModelName}
	}

	var groupIDs []int
	if err := db.GetDB().WithContext(ctx).
		Model(&model.GroupItem{}).
		Distinct("group_id").
		Where("(channel_id, model_name) IN ?", conditions).
		Pluck("group_id", &groupIDs).Error; err != nil {
		return fmt.Errorf("failed to find group ids: %w", err)
	}

	if len(groupIDs) == 0 {
		return nil
	}

	if err := db.GetDB().WithContext(ctx).
		Where("(channel_id, model_name) IN ?", conditions).
		Delete(&model.GroupItem{}).Error; err != nil {
		return fmt.Errorf("failed to delete group items: %w", err)
	}

	if err := deleteEmptyAutoCreatedGroups(db.GetDB().WithContext(ctx), groupIDs, ctx); err != nil {
		return fmt.Errorf("failed to delete empty auto-created groups: %w", err)
	}

	if err := groupRefreshCache(ctx); err != nil {
		return fmt.Errorf("failed to refresh group cache: %w", err)
	}

	return nil
}

func deleteEmptyAutoCreatedGroups(conn *gorm.DB, groupIDs []int, ctx context.Context) error {
	if len(groupIDs) == 0 {
		return nil
	}

	var groups []model.Group
	if err := conn.WithContext(ctx).
		Preload("Items").
		Where("id IN ? AND auto_created = ?", groupIDs, true).
		Find(&groups).Error; err != nil {
		return err
	}

	emptyIDs := make([]int, 0, len(groups))
	for _, group := range groups {
		if len(group.Items) == 0 {
			emptyIDs = append(emptyIDs, group.ID)
		}
	}
	if len(emptyIDs) == 0 {
		return nil
	}

	return conn.WithContext(ctx).Delete(&model.Group{}, emptyIDs).Error
}

func GroupItemList(groupID int, ctx context.Context) ([]model.GroupItem, error) {
	var items []model.GroupItem
	if err := db.GetDB().WithContext(ctx).
		Where("group_id = ?", groupID).
		Order("priority ASC").
		Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func groupRefreshCache(ctx context.Context) error {
	groups := []model.Group{}
	if err := db.GetDB().WithContext(ctx).
		Preload("Items").
		Find(&groups).Error; err != nil {
		return err
	}
	byID := make(map[int]model.Group, len(groups))
	byName := make(map[string]model.Group, len(groups))
	for _, group := range groups {
		originalName := group.Name
		normalizeGroupForRuntime(&group)
		byID[group.ID] = group
		byName[group.Name] = group
		if strings.TrimSpace(originalName) != "" {
			byName[originalName] = group
		}
	}
	groupCache.ReplaceAll(byID)
	groupMap.ReplaceAll(byName)
	return nil
}

func groupRefreshCacheByID(id int, ctx context.Context) error {
	var group model.Group
	if err := db.GetDB().WithContext(ctx).
		Preload("Items").
		First(&group, id).Error; err != nil {
		return err
	}
	originalName := group.Name
	normalizeGroupForRuntime(&group)
	groupCache.Set(group.ID, group)
	groupMap.Set(group.Name, group)
	if strings.TrimSpace(originalName) != "" {
		groupMap.Set(originalName, group)
	}
	return nil
}

func groupRefreshCacheByIDs(ids []int, ctx context.Context) error {
	if len(ids) == 0 {
		return nil
	}
	var groups []model.Group
	if err := db.GetDB().WithContext(ctx).
		Preload("Items").
		Where("id IN ?", ids).
		Find(&groups).Error; err != nil {
		return err
	}
	for _, group := range groups {
		originalName := group.Name
		normalizeGroupForRuntime(&group)
		groupCache.Set(group.ID, group)
		groupMap.Set(group.Name, group)
		if strings.TrimSpace(originalName) != "" {
			groupMap.Set(originalName, group)
		}
	}
	return nil
}
