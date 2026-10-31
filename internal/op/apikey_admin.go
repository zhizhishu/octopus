package op

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// apiKeyCacheLock 串起对同一份缓存 APIKey 的「读-改-写」：记最近使用、批量启停、编辑保存。
// 不加锁的话，转发路径刚写进去的 last_used_* 会被同时进行的保存用旧快照盖掉（反之亦然）。
var apiKeyCacheLock sync.Mutex

// apiKeyLastUsedPending 暂存待落库的「最近使用」（同一把 Key 只留最后一次），
// 与 userRelayIPPending 同一思路：转发路径只改内存，周期任务批量补齐 DB。
var apiKeyLastUsedPending = make(map[int]userRelayIPRecord)
var apiKeyLastUsedPendingLock sync.Mutex

const apiKeyBatchMaxIDs = 500

// APIKeyRecordUse 记录这把 Key 最近一次转发的时间和来源 IP。只更新内存缓存 + 挂进待落库表。
func APIKeyRecordUse(apiKeyID int, requestIP string, usedAt int64) {
	if apiKeyID <= 0 {
		return
	}
	requestIP = normalizeRequestIP(requestIP)
	if usedAt <= 0 {
		usedAt = time.Now().Unix()
	}
	apiKeyCacheLock.Lock()
	key, ok := apiKeyCache.Get(apiKeyID)
	if !ok || usedAt < key.LastUsedAt {
		apiKeyCacheLock.Unlock()
		return
	}
	key.LastUsedAt = usedAt
	if requestIP != "" {
		key.LastUsedIP = requestIP
	}
	apiKeyCache.Set(apiKeyID, key)
	apiKeyCacheLock.Unlock()

	apiKeyLastUsedPendingLock.Lock()
	apiKeyLastUsedPending[apiKeyID] = userRelayIPRecord{ip: key.LastUsedIP, relayAt: usedAt}
	apiKeyLastUsedPendingLock.Unlock()
}

// APIKeyLastUsedSaveDB 把待落库的「最近使用」批量写进数据库。写失败不回队（同 UserRelayIPSaveDB）。
func APIKeyLastUsedSaveDB(ctx context.Context) error {
	apiKeyLastUsedPendingLock.Lock()
	if len(apiKeyLastUsedPending) == 0 {
		apiKeyLastUsedPendingLock.Unlock()
		return nil
	}
	pending := apiKeyLastUsedPending
	apiKeyLastUsedPending = make(map[int]userRelayIPRecord, len(pending))
	apiKeyLastUsedPendingLock.Unlock()

	var firstErr error
	for id, record := range pending {
		if err := db.GetDB().WithContext(ctx).Model(&model.APIKey{}).Where("id = ?", id).Updates(map[string]any{
			"last_used_ip": record.ip,
			"last_used_at": record.relayAt,
		}).Error; err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to record API key last use: %w", err)
		}
	}
	return firstErr
}

// apiKeyOverlayPendingLastUsed 让「从库重载缓存」不丢掉还没落库的最近使用。
func apiKeyOverlayPendingLastUsed(byID map[int]model.APIKey) {
	apiKeyLastUsedPendingLock.Lock()
	defer apiKeyLastUsedPendingLock.Unlock()
	for id, record := range apiKeyLastUsedPending {
		key, ok := byID[id]
		if !ok || record.relayAt < key.LastUsedAt {
			continue
		}
		key.LastUsedAt = record.relayAt
		key.LastUsedIP = record.ip
		byID[id] = key
	}
}

func apiKeyLastUsedSaveDBTask() {
	ctx, cancel := context.WithTimeout(context.Background(), userRelayIPSaveTimeout)
	defer cancel()
	if err := APIKeyLastUsedSaveDB(ctx); err != nil {
		log.Warnf("api key last used save db error: %v", err)
	}
}

