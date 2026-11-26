package op

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/cache"
	"gorm.io/gorm"
)

var apiKeyCache = cache.New[int, model.APIKey](16)
var apiKeyIDMap = cache.New[string, int](16)

var errAPIKeyExpirePast = errors.New("API key expire time must be in the future")

// ExpireAt 是 Unix 秒，0 = 永不过期。鉴权中间件按 ExpireAt < now 判过期。
func apiKeyExpireInPast(expireAt int64) bool {
	return expireAt > 0 && expireAt <= time.Now().Unix()
}

func APIKeyCreate(key *model.APIKey, ctx context.Context) error {
	if key.UserID <= 0 {
		admin, err := UserDefaultAdmin(ctx)
		if err != nil {
			return err
		}
		key.UserID = admin.ID
	}
	if _, err := UserGet(key.UserID); err != nil {
		return fmt.Errorf("API key owner not found")
	}
	key.Name = strings.TrimSpace(key.Name)
	if key.Name == "" {
		return fmt.Errorf("API key name is required")
	}
	if apiKeyExpireInPast(key.ExpireAt) {
		return errAPIKeyExpirePast
	}
	endpointFamilies, err := model.NormalizeAPIKeyEndpointFamilies(key.EndpointFamilies)
	if err != nil {
		return err
	}
	key.EndpointFamilies = endpointFamilies
	wantEnabled := key.Enabled
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(key).Error; err != nil {
			return err
		}
		if !wantEnabled {
			if err := persistDisabledAfterCreate(tx, &model.APIKey{}, []int{key.ID}); err != nil {
				return err
			}
			key.Enabled = false
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create API key: %w", err)
	}
	apiKeyCache.Set(key.ID, *key)
	apiKeyIDMap.Set(key.APIKey, key.ID)
	if err := APIKeyAccessPlanBindDefaultIfEmpty(key.ID, ctx); err != nil {
		return fmt.Errorf("failed to bind default access plan: %w", err)
	}
	attachAPIKeyAccessPlans(key)
	return nil
}

func APIKeyUpdate(key *model.APIKey, actor model.User, ctx context.Context) error {
	existing, ok := apiKeyCache.Get(key.ID)
	if !ok {
		return fmt.Errorf("API key not found")
	}
	if !actor.IsAdmin() && existing.UserID != actor.ID {
		return fmt.Errorf("API key not found")
	}
	// 访客 Key 只走 /api/v1/guestkey/update：这里改不了它（防止经普通接口改归属 / 去掉模型白名单）。
	// 普通 Key 也不能经这里变成访客 Key、不能撤销作废。
	if existing.Guest {
		if !actor.IsAdmin() {
			return fmt.Errorf("API key not found")
		}
		return errGuestKeyUseGuestAPI
	}
	key.Guest = false
	key.RevokedAt = existing.RevokedAt
	key.Note = existing.Note
	if !actor.IsAdmin() {
		key.UserID = existing.UserID
	} else if key.UserID <= 0 {
		key.UserID = existing.UserID
	}
	if _, err := UserGet(key.UserID); err != nil {
		return fmt.Errorf("API key owner not found")
	}
	key.Name = strings.TrimSpace(key.Name)
	if key.Name == "" {
		return fmt.Errorf("API key name is required")
	}
	// 只拦"新设的"过去时间；已过期的 Key 不改过期时间、只改别的字段照常保存。
	if key.ExpireAt != existing.ExpireAt && apiKeyExpireInPast(key.ExpireAt) {
		return errAPIKeyExpirePast
	}
	if key.EndpointFamilies == nil {
		key.EndpointFamilies = existing.EndpointFamilies
	} else {
		endpointFamilies, err := model.NormalizeAPIKeyEndpointFamilies(key.EndpointFamilies)
		if err != nil {
			return err
		}
		key.EndpointFamilies = endpointFamilies
	}
	// last_used_* 是转发路径写的审计字段，请求体里没有（或是旧值），保存时不覆盖。
	if err := db.GetDB().WithContext(ctx).Omit("api_key", "last_used_at", "last_used_ip", "guest", "revoked_at", "note").Save(key).Error; err != nil {
		return fmt.Errorf("failed to update API key: %w", err)
	}
	key.APIKey = existing.APIKey
	apiKeyCacheLock.Lock()
	defer apiKeyCacheLock.Unlock()
	latest, ok := apiKeyCache.Get(key.ID)
	if !ok {
		latest = existing
	}
	key.LastUsedAt = latest.LastUsedAt
	key.LastUsedIP = latest.LastUsedIP
	apiKeyCache.Set(key.ID, *key)
	return nil
}

