package model

// StatsUsage 是一段时间、一个分组里的用量合计，全部来自请求日志（relay_logs）。
//
// 字段名和 StatsMetrics 对齐（input_token / cache_hit_token / …），前端可以用同一套
// 公式算 Tokens 合计和缓存命中率，同一个标签在主页和仪表盘上是同一个口径。
// 日志只有一个总价，所以这里是 total_cost，不拆输入/输出价。
type StatsUsage struct {
	RequestSuccess  int64   `json:"request_success"`
	RequestFailed   int64   `json:"request_failed"`
	InputToken      int64   `json:"input_token"`
	OutputToken     int64   `json:"output_token"`
	CacheHitToken   int64   `json:"cache_hit_token"`
	CacheWriteToken int64   `json:"cache_write_token"`
	CacheInputToken int64   `json:"cache_input_token"`
	TotalCost       float64 `json:"total_cost"`
	// WaitTime 是 use_time（毫秒）之和；平均用时 = WaitTime / 请求数。
	WaitTime int64 `json:"wait_time"`
}

// Add 把另一份用量累加进来。
func (u *StatsUsage) Add(other StatsUsage) {
	u.RequestSuccess += other.RequestSuccess
	u.RequestFailed += other.RequestFailed
	u.InputToken += other.InputToken
	u.OutputToken += other.OutputToken
	u.CacheHitToken += other.CacheHitToken
	u.CacheWriteToken += other.CacheWriteToken
	u.CacheInputToken += other.CacheInputToken
	u.TotalCost += other.TotalCost
	u.WaitTime += other.WaitTime
}

// Requests 是请求总数（成功 + 失败）。
func (u StatsUsage) Requests() int64 { return u.RequestSuccess + u.RequestFailed }

// StatsBreakdownItem 是按某个维度拆开后的一行。
type StatsBreakdownItem struct {
	// Key 是分组键：模型名，或者用户/Key/渠道/方案的数字 id，或者小时序号。
	Key string `json:"key"`
	// ID 是用户/Key/渠道/方案的 id（模型和小时为 0）。
	ID   int    `json:"id"`
	Name string `json:"name"`
	// UserID / UserName 只在 by=apikey 时有值：这个 Key 属于谁。
	UserID   int    `json:"user_id,omitempty"`
	UserName string `json:"user_name,omitempty"`
	// Missing = 日志里有、但用户/Key/渠道/方案已经被删了；Name 退回日志里记的名字。
	Missing bool `json:"missing,omitempty"`
	// Start 只在 by=hour 时有值：这一小时开始的 unix 秒。
	Start int64 `json:"start,omitempty"`
	StatsUsage
}

// StatsBreakdown 是 GET /api/v1/stats/breakdown 的返回。
type StatsBreakdown struct {
	From string `json:"from"`
	To   string `json:"to"`
	By   string `json:"by"`
	Days int    `json:"days"`
	// Total 是整个时间段的合计（不受 limit 影响）。
	Total StatsUsage           `json:"total"`
	Items []StatsBreakdownItem `json:"items"`
	// ItemCount 是分组总数；超过 limit 的部分合进 Others。
	ItemCount   int         `json:"item_count"`
	Others      *StatsUsage `json:"others,omitempty"`
	OthersCount int         `json:"others_count"`
	// ActiveUsers 只在 by=user 时返回：这段时间里发过请求的用户数（user_id > 0 去重）。
	ActiveUsers *int `json:"active_users,omitempty"`
	// OldestLogTime 是现在库里最早一条日志的时间（unix 秒，0 = 没有日志）。
	// 早于它的日子日志已被清理，算不出来——页面据此提示。
	OldestLogTime int64 `json:"oldest_log_time"`
}

// StatsUserTrendUser 是「前 N 名每天花多少」里的一条线。
type StatsUserTrendUser struct {
	UserID  int          `json:"user_id"`
	Name    string       `json:"name"`
	Missing bool         `json:"missing,omitempty"`
	Total   StatsUsage   `json:"total"`
	Points  []StatsUsage `json:"points"`
}

// StatsUserTrend 是 GET /api/v1/stats/user-trend 的返回；Points 与 Dates 一一对应。
type StatsUserTrend struct {
	From          string               `json:"from"`
	To            string               `json:"to"`
	Dates         []string             `json:"dates"`
	Users         []StatsUserTrendUser `json:"users"`
	OldestLogTime int64                `json:"oldest_log_time"`
}