// APIKeySetEnabled 只改启用状态（批量启停用），不碰其他字段。普通用户只能动自己的 Key。
func APIKeySetEnabled(id int, enabled bool, actor model.User, ctx context.Context) error {
	existing, ok := apiKeyCache.Get(id)
	if !ok || (!actor.IsAdmin() && existing.UserID != actor.ID) {
		return fmt.Errorf("API key not found")
	}
	// 作废是终态：不能再启用（停用照常放行，本来就是停的）。
	if enabled && existing.IsRevoked() {
		return errAPIKeyRevoked
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.APIKey{}).Where("id = ?", id).Update("enabled", enabled).Error; err != nil {
		return fmt.Errorf("failed to update API key: %w", err)
	}
	apiKeyCacheLock.Lock()
	defer apiKeyCacheLock.Unlock()
	if key, ok := apiKeyCache.Get(id); ok {
		key.Enabled = enabled
		apiKeyCache.Set(id, key)
	}
	return nil
}

// APIKeyBatch 对一批 Key 做同一件事（启用 / 停用 / 删除），逐个执行、逐个报结果。
// 每一把都走单个操作的同一套规则（普通用户只能动自己的；删除行为与单个删除完全一样）。
func APIKeyBatch(req model.APIKeyBatchRequest, actor model.User, ctx context.Context) (model.APIKeyBatchResult, error) {
	seen := make(map[int]struct{}, len(req.IDs))
	ids := make([]int, 0, len(req.IDs))
	for _, id := range req.IDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return model.APIKeyBatchResult{}, fmt.Errorf("no API key selected")
	}
	if len(ids) > apiKeyBatchMaxIDs {
		return model.APIKeyBatchResult{}, fmt.Errorf("too many API keys in one batch (max %d)", apiKeyBatchMaxIDs)
	}
	var apply func(id int) error
	switch req.Action {
	case model.APIKeyBatchEnable:
		apply = func(id int) error { return APIKeySetEnabled(id, true, actor, ctx) }
	case model.APIKeyBatchDisable:
		apply = func(id int) error { return APIKeySetEnabled(id, false, actor, ctx) }
	case model.APIKeyBatchDelete:
		apply = func(id int) error { return APIKeyDelete(id, actor, ctx) }
	default:
		return model.APIKeyBatchResult{}, fmt.Errorf("invalid action")
	}
	result := model.APIKeyBatchResult{Done: make([]int, 0, len(ids)), Failed: make([]model.UserBatchFailure, 0)}
	for _, id := range ids {
		if err := apply(id); err != nil {
			result.Failed = append(result.Failed, model.UserBatchFailure{ID: id, Error: err.Error()})
			continue
		}
		result.Done = append(result.Done, id)
	}
	return result, nil
}

// APIKeyUsage 返回一把 Key 在 today / 7d / 30d / all 里的用量，形状与 UserUsage 相同。
func APIKeyUsage(id int, rangeKey string, actor model.User, ctx context.Context) (model.UserUsageResponse, error) {
	return apiKeyUsageAt(id, rangeKey, actor, time.Now(), ctx)
}

func apiKeyUsageAt(id int, rangeKey string, actor model.User, now time.Time, ctx context.Context) (model.UserUsageResponse, error) {
	key, ok := apiKeyCache.Get(id)
	if !ok || (!actor.IsAdmin() && key.UserID != actor.ID) {
		return model.UserUsageResponse{}, fmt.Errorf("API key not found")
	}
	out, err := relayUsageAt("api_key_id", id, rangeKey, now, ctx)
	if err != nil {
		return model.UserUsageResponse{}, err
	}
	out.UserID = key.UserID
	out.APIKeyID = id
	return out, nil
}

// APIKeyModelsForUser 列出某个用户的 Key 能限定的模型：他名下（启用的）方案里当前有可用渠道的模型名。
// 没绑方案的用户按全部启用方案算（与转发时的兜底一致）。
func APIKeyModelsForUser(userID int, ctx context.Context) ([]string, error) {
	if _, err := UserGet(userID); err != nil {
		return nil, err
	}
	if err := ensureAccessPlanCache(ctx); err != nil {
		return nil, err
	}
	allowed, restricted, err := userAllowedAccessPlanIDSet(userID, ctx)
	if err != nil {
		return nil, err
	}
	plans := enabledAccessPlans()
	if restricted {
		filtered := make([]model.AccessPlan, 0, len(plans))
		for _, plan := range plans {
			if _, ok := allowed[plan.ID]; ok {
				filtered = append(filtered, plan)
			}
		}
		plans = filtered
	}
	return accessPlanRouteModelsFromPlans(plans), nil
}
