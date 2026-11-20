package relay

import (
	"net/http"
	"time"
)

// 断路器探测的归因窄缝（给 internal/modeltest 用）。
//
// 为什么需要这条缝：渠道探测（internal/modeltest/circuit_probe.go）必须与真实转发
// **同一套**上游失败归因，否则"探测判定"和"生产判定"会漂移——探针把一次 400 请求体
// 拒收算成渠道故障、生产却不算（或反之），熔断状态就会互相打架。
//
// 归因规则本来就在本包（shouldRecordBreakerFailure / newUpstreamError /
// retryAfterFromHeader / upstreamRetryAfter），但它们是未导出的；而 relay 不能
// import modeltest（modeltest 已经 import 本包，反向即成环）。所以这里只开一条
// **只读的窄缝**：把上游原文交给既有判定，原样返回它的结论。判定本身一个字节都
// 没有改，也没有新增第二套规则。
//
// 注意：这两个函数都不写任何熔断状态、不记账、不动渠道健康度。它们只回答
// "这次失败能不能算在渠道账上"以及"上游让等多久"。计数与状态机在上层。

// ClassifyCircuitProbeFailure 把一次**收到了 HTTP 响应**的上游失败交回既有的
// 上游失败归因：内部构造与真实转发同款的 typed upstreamError（状态码 + 原始响应体），
// 按生产口径挂上 Retry-After，然后跑 shouldRecordBreakerFailure 原样判定。
//
// 这样 typed 400/422 请求体拒收（invalid_request_error / INVALID_ARGUMENT /
// serde 反序列化失败 / 供应停供）、context-window 溢出一律不算渠道故障，
// 429/5xx 与无标记的 4xx 才计数——与生产转发完全一致。
//
// body 只在内存里参与判定，绝不返回、绝不落库；返回的只有"是否计数"和
// "上游 Retry-After"（无 Retry-After 时为 0）。
func ClassifyCircuitProbeFailure(statusCode int, body []byte, retryAfterHeader string) (counted bool, retryAfter time.Duration) {
	upErr := newUpstreamError(statusCode, body)
	// 与生产 relay.go 的 handleResponse 同一条路径：从响应的 Retry-After 头取冷却，
	// 取到了才置 hasRetryAfter（否则 upstreamRetryAfter 会把它当成"上游没给"）。
	header := http.Header{}
	header.Set("Retry-After", retryAfterHeader)
	if d, ok := retryAfterFromHeader(header); ok {
		upErr.retryAfter = d
		upErr.hasRetryAfter = true
	}
	return shouldRecordBreakerFailure(statusCode, upErr), upstreamRetryAfter(upErr)
}

// ClassifyCircuitProbeTransportFailure 把一次**没有收到 HTTP 响应**的失败（dial 失败、
// TLS 握手失败、连接被重置、读超时等）交回同一套归因：没有状态码，直接跑
// shouldRecordBreakerFailure(0, err)。
//
// 调用方仍然要自己区分"探针自己的时钟到点"和"调用方取消/调用方预算到点"：前者是
// 渠道没能在窗口内应答（渠道侧），后者是探测被撤掉（不是渠道故障）。本函数只负责
// 其余传输类失败的判定。
func ClassifyCircuitProbeTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	return shouldRecordBreakerFailure(0, err)
}
