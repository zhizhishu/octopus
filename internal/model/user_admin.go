package model

// UserBalanceLog 是一条余额变动记录（管理员加 / 减 / 直接改成）。
//
// 只记管理员手动改余额的动作：中继扣费、签到、兑换码各有自己的账（日志 / 签到表 /
// 兑换码表），不在这里重复记，免得每次请求都多写一行。
// Delta 是库里真实发生的变化（BalanceAfter - BalanceBefore）。
type UserBalanceLog struct {
	ID            int64   `json:"id" gorm:"primaryKey"`
	UserID        int     `json:"user_id" gorm:"index;not null"`
	ActorID       int     `json:"actor_id" gorm:"default:0"`
	ActorName     string  `json:"actor_name" gorm:"size:128"`
	Mode          string  `json:"mode" gorm:"size:16"`
	Delta         float64 `json:"delta" gorm:"type:real;default:0"`
	BalanceBefore float64 `json:"balance_before" gorm:"type:real;default:0"`
	BalanceAfter  float64 `json:"balance_after" gorm:"type:real;default:0"`
	Reason        string  `json:"reason" gorm:"size:255"`
	CreatedAt     int64   `json:"created_at" gorm:"autoCreateTime;index"`
}

const (
	UserBalanceModeAdd      = "add"
	UserBalanceModeSubtract = "subtract"
	UserBalanceModeSet      = "set"
)

// UserBalanceAdjustRequest 是 POST /api/v1/user/balance 的请求体。
// add / subtract 用一条 `balance = balance ± ?` 的 UPDATE 完成，不会盖掉同时发生的扣费；
// set 才是「直接改成」。
type UserBalanceAdjustRequest struct {
	ID     int     `json:"id"`
	Mode   string  `json:"mode"`
	Amount float64 `json:"amount"`
	Reason string  `json:"reason"`
}

type UserBalanceAdjustResponse struct {
	User UserResponse   `json:"user"`
	Log  UserBalanceLog `json:"log"`
}

// UserPatchRequest 是部分更新：没发的字段（nil）保持原值。
// /api/v1/user/update 用它解析请求体，所以旧的「整条发」客户端行为不变，
// 新客户端只发改了的字段，不会把没发的列清空。
type UserPatchRequest struct {
	ID                  int         `json:"id"`
	Username            *string     `json:"username,omitempty"`
	Role                *UserRole   `json:"role,omitempty"`
	Status              *UserStatus `json:"status,omitempty"`
	Balance             *float64    `json:"balance,omitempty"`
	BalanceReason       *string     `json:"balance_reason,omitempty"`
	MonthlyLimit        *float64    `json:"monthly_limit,omitempty"`
	MonthlyUsed         *float64    `json:"monthly_used,omitempty"`
	MonthlyExpireAt     *int64      `json:"monthly_expire_at,omitempty"`
	MonthlyResetAt      *int64      `json:"monthly_reset_at,omitempty"`
	DailyLimit          *float64    `json:"daily_limit,omitempty"`
	DailyQuota          *float64    `json:"daily_quota,omitempty"`
	DailyUsed           *float64    `json:"daily_used,omitempty"`
	MonthlyExpireAtISO  *string     `json:"monthly_expire_at_iso,omitempty"`
	MonthlyResetAtISO   *string     `json:"monthly_reset_at_iso,omitempty"`
	NextResetAtISO      *string     `json:"next_reset_at_iso,omitempty"`
	Note                *string     `json:"note,omitempty"`
	AccessPlanIDs       *[]int      `json:"access_plan_ids,omitempty"`
	DefaultAccessPlanID *int        `json:"default_access_plan_id,omitempty"`
}

const (
	UserBatchEnable     = "enable"
	UserBatchDisable    = "disable"
	UserBatchAddBalance = "add_balance"
	UserBatchSetPlans   = "set_plans"
	UserBatchDelete     = "delete"
)

// UserBatchRequest 是 POST /api/v1/user/batch 的请求体。
type UserBatchRequest struct {
	IDs                 []int   `json:"ids"`
	Action              string  `json:"action"`
	Amount              float64 `json:"amount,omitempty"`
	Reason              string  `json:"reason,omitempty"`
	AccessPlanIDs       []int   `json:"access_plan_ids,omitempty"`
	DefaultAccessPlanID int     `json:"default_access_plan_id,omitempty"`
}

type UserBatchFailure struct {
	ID    int    `json:"id"`
	Error string `json:"error"`
}

// UserBatchResult 逐个报结果：一个人失败（比如最后一个管理员不能停用）不拖累其他人。
type UserBatchResult struct {
	Done   []int              `json:"done"`
	Failed []UserBatchFailure `json:"failed"`
}

// UserUsageDay 是某一天（服务器本地时区）的用量。
type UserUsageDay struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Cost     float64 `json:"cost"`
}

type UserUsageModel struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// UserUsageResponse 是 GET /api/v1/user/usage 的结果：一个用户在一段时间里的用量。
// 口径与 /stats/model-rank 相同（同一套日志、同一个排除规则、同一个花费算法）。
type UserUsageResponse struct {
	UserID         int              `json:"user_id"`
	Range          string           `json:"range"`
	Start          int64            `json:"start"`
	End            int64            `json:"end"`
	RequestSuccess int64            `json:"request_success"`
	RequestFailed  int64            `json:"request_failed"`
	InputTokens    int64            `json:"input_tokens"`
	OutputTokens   int64            `json:"output_tokens"`
	TotalCost      float64          `json:"total_cost"`
	Daily          []UserUsageDay   `json:"daily"`
	TopModels      []UserUsageModel `json:"top_models"`
}
