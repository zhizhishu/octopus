package batchimage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 渠道选择。
//
// 【为什么选渠在这一层而不是复用 relay 的迭代器】relay 的迭代器是围绕"一次 HTTP
// 请求、同步等上游"设计的：它按请求上下文挑渠道、当场拿 key 发请求、失败当场换
// 下一个。批处理完全不同——提交要立刻返回、真正干活是几分钟后、期间渠道可能被
// 停用。硬套那个迭代器会把"失败换渠道"的语义带进来，而批处理的"换渠道"意味着
// 上游换了一家、批次句柄作废，是另一回事。
//
// 【所以选渠的规则很窄】只在**提交那一刻**选一次，把 channel_id / channel_key_id
// 钉在任务行上，之后整条生命周期都用同一个渠道。中途渠道挂了就如实失败退款，
// 而不是偷偷换一家重跑（那会让用户拿到两家上游混在一起的图，且钱没法算）。

// 选渠的错误。
var (
	// ErrNoChannelAvailable 表示当前没有任何渠道能接这批活。
	ErrNoChannelAvailable = errors.New("no channel available for batch image")
	// ErrModelNotEnabled 表示没有渠道声明承接这个模型。
	ErrModelNotEnabled = errors.New("no channel enables this model for batch image")
)

// dbChannelResolver 是 ChannelResolver 的生产实现。
type dbChannelResolver struct{}

// defaultResolver 是包级默认实现（不持有状态，可以共享）。
var defaultResolver ChannelResolver = dbChannelResolver{}

// Resolve 按 id 取渠道与密钥。
func (dbChannelResolver) Resolve(ctx context.Context, channelID, channelKeyID int) (*model.Channel, *model.ChannelKey, error) {
	if channelID <= 0 {
		return nil, nil, fmt.Errorf("batch image: invalid channel id %d", channelID)
	}
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil {
		return nil, nil, err
	}
	if channel == nil {
		return nil, nil, fmt.Errorf("batch image: channel %d not found", channelID)
	}
	if !channel.Enabled {
		return nil, nil, fmt.Errorf("batch image: channel %d is disabled", channelID)
	}
	if channelKeyID <= 0 {
		return channel, nil, nil
	}
	for index := range channel.Keys {
		if channel.Keys[index].ID == channelKeyID {
			// 刻意用**值拷贝**返回：调用方拿到的是快照，不会在我们不知情时
			// 被别处改掉（channel 对象活在缓存里，会被其它请求并发更新）。
			key := channel.Keys[index]
			return channel, &key, nil
		}
	}
	return nil, nil, fmt.Errorf("batch image: key %d not found on channel %d", channelKeyID, channelID)
}

// Selection 是一次选渠的结果。
type Selection struct {
	Channel    *model.Channel
	ChannelKey *model.ChannelKey
	// Provider 是这个渠道配置的批处理上游。
	Provider string
	// UpstreamModel 是映射之后真正发给上游的模型名。
	UpstreamModel string
}

// SelectChannel 为一次提交挑渠道。
//
// 规则（按顺序）：
//  1. 渠道启用、且配置了 batch_image；
//  2. 渠道声明承接这个模型（BatchImage.Models）；
//  3. 渠道有可用的 key；
//  4. 渠道类型与上游协议不矛盾（gemini_api 要 Gemini 型渠道）；
//  5. 在合格的候选里，选 key 延迟最低的（与 GetBaseUrl 的口径一致：延迟低=更快）。
//
// 【为什么按延迟选而不是轮询】批处理是长时间占用，一次选错要错几十分钟。
// 延迟在这里是个粗糙但真实的健康信号；轮询会把活派给正在恢复的渠道。
func SelectChannel(ctx context.Context, modelName string) (*Selection, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return nil, fmt.Errorf("batch image: empty model")
	}

	channels, err := op.ChannelList(ctx)
	if err != nil {
		return nil, err
	}

	type candidate struct {
		channel *model.Channel
		key     model.ChannelKey
		delay   int
	}
	var candidates []candidate
	var sawProviderChannel bool

	for index := range channels {
		channel := channels[index]
		if !channel.Enabled || channel.BatchImage == nil {
			continue
		}
		provider := channel.BatchImageProvider()
		if !model.IsSupportedBatchImageProvider(provider) {
			continue
		}
		sawProviderChannel = true

		if !channel.BatchImageSupportsModel(modelName) {
			continue
		}
		// gemini_api 要求渠道本身的类型也是 Gemini：批处理走的是 Gemini 的原生协议，
		// 挂在一个 OpenAI 兼容渠道上等于用错误的 baseURL/密钥去谈 Gemini 协议。
		if provider == model.BatchImageProviderGeminiAPI &&
			channel.Type != outbound.OutboundTypeGemini {
			continue
		}
		// Vertex 只要求有 key（服务账号 JSON）和桶，与渠道 type 无关：
		// 它访问的是 aiplatform，不是 generativelanguage。

		keys := channel.GetAvailableChannelKeys()
		if len(keys) == 0 {
			// 全部 key 都在冷却时用 last-resort 那档：批处理是低频操作，
			// 一次冷却不该让用户几分钟都提交不了。
			keys = channel.GetAvailableChannelKeysLastResort()
		}
		if len(keys) == 0 {
			continue
		}
		// 延迟取渠道 BaseURL 的健康延迟（与 GetBaseUrl 同一口径）。
		// 这是选渠唯一的排序依据：批处理一次占用很久，把活派给正在恢复的渠道
		// 会让用户等几十分钟才失败。
		delay := channelBaseDelay(&channels[index])
		for _, key := range keys {
			candidates = append(candidates, candidate{
				channel: &channels[index],
				key:     key,
				delay:   delay,
			})
		}
	}

	if len(candidates) == 0 {
		if sawProviderChannel {
			return nil, fmt.Errorf("%w: %s", ErrModelNotEnabled, modelName)
		}
		return nil, ErrNoChannelAvailable
	}

	// 稳定排序：延迟相同就按渠道 id 排，保证同样的输入选出同样的渠道
	// （否则每次提交飘到不同渠道，排查时对不上）。
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].delay != candidates[j].delay {
			return candidates[i].delay < candidates[j].delay
		}
		return candidates[i].channel.ID < candidates[j].channel.ID
	})

	best := candidates[0]
	upstreamModel := modelName
	if mapped, ok := best.channel.ModelMapping[modelName]; ok && strings.TrimSpace(mapped) != "" {
		upstreamModel = strings.TrimSpace(mapped)
	}
	key := best.key
	return &Selection{
		Channel:       best.channel,
		ChannelKey:    &key,
		Provider:      best.channel.BatchImageProvider(),
		UpstreamModel: upstreamModel,
	}, nil
}

// channelBaseDelay 返回渠道里最低的 BaseURL 延迟（没有配置时返回 0）。
//
// 与 GetBaseUrl 的口径一致：0 表示"没有测量数据"，按最优先处理——
// 没测过的地址通常比一个测出来很慢的地址更值得试。
func channelBaseDelay(channel *model.Channel) int {
	if channel == nil || len(channel.BaseUrls) == 0 {
		return 0
	}
	best := 0
	bestSet := false
	for _, baseURL := range channel.BaseUrls {
		if baseURL.URL == "" {
			continue
		}
		if !bestSet || baseURL.Delay < best {
			best = baseURL.Delay
			bestSet = true
		}
	}
	return best
}

// DefaultResolver 返回生产用的渠道解析器（不持有状态，可以共享）。
func DefaultResolver() ChannelResolver {
	return defaultResolver
}
