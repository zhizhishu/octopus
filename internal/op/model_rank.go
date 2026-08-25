package op

import (
	"context"
	"sort"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

const modelRankRecentActualLimit = 5

type modelRankActualSample struct {
	name string
	time int64
	id   int64
}

type modelRankAccumulator struct {
	model           string
	success         int64
	failed          int64
	inputToken      int64
	outputToken     int64
	cacheHitToken   int64
	cacheInputToken int64
	cacheWriteToken int64
	cacheRateBase   int64
	totalCost       float64
	ftutMs          []int
	outputForTPS    int64
	generateMs      int64
	actualModels    map[string]modelRankActualSample
}

func ModelRequestRank(ctx context.Context) ([]model.ModelRankItem, error) {
	return modelRankCache.getOrCompute("all", modelRankCacheTTL, func() ([]model.ModelRankItem, error) {
		return computeModelRequestRank(ctx)
	})
}

func computeModelRequestRank(ctx context.Context) ([]model.ModelRankItem, error) {
	logs, err := modelRankLogs(ctx)
	if err != nil {
		return nil, err
	}

	accs := make(map[string]*modelRankAccumulator)
	for _, relayLog := range logs {
		if relayLogExcludedFromModelTelemetry(relayLog) {
			continue
		}
		name := modelRankRequestedModelName(relayLog)
		acc := accs[name]
		if acc == nil {
			acc = &modelRankAccumulator{
				model:        name,
				actualModels: make(map[string]modelRankActualSample),
			}
			accs[name] = acc
		}
		modelRankAddLog(acc, relayLog)
	}

	result := make([]model.ModelRankItem, 0, len(accs))
	for _, acc := range accs {
		result = append(result, modelRankBuildItem(acc))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].RequestCount == result[j].RequestCount {
			return result[i].Model < result[j].Model
		}
		return result[i].RequestCount > result[j].RequestCount
	})
	return result, nil
}

func modelRankLogs(ctx context.Context) ([]model.RelayLog, error) {
	var dbLogs []model.RelayLog
	err := db.GetDB().WithContext(ctx).
		Model(&model.RelayLog{}).
		Select(
			"id",
			"time",
			"request_model_name",
			"actual_model_name",
			"billing_model",
			"input_tokens",
			"output_tokens",
			"cache_hit_tokens",
			"cache_input_tokens",
			"cache_write_tokens",
			"ftut",
			"use_time",
			"cost",
			"final_input_cost",
			"final_output_cost",
			"final_cache_read_cost",
			"final_cache_write_cost",
			"error",
			"error_code",
			"error_strategy",
		).
		Find(&dbLogs).Error
	if err != nil {
		return nil, err
	}

	seen := make(map[int64]struct{}, len(dbLogs))
	logs := make([]model.RelayLog, 0, len(dbLogs)+len(relayLogCache))
	for _, relayLog := range dbLogs {
		seen[relayLog.ID] = struct{}{}
		logs = append(logs, relayLog)
	}

	relayLogCacheLock.Lock()
	cachedLogs := make([]model.RelayLog, len(relayLogCache))
	copy(cachedLogs, relayLogCache)
	relayLogCacheLock.Unlock()

	for _, relayLog := range cachedLogs {
		if _, ok := seen[relayLog.ID]; ok {
			continue
		}
		logs = append(logs, relayLog)
	}

	return logs, nil
}

func modelRankRequestedModelName(relayLog model.RelayLog) string {
	if name := strings.TrimSpace(relayLog.RequestModelName); name != "" {
		return name
	}
	if name := strings.TrimSpace(relayLog.ActualModelName); name != "" {
		return name
	}
	if name := strings.TrimSpace(relayLog.BillingModel); name != "" {
		return name
	}
	return "unknown"
}