func APIKeyList(ctx context.Context, actor model.User) ([]model.APIKey, error) {
	keys := make([]model.APIKey, 0, apiKeyCache.Len())
	for _, apiKey := range apiKeyCache.GetAll() {
		if !actor.IsAdmin() && apiKey.UserID != actor.ID {
			continue
		}
		// 访客 Key 只在「访客」页（/api/v1/guestkey/list）列出，普通 Key 列表里不出现。
		if apiKey.Guest {
			continue
		}
		attachAPIKeyUserName(&apiKey)
		attachAPIKeyAccessPlans(&apiKey)
		keys = append(keys, apiKey)
	}
	return keys, nil
}

func APIKeyListAll(ctx context.Context) ([]model.APIKey, error) {
	keys := make([]model.APIKey, 0, apiKeyCache.Len())
	for _, apiKey := range apiKeyCache.GetAll() {
		attachAPIKeyUserName(&apiKey)
		attachAPIKeyAccessPlans(&apiKey)
		keys = append(keys, apiKey)
	}
	return keys, nil
}

func APIKeyGet(id int, ctx context.Context) (model.APIKey, error) {
	apiKey, ok := apiKeyCache.Get(id)
	if !ok {
		return model.APIKey{}, fmt.Errorf("API key not found")
	}
	attachAPIKeyUserName(&apiKey)
	attachAPIKeyAccessPlans(&apiKey)
	return apiKey, nil
}

func APIKeyGetScoped(id int, actor model.User, ctx context.Context) (model.APIKey, error) {
	apiKey, err := APIKeyGet(id, ctx)
	if err != nil {
		return model.APIKey{}, err
	}
	if !actor.IsAdmin() && apiKey.UserID != actor.ID {
		return model.APIKey{}, fmt.Errorf("API key not found")
	}
	return apiKey, nil
}

func APIKeyGetByAPIKey(apiKey string, ctx context.Context) (model.APIKey, error) {
	id, ok := apiKeyIDMap.Get(apiKey)
	if !ok {
		return model.APIKey{}, fmt.Errorf("API key not found")
	}
	return APIKeyGet(id, ctx)
}

func APIKeyDelete(id int, actor model.User, ctx context.Context) error {
	existing, ok := apiKeyCache.Get(id)
	if !ok {
		return fmt.Errorf("API key not found")
	}
	if !actor.IsAdmin() && existing.UserID != actor.ID {
		return fmt.Errorf("API key not found")
	}
	k := model.APIKey{
		ID: id,
	}
	if err := StatsAPIKeyDel(id); err != nil {
		return fmt.Errorf("failed to delete stats API key: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Where("api_key_id = ?", id).Delete(&model.APIKeyAccessPlan{}).Error; err != nil {
		return fmt.Errorf("failed to delete API key access plans: %w", err)
	}
	result := db.GetDB().WithContext(ctx).Delete(&k)
	if result.RowsAffected == 0 {
		return fmt.Errorf("API key not found")
	}
	if result.Error != nil {
		return fmt.Errorf("failed to delete API key: %w", result.Error)
	}
	apiKeyCache.Del(k.ID)
	apiKeyIDMap.Del(existing.APIKey)
	if err := accessPlanRefreshCache(ctx); err != nil {
		return fmt.Errorf("failed to refresh access plan cache: %w", err)
	}
	return nil
}

func attachAPIKeyUserName(apiKey *model.APIKey) {
	if apiKey == nil || apiKey.UserID <= 0 {
		return
	}
	if user, err := UserGet(apiKey.UserID); err == nil {
		apiKey.UserName = user.Username
	}
}

func apiKeyRefreshCache(ctx context.Context) error {
	apiKeys := []model.APIKey{}
	if err := db.GetDB().WithContext(ctx).Find(&apiKeys).Error; err != nil {
		return err
	}
	byID := make(map[int]model.APIKey, len(apiKeys))
	byKey := make(map[string]int, len(apiKeys))
	for _, apiKey := range apiKeys {
		byID[apiKey.ID] = apiKey
		byKey[apiKey.APIKey] = apiKey.ID
	}
	apiKeyOverlayPendingLastUsed(byID)
	apiKeyCacheLock.Lock()
	apiKeyCache.ReplaceAll(byID)
	apiKeyCacheLock.Unlock()
	apiKeyIDMap.ReplaceAll(byKey)
	return nil
}
