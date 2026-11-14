package model

// UserQuotaPolicy is independent of daily card fields and access routing plans.
// Zero limits mean no limit. Disabling a policy never deletes its usage.
type UserQuotaPolicy struct {
	UserID           int     `json:"user_id" gorm:"primaryKey"`
	Enabled          bool    `json:"enabled"`
	Timezone         string  `json:"timezone" gorm:"size:128;not null"`
	AnchorAt         int64   `json:"anchor_at"`
	FiveHourCost     float64 `json:"five_hour_cost"`
	WeeklyCost       float64 `json:"weekly_cost"`
	MonthlyCost      float64 `json:"monthly_cost"`
	FiveHourRequests int64   `json:"five_hour_requests"`
	WeeklyRequests   int64   `json:"weekly_requests"`
	MonthlyRequests  int64   `json:"monthly_requests"`
	AllowedModels    string  `json:"allowed_models" gorm:"type:text"`
	Revision         int64   `json:"revision"`
	Serial           int64   `json:"-"`
	CreatedAt        int64   `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt        int64   `json:"updated_at" gorm:"autoUpdateTime"`
}

// UserQuotaWindow stores counters, not relay content. The policy's stable
// user ID and original start time survive policy edits and late refunds.
type UserQuotaWindow struct {
	UserID         int     `json:"-" gorm:"primaryKey;autoIncrement:false"`
	Period         string  `json:"period" gorm:"primaryKey;size:16"`
	StartAt        int64   `json:"start_at" gorm:"primaryKey;autoIncrement:false"`
	EndAt          int64   `json:"end_at"`
	CostUsed       float64 `json:"cost_used"`
	CostReserved   float64 `json:"cost_reserved"`
	Accepted       int64   `json:"accepted"`
	FailedAccepted int64   `json:"failed_accepted"`
}

type UserQuotaReceipt struct {
	SourceKey      string  `json:"-" gorm:"primaryKey;size:128"`
	UserID         int     `json:"-" gorm:"index;not null"`
	SourceKind     string  `json:"-" gorm:"size:24;index"`
	RequestModel   string  `json:"-" gorm:"size:255"`
	PolicyRevision int64   `json:"-"`
	AcceptedAt     int64   `json:"-" gorm:"index"`
	Units          int64   `json:"-"`
	FiveHourStart  int64   `json:"-"`
	WeekStart      int64   `json:"-"`
	MonthStart     int64   `json:"-"`
	Cost           float64 `json:"-"`
	Reserved       float64 `json:"-"`
	FailedUnits    int64   `json:"-"`
	Outcome        string  `json:"-" gorm:"size:24;index"`
	SettledAt      int64   `json:"-" gorm:"index"`
}

type UserQuotaWindowStatus struct {
	UserQuotaWindow
	CostLimit    float64 `json:"cost_limit"`
	RequestLimit int64   `json:"request_limit"`
}

type UserQuotaStatus struct {
	Policy            *UserQuotaPolicy        `json:"policy"`
	Windows           []UserQuotaWindowStatus `json:"windows"`
	PendingReceipts   int64                   `json:"pending_receipts"`
	RealtimeSupported bool                    `json:"realtime_supported"`
	CountSemantics    string                  `json:"count_semantics"`
	CostSemantics     string                  `json:"cost_semantics"`
}
