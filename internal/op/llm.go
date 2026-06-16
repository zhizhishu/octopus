package op

import (
	"context"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
)

var llmModelCache = cache.New[string, model.LLMPrice](16)

func LLMList(ctx context.Context) ([]model.LLMInfo, error) {
	merged := make(map[string]model.LLMPrice, llmModelCache.Len())
	for m, cost := range llmModelCache.GetAll() {
		name := model.CleanOneMillionCapabilityModelName(m)
		if name == "" {
			continue
		}
		merged[name] = cost
	}
	models := make([]model.LLMInfo, 0, len(merged))
	for name, cost := range merged {
		models = append(models, model.LLMInfo{Name: name, LLMPrice: cost})
	}
	return models, nil
}

func LLMUpdate(info model.LLMInfo, ctx context.Context) error {
	info.Name = strings.ToLower(model.CleanOneMillionCapabilityModelName(info.Name))
	_, ok := llmModelCache.Get(info.Name)
	if !ok {
		return fmt.Errorf("model not found")
	}
	if err := db.GetDB().WithContext(ctx).Save(info).Error; err != nil {
		return err
	}
	llmModelCache.Set(info.Name, info.LLMPrice)
	return nil
}

func LLMDelete(modelName string, ctx context.Context) error {
	modelName = strings.ToLower(model.CleanOneMillionCapabilityModelName(modelName))
	_, ok := llmModelCache.Get(modelName)
	if !ok {
		return fmt.Errorf("model not found")
	}
	if err := db.GetDB().WithContext(ctx).Delete(&model.LLMInfo{Name: modelName}).Error; err != nil {
		return err
	}
	llmModelCache.Del(modelName)
	return nil
}
func LLMBatchDelete(modelNames []string, ctx context.Context) error {
	if len(modelNames) == 0 {
		return nil
	}
	modelNames = model.NormalizeChannelModelNames(modelNames)
	for i := range modelNames {
		modelNames[i] = strings.ToLower(modelNames[i])
	}
	if err := db.GetDB().WithContext(ctx).Where("name IN ?", modelNames).Delete(&model.LLMInfo{}).Error; err != nil {
		return err
	}
	llmModelCache.Del(modelNames...)
	return nil
}
func LLMCreate(info model.LLMInfo, ctx context.Context) error {
	info.Name = strings.ToLower(model.CleanOneMillionCapabilityModelName(info.Name))
	_, ok := llmModelCache.Get(info.Name)
	if ok {
		return fmt.Errorf("model already exists")
	}
	if err := db.GetDB().WithContext(ctx).Create(&info).Error; err != nil {
		return err
	}
	llmModelCache.Set(info.Name, info.LLMPrice)
	return nil
}
func LLMBatchCreate(llmInfos []model.LLMInfo, ctx context.Context) error {
	if len(llmInfos) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(llmInfos))
	newLLMInfos := make([]model.LLMInfo, 0, len(llmInfos))
	for _, llmInfo := range llmInfos {
		llmInfo.Name = strings.ToLower(model.CleanOneMillionCapabilityModelName(llmInfo.Name))
		if _, ok := seen[llmInfo.Name]; ok {
			continue
		}
		if _, ok := llmModelCache.Get(llmInfo.Name); ok {
			continue
		}
		seen[llmInfo.Name] = struct{}{}
		newLLMInfos = append(newLLMInfos, llmInfo)
	}
	if len(newLLMInfos) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).Create(&newLLMInfos).Error; err != nil {
		return err
	}
	for _, llmInfo := range newLLMInfos {
		llmModelCache.Set(llmInfo.Name, llmInfo.LLMPrice)
	}
	return nil
}
func LLMGet(name string) (model.LLMPrice, error) {
	name = strings.ToLower(model.CleanOneMillionCapabilityModelName(name))
	price, ok := llmModelCache.Get(name)
	if !ok {
		return model.LLMPrice{}, fmt.Errorf("model not found")
	}
	return price, nil
}

func llmRefreshCache(ctx context.Context) error {
	models := []model.LLMInfo{}
	if err := db.GetDB().WithContext(ctx).Find(&models).Error; err != nil {
		return err
	}
	modelMap := make(map[string]model.LLMPrice, len(models))
	for _, info := range models {
		name := strings.ToLower(model.CleanOneMillionCapabilityModelName(info.Name))
		if name == "" {
			continue
		}
		modelMap[name] = info.LLMPrice
	}
	llmModelCache.ReplaceAll(modelMap)
	return nil
}
