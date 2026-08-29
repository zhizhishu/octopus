package op

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

const (
	modelHealthProviderOpenAI    = "OpenAI"
	modelHealthProviderGemini    = "Gemini"
	modelHealthProviderAnthropic = "Anthropic"
)

var modelHealthProviderOrder = []string{
	modelHealthProviderOpenAI,
	modelHealthProviderGemini,
	modelHealthProviderAnthropic,
}

type modelHealthAccumulator struct {
	success      int64
	failed       int64
	ftutMs       []int
	outputTokens int64
	generateMs   int64
	cacheHit     int64
	cacheInput   int64
}

type modelHealthModelAccumulator struct {
	model   string
	hours   [24]modelHealthAccumulator
	summary modelHealthAccumulator
}

func ModelHourlyHealth(ctx context.Context) (model.ModelHealthResponse, error) {
	now := statsNow()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.Add(24 * time.Hour)
	date := start.Format("20060102")

	return modelHealthCache.getOrCompute(date, modelHealthCacheTTL, func() (model.ModelHealthResponse, error) {
		return computeModelHourlyHealth(ctx, start, end, date)
	})
}

func computeModelHourlyHealth(ctx context.Context, start, end time.Time, date string) (model.ModelHealthResponse, error) {
	logs, err := modelHealthLogs(ctx, start.Unix(), end.Unix())
	if err != nil {
		return model.ModelHealthResponse{}, err
	}

	providerModels := make(map[string]map[string]*modelHealthModelAccumulator, len(modelHealthProviderOrder))
	for _, provider := range modelHealthProviderOrder {
		providerModels[provider] = make(map[string]*modelHealthModelAccumulator)
	}

	for _, relayLog := range logs {
		if relayLogExcludedFromModelTelemetry(relayLog) {
			continue
		}
		t := time.Unix(relayLog.Time, 0)
		if t.Before(start) || !t.Before(end) {
			continue
		}
		hour := t.Hour()
		if hour < 0 || hour > 23 {
			continue
		}

		modelName := modelHealthModelName(relayLog)
		provider := modelHealthProvider(relayLog, modelName)
		models := providerModels[provider]
		if models == nil {
			continue
		}
		acc := models[modelName]
		if acc == nil {
			acc = &modelHealthModelAccumulator{model: modelName}
			models[modelName] = acc
		}

		modelHealthAddLog(&acc.hours[hour], relayLog)
		modelHealthAddLog(&acc.summary, relayLog)
	}

	providers := make([]model.ModelHealthProvider, 0, len(modelHealthProviderOrder))
	for _, provider := range modelHealthProviderOrder {
		items := make([]model.ModelHealthModel, 0, len(providerModels[provider]))
		for _, acc := range providerModels[provider] {
			items = append(items, modelHealthBuildModel(acc))
		}
		sort.Slice(items, func(i, j int) bool {
			left := items[i].Summary.RequestSuccess + items[i].Summary.RequestFailed
			right := items[j].Summary.RequestSuccess + items[j].Summary.RequestFailed
			if left == right {
				return items[i].Model < items[j].Model
			}
			return left > right
		})
		providers = append(providers, model.ModelHealthProvider{
			Provider: provider,
			Models:   items,
		})
	}

	return model.ModelHealthResponse{
		Date:      date,
		Providers: providers,
	}, nil
}

func modelHealthLogs(ctx context.Context, start, end int64) ([]model.RelayLog, error) {
	var dbLogs []model.RelayLog
	err := db.GetDB().WithContext(ctx).
		Model(&model.RelayLog{}).
		Select("id", "time", "request_model_name", "channel_id", "actual_model_name", "input_tokens", "output_tokens", "cache_hit_tokens", "cache_write_tokens", "cache_input_tokens", "ftut", "use_time", "error", "error_code", "error_strategy").
		Where("time >= ? AND time < ?", start, end).
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
		if relayLog.Time < start || relayLog.Time >= end {
			continue
		}
		if _, ok := seen[relayLog.ID]; ok {
			continue
		}
		logs = append(logs, relayLog)
	}

	return logs, nil
}

func modelHealthModelName(relayLog model.RelayLog) string {
	if name := strings.TrimSpace(relayLog.RequestModelName); name != "" {
		return name
	}
	if name := strings.TrimSpace(relayLog.ActualModelName); name != "" {
		return name
	}
	return "unknown"
}

func modelHealthProvider(relayLog model.RelayLog, modelName string) string {
	if ch, ok := channelCache.Get(relayLog.ChannelId); ok {
		switch ch.Type {
		case outbound.OutboundTypeAnthropic:
			return modelHealthProviderAnthropic
		case outbound.OutboundTypeGemini:
			return modelHealthProviderGemini
		default:
			return modelHealthProviderOpenAI
		}
	}

	lower := strings.ToLower(modelName)
	switch {
	case strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic"):
		return modelHealthProviderAnthropic
	case strings.Contains(lower, "gemini"):
		return modelHealthProviderGemini
	default:
		return modelHealthProviderOpenAI
	}
}

func modelHealthAddLog(acc *modelHealthAccumulator, relayLog model.RelayLog) {
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
				acc.outputTokens += int64(relayLog.OutputTokens)
				acc.generateMs += int64(generateMs)
			}
		}
	} else {
		acc.failed++
	}

	acc.cacheHit += int64(relayLog.CacheHitTokens)
	cacheInput := relayLogCacheRateBase(relayLog)
	if cacheInput > 0 {
		acc.cacheInput += cacheInput
	}
}

func modelHealthBuildModel(acc *modelHealthModelAccumulator) model.ModelHealthModel {
	hours := make([]model.ModelHealthHour, 0, 24)
	for hour := 0; hour < 24; hour++ {
		hours = append(hours, model.ModelHealthHour{
			Hour:               hour,
			ModelHealthSummary: modelHealthSummary(acc.hours[hour]),
		})
	}
	return model.ModelHealthModel{
		Model:   acc.model,
		Hours:   hours,
		Summary: modelHealthSummary(acc.summary),
	}
}

func modelHealthSummary(acc modelHealthAccumulator) model.ModelHealthSummary {
	summary := model.ModelHealthSummary{
		RequestSuccess:  acc.success,
		RequestFailed:   acc.failed,
		FirstTokenP90Ms: int64(modelHealthP90(acc.ftutMs)),
	}
	if acc.outputTokens > 0 && acc.generateMs > 0 {
		summary.AvgThroughput = float64(acc.outputTokens) / (float64(acc.generateMs) / 1000)
	}
	if acc.cacheHit > 0 && acc.cacheInput > 0 {
		summary.CacheHitRate = float64(acc.cacheHit) / float64(acc.cacheInput)
	}
	return summary
}

func modelHealthP90(values []int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int, len(values))
	copy(sorted, values)
	sort.Ints(sorted)
	idx := int(math.Ceil(float64(len(sorted))*0.9)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
