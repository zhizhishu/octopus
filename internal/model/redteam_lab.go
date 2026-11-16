package model

import "time"

// RedTeamLabRun records one explicitly authorized administrator test.
// Only a bounded, visible response excerpt is retained. It is not a
// model-authenticity claim and is never inserted into passive audit evidence.
type RedTeamLabRun struct {
	ID              int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt       time.Time `json:"created_at" gorm:"index;not null"`
	ChannelID       int       `json:"channel_id" gorm:"index;not null"`
	ChannelName     string    `json:"channel_name" gorm:"size:128"`
	RequestModel    string    `json:"request_model" gorm:"size:200"`
	CaseID          string    `json:"case_id" gorm:"size:80"`
	CaseName        string    `json:"case_name" gorm:"size:120"`
	PromptText      string    `json:"prompt_text" gorm:"type:text"`
	ExpectedText    string    `json:"expected_text" gorm:"size:160"`
	ResponseText    string    `json:"response_text" gorm:"type:text"`
	ResponseClipped bool      `json:"response_clipped"`
	Status          string    `json:"status" gorm:"size:24;index;not null"`
	Verdict         string    `json:"verdict" gorm:"size:24"` // matched, not_matched, unscored
	ErrorMessage    string    `json:"error_message" gorm:"size:500"`
	DurationMs      int64     `json:"duration_ms"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	OperatorID      int       `json:"operator_id" gorm:"index;not null"`
}
