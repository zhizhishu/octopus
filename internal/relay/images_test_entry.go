package relay

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// 图片渠道测试的"钉住渠道"入口。
//
// 为什么需要这一层：图片模型的渠道测试必须**走真链路**（与生产
// POST /v1/images/generations 用同一份转发实现），否则测的和用的不是一条路，
// 测出来的绿不作数。而 ImagesHandler 自己是按分组选路的
// （selectRouteGroup → balancer.NewIteratorWithSession → op.ChannelGet），渠道测试的
// 对象却是前端表单里的**一个渠道对象**：它可能还没入库、可能带着未保存的编辑、
// 也可能是被禁用的渠道。靠分组选路它一辈子选不中，回库重查又会测到"已保存的旧
// 版本"，同样是假绿灯。所以必须有一个"外部指定渠道"的挂钩。
//
// 方案：gin context 里放一个**锁定标记**（不是裸指针，见 imagesChannelTestPin），
// ImagesHandler 见到标记就走单渠道转发分支，跳过分组选路。
//
// 生产路径零变化的依据：这个键只能由本包的 WithImagesChannelTest 写入，
// 而它的唯一调用方是渠道测试（internal/modeltest）。鉴权中间件与任何 handler 都
// 不写这个键，所以生产请求进 ImagesHandler 时 imagesChannelTestPinFor 恒返回
// false，后面的 selectRouteGroup / 迭代器 / metrics 保存全部按原样执行。

// imagesChannelTestPinContextKey 是"本次 ImagesHandler 钉在哪个渠道上"的 context 键。
const imagesChannelTestPinContextKey = "octopus_images_channel_test_pin"

// imagesChannelTestPinReportContextKey 存本次测试实际打到的东西（上游路径 / 用的 key），
// 供测试报告读回，不进响应、不进日志。
const (
	imagesChannelTestUpstreamPathContextKey = "octopus_images_channel_test_upstream_path"
	imagesChannelTestKeyIDContextKey        = "octopus_images_channel_test_key_id"
)

// imagesChannelTestPin 是锁定标记。故意用未导出结构体包一层而不是直接放
// *model.Channel：别的代码就算恰好用了同名键也塞不进这个类型，锁定分支不会被误触发。
type imagesChannelTestPin struct {
	channel *model.Channel
}

// WithImagesChannelTest 让接下来这次 ImagesHandler 调用跳过分组选路，直接钉在
// channel 上（渠道测试专用）。
//
// 一次只钉一个渠道：同一个 gin context 重复调用以最后一次为准。
func WithImagesChannelTest(c *gin.Context, channel *model.Channel) {
	if c == nil || channel == nil {
		return
	}
	c.Set(imagesChannelTestPinContextKey, imagesChannelTestPin{channel: channel})
}

// ImagesChannelTestUpstreamPath 读回本次渠道测试真正打到的上游路径（没有则空串）。
func ImagesChannelTestUpstreamPath(c *gin.Context) string {
	return imagesChannelTestString(c, imagesChannelTestUpstreamPathContextKey)
}

// ImagesChannelTestChannelKeyID 读回本次渠道测试实际用上的渠道 key id（没有则 0）。
func ImagesChannelTestChannelKeyID(c *gin.Context) int {
	if c == nil {
		return 0
	}
	value, ok := c.Get(imagesChannelTestKeyIDContextKey)
	if !ok {
		return 0
	}
	id, _ := value.(int)
	return id
}

