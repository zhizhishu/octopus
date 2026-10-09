package model

// ScheduledAuditResult 是一次定时快检对单个渠道+模型的结果。
type ScheduledAuditResult struct {
	ChannelID   int    `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Model       string `json:"model"`
	Trigger     string `json:"trigger"`
	// LogID 是触发本次跟进/检测的那条日志行 ID(0 = 不是由单条日志触发, 例如整轮定时快检)。
	// 过渡期兼容字段: 结论落库后前端优先读日志行自身的审计列, 这个 ID 只用于旧行回退。
	LogID      int64  `json:"log_id,omitempty"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`
	Verdict    string `json:"verdict,omitempty"`
	Score      int    `json:"score,omitempty"`
	FindingN   int    `json:"finding_count,omitempty"`
	ErrorN     int    `json:"error_count,omitempty"`
}

// ScheduledAuditSnapshot 是最近一次定时快检的内存快照。不落库。
type ScheduledAuditSnapshot struct {
	LastRunUnix int64                  `json:"last_run_unix"`
	TargetCount int                    `json:"target_count"`
	RanCount    int                    `json:"ran_count"`
	Results     []ScheduledAuditResult `json:"results"`
	Disclaimer  string                 `json:"disclaimer"`
}
