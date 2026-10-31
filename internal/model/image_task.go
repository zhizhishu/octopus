package model

// 图片异步任务（POST /v1/images/generations/async）的状态取值。
//
// 【只许三值】processing / completed / failed。刻意没有 pending / queued / cancelled：
// 状态机多一个值，客户端就多一条必须处理而多数人不会处理的分支，而这张表的状态
// 恰恰是靠一个后台执行器推进的——多出来的值很容易变成"永远停在那儿"。
//
// 取值是**对外契约的一部分**（客户端按字符串判断），所以逐字固定在这里，
// 任何新增/改名都等于改 API。
const (
	// ImageTaskStatusProcessing 已受理、尚未出结果（提交即进入这个状态，
	// 也是唯一允许的初始状态）。
	ImageTaskStatusProcessing = "processing"
	// ImageTaskStatusCompleted 上游成功返回，结果在 ImageTask.ResultJSON / ImageURL 里。
	ImageTaskStatusCompleted = "completed"
	// ImageTaskStatusFailed 上游明确失败、或超过 expires_at 仍未完成。
	// 两者的区别只在 ErrorMessage 的文字上，状态一律 failed。
	ImageTaskStatusFailed = "failed"
)

// 图片异步任务的子路径。存的是"回到哪条上游路径重放"，不是客户端看到的完整 URL。
//
// 空值 = generations（见 ImageTask.Endpoint 的注释：存量/默认走这条）。
const (
	ImageTaskEndpointGenerations = "generations"
	ImageTaskEndpointEdits       = "edits"
)

