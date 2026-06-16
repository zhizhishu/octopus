package op

import (
	"context"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
)

var apiKeyCache = cache.New[int, model.APIKey](16)
var apiKeyIDMap = cache.New[string, int](16)

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
	endpointFamilies, err := model.NormalizeAPIKeyEndpointFamilies(key.EndpointFamilies)
	if err != nil {
		return err
	}
	key.EndpointFamilies = endpointFamilies
	if err := db.GetDB().WithContext(ctx).Create(key).Error; err != nil {
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
	if key.EndpointFamilies == nil {
		key.EndpointFamilies = existing.EndpointFamilies
	} else {
		endpointFamilies, err := model.NormalizeAPIKeyEndpointFamilies(key.EndpointFamilies)
		if err != nil {
			return err
		}
		key.EndpointFamilies = endpointFamilies
	}
	if err := db.GetDB().WithContext(ctx).Omit("api_key").Save(key).Error; err != nil {
		return fmt.Errorf("failed to update API key: %w", err)
	}
	key.APIKey = existing.APIKey
	apiKeyCache.Set(key.ID, *key)
	return nil
}

func APIKeyList(ctx context.Context, actor model.User) ([]model.APIKey, error) {
	keys := make([]model.APIKey, 0, apiKeyCache.Len())
	for _, apiKey := range apiKeyCache.GetAll() {
		if !actor.IsAdmin() && apiKey.UserID != actor.ID {
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
	apiKeyCache.ReplaceAll(byID)
	apiKeyIDMap.ReplaceAll(byKey)
	return nil
}
