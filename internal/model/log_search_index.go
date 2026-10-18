package model

// RelayLogSearchIndex 是 relay_logs 的"搜索投影表": 只存参与搜索的那几列,
// 目的是让 LIKE '%x%' 扫描时不必把每行的 request_content/response_content
// (真实日志常在几百字节到几十 KB)一起从磁盘读出来。
//
// 为什么需要它(实测, 5 万行):
//   - 直接在 relay_logs 上搜: 277ms
//   - 在等价的窄表上搜:        9ms
//   - 只搜 1 列 vs 搜 12 列:   266ms vs 420ms —— 差距远小于"回表读大字段"的成本
//
// 也就是说 LIKE '%x%' 本身的 CPU 成本很低, 慢的是每行都要触到溢出页。
// 把搜索列单独放一张窄表, 扫描就只碰这几列。
//
// 一致性: 由 op 层在写入/删除时同步维护(不是触发器, 触发器在 MySQL/PG/SQLite
// 上的写法不通用, 且历史数据回填也要单独处理)。迁移 014 负责回填存量。
type RelayLogSearchIndex struct {
	// ID 与 relay_logs.id 一一对应, 同时作为主键。
	ID int64 `json:"id" gorm:"primaryKey;autoIncrement:false"`
	// 以下列与 RelayLog 中同名列保持同值, 只用于 LIKE 匹配。
	UserName              string `gorm:"size:255"`
	RequestAPIKeyName     string `gorm:"size:255"`
	RequestModelName      string `gorm:"size:255"`
	ActualModelName       string `gorm:"size:255"`
	UpstreamResponseModel string `gorm:"size:255"`
	ChannelName           string `gorm:"size:255"`
	RequestEndpoint       string `gorm:"size:64"`
	RequestPath           string `gorm:"size:256"`
	SessionKey            string `gorm:"size:96"`
	Error                 string `gorm:"type:text"`
	ErrorCode             string `gorm:"size:128"`
	RequestUserAgent      string `gorm:"size:512"`
	// ChannelId 单独存一份: 数字搜索("channel 3")要能直接命中, 否则就得回
	// relay_logs 查, 又把大字段拖进来。
	ChannelId int `gorm:"column:channel_id;index"`
}

// TableName 固定表名, 避免 GORM 复数化规则变化导致迁移与运行时不一致。
func (RelayLogSearchIndex) TableName() string { return "relay_log_search" }
