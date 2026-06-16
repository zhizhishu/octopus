package task

import (
	"context"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/diff"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/xstrings"
)

var lastSyncModelsTime = time.Now()

// SyncModelsTask 同步模型任务
func SyncModelsTask() {
	log.Debugf("sync models task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("sync models task finished, sync time: %s", time.Since(startTime))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	channels, err := op.ChannelList(ctx)
	if err != nil {
		log.Errorf("failed to list channels: %v", err)
		return
	}
	totalNewModels := make([]string, 0, 128)
	seenTotalNewModels := make(map[string]struct{}, 128)
	addTotalModel := func(modelName string) {
		m := strings.ToLower(strings.TrimSpace(modelName))
		if m == "" {
			return
		}
		if _, ok := seenTotalNewModels[m]; ok {
			return
		}
		seenTotalNewModels[m] = struct{}{}
		totalNewModels = append(totalNewModels, m)
	}
	for _, channel := range channels {
		if !channel.AutoSync {
			continue
		}
		fetchModels, err := helper.FetchModels(ctx, channel)
		if err != nil {
			log.Warnf("failed to fetch models for channel %s: %v", channel.Name, err)
			for _, m := range model.ChannelSelectedModelNames(channel) {
				addTotalModel(m)
			}
			continue
		}
		newModels := xstrings.TrimCompact(fetchModels)
		if len(newModels) == 0 {
			log.Warnf("skip syncing channel %s models: fetched empty model list", channel.Name)
			for _, m := range model.ChannelSelectedModelNames(channel) {
				addTotalModel(m)
			}
			continue
		}
		groupingChannel := channel
		discoveredModels := model.NormalizeChannelModelNames(newModels)
		if !sameModelNames(model.ChannelDiscoveredModelNames(channel), discoveredModels) {
			updatedChannel, err := op.ChannelUpdate(&model.ChannelUpdateRequest{
				ID:               channel.ID,
				DiscoveredModels: &discoveredModels,
			}, ctx)
			if err != nil {
				log.Errorf("failed to update channel %s: %v", channel.Name, err)
				continue
			}
			groupingChannel = *updatedChannel
		}

		// 自动分组
		if len(model.ChannelSelectedModelNames(groupingChannel)) > 0 {
			helper.ChannelEnsureModelGroups(&groupingChannel, ctx)
			helper.ChannelAutoGroup(&groupingChannel, ctx)
		}
		for _, m := range model.ChannelSelectedModelNames(groupingChannel) {
			addTotalModel(m)
		}
	}
	llmPrice, err := op.LLMList(ctx)
	if err != nil {
		log.Errorf("failed to list models price: %v", err)
		return
	}
	llmPriceNames := make([]string, 0, len(llmPrice))
	for _, price := range llmPrice {
		llmPriceNames = append(llmPriceNames, price.Name)
	}

	deletedNorm, addedNorm := diff.Diff(llmPriceNames, totalNewModels)
	if len(deletedNorm) > 0 {
		if err := helper.LLMPriceDeleteFromDBWithNoPrice(deletedNorm, ctx); err != nil {
			log.Errorf("failed to batch delete models price: %v", err)
		}
	}
	if len(addedNorm) > 0 {
		if err := helper.LLMPriceAddToDB(addedNorm, ctx); err != nil {
			log.Errorf("failed to add models price: %v", err)
		}
	}
	lastSyncModelsTime = time.Now()
}

func GetLastSyncModelsTime() time.Time {
	return lastSyncModelsTime
}

func reconcileSyncedChannelModels(selectedModels []string, fetchedModels []string) ([]string, []string, bool) {
	return helper.ReconcileSelectedModelsWithFetched(selectedModels, fetchedModels)
}

func sameModelNames(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
