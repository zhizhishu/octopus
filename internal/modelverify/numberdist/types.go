// Package numberdist 实现 hlwy「1–355 单数字采样」的解析、355 桶分布统计、原评分
// 公式与基准加载/导入。
//
// 边界（照抄计划 §4）：本包只做数字解析、桶统计、基准校验、公式与类型。不读数据库／
// 环境变量／Key，不自建 HTTP 客户端，不运行采样循环——发题循环由后端（worker）持有。
// 采样输入是既有的 modelverify.Sender：backend 用 Sender.Ask 取回可见正文，再把正文
// 交给本包的 Parse；本包不 import modelverify 之外的东西，也不改 Sender 形状。
//
// 数据来自 hlwy（LGPL-2.1），见 data/hlwy/README.md 与 data/hlwy/LICENSE。
package numberdist

import "time"

// SchemaVersion 基准外壳结构版本。
const SchemaVersion = 1

const (
	// DistLength 是分布的桶数：整数 1..355 共 355 个桶。
	DistLength = 355
	// ProtocolID 标识本次采用的题目/解析/统计规则。
	ProtocolID = "hlwy-number355-v1"
	// ParserVersion 标识解析实现版本（首数字 ASCII 规则）。
	ParserVersion = "hlwy-extract-v1"
	// AlgorithmVer 标识评分公式版本。
	AlgorithmVer = "hlwy-number355-score-v1"
	// TransportProfile 表示经本项目渠道链采样（不是原工具裸客户端）。
	TransportProfile = "octopus-channel-v1"
	// Prompt 是 hlwy 的固定题目，逐字采用，不改措辞。
	Prompt = "请从1到355之间随机选择一个数字，只输出这个数字，不要有任何其他内容。"
	// MinScorable 是上游「有效样本至少 40 才展示相似度」的门槛（兼容行为，
	// 不代表 40 个样本已具备鉴别力）。
	MinScorable = 40
	// DefaultMaxImportBytes 导入文件字节上限。
	DefaultMaxImportBytes = 1 << 20 // 1 MiB
	// DefaultMaxNumbers 单份基准 rawData 数字个数上限。
	DefaultMaxNumbers = 10000
	// MethodSingleNumber 是本实现支持的单数字采样方法标识；导入时声明其它方法
	// （如序列指纹）应被拒绝，不能当作同协议数据。
	MethodSingleNumber = "single-number"
	// MaxNameBytes/MaxModelBytes/MaxAPITypeBytes 是导入文本字段的字节上限，
	// 防止把超长或不可控内容塞进下游与 ID。
	MaxNameBytes    = 255
	MaxModelBytes   = 255
	MaxAPITypeBytes = 64
)

// 温度/采样参数的三态：unknown（未观测）、absent（观测到最终请求体省略该字段）、
// value（观测到显式值）。缺失一律 unknown；不能用 nil 推断成 absent 或某个默认值。
const (
	StateUnknown = "unknown"
	StateAbsent  = "absent"
	StateValue   = "value"
)

// SourceKind 基准来源类别。
type SourceKind string

const (
	SourceBuiltin  SourceKind = "builtin_hlwy"
	SourceImported SourceKind = "imported_hlwy"
	SourceLocal    SourceKind = "local_capture"
)

// Source 来源声明（与证明分开）。
type Source struct {
	Kind   SourceKind `json:"kind"`
	Repo   string     `json:"repo,omitempty"`
	Commit string     `json:"commit,omitempty"`
	Path   string     `json:"path,omitempty"`
	Note   string     `json:"note,omitempty"`
}

// Conditions 记录一次采样「实际发出的请求条件」。
//
// 三态语义：
//   - TemperatureState/TopPState 取 unknown|absent|value，配套 Temperature/TopP 可空数值；
//   - 余下可空字段（Stream、MaxTokens、SystemInjected、ToolsInjected）：nil 表示 unknown，
//     非 nil 表示观测到的取值（存在即已观测）。
//
// 「缺省 temperature」不能替换成「实际温度 1」；nil 也绝不等于「观测到省略」。
type Conditions struct {
	// Protocol 是实际出站协议（如 anthropic-messages / openai-chat）。
	Protocol         string `json:"protocol,omitempty"`
	ProtocolID       string `json:"protocol_id,omitempty"`
	ParserVersion    string `json:"parser_version,omitempty"`
	PromptSHA256     string `json:"prompt_sha256,omitempty"`
	TransportProfile string `json:"transport_profile,omitempty"`

	TemperatureState string   `json:"temperature_state"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopPState        string   `json:"top_p_state"`
	TopP             *float64 `json:"top_p,omitempty"`

	Stream        *bool  `json:"stream,omitempty"`
	MaxTokenField string `json:"max_token_field,omitempty"`
	MaxTokens     *int   `json:"max_tokens,omitempty"`

	Thinking        string `json:"thinking,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	SystemInjected *bool `json:"system_injected,omitempty"`
	ToolsInjected  *bool `json:"tools_injected,omitempty"`

	// Completeness 取 unknown|partial|complete；UnknownFields 列出仍未观测的字段名。
	Completeness  string   `json:"completeness"`
	UnknownFields []string `json:"unknown_fields,omitempty"`
}

