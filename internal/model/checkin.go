package model

type CheckInRewardMode string

const (
	CheckInRewardModeFixed  CheckInRewardMode = "fixed"
	CheckInRewardModeRandom CheckInRewardMode = "random"
)

type UserCheckIn struct {
	ID        int     `json:"id" gorm:"primaryKey"`
	UserID    int     `json:"user_id" gorm:"uniqueIndex:idx_user_checkin_day;not null"`
	Day       string  `json:"day" gorm:"size:10;uniqueIndex:idx_user_checkin_day;not null"`
	Amount    float64 `json:"amount" gorm:"type:real;default:0"`
	CreatedAt int64   `json:"created_at" gorm:"autoCreateTime"`
}

type UserCheckInStatus struct {
	Enabled       bool              `json:"enabled"`
	CheckedToday  bool              `json:"checked_today"`
	Today         string            `json:"today"`
	RewardMode    CheckInRewardMode `json:"reward_mode"`
	RewardAmount  float64           `json:"reward_amount"`
	RewardMin     float64           `json:"reward_min"`
	RewardMax     float64           `json:"reward_max"`
	LastAmount    float64           `json:"last_amount"`
	Balance       float64           `json:"balance"`
	CheckedAt     int64             `json:"checked_at"`
	NextCheckInAt int64             `json:"next_check_in_at"`
}

type UserCheckInResponse struct {
	User   UserResponse      `json:"user"`
	Status UserCheckInStatus `json:"status"`
	Reward float64           `json:"reward"`
}