func modelRankAddLog(acc *modelRankAccumulator, relayLog model.RelayLog) {
	if strings.TrimSpace(relayLog.Error) == "" {
		acc.success++
		if relayLog.Ftut > 0 {
			acc.ftutMs = append(acc.ftutMs, relayLog.Ftut)
		}
		if relayLog.OutputTokens > 0 {
			generateMs := relayLog.UseTime
			if relayLog.Ftut > 0 && relayLog.UseTime > relayLog.Ftut {
				generateMs = relayLog.UseTime - relayLog.Ftut
			}
			if generateMs > 0 {
				acc.outputForTPS += int64(relayLog.OutputTokens)
				acc.generateMs += int64(generateMs)
			}
		}
	} else {
		acc.failed++
	}

	acc.inputToken += int64(relayLog.InputTokens)
	acc.outputToken += int64(relayLog.OutputTokens)
	acc.cacheHitToken += int64(relayLog.CacheHitTokens)
	acc.cacheInputToken += int64(relayLog.CacheInputTokens)
	acc.cacheWriteToken += int64(relayLog.CacheWriteTokens)
	acc.totalCost += modelRankLogCost(relayLog)

	cacheRateBase := relayLogCacheRateBase(relayLog)
	if cacheRateBase > 0 {
		acc.cacheRateBase += cacheRateBase
	}

	if actual := strings.TrimSpace(relayLog.ActualModelName); actual != "" {
		sample, ok := acc.actualModels[actual]
		if !ok || relayLog.Time > sample.time || (relayLog.Time == sample.time && relayLog.ID > sample.id) {
			acc.actualModels[actual] = modelRankActualSample{
				name: actual,
				time: relayLog.Time,
				id:   relayLog.ID,
			}
		}
	}
}

func modelRankLogCost(relayLog model.RelayLog) float64 {
	if relayLog.Cost != 0 {
		return relayLog.Cost
	}
	return relayLog.FinalInputCost + relayLog.FinalOutputCost + relayLog.FinalCacheReadCost + relayLog.FinalCacheWriteCost
}

func modelRankBuildItem(acc *modelRankAccumulator) model.ModelRankItem {
	effectiveInputToken := acc.inputToken
	if acc.cacheRateBase > effectiveInputToken {
		effectiveInputToken = acc.cacheRateBase
	}

	item := model.ModelRankItem{
		Model:              acc.model,
		RequestSuccess:     acc.success,
		RequestFailed:      acc.failed,
		InputToken:         acc.inputToken,
		OutputToken:        acc.outputToken,
		CacheHitToken:      acc.cacheHitToken,
		CacheInputToken:    acc.cacheInputToken,
		CacheWriteToken:    acc.cacheWriteToken,
		TotalToken:         effectiveInputToken + acc.outputToken,
		TotalCost:          acc.totalCost,
		FirstTokenP90Ms:    int64(modelHealthP90(acc.ftutMs)),
		RecentActualModels: modelRankRecentActualModels(acc),
	}
	item.RequestCount = item.RequestSuccess + item.RequestFailed
	if acc.cacheHitToken > 0 && acc.cacheRateBase > 0 {
		item.CacheHitRate = float64(acc.cacheHitToken) / float64(acc.cacheRateBase)
	}
	if acc.outputForTPS > 0 && acc.generateMs > 0 {
		item.AvgThroughput = float64(acc.outputForTPS) / (float64(acc.generateMs) / 1000)
	}
	return item
}

func modelRankRecentActualModels(acc *modelRankAccumulator) []string {
	samples := make([]modelRankActualSample, 0, len(acc.actualModels))
	for _, sample := range acc.actualModels {
		samples = append(samples, sample)
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].time == samples[j].time {
			if samples[i].id == samples[j].id {
				return samples[i].name < samples[j].name
			}
			return samples[i].id > samples[j].id
		}
		return samples[i].time > samples[j].time
	})
	if len(samples) > modelRankRecentActualLimit {
		samples = samples[:modelRankRecentActualLimit]
	}
	names := make([]string, 0, len(samples))
	for _, sample := range samples {
		names = append(names, sample.name)
	}
	return names
}
