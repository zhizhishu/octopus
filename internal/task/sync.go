package task

import (
	"context"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/helper"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/diff"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/xstrings"
)

var lastSyncModelsTime = time.Now()

// SyncModelsTask 同步模型任务。
//
// 它只补缺的价格占位行，从不删除价格行——启动、定时、管理员点「同步」都一样。
// 以前它把不在「自动同步渠道模型」里的无价行全删了，于是只挂在没开自动同步的渠道上的
// 模型、管理员手建的空行，每次重启都被删一遍。参考 new-api / sub2api：价格从不自动删除，
// 只有管理员在价格页显式删除。
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
			// Not synced from upstream, but its models are in use: give them a
			// placeholder price row like every other channel model.
			for _, m := range model.ChannelSelectedModelNames(channel) {
				addTotalModel(m)
			}
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

	// Only add: a price row an admin may have created or edited is never removed here.
	_, addedNorm := diff.Diff(llmPriceNames, totalNewModels)
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