// ImageTask 是一条异步图片生成任务。
//
// 为什么必须独立成表：图片生成动辄 20–60 秒。同步路径下这段等待全部压在那一次
// HTTP 请求上——客户端切后台、网络抖动、进程重启，任何一处断掉用户都不知道图
// 到底画没画成。异步版把"这次生成"变成可持久化、可续查的一行状态：提交立刻
// 返回任务号，后台执行器把活干完写到这一行上，客户端过几秒来问一次。
//
// 【铁律】本表**绝不存渠道密钥**，只存 ChannelKeyID 引用（理由同 VideoTask：
// key 会轮换、会禁用，只有存 id 才能在执行时重新取到当前有效的那把）。
//
// 【敏感数据】RequestBody 含用户的 prompt。它不许出现在任何日志/错误串里
// （包括 log.Debugf），json tag 是 "-" 也不会让它出现在任何对外 JSON 里。
type ImageTask struct {
	ID     int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	TaskID string `json:"task_id" gorm:"size:64;uniqueIndex;default:''"`

	CreatedAt   int64 `json:"created_at" gorm:"index"`
	CompletedAt int64 `json:"completed_at;default:0"`
	// ExpiresAt 过了之后任务一律不可见（查询 404），后台执行器也不再执行它，
	// 由过期清扫统一标 failed 并退款。
	ExpiresAt int64 `json:"expires_at" gorm:"index"`

	// Status 见 ImageTaskStatus* 常量，只有 processing/completed/failed 三值。
	Status string `json:"status" gorm:"size:16;index;default:'processing'"`

	// OwnerTokenID / OwnerUserID 归属，用于「只能看自己的任务」。
	// 查询侧必须按这两个字段过滤，不能只按 TaskID —— 否则知道 id 就能看别人的图。
	OwnerTokenID int `json:"owner_token_id" gorm:"index;default:0"`
	OwnerUserID  int `json:"owner_user_id" gorm:"index;default:0"`

	// 渠道引用（不是密钥），重启后靠它恢复「该去哪个上游问」。
	ChannelID    int `json:"channel_id" gorm:"index;default:0"`
	ChannelKeyID int `json:"channel_key_id" gorm:"default:0"`

	Model string `json:"model" gorm:"size:255;default:''"`

	// Endpoint 记录是 generations 还是 edits —— 两者上游路径不同，重放必须回到同一个。
	// 空值 = generations（存量/默认），保持向后兼容。
	Endpoint string `json:"endpoint" gorm:"size:32;default:''"`

	// 请求体原文，供后台执行器重放。**含用户 prompt**，属敏感数据：
	// 不许出现在进程日志（stdout）与错误串里。历史日志（relay_logs）按与同步图片
	// 完全相同的规则记录它（请求内容脱敏密钥、出站请求体受开关控制原样记录）。
	RequestBody string `json:"-" gorm:"type:text"`

	// 以下三列只为执行器落历史日志：任务在后台跑完时已经没有原始 HTTP 请求可读，
	// 不在提交时记下来，日志行就缺"客户端请求的模型名 / 来源 IP / UA"，
	// 与同步图片日志对不齐。只进 relay_logs，对外接口不返回（json:"-"）。
	RequestModel     string `json:"-" gorm:"size:255;default:''"`
	RequestIP        string `json:"-" gorm:"size:128;default:''"`
	RequestUserAgent string `json:"-" gorm:"size:512;default:''"`

	// 上游原始返回，完成后填。诊断用，别在前端直接展示（可能含上游内网信息）。
	// 写入侧会剔除 b64_json 图像数据（见 relay.imageTaskResultPayload），
	// 否则几 MB 的 base64 会把这一列和日志页一起撑爆。
	ResultJSON string `json:"result_json" gorm:"type:text"`

	// 失败原因（已脱敏）。前端展示这一列，不展示 ResultJSON。
	ErrorMessage string `json:"error_message" gorm:"type:text"`

	// 计费：处理中 = 提交时预扣的钱（按张计费的模型）；完成后 = 这一单最终收的钱
	// （按 token 计费的模型在完成时按上游用量结算，见 relay.ImageTaskSettle）。
	// 金额单位与项目其余部分一致。
	//
	// 它同时是退款幂等的凭据：退款路径用 `UPDATE ... SET cost = 0 WHERE id = ? AND cost > 0`
	// 抢这一笔，抢到的那一次才真退（见 op.ImageTaskClaimRefund）。
	Cost float64 `json:"cost" gorm:"default:0"`

	// 以下三列是提交时冻结的计费口径（与 new-api 的 Task.PrivateData.BillingContext 同义）：
	// 任务可能排队、跑很久，期间管理员改价不该改变一条已经受理的任务的价格。
	//   - BillingMode：per_image（按张，提交即预扣，完成不再结算）/ token（完成时按
	//     上游用量结算）/ 空（改动前提交的存量任务：只认预扣，不结算）。
	//   - ImageCount：请求的张数 n。
	//   - BillingSnapshot：访问计划倍率 + token 单价（JSON，model.AccessPlanBillingSnapshot）。
	BillingMode     string `json:"-" gorm:"size:16;default:''"`
	ImageCount      int    `json:"-" gorm:"default:0"`
	BillingSnapshot string `json:"-" gorm:"type:text"`
}

// TableName 固定表名，避免 GORM 复数化规则将来变动导致迁移与运行时错位。
func (ImageTask) TableName() string { return "image_tasks" }

// IsImageTaskStatusAllowed 报告 s 是否是对外契约里的三个状态之一。
func IsImageTaskStatusAllowed(s string) bool {
	switch s {
	case ImageTaskStatusProcessing, ImageTaskStatusCompleted, ImageTaskStatusFailed:
		return true
	default:
		return false
	}
}

// IsImageTaskTerminal 报告该状态是否已经是终态。
//
// 终态 = 不会再有执行器去动它 = 必须已经释放过在途预留（成功/失败/过期三条路径）。
func IsImageTaskTerminal(s string) bool {
	return s == ImageTaskStatusCompleted || s == ImageTaskStatusFailed
}

// NormalizeImageTaskStatus 把任意输入归一成合法状态：合法值原样返回，其余一律
// 退回 processing。
//
// 只在**写入侧兜底**用（op.ImageTaskCreate）：空串/脏值落库会让这一行既不是
// "在跑"也不是终态，永远没人推进它。真正的状态推进走 op.ImageTaskStatusCAS，
// 那里对非法目标值直接报错（拒绝，不归一）——因为把一次状态推进悄悄改写成
// processing 会让执行器无限重跑同一条任务。
func NormalizeImageTaskStatus(s string) string {
	if IsImageTaskStatusAllowed(s) {
		return s
	}
	return ImageTaskStatusProcessing
}
