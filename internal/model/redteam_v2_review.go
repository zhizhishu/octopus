package model

import "time"

// RedTeamV2Review is an append-only administrator judgment, explicitly
// separate from the deterministic marker and from model-authenticity audits.
type RedTeamV2Review struct {
	ID         int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	RunID      int64     `json:"run_id" gorm:"index;not null"`
	ReviewerID int       `json:"reviewer_id" gorm:"not null"`
	Decision   string    `json:"decision" gorm:"size:24;not null"` // confirmed, rejected, uncertain
	Reason     string    `json:"reason" gorm:"size:800"`
	CreatedAt  time.Time `json:"created_at" gorm:"index;not null"`
}
