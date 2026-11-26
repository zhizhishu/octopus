package relay

import (
	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// 内部钉扎转发（internal pinned relay）。
//
// 渠道测试 / 模型检测（含鹈鹕 creative）不再走 modeltest 自建探针出站，而是拿
// 内置「系统」Key 走**正常转发链路**（relay.Handler 全链：指纹/形状/cloak/闸门/
// attempts/转发日志一个不少）。为此需要一个「钉死被测渠道」的机制：
//
//   - 钉扎值是一个完整的 dbmodel.Channel 值（不是 ID）：渠道测试的「测试接口」
//     下拉会派生出改了 Type 的渠道副本（channelForTestEndpoint），前端也可能
//     提交未保存的草稿渠道——按 ID 回缓存查会把这些差异全部抹掉，测的就不是
//     操作者选的东西了。
//   - 钉扎标记只能由**进程内 Go 代码**通过 MarkInternalPinnedChannel 写进
//     gin context；它不是请求头、不是请求体字段，外部调用者无法伪造。relay
//     的 HTTP 入口（中间件链）从来不会设置它。
//   - 钉扎请求的语义约束（见 Handler 里的分支）：
//     1. 候选只有钉扎的这一个渠道——不跳渠道、无兜底分组、无救援回合；
//     2. 渠道停用不拦（测试一条停用渠道是合法操作，探针时代也如此）；
//     3. 不占用户额度、不受用户/分组限额（metrics 的计费路径跳过）；
//     4. 不注入提示词规则（检测语义与旧探针一致：题目就是题目）；
//     5. 转发日志照常落地（attempts/耗时/费用），api_key_id 是系统 Key。
const (
	// internalPinnedChannelContextKey 是 gin context 里钉扎渠道的键。
	internalPinnedChannelContextKey = "octopus_internal_pinned_channel"
	// internalResponseContextKey 是 Handler 结束时回写给内部调用方的最终
	// 内部响应（metrics.InternalResponse 的快照）。只有钉扎请求会写。
	internalResponseContextKey = "octopus_internal_response"
)

// MarkInternalPinnedChannel 把一次内部转发钉死到指定渠道。
//
// 只给进程内调用方用（modeltest 的 relay 发送器）；channelID<=0 或 c 为空时是
// 无操作。调用方必须先把 gin context 的用户身份键（api_key_id/user_id/
// user_role/request_ip/supported_models）设成系统 Key 的身份——钉扎本身不做
// 鉴权，因为它不出现在任何 HTTP 路径上。
func MarkInternalPinnedChannel(c *gin.Context, channel dbmodel.Channel) {
	if c == nil || channel.ID <= 0 {
		return
	}
	c.Set(internalPinnedChannelContextKey, channel)
}

// internalPinnedChannel 取出钉扎的渠道值；未钉扎返回 nil。
func internalPinnedChannel(c *gin.Context) *dbmodel.Channel {
	if c == nil {
		return nil
	}
	v, ok := c.Get(internalPinnedChannelContextKey)
	if !ok {
		return nil
	}
	channel, ok := v.(dbmodel.Channel)
	if !ok || channel.ID <= 0 {
		return nil
	}
	return &channel
}

// stashInternalResponse 把最终内部响应回写到 gin context（仅钉扎请求）。
// Handler 里以 defer 挂在收尾，metrics.Save 之后、Handler 返回之前执行，
// 内部发送器在 relay.Handler 返回后读取。
func stashInternalResponse(c *gin.Context, metrics *RelayMetrics) {
	if c == nil || metrics == nil || metrics.InternalResponse == nil {
		return
	}
	c.Set(internalResponseContextKey, metrics.InternalResponse)
}

// InternalPinnedResponse 供进程内调用方（modeltest 的 relay 发送器）在
// relay.Handler 返回后取回最终内部响应；未钉扎或没有响应时返回 nil。
func InternalPinnedResponse(c *gin.Context) *transformerModel.InternalLLMResponse {
	if c == nil {
		return nil
	}
	v, ok := c.Get(internalResponseContextKey)
	if !ok {
		return nil
	}
	resp, ok := v.(*transformerModel.InternalLLMResponse)
	if !ok {
		return nil
	}
	return resp
}
