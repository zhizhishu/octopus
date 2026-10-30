package helper

import (
	"net/url"

	"github.com/zhizhishu/octopus/internal/model"
)

// ChannelProxyURL 返回渠道出站应走的代理（nil = 直连），与 ChannelHttpClientContext
// 对无面容流量的取舍完全一致（渠道开关 → 渠道自定义代理 → 实例级代理）。
//
// 只给**自带出站客户端**的新路径用（目前是 /v1/realtime 的 WebSocket 拨号器）：
// WebSocket 握手不能复用 *http.Client，但代理选择必须与 HTTP 路径同一口径，
// 否则配了代理的渠道在实时语音上会悄悄直连。本函数只读、不碰任何 TLS 面容或头逻辑。
func ChannelProxyURL(channel *model.Channel) (*url.URL, error) {
	return channelProxyURL(channel)
}
