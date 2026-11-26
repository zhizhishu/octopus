package relay

import (
	"github.com/gin-gonic/gin"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// 渠道测试/模型检测的内部入口（用户 2026-11-26 拍板，学 new-api
// controller/channel-test.go 的 testChannel）。
//
// 与正常转发的关系：入口不同，链路同一。HandlerForChannelTest（在 relay.go）
// 直接以参数指定被测渠道——不是请求头、不是请求体字段、不是 gin context 里
// 偷渡的标记，外部调用者无法伪造，relay 的 HTTP 入口永远拿不到它。检测请求
// 与一次真实转发在字节层面无法区分（指纹/形状/cloak/闸门/attempts/转发日志
// 全链一致），差异全部是「不碰用户侧」：不鉴权、不选路、不占额度、不注入
// 提示词、不计渠道 RPM。
//
// 身份：合成上下文不带任何 API Key/用户（api_key_id/user_id 全零值）。统计与
// last_used 的 op 层函数对零 id 本来就是空操作；日志行以
// request_source=model_test 标记（见 metrics.buildRelayLog）。

// internalResponseContextKey 是 gin context 里回写最终内部响应的键。
// 只有测试入口的请求会写——它是进程内调用方取结果的回传通道，不是输入标记。
const internalResponseContextKey = "octopus_internal_test_response"

// stashInternalResponse 把最终内部响应回写到 gin context（仅测试入口请求）。
// handlerImpl 里以 defer 挂在收尾，metrics.Save 之后、返回之前执行，内部发送器
// 在 HandlerForChannelTest 返回后读取。
func stashInternalResponse(c *gin.Context, metrics *RelayMetrics) {
	if c == nil || metrics == nil || metrics.InternalResponse == nil {
		return
	}
	c.Set(internalResponseContextKey, metrics.InternalResponse)
}

// InternalTestResponse 供进程内调用方（modeltest 的 relay 发送器）在
// HandlerForChannelTest 返回后取回最终内部响应；非测试入口或没有响应时返回 nil。
func InternalTestResponse(c *gin.Context) *transformerModel.InternalLLMResponse {
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
