package model

import "time"

// 模型检测任务与结果：独立于日志表存储。
//
// 为什么必须独立成表：此前一次检测的结论是**写回被检测的那一行日志**的
// （relay_logs 上的 model_audit_verdict / score / report 五列，且"先到先得"）。
// 于是检测记录的生命周期被日志保留了绑死——日志一滚动、一按上限清理、一换页，
// 用户就再也找不到"我上次测了这个模型"的结果，只能重测一遍重新花钱。
//
// 检测本身是**主动花钱的动作**（每次都要打真上游、消耗额度），它的产出必须比
// 日志活得更久，也必须能被回头查阅。所以这里把它拆成两张表：
//
//	model_audit_jobs    一次"点了哪些模型"的批量任务（排队单位）
//	model_audit_results  任务里每个模型的逐条结论
//
// 与 relay_logs 的关系刻意只保留一个**可空**的弱引用（LogID）：检测仍可从某条
// 日志发起，但日志被清理后本表记录不受影响、只是那个引用悬空。绝不做级联删除。

// 检测任务状态。
const (
	// AuditJobQueued 已入队，尚未开始。
	AuditJobQueued = "queued"
	// AuditJobRunning 正在跑（至少一个模型在检测中）。
	AuditJobRunning = "running"
	// AuditJobDone 全部模型已出结论（个别失败也算 done，失败在结果行里体现）。
	AuditJobDone = "done"
	// AuditJobCanceled 用户主动取消。
	AuditJobCanceled = "canceled"
)

// 单个模型的检测状态。
const (
	AuditResultPending = "pending"
	AuditResultRunning = "running"
	AuditResultDone    = "done"
	AuditResultFailed  = "failed"
	AuditResultSkipped = "skipped"
)

// ModelAuditJob 是一次批量检测任务。
//
// 排队单位定在"任务"而不是"模型"，是因为用户的心智模型是"我点了一批模型让它去跑"：
// 进度、取消、重试都应该以这一批为单位呈现，而不是让用户逐个盯。
type ModelAuditJob struct {
	ID int64 `json:"id" gorm:"primaryKey;autoIncrement"`

	// ChannelID 本批检测固定的渠道。0 表示跨渠道（每个结果行自带渠道）。
	ChannelID int `json:"channel_id" gorm:"index;default:0"`
	// ChannelName 入队时的渠道名快照：渠道改名或删除后历史记录仍可读。
	ChannelName string `json:"channel_name" gorm:"size:255;default:''"`

	// Status 见 AuditJob* 常量。
	Status string `json:"status" gorm:"size:16;index;default:'queued'"`

	// Total / Finished 用于算进度。Finished 含成功、失败、跳过。
	Total    int `json:"total" gorm:"default:0"`
	Finished int `json:"finished" gorm:"default:0"`

	// Probes 本批启用的探针（逗号分隔）。空 = 默认集合。
	Probes string `json:"probes" gorm:"size:255;default:''"`
	// TimeoutSeconds 单模型超时上限。
	TimeoutSeconds int `json:"timeout_seconds" gorm:"default:0"`

	// Trigger 来源：manual（手动）/ schedule（定时）。
	Trigger string `json:"trigger" gorm:"size:32;default:'manual'"`

	CreatedAt time.Time  `json:"created_at" gorm:"index"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// ModelAuditResult 是一个模型的检测结论。
//
// 判定字段沿用 modelverify/behavior 的既有语义（Verdict: none/low/medium/high/unknown，
// Score 0-100 越高越可疑），**不在这一层改写判定规则**——只负责把结论持久化，
// 翻译成人话是展示层的事。
type ModelAuditResult struct {
	ID    int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	JobID int64 `json:"job_id" gorm:"index"`
	// LogID 发起本次检测的日志行（可空、可悬空）。0 = 与具体日志无关。
	LogID int64 `json:"log_id" gorm:"index;default:0"`

	ChannelID   int    `json:"channel_id" gorm:"index;default:0"`
	ChannelName string `json:"channel_name" gorm:"size:255;default:''"`
	// Model 请求的模型名。
	Model string `json:"model" gorm:"size:255;index;default:''"`
	// UpstreamModel 实际发往上游的型号（有映射时与 Model 不同）。
	UpstreamModel string `json:"upstream_model" gorm:"size:255;default:''"`

	// Status 见 AuditResult* 常量。
	Status string `json:"status" gorm:"size:16;index;default:'pending'"`

	// Verdict / Score / FindingN 与旧日志列同义。
	Verdict  string  `json:"verdict" gorm:"size:16;index;default:''"`
	Score    float64 `json:"score" gorm:"default:0"`
	FindingN int     `json:"finding_n" gorm:"default:0"`

	// Report 完整报告 JSON（含每条探针的证据），供详情页展开。
	Report string `json:"report" gorm:"type:text"`
	// ErrorMsg 检测没跑成时的原因（网络/鉴权/超时），与"查出问题"分开记。
	ErrorMsg string `json:"error_msg" gorm:"type:text"`

	// Attempt 已经试过几次。0 = 还没开始；1 = 第一次；以此类推。
	//
	// 为什么要记：检测会真的打上游、真的花钱。一次失败可能是上游抖了一下、
	// 也可能是这个模型压根不通——不重试就把后者当成"结论"是错的，无限重试
	// 又会烧钱。所以次数要落在行上，看得见、能设上限、能人工判断。
	Attempt int `json:"attempt" gorm:"default:0"`
	// NextRetryAt 下一次可以再试的时间（退避）。nil/零 = 立刻可跑。
	//
	// 用时间点而不是"睡一会儿"，是为了让重试在进程重启后依然有效：
	// 睡在内存里的重试一重启就没了，落在库里的时间点不会。
	NextRetryAt *time.Time `json:"next_retry_at,omitempty" gorm:"index"`

	DurationMs int64      `json:"duration_ms" gorm:"default:0"`
	CreatedAt  time.Time  `json:"created_at" gorm:"index"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// TableName 固定表名，避免 GORM 复数化规则将来变动导致迁移错位。
func (ModelAuditJob) TableName() string    { return "model_audit_jobs" }
func (ModelAuditResult) TableName() string { return "model_audit_results" }
