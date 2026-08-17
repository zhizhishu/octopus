package model

import "time"

// ResponseSession stores the owner of an OpenAI Responses response id.
// The raw response id is never persisted; ResponseIDHash is sha256(response_id).
//
// OwnerTokenID / OwnerUserID pin the tenant that created the response id so a
// cross-tenant request that presents someone else's previous_response_id can be
// rejected (fail-closed) instead of being routed to the owner's channel and
// replaying the owner's history. Legacy rows written before this column existed
// carry 0/0 and are treated as unrestricted for backward compatibility; they
// expire within the session TTL, after which every live row is identity-bound.
//
// RootHash is sha256 of the conversation root response id (stable across every
// turn of the same responses conversation) so an upstream prompt cache can be
// keyed to the conversation rather than to a per-turn message hash.
//
// Source marks how the session id was minted:
//   - "responses" (or empty/legacy): a real OpenAI Responses id that may be
//     forwarded as previous_response_id to a stateful responses upstream
//   - "chat": a local id from /v1/chat/completions — never a real Responses
//     store id; the next turn must rebuild from TranscriptJSON instead
type ResponseSession struct {
	ResponseIDHash string    `json:"response_id_hash" gorm:"primaryKey;size:64"`
	ChannelID      int       `json:"channel_id" gorm:"index;not null"`
	ChannelKeyID   int       `json:"channel_key_id" gorm:"index;not null"`
	OwnerTokenID   int       `json:"owner_token_id" gorm:"index;not null;default:0"`
	OwnerUserID    int       `json:"owner_user_id" gorm:"index;not null;default:0"`
	RootHash       string    `json:"root_hash" gorm:"size:64;not null;default:''"`
	Source         string    `json:"source" gorm:"size:16;not null;default:''"`
	TranscriptJSON string    `json:"-" gorm:"type:text"`
	ToolsJSON      string    `json:"-" gorm:"type:text"`
	ExpiresAt      time.Time `json:"expires_at" gorm:"index;not null"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
