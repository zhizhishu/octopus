package model

type GroupMode int

const (
	GroupModeRoundRobin GroupMode = 1 // 轮询/均摊(对外 Spread)：同优先级容量感知均摊
	GroupModeFailover   GroupMode = 3 // 故障转移(对外 FillFirst)：按优先级选择，失败时降级到下一个
)

// Product-facing modes. The UI exposes only these two, and both are capacity-aware
// (recent health, in-flight + selection reservations, latency, throughput,
// circuit/cooldown). The retired random(2)/weighted(4)/smart(5) modes were removed;
// GetBalancer still folds any unknown value into Spread as a safety net.
const (
	// GroupModeFillFirst keeps a stable priority order so traffic stays
	// concentrated on the top healthy channel (best upstream prompt-cache hit
	// rate) and only sinks to the next one when it trips / cools down / is rate
	// limited.
	GroupModeFillFirst = GroupModeFailover
	// GroupModeSpread load-balances across same-priority channels; priority
	// remains a hard boundary.
	GroupModeSpread = GroupModeRoundRobin
	// GroupModeSpreadTiered 是画布「分层轮询」(用户定 2026-09-30)：条目 Priority 是外层硬边界,
	// 数字越大越先尝试; 同一数字层内仍按 Spread 的渠道优先级 + 健康/容量轮询, 高层整体不可用
	// 时才下沉到低层。
	//
	// 为什么单独一个模式值: 纯轮询(1) 故意忽略条目 Priority —— 拖拽序号天然唯一, 一旦让它当
	// 硬边界就会退化成固定顺序、永不轮转(见 balancer.Spread 注释)。分层语义必须另立, 不能改
	// 纯轮询的比较器, 否则既有轮询规则全部变形。
	//
	// 用 6 而不是 4: 上游历史版本里 2/4/5 是已退役的 random/weighted/smart 模式, 复用 4 会让
	// 老库里残留的 weighted 分组被重新赋义成分层轮询。6 从未被任何版本写入过。
	GroupModeSpreadTiered GroupMode = 6
)

// IsSpreadFamily 表示「轮询家族」: 纯轮询(1) 与分层轮询(6) 都按轮询选路, 而非优先填充。
// 判断「这条规则是不是轮询类」一律用它, 不要再逐个字面量比较。
func (m GroupMode) IsSpreadFamily() bool {
	return m == GroupModeRoundRobin || m == GroupModeSpreadTiered
}

type Group struct {
	ID   int       `json:"id" gorm:"primaryKey"`
	Name string    `json:"name" gorm:"unique;not null"`
	Mode GroupMode `json:"mode" gorm:"not null"`
	// ModeLocked records that an admin explicitly chose this group's Mode (via the
	// access-plan canvas). A locked group keeps its own mode even when the fleet-wide
	// route_mode_override setting is set; unlocked groups follow the global default.
	ModeLocked        bool   `json:"mode_locked" gorm:"not null;default:false"`
	MatchRegex        string `json:"match_regex"`
	FirstTokenTimeOut int    `json:"first_token_time_out"` // 单个渠道首个Token响应超时时间(秒)
	// TotalTimeOut is the per-group override for the whole-request ceiling
	// (relay_request_total_timeout_seconds). 0 = fall back to the global default; a
	// negative value is treated the same way. Unit: seconds.
	TotalTimeOut    int         `json:"total_time_out"`                  // 分组级整次请求时长上限(秒), 0=用全局默认
	SessionKeepTime int         `json:"session_keep_time"`               // 会话保持时间(秒) 0 为禁用
	MaxConcurrent   int         `json:"max_concurrent" gorm:"default:0"` // 分组级并发上限(整组在途请求数), 0=不限. 到顶硬拒(429)以保护上游
	RPMLimit        int         `json:"rpm_limit" gorm:"default:0"`      // 分组级每分钟请求上限(整组近60s请求数), 0=不限. 到顶硬拒(429), 保护上游不被打满
	AutoCreated     bool        `json:"auto_created" gorm:"default:false"`
	Items           []GroupItem `json:"items,omitempty" gorm:"foreignKey:GroupID"`
}

type GroupItem struct {
	ID                   int                      `json:"id" gorm:"primaryKey"`
	GroupID              int                      `json:"group_id" gorm:"not null;index:idx_group_channel_model,unique"` // 创建时不携带此字段,更新时需要
	ChannelID            int                      `json:"channel_id" gorm:"not null;index:idx_group_channel_model,unique"`
	ModelName            string                   `json:"model_name" gorm:"not null;index:idx_group_channel_model,unique"`
	Priority             int                      `json:"priority"`
	Weight               int                      `json:"weight"`
	RoutingWeight        int                      `json:"-" gorm:"-"`
	ChannelPriority      int                      `json:"channel_priority,omitempty" gorm:"-"`
	ChannelStats         StatsChannel             `json:"channel_stats,omitempty" gorm:"-"`
	RoutingStats         RoutingRuntimeStats      `json:"-" gorm:"-"`
	BillingModelSource   AccessBillingModelSource `json:"billing_model_source,omitempty" gorm:"-"`
	BillingModelOverride string                   `json:"billing_model_override,omitempty" gorm:"-"`
}

// RoutingRuntimeStats is an in-memory, request-local snapshot used by smart
// routing. It intentionally is not persisted: durable stats stay in
// StatsChannel, while these fields keep recent latency/health signals reactive
// enough for CLI streaming turns.
type RoutingRuntimeStats struct {
	PreferStream         bool
	HasRuntime           bool
	LatencyEWMAms        float64
	FirstTokenEWMAms     float64
	ThroughputEWMA       float64
	LatencyStale         bool
	InFlight             int64
	PendingSelections    int64
	Attempts             int64
	RequestSuccess       int64
	RequestFailed        int64
	ConsecutiveFailures  int64
	LastFailureUnix      int64
	CooldownRemainingMs  int64
	AvailableKeyCount    int
	HealthyKeyCount      int
	MaxConcurrent        int   // 渠道并发上限(0=不限)，由 enrichGroupForSmartRouting 从 Channel 配置填入，供 spreadTier 判定是否到顶降档
	RPMLimit             int   // 渠道每分钟请求上限(0=不限)，由 enrichGroupForSmartRouting 从 Channel 配置填入，供 spreadTier 判定是否到顶降档
	RecentRequestCount   int64 // 近60s滑动窗口内本渠道(channel+model)发起的上游尝试数，由 telemetry 快照填入，与 RPMLimit 比较
	CircuitTripped       bool
	CircuitOpenKeys      int
	CircuitRemainingMs   int64
	KeyCooldownOpenCount int
}