// unknownConditions 是「旧数据/未观测」的条件快照：所有参数 unknown，绝非默认值。
func unknownConditions() Conditions {
	return Conditions{
		TemperatureState: StateUnknown,
		TopPState:        StateUnknown,
		Completeness:     "unknown",
		UnknownFields: []string{
			"temperature", "top_p", "max_tokens", "stream",
			"thinking", "reasoning_effort", "system", "tools",
		},
	}
}

// Stats 一组数字的描述统计。Median 是上中位数（sorted[n/2]）；Mode 并列时取最小整数
// （对齐原 JS 整数属性升序枚举），ModeCount 为该众数出现次数。
type Stats struct {
	Mean      float64 `json:"mean"`
	StdDev    float64 `json:"std_dev"`
	Median    int     `json:"median"`
	Min       int     `json:"min"`
	Max       int     `json:"max"`
	Unique    int     `json:"unique"`
	Mode      int     `json:"mode"`
	ModeCount int     `json:"mode_count"`
}

// Baseline 一份不可变的基准。ID 与 PayloadSHA256 由内容决定，不可原地改写。
type Baseline struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Label         string `json:"label"`
	PayloadSHA256 string `json:"payload_sha256"`
	DeclaredModel string `json:"declared_model"`
	// APIType 是旧文件自报的接口类型（openai/openai-responses/anthropic），作为来源事实保留。
	APIType string `json:"api_type,omitempty"`
	// CollectedAt 可空：旧文件 timestamp 缺失/无效时为 nil，绝不用今天补齐。
	CollectedAt *time.Time `json:"collected_at,omitempty"`
	ImportedAt  time.Time  `json:"imported_at"`
	Source      Source     `json:"source"`
	Conditions  Conditions `json:"conditions"`
	// RawNumbers 是有效数字（1..355），用于对照重算。
	RawNumbers []int           `json:"raw_numbers"`
	Counts     [DistLength]int `json:"counts"`
	Stats      Stats           `json:"stats"`
}

// ParseResult 一次可见回答的解析结果。Truncated 不在此推断，由调用方从 finish reason
// 记录。Number 仅在 Valid 时有意义。
type ParseResult struct {
	Number        int    `json:"number"`
	Valid         bool   `json:"valid"`
	StrictInteger bool   `json:"strict_integer"`
	Reason        string `json:"reason,omitempty"`
}

// ScoreResult 单个候选基准的对照分数。不含概率、通过/失败标签或阈值结论。
type ScoreResult struct {
	BaselineID string  `json:"baseline_id"`
	Cosine     float64 `json:"cosine"`
	JSD        float64 `json:"jsd"`
	ModeScore  float64 `json:"mode_score"`
	Overall    float64 `json:"overall"`
	Rank       int     `json:"rank"`
	// GapToPrevious 是与前一名（更高分）的 overall 差，单位同 overall（0..1）；
	// 展示时 ×100 为百分点。第一名无前一名，故为 nil。
	GapToPrevious *float64 `json:"gap_to_previous,omitempty"`
}

// Comparison 一次采样对一组基准的对照结果。
type Comparison struct {
	Scorable         bool                `json:"scorable"`
	Reason           string              `json:"reason,omitempty"`
	ValidCount       int                 `json:"valid_count"`
	Distribution     [DistLength]float64 `json:"distribution"`
	Stats            Stats               `json:"stats"`
	ChosenBaselineID string              `json:"chosen_baseline_id,omitempty"`
	// Matches 按 overall 降序；并列时按 BaselineID 升序稳定排序。不可评分时为空。
	Matches []ScoreResult `json:"matches"`
}

// ImportOptions 导入配置。零值用默认上限。
type ImportOptions struct {
	MaxBytes   int       // <=0 用 DefaultMaxImportBytes
	MaxNumbers int       // <=0 用 DefaultMaxNumbers
	Source     Source    // 来源类别/备注覆盖；零值按 imported_hlwy
	ImportedAt time.Time // 零值用当前时间（UTC）
}

// normalize 返回填好默认值的副本：上限回退到默认，ImportedAt 缺省为 now。
func (o ImportOptions) normalize() ImportOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxImportBytes
	}
	if o.MaxNumbers <= 0 {
		o.MaxNumbers = DefaultMaxNumbers
	}
	if o.Source.Kind == "" {
		o.Source = Source{Kind: SourceImported, Repo: "hlwy"}
	}
	if o.ImportedAt.IsZero() {
		o.ImportedAt = nowUTC()
	}
	return o
}
