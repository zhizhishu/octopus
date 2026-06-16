package model

import "time"

// ResponseSession stores the owner of an OpenAI Responses response id.
// The raw response id is never persisted; ResponseIDHash is sha256(response_id).
type ResponseSession struct {
	ResponseIDHash string    `json:"response_id_hash" gorm:"primaryKey;size:64"`
	ChannelID      int       `json:"channel_id" gorm:"index;not null"`
	ChannelKeyID   int       `json:"channel_key_id" gorm:"index;not null"`
	ExpiresAt      time.Time `json:"expires_at" gorm:"index;not null"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
