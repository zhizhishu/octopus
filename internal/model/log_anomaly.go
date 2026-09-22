package model

// LogAnomalyFinding 是一条从近期真实调用日志里扫出来的线索。
// 不是假模型证明：输出塌了、变慢了、回显不符，都只说明行为变了。
type LogAnomalyFinding struct {
	Code        string         `json:"code"`
	Severity    string         `json:"severity"`
	ChannelID   int            `json:"channel_id"`
	ChannelName string         `json:"channel_name"`
	Model       string         `json:"model"`
	Title       string         `json:"title"`
	Evidence    map[string]any `json:"evidence,omitempty"`
}

// LogAnomalyReport 是一次只读日志扫描的结果。不发上游请求、不写库。
type LogAnomalyReport struct {
	WindowHours int                 `json:"window_hours"`
	SampleCount int                 `json:"sample_count"`
	BucketCount int                 `json:"bucket_count"`
	Findings    []LogAnomalyFinding `json:"findings"`
	Disclaimer  string              `json:"disclaimer"`
}