func imagesChannelTestString(c *gin.Context, key string) string {
	if c == nil {
		return ""
	}
	value, ok := c.Get(key)
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

// imagesChannelTestPinFor 取出本次调用的锁定渠道。第二个返回值为 false 表示这是一次
// 正常的生产请求，调用方必须继续走原路径。
func imagesChannelTestPinFor(c *gin.Context) (*model.Channel, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.Get(imagesChannelTestPinContextKey)
	if !ok {
		return nil, false
	}
	pin, ok := value.(imagesChannelTestPin)
	if !ok || pin.channel == nil {
		return nil, false
	}
	return pin.channel, true
}

// ChannelTypeSupportsImages 判断渠道类型能否承接 /v1/images/* 流量。
//
// 判据直接复用 relay 自己的 isImagesEndpointCompatibleChannelType（ImagesHandler
// 里调度用的就是它）：OpenAI Chat / OpenAI Responses / Custom OpenAI Chat 走
// OpenAI 兼容透传，Gemini 走它自己的 predict / generateContent 适配。
// Anthropic / Volcengine / OpenAIEmbedding 三类在 ImagesHandler 里会被
// "unsupported channel type" 跳过，所以它们永远测不出图片——渠道测试必须提前
// 明说跳过原因，不能送进去换一条看不懂的报错。
func ChannelTypeSupportsImages(channelType outbound.OutboundType) bool {
	return isImagesEndpointCompatibleChannelType(channelType)
}

// forwardImagesChannelTest 用一个指定渠道完成一次 images 转发。
//
// 转发本身走的就是生产那两个实现（imagesAttempt / geminiImagesAttempt）：同一个
// base_url 拼接、同一份 header 透传、同一套 grok/agnes 报文规范化、同一套流式/非流式
// 透传与错误体处理。所以"测"与"用"在线上是同一条路。
//
// 与生产主循环**故意不同**的三件事（都是"测试不许污染生产状态"）：
//  1. 不回库取渠道（用调用方给的对象，见文件头）；
//  2. 不写熔断器 / key 健康 / 渠道统计 / relay 日志——一次测试不许把生产渠道打进
//     冷却，也不许在日志里造出一条计费记录；
//  3. 不读粘性会话、不做 key 健康排序（测试只有一个渠道，排不排都一样）。
func forwardImagesChannelTest(
	c *gin.Context,
	endpoint string,
	channel *model.Channel,
	requestModel string,
	stream bool,
	isMultipart bool,
	bc *bodycache.BodyCache,
	boundary string,
	jsonPayload map[string]any,
) {
	actualModel := requestModel
	if mapped, ok := channel.ModelMapping[actualModel]; ok && strings.TrimSpace(mapped) != "" {
		actualModel = mapped
	}

	// key 策略对齐生产：渠道关了熔断器才允许用冷却中的 key，禁用的空 key 一律不用。
	var keys []model.ChannelKey
	if channel.DisableCircuitBreaker {
		keys = channel.GetAllEnabledChannelKeys()
	} else {
		keys = channel.GetAvailableChannelKeys()
	}
	if len(keys) == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available key")
		return
	}

	ctx := c.Request.Context()
	metrics := newImagesRelayMetrics(0, 0, "", requestModel, imageEndpointName(endpoint), c.Request.URL.Path)

	var lastErr error
	for _, usedKey := range keys {
		statusCode := 0
		var (
			written bool
			fwdErr  error
		)
		if channel.Type == outbound.OutboundTypeGemini {
			statusCode, written, _, _, fwdErr = geminiImagesAttempt(ctx, endpoint, c, isMultipart, jsonPayload, stream, channel, usedKey.ChannelKey, metrics, actualModel)
		} else {
			// firstTokenTimeOutSec 传 0：渠道测试不设首字节超时，整体由调用方 context 兜底。
			statusCode, written, _, _, fwdErr = imagesAttempt(ctx, endpoint, c, bc, isMultipart, boundary, jsonPayload, stream, channel, usedKey.ChannelKey, 0, metrics, actualModel)
		}
		if fwdErr == nil {
			c.Set(imagesChannelTestUpstreamPathContextKey, imagesChannelTestUpstreamPath(channel, endpoint, actualModel, usedKey.ChannelKey, jsonPayload))
			c.Set(imagesChannelTestKeyIDContextKey, usedKey.ID)
			return
		}
		lastErr = fwdErr
		if written || c.Writer.Written() {
			// 已经写下游了，不能再换 key（否则会把两份响应拼在一起）。
			return
		}
		if !shouldTryNextChannelKey(statusCode) {
			break
		}
	}

	status, code, message := relayErrorResponse(lastErr)
	resp.ErrorWithCode(c, status, code, message)
}

// imagesChannelTestUpstreamPath 只为报告算出"这次实际打到的上游路径"。
//
// 它不改转发：真正的 URL 由 imagesAttempt / geminiImagesAttempt 用同一批 helper
// 拼出来。这里调用同一个构造函数把路径反推回来，好让渠道测试页显示的是真路径
// （Gemini 的 predict / generateContent 与 OpenAI 兼容的 /v1/images/generations 不同）。
// 只取 Path，查询串（Gemini 把 key 放在 ?key=）一律丢弃，不会把密钥带进报告。
func imagesChannelTestUpstreamPath(channel *model.Channel, endpoint, actualModel, channelKey string, jsonPayload map[string]any) string {
	if channel == nil {
		return ""
	}
	if channel.Type == outbound.OutboundTypeGemini {
		_, targetURL, err := geminiImageRequestBodyAndURL(channel.GetBaseUrl(), actualModel, channelKey, jsonPayload, stringFromPayload(jsonPayload, "prompt"))
		if err != nil {
			return ""
		}
		return urlPath(targetURL)
	}
	targetURL, err := xurl.JoinOpenAIPath(channel.GetBaseUrl(), "/v1"+endpoint)
	if err != nil {
		return ""
	}
	return urlPath(targetURL)
}

func urlPath(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil {
		return ""
	}
	if path := parsed.EscapedPath(); path != "" {
		return path
	}
	return parsed.Path
}
