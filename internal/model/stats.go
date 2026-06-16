package model

type StatsMetrics struct {
	InputToken      int64   `json:"input_token" gorm:"bigint"`
	OutputToken     int64   `json:"output_token" gorm:"bigint"`
	InputCost       float64 `json:"input_cost" gorm:"type:real"`
	OutputCost      float64 `json:"output_cost" gorm:"type:real"`
	WaitTime        int64   `json:"wait_time" gorm:"bigint"`
	RequestSuccess  int64   `json:"request_success" gorm:"bigint"`
	RequestFailed   int64   `json:"request_failed" gorm:"bigint"`
	CacheHitToken   int64   `json:"cache_hit_token" gorm:"bigint"`
	CacheWriteToken int64   `json:"cache_write_token" gorm:"bigint"`
	CacheInputToken int64   `json:"cache_input_token" gorm:"bigint"`
}

type StatsTotal struct {
	ID int `gorm:"primaryKey"`
	StatsMetrics
}

type StatsHourly struct {
	Hour int    `json:"hour" gorm:"primaryKey"`
	Date string `json:"date" gorm:"not null"` // 记录最后更新日期，格式：20060102
	StatsMetrics
}

type StatsDaily struct {
	Date string `json:"date" gorm:"primaryKey"`
	StatsMetrics
}

type StatsModel struct {
	ID        int    `json:"id" gorm:"primaryKey"`
	Name      string `json:"name" gorm:"not null"`
	ChannelID int    `json:"channel_id" gorm:"not null"`
	StatsMetrics
}

type StatsChannel struct {
	ChannelID int `json:"channel_id" gorm:"primaryKey"`
	StatsMetrics
}

type StatsAPIKey struct {
	APIKeyID int `json:"api_key_id" gorm:"primaryKey"`
	StatsMetrics
}

type StatsAPIKeyUsageMetrics struct {
	InputToken     int64   `json:"input_token"`
	OutputToken    int64   `json:"output_token"`
	InputCost      float64 `json:"input_cost"`
	OutputCost     float64 `json:"output_cost"`
	WaitTime       int64   `json:"wait_time"`
	RequestSuccess int64   `json:"request_success"`
	RequestFailed  int64   `json:"request_failed"`
}

type StatsAPIKeyUsage struct {
	APIKeyID int `json:"api_key_id"`
	StatsAPIKeyUsageMetrics
}

func NewStatsAPIKeyUsage(stats StatsAPIKey) StatsAPIKeyUsage {
	return StatsAPIKeyUsage{
		APIKeyID: stats.APIKeyID,
		StatsAPIKeyUsageMetrics: StatsAPIKeyUsageMetrics{
			InputToken:     stats.InputToken,
			OutputToken:    stats.OutputToken,
			InputCost:      stats.InputCost,
			OutputCost:     stats.OutputCost,
			WaitTime:       stats.WaitTime,
			RequestSuccess: stats.RequestSuccess,
			RequestFailed:  stats.RequestFailed,
		},
	}
}

func NewStatsAPIKeyUsageList(stats []StatsAPIKey) []StatsAPIKeyUsage {
	usage := make([]StatsAPIKeyUsage, 0, len(stats))
	for _, item := range stats {
		usage = append(usage, NewStatsAPIKeyUsage(item))
	}
	return usage
}

type ModelHealthSummary struct {
	RequestSuccess  int64   `json:"request_success"`
	RequestFailed   int64   `json:"request_failed"`
	FirstTokenP90Ms int64   `json:"first_token_p90_ms"`
	AvgThroughput   float64 `json:"avg_throughput"`
	CacheHitRate    float64 `json:"cache_hit_rate"`
}

type ModelHealthHour struct {
	Hour int `json:"hour"`
	ModelHealthSummary
}

type ModelHealthModel struct {
	Model   string             `json:"model"`
	Hours   []ModelHealthHour  `json:"hours"`
	Summary ModelHealthSummary `json:"summary"`
}

type ModelHealthProvider struct {
	Provider string             `json:"provider"`
	Models   []ModelHealthModel `json:"models"`
}

type ModelHealthResponse struct {
	Date      string                `json:"date"`
	Providers []ModelHealthProvider `json:"providers"`
}

type ModelRankItem struct {
	Model              string   `json:"model"`
	RequestCount       int64    `json:"request_count"`
	RequestSuccess     int64    `json:"request_success"`
	RequestFailed      int64    `json:"request_failed"`
	InputToken         int64    `json:"input_token"`
	OutputToken        int64    `json:"output_token"`
	CacheHitToken      int64    `json:"cache_hit_token"`
	CacheInputToken    int64    `json:"cache_input_token"`
	CacheWriteToken    int64    `json:"cache_write_token"`
	TotalToken         int64    `json:"total_token"`
	TotalCost          float64  `json:"total_cost"`
	CacheHitRate       float64  `json:"cache_hit_rate"`
	FirstTokenP90Ms    int64    `json:"first_token_p90_ms"`
	AvgThroughput      float64  `json:"avg_throughput"`
	RecentActualModels []string `json:"recent_actual_models"`
}

// Add aggregates another StatsMetrics into the current one.
func (s *StatsMetrics) Add(delta StatsMetrics) {
	s.InputToken += delta.InputToken
	s.OutputToken += delta.OutputToken
	s.InputCost += delta.InputCost
	s.OutputCost += delta.OutputCost
	s.WaitTime += delta.WaitTime
	s.RequestSuccess += delta.RequestSuccess
	s.RequestFailed += delta.RequestFailed
	s.CacheHitToken += delta.CacheHitToken
	s.CacheWriteToken += delta.CacheWriteToken
	s.CacheInputToken += delta.CacheInputToken
}
