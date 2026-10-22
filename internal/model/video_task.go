package model

import "encoding/json"

// VideoTaskStatus 是一条视频生成任务在上游的生命周期状态。
//
// 取值刻意与上游（火山 Ark / 豆包视频生成）的 status 字段逐字对齐，
// 这样轮询回来的原始状态可以直接落库，不需要在中间层做一次易错的状态翻译。
type VideoTaskStatus string

const (
	// VideoTaskNotStart 任务已创建但尚未提交给上游。
	VideoTaskNotStart VideoTaskStatus = "not_start"
	// VideoTaskSubmitted 已提交上游，等上游受理。
	VideoTaskSubmitted VideoTaskStatus = "submitted"
	// VideoTaskQueued 上游已受理，在排队。
	VideoTaskQueued VideoTaskStatus = "queued"
	// VideoTaskInProgress 上游正在生成。
	VideoTaskInProgress VideoTaskStatus = "in_progress"
	// VideoTaskSuccess 生成成功，结果地址在 ResultURL。
	VideoTaskSuccess VideoTaskStatus = "success"
	// VideoTaskFailure 生成失败，原因在 FailReason。
	VideoTaskFailure VideoTaskStatus = "failure"
	// VideoTaskUnknown 上游返回了本层不认识的状态。**不要**把它当终态，
	// 也不要当成功——继续轮询，让上游自己收敛。
	VideoTaskUnknown VideoTaskStatus = "unknown"
)

// VideoTask 是一条异步视频生成任务。
//
// 为什么必须独立成表：视频生成不是请求-响应式的，提交后可能几分钟到几十分钟
// 才出结果，而这个结果必须比发起它的那次 HTTP 请求、比进程重启活得更久。
// 状态、进度、失败原因、结果地址都落在行上，轮询进程只是这一行状态的推进者。
//
// 【铁律】本表**绝不存渠道密钥**。
// 只存 ChannelKeyID（那把 key 的 id 引用）。原因是：这张表会被列表/详情接口
// 直接读出返回给用户，任何明文密钥一旦落在这里就等于默认泄露；而 key 会轮换、
// 会禁用，存 id 才能在轮询时重新取到当前有效的那把。轮询需要鉴权时按
// ChannelKeyID 去 channels 表现取，不在这里缓存副本。
type VideoTask struct {
	ID        int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt int64 `json:"created_at" gorm:"index"`
	UpdatedAt int64 `json:"updated_at"`

	// UpstreamTaskID 上游返回的任务 id，轮询时用它去查。
	UpstreamTaskID string `json:"upstream_task_id" gorm:"size:128;index;default:''"`

	// ChannelID / ChannelKeyID 发起时用的渠道与 key 的**引用**（不是密钥本身）。
	ChannelID    int `json:"channel_id" gorm:"index;default:0"`
	ChannelKeyID int `json:"channel_key_id" gorm:"default:0"`

	Model string `json:"model" gorm:"size:255;default:''"`

	// OwnerTokenID / OwnerUserID 任务的归属，用于「只能看自己的任务」。
	OwnerTokenID int `json:"owner_token_id" gorm:"index;default:0"`
	OwnerUserID  int `json:"owner_user_id" gorm:"index;default:0"`

	// Status 见 VideoTask* 常量。
	Status VideoTaskStatus `json:"status" gorm:"size:16;index;default:'not_start'"`

	Progress string `json:"progress" gorm:"size:32;default:''"`

	FailReason string `json:"fail_reason" gorm:"type:text"`
	ResultURL  string `json:"result_url" gorm:"type:text"`

	// SubmitTime / StartTime / FinishTime 是上游给出的时间点（unix 秒），0 = 未到达。
	SubmitTime int64 `json:"submit_time" gorm:"index;default:0"`
	StartTime  int64 `json:"start_time" gorm:"default:0"`
	FinishTime int64 `json:"finish_time" gorm:"default:0"`

	// RequestData 提交给上游的原始请求体，留存用于复现/对账。
	// 存原始 JSON 而不是解析后的结构：上游加字段不该让我们丢信息。
	RequestData json.RawMessage `json:"request_data" gorm:"serializer:json"`
}

// TableName 固定表名，避免 GORM 复数化规则将来变动导致迁移与运行时错位。
func (VideoTask) TableName() string { return "video_tasks" }

// IsVideoTaskTerminal 报告该状态是否已经是终态。
//
// 只有 success / failure 是终态。unknown 刻意不算：上游返回一个本层不认识的
// 状态时，最安全的假设是「还在变」，继续轮询即可；把它当终态会让任务永远卡住。
func IsVideoTaskTerminal(s VideoTaskStatus) bool {
	return s == VideoTaskSuccess || s == VideoTaskFailure
}
