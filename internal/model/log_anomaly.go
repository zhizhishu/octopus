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

// LogAnomalyProbeBucket 是被动探针结论的聚合桶: 按 channel + model + 探针码分桶。
//
// 与 LogAnomalyFinding 的区别很重要: finding 是**行为变了**(输出塌了、变慢了),
// probe 是**这条日志自相矛盾**(账算不平、分词器口径不对)。后者更硬, 但也更需要
// 计数支撑——单条对不上可能是上游一时的 bug, 反复出现才是模式。
type LogAnomalyProbeBucket struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	ChannelID   int    `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Model       string `json:"model"`
	// Count 是命中条数, Total 是同桶内参与检查的日志条数。
	// 只给 Count 会让人以为是全部日志都出问题, 必须带上分母。
	Count int `json:"count"`
	Total int `json:"total"`
	// Title 取该桶内第一条的文案, 供界面直接展示。
	Title string `json:"title"`
}

// LogAnomalyReport 是一次只读日志扫描的结果。不发上游请求、不写库。
type LogAnomalyReport struct {
	WindowHours int                 `json:"window_hours"`
	SampleCount int                 `json:"sample_count"`
	BucketCount int                 `json:"bucket_count"`
	Findings    []LogAnomalyFinding `json:"findings"`
	// PassiveObservations 是被动观察摘要的独立汇总, 不参与上面的 findings 判定。
	PassiveObservations []LogAnomalyPassiveBucket `json:"passive_observations,omitempty"`
	// ProbeFindings 是硬事实校验的汇总(账目/缓存/分词器口径), 与上面的行为类
	// findings 分开列, 因为两者证据强度不同, 混在一起会拉低硬结论的可信度。
	ProbeFindings []LogAnomalyProbeBucket `json:"probe_findings,omitempty"`
	Disclaimer    string                  `json:"disclaimer"`
}
