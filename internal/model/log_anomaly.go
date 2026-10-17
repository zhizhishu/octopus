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

// LogAnomalyPassiveBucket 是被动观察摘要的聚合桶: 按 channelID + actual model 优先
// + request endpoint + is_stream + language + feature version 分桶, 只汇 count,
// 不告警、不立历史为官方基准、不计算漂移阈值。
type LogAnomalyPassiveBucket struct {
	ChannelID      int    `json:"channel_id"`
	Model          string `json:"model"`
	Endpoint       string `json:"endpoint"`
	IsStream       bool   `json:"is_stream"`
	Language       string `json:"language"`
	FeatureVersion string `json:"feature_version"`
	Count          int    `json:"count"`
	NoProfileCount int    `json:"no_profile_count"`
}

// LogAnomalyReport 是一次只读日志扫描的结果。不发上游请求、不写库。
type LogAnomalyReport struct {
	WindowHours int                 `json:"window_hours"`
	SampleCount int                 `json:"sample_count"`
	BucketCount int                 `json:"bucket_count"`
	Findings    []LogAnomalyFinding `json:"findings"`
	// PassiveObservations 是被动观察摘要的独立汇总, 不参与上面的 findings 判定。
	PassiveObservations []LogAnomalyPassiveBucket `json:"passive_observations,omitempty"`
	Disclaimer          string                    `json:"disclaimer"`
}
