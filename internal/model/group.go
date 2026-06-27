package model

type GroupMode int

const (
	GroupModeRoundRobin GroupMode = 1 // 轮询：依次循环选择渠道
	GroupModeRandom     GroupMode = 2 // 随机：每次随机选择一个渠道
	GroupModeFailover   GroupMode = 3 // 故障转移：按优先级选择，失败时降级到下一个
	GroupModeWeighted   GroupMode = 4 // 加权分配：按优权重分配流量
	GroupModeSmart      GroupMode = 5 // 智能评分：综合优先级、权重、成功率、失败率和等待时间排序
)

// Product-facing modes. The UI now exposes only these two, and both are
// capacity-aware (recent health, in-flight + selection reservations, latency,
// throughput, circuit/cooldown). Legacy stored values (random/weighted/smart)
// fold into spread/round-robin inside GetBalancer, so existing groups keep
// working without a data migration.
const (
	// GroupModeFillFirst keeps a stable priority order so traffic stays
	// concentrated on the top healthy channel (best upstream prompt-cache hit
	// rate) and only sinks to the next one when it trips / cools down / is rate
	// limited.
	GroupModeFillFirst = GroupModeFailover
	// GroupModeSpread load-balances across same-priority channels; priority
	// remains a hard boundary.
	GroupModeSpread = GroupModeRoundRobin
)

type Group struct {
	ID                int         `json:"id" gorm:"primaryKey"`
	Name              string      `json:"name" gorm:"unique;not null"`
	Mode              GroupMode   `json:"mode" gorm:"not null"`
	MatchRegex        string      `json:"match_regex"`
	FirstTokenTimeOut int         `json:"first_token_time_out"`            // 单个渠道首个Token响应超时时间(秒)
	SessionKeepTime   int         `json:"session_keep_time"`               // 会话保持时间(秒) 0 为禁用
	MaxConcurrent     int         `json:"max_concurrent" gorm:"default:0"` // 分组级并发上限(整组在途请求数), 0=不限. 与渠道级软降档不同: 模型已路由到本组无处可铺, 到顶硬拒(429)以保护上游
	RPMLimit          int         `json:"rpm_limit" gorm:"default:0"`      // 分组级每分钟请求上限(整组近60s请求数), 0=不限. 到顶硬拒(429), 保护上游不被打满
	AutoCreated       bool        `json:"auto_created" gorm:"default:false"`
	Items             []GroupItem `json:"items,omitempty" gorm:"foreignKey:GroupID"`
}

type GroupItem struct {
	ID                   int                      `json:"id" gorm:"primaryKey"`
	GroupID              int                      `json:"group_id" gorm:"not null;index:idx_group_channel_model,unique"` // 创建时不携带此字段,更新时需要
	ChannelID            int                      `json:"channel_id" gorm:"not null;index:idx_group_channel_model,unique"`
	ModelName            string                   `json:"model_name" gorm:"not null;index:idx_group_channel_model,unique"`
	Priority             int                      `json:"priority"`
	Weight               int                      `json:"weight"`
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

// GroupUpdateRequest 分组更新请求 - 仅包含变更的数据
type GroupUpdateRequest struct {
	ID                int                      `json:"id" binding:"required"`
	Name              *string                  `json:"name,omitempty"`                 // 仅在名称变更时发送
	Mode              *GroupMode               `json:"mode,omitempty"`                 // 仅在模式变更时发送
	MatchRegex        *string                  `json:"match_regex,omitempty"`          // 仅在匹配正则变更时发送
	FirstTokenTimeOut *int                     `json:"first_token_time_out,omitempty"` // 仅在超时变更时发送(秒)
	SessionKeepTime   *int                     `json:"session_keep_time,omitempty"`    // 仅在会话保持时间变更时发送(秒)
	MaxConcurrent     *int                     `json:"max_concurrent,omitempty"`       // 分组级并发上限, 0=不限
	RPMLimit          *int                     `json:"rpm_limit,omitempty"`            // 分组级每分钟请求上限, 0=不限
	ItemsToAdd        []GroupItemAddRequest    `json:"items_to_add,omitempty"`         // 新增的 items
	ItemsToUpdate     []GroupItemUpdateRequest `json:"items_to_update,omitempty"`      // 更新的 items (priority 变更)
	ItemsToDelete     []int                    `json:"items_to_delete,omitempty"`      // 删除的 item IDs
}

// GroupItemAddRequest 新增 item 请求
type GroupItemAddRequest struct {
	ChannelID int    `json:"channel_id" binding:"required"`
	ModelName string `json:"model_name" binding:"required"`
	Priority  int    `json:"priority,omitempty"`
	Weight    int    `json:"weight,omitempty"`
}

// GroupItemUpdateRequest 更新 item 请求
type GroupItemUpdateRequest struct {
	ID       int `json:"id" binding:"required"`
	Priority int `json:"priority,omitempty"`
	Weight   int `json:"weight,omitempty"`
}
type GroupIDAndLLMName struct {
	ChannelID int
	ModelName string
}
