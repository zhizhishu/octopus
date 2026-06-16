package model

type RedeemCodeType string

const (
	RedeemCodeTypeBalance RedeemCodeType = "balance"
	RedeemCodeTypeMonthly RedeemCodeType = "monthly"
)

type RedeemCode struct {
	ID              int            `json:"id" gorm:"primaryKey"`
	Code            string         `json:"code" gorm:"uniqueIndex;not null"`
	Type            RedeemCodeType `json:"type" gorm:"index;not null"`
	Enabled         bool           `json:"enabled" gorm:"default:true"`
	Used            bool           `json:"used" gorm:"default:false;index"`
	BalanceAmount   float64        `json:"balance_amount" gorm:"type:real;default:0"`
	MonthlyLimit    float64        `json:"monthly_limit" gorm:"type:real;default:0"`
	MonthlyDays     int            `json:"monthly_days" gorm:"default:0"`
	CreatedByUserID int            `json:"created_by_user_id" gorm:"index;default:0"`
	UsedByUserID    int            `json:"used_by_user_id" gorm:"index;default:0"`
	UsedAt          int64          `json:"used_at" gorm:"default:0"`
	Note            string         `json:"note,omitempty"`
	CreatedAt       int64          `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt       int64          `json:"updated_at" gorm:"autoUpdateTime"`
}

type RedeemCodeResponse struct {
	RedeemCode
	DailyQuota float64 `json:"daily_quota"`
	ValidDays  int     `json:"valid_days"`
}

type RedeemCodeGenerateRequest struct {
	Type          RedeemCodeType `json:"type"`
	Count         int            `json:"count"`
	BalanceAmount float64        `json:"balance_amount"`
	MonthlyLimit  float64        `json:"monthly_limit"`
	MonthlyDays   int            `json:"monthly_days"`
	DailyQuota    *float64       `json:"daily_quota,omitempty"`
	ValidDays     *int           `json:"valid_days,omitempty"`
	Enabled       bool           `json:"enabled"`
	Note          string         `json:"note"`
}

type RedeemCodeUpdateRequest struct {
	ID      int    `json:"id"`
	Enabled bool   `json:"enabled"`
	Note    string `json:"note"`
}

type RedeemCodeRedeemRequest struct {
	Code string `json:"code"`
}

func NewRedeemCodeResponse(code RedeemCode) RedeemCodeResponse {
	return RedeemCodeResponse{
		RedeemCode: code,
		DailyQuota: code.MonthlyLimit,
		ValidDays:  code.MonthlyDays,
	}
}

func NewRedeemCodeResponses(codes []RedeemCode) []RedeemCodeResponse {
	responses := make([]RedeemCodeResponse, 0, len(codes))
	for _, code := range codes {
		responses = append(responses, NewRedeemCodeResponse(code))
	}
	return responses
}
