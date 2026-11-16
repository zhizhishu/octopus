package model

import "time"

// RedTeamV2Case holds only the pointer to the latest immutable revision.
type RedTeamV2Case struct {
	ID              string    `json:"id" gorm:"primaryKey;size:48"`
	CurrentRevision int       `json:"current_revision" gorm:"not null"`
	CreatedAt       time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt       time.Time `json:"updated_at" gorm:"not null"`
}

// RedTeamV2Revision never changes after creation. A run pins an exact revision.
type RedTeamV2Revision struct {
	ID         int64     `json:"-" gorm:"primaryKey;autoIncrement"`
	CaseID     string    `json:"case_id" gorm:"size:48;uniqueIndex:idx_redteam_v2_revision"`
	Revision   int       `json:"revision" gorm:"uniqueIndex:idx_redteam_v2_revision"`
	SpecJSON   string    `json:"-" gorm:"type:text;not null"`
	SpecSHA256 string    `json:"spec_sha256" gorm:"size:64;not null"`
	CreatedAt  time.Time `json:"created_at" gorm:"not null"`
	CreatedBy  int       `json:"created_by" gorm:"index;not null"`
}

// RedTeamV2Run is separate from the legacy Lab and every model-audit table.
// TargetRequests is incremented before each call, so an interrupted run never
// pretends no charge was possible. Do not automatically retry an unknown send.
type RedTeamV2Run struct {
	ID              int64      `json:"id" gorm:"primaryKey;autoIncrement"`
	CaseID          string     `json:"case_id" gorm:"size:48;index;not null"`
	CaseRevision    int        `json:"case_revision" gorm:"not null"`
	SpecSHA256      string     `json:"spec_sha256" gorm:"size:64;not null"`
	OperatorID      int        `json:"operator_id" gorm:"index;not null"`
	IdempotencyKey  string     `json:"idempotency_key" gorm:"uniqueIndex;size:72;not null"`
	ChannelID       int        `json:"channel_id" gorm:"index;not null"`
	ChannelName     string     `json:"channel_name" gorm:"size:128"`
	RequestModel    string     `json:"request_model" gorm:"size:160"`
	Status          string     `json:"status" gorm:"size:32;index;not null"`
	Verdict         string     `json:"verdict" gorm:"size:24"`
	CancelRequested bool       `json:"cancel_requested" gorm:"not null;default:false"`
	TargetRequests  int        `json:"target_requests" gorm:"not null;default:0"`
	InputTokens     int        `json:"input_tokens"`
	OutputTokens    int        `json:"output_tokens"`
	ResponseText    string     `json:"response_text" gorm:"type:text"`
	ResponseClipped bool       `json:"response_clipped"`
	ErrorMessage    string     `json:"error_message" gorm:"size:500"`
	CreatedAt       time.Time  `json:"created_at" gorm:"index;not null"`
	StartedAt       *time.Time `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at"`
}

// RedTeamV2Turn saves bounded output from one and only one target call.
// This is evidence, not a model-authenticity report.
type RedTeamV2Turn struct {
	ID              int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	RunID           int64     `json:"run_id" gorm:"index;not null"`
	Sequence        int       `json:"sequence" gorm:"not null"`
	InputText       string    `json:"input_text" gorm:"type:text"`
	ResponseText    string    `json:"response_text" gorm:"type:text"`
	ResponseClipped bool      `json:"response_clipped"`
	Status          string    `json:"status" gorm:"size:32"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	CreatedAt       time.Time `json:"created_at" gorm:"not null"`
}

// RedTeamV2Lane is a DB-enforced global fail-closed single-flight. A crash
// intentionally leaves it locked; an admin must investigate a possibly billed
// unknown outcome before any new run can be authorized.
type RedTeamV2Lane struct {
	ID          int   `json:"-" gorm:"primaryKey"`
	ActiveRunID int64 `json:"active_run_id" gorm:"not null;default:0"`
}
