package model

import (
	"net/http"
	"sort"
	"strings"
)

// RelayLogUpstreamEnvelope 是「收到上游回复之后顺手抄下来的几个标识位」。
//
// 只在内存里传给被动来源计算（见 op/log_passive_source.go），不落库、不出 API：
// RelayLog.UpstreamEnvelope 的 tag 是 json:"-" gorm:"-"。
//
// 为什么单开一个结构体、并且只留类别/前缀：
//   - 响应头原值、完整编号往往带上游主机名/账号/请求号，落库就是长期留存，
//     而这些位只对「这一次收到的回复」有意义，跨请求聚合没有价值；
//   - 所以这里只留不可反查的两类东西：头家族名、编号前缀。
//
// 三条自律（与 RelayLogPassiveSource 一致）：只证伪不认证、反证优先、单条不够。
type RelayLogUpstreamEnvelope struct {
	// HeaderFamilies 是上游响应头命中的基础设施家族类别，见
	// ClassifyUpstreamHeaderFamilies（只记类别，不记原值）。
	HeaderFamilies []string
	// ResponseID 是上游回复顶层 id 的原值，只在 Anthropic 出站路径采集（该路径的
	// id 是上游原文，见 transformer/outbound/authropic 的 TransformResponse/TransformStream）。
	ResponseID string
	// FirstToolCallID 是第一个非空 tool call id 的原值，采集条件同 ResponseID。
	FirstToolCallID string
	// SignatureClaudeHash 表示 thinking 签名原文以 "claude#" 开头（Vertex 侧形态）。
	// 只存这个布尔位，签名原文一概不留。
	SignatureClaudeHash bool
	// ModelSwitchedTo 是**同一条流**里第二个不同的自报模型名（只写一次）。
	// 首个自报名字不在这里——它在 RelayLog.UpstreamResponseModel 列里。
	ModelSwitchedTo string
}

// ClassifyUpstreamHeaderFamilies 只看响应头的**名字**（小写比较），不看值，
// 返回命中的基础设施家族类别，去重后按字典序排序。
//
// 出处：参考项目 modelprint probes/net-headerdna.js:13-42 的头家族表。
//
// 为什么只记类别：一个响应头原值常常带主机名/账号/请求号，存下来就是长期留存；
// 而「这批日志里有多少条带 aws 家族头」这种聚合问题，类别本身已经够了。
//
// 规则表（与参考项目对齐）：
//   - x-amzn-* / x-amz-*                → aws
//   - x-envoy-upstream-service-time     → envoy
//   - openai-processing-ms / openai-version → openai
//   - anthropic-ratelimit-*             → anthropic_ratelimit
//   - cf-ray                            → cloudflare
func ClassifyUpstreamHeaderFamilies(h http.Header) []string {
	if len(h) == 0 {
		return nil
	}
	set := make(map[string]struct{}, 5)
	for name := range h {
		lower := strings.ToLower(strings.TrimSpace(name))
		switch {
		case strings.HasPrefix(lower, "x-amzn-"), strings.HasPrefix(lower, "x-amz-"):
			set["aws"] = struct{}{}
		case lower == "x-envoy-upstream-service-time":
			set["envoy"] = struct{}{}
		case lower == "openai-processing-ms", lower == "openai-version":
			set["openai"] = struct{}{}
		case strings.HasPrefix(lower, "anthropic-ratelimit-"):
			set["anthropic_ratelimit"] = struct{}{}
		case lower == "cf-ray":
			set["cloudflare"] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
