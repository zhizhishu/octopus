package model

// 日志模式（log_mode）：普通 / 调试。
//
// 普通（默认）：运行日志只打 warn 及以上；中继日志照旧记录，不多存任何东西。
// 调试：运行日志打到 debug；中继日志额外带一份 RelayLogDebugInfo（见下）。
//
// 🔴 调试只是「多看」，不是「多做」：它不改发往上游的任何字节（头、头序、TLS、body），
// 不改路由（优先/轮询照旧），不改计费。所有采集都发生在请求发出之后的只读副本上。
const (
	// 日志模式：normal（普通，默认）/ debug（调试）。即时生效，不用重启。
	SettingKeyLogMode SettingKey = "log_mode"
	// 调试模式开着多少小时后自动回到普通（0 = 不自动关，最长 168）。默认 24。
	SettingKeyLogDebugAutoOffHours SettingKey = "log_debug_auto_off_hours"
	// 调试模式这一次是什么时候打开的（Unix 秒，0 = 没开）。由服务端在切到调试时写入，
	// 与上面的小时数一起算出自动关闭的时刻。
	SettingKeyLogDebugSince SettingKey = "log_debug_since"
)

const (
	LogModeNormal = "normal"
	LogModeDebug  = "debug"

	// DefaultLogDebugAutoOffHours：调试模式默认 24 小时后自动回到普通，防止忘关。
	DefaultLogDebugAutoOffHours = "24"
	// MaxLogDebugAutoOffHours：自动关闭最长 7 天。0 = 不自动关。
	MaxLogDebugAutoOffHours = 168
)

// 调试详情的体量上限。每一项都有上限，整份再有一个总上限，
// 保证「调试开着跑一天」也不会把日志表撑爆。
const (
	RelayLogDebugMaxCalls        = 12        // 最多记录 12 次上游调用（重试/换家/竞速）
	RelayLogDebugMaxHeaders      = 64        // 每组请求头/响应头最多 64 个
	RelayLogDebugMaxHeaderValue  = 512       // 单个头的值最多 512 字节
	RelayLogDebugMaxErrorBody    = 8 * 1024  // 单次上游错误原文最多 8KB
	RelayLogDebugMaxRouteTrace   = 4 * 1024  // 选路说明最多 4KB
	RelayLogDebugMaxNotes        = 8         // 每次调用最多 8 条备注
	RelayLogDebugMaxRoutes       = 8         // 最多记 8 次选路（正常 / 回退 / 救援重选）
	RelayLogDebugMaxTotalBytes   = 48 * 1024 // 整份调试详情序列化后最多 48KB
	RelayLogDebugRetentionDays   = 3         // 调试详情只留 3 天，过期只清这一列，日志本身照常保留
	RelayLogDebugHeaderHiddenTag = "[已隐藏]"
)

// RelayLogDebugInfo 是调试模式下额外记进一条中继日志的明细。
// 只在详情接口返回；列表 / 实时流 / 导出摘要都不带；非管理员读取时清空。
type RelayLogDebugInfo struct {
	// InboundHeaders 是客户端发给我们的请求头（凭据类整值隐藏，其余值做密钥打码）。
	InboundHeaders []RelayLogDebugHeader `json:"inbound_headers,omitempty"`
	// Routes 是选路决定：哪个分组、优先还是轮询、候选顺序和原因。
	Routes []RelayLogDebugRoute `json:"routes,omitempty"`
	// Calls 是每一次真正发往上游的 HTTP 调用，按发出顺序。
	Calls []RelayLogDebugCall `json:"calls,omitempty"`
	// Timing 是整次请求的分段耗时（毫秒；-1 = 没测到）。
	Timing RelayLogDebugTiming `json:"timing"`
	// Truncated 为 true 表示有内容因为上限被截掉了。
	Truncated bool `json:"truncated,omitempty"`
	// DroppedCalls 是超出 RelayLogDebugMaxCalls 没记下的调用次数。
	DroppedCalls int `json:"dropped_calls,omitempty"`
}

type RelayLogDebugHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type RelayLogDebugRoute struct {
	// Stage：primary = 正常选路；fallback = 路由规则的回退池。
	Stage     string `json:"stage"`
	GroupID   int    `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`
	// Mode：fill_first（优先）/ spread（轮询）/ 其它原样。
	Mode       string `json:"mode"`
	Sticky     bool   `json:"sticky,omitempty"`
	Candidates int    `json:"candidates"`
	// Trace 是负载均衡器给出的候选排序说明（优先级、层级、负载、失败数、粘性）。
	Trace string `json:"trace,omitempty"`
}

type RelayLogDebugCall struct {
	ChannelID   int    `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	// ChannelKeyID 只记 Key 的编号，绝不记 Key 本身。
	ChannelKeyID int    `json:"channel_key_id,omitempty"`
	Method       string `json:"method,omitempty"`
	// URL 已去掉查询串里的凭据。
	URL string `json:"url,omitempty"`
	// StartMs 是相对请求开始的毫秒偏移；HeaderMs 是从发出到收到响应头的耗时。
	StartMs  int64 `json:"start_ms"`
	HeaderMs int64 `json:"header_ms"`
	Status   int   `json:"status,omitempty"`
	// RequestHeaders 是发往上游的请求头快照（发送后读取，按名字排序；真实顺序由传输层决定）。
	RequestHeaders  []RelayLogDebugHeader `json:"request_headers,omitempty"`
	ResponseHeaders []RelayLogDebugHeader `json:"response_headers,omitempty"`
	// Error 是这次调用在拿到响应头之前的错误（连接失败、超时等）。
	Error string `json:"error,omitempty"`
	// ErrorBody 是上游非 2xx 时返回的原文（密钥打码，上限 8KB）。
	ErrorBody string `json:"error_body,omitempty"`
	// Notes 是这次调用之后网关做了什么（同 Key 改发、降级等）。
	Notes []string `json:"notes,omitempty"`
}

type RelayLogDebugTiming struct {
	TotalMs       int64 `json:"total_ms"`
	BeforeSendMs  int64 `json:"before_send_ms"` // 请求开始 → 第一次发往上游
	HeaderWaitMs  int64 `json:"header_wait_ms"` // 最后一次调用：发出 → 响应头
	FirstTokenMs  int64 `json:"first_token_ms"` // 请求开始 → 首字
	GenerationMs  int64 `json:"generation_ms"`  // 首字 → 结束
	UpstreamCalls int   `json:"upstream_calls"` // 一共发了几次上游请求
}
