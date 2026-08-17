package op

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ResponseSessionTranscript struct {
	Messages []transformerModel.Message `json:"messages"`
	Tools    []transformerModel.Tool    `json:"tools,omitempty"`
}

func ResponseSessionIDHash(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(responseID))
	return hex.EncodeToString(sum[:])
}

// ResponseSessionBind is the backward-compatible entry point (no owner identity /
// conversation root). Prefer ResponseSessionBindOwned so the tenant that created
// the response id is recorded for cross-tenant isolation.
func ResponseSessionBind(ctx context.Context, responseID string, channelID, channelKeyID int, ttl time.Duration) error {
	return ResponseSessionBindOwned(ctx, responseID, channelID, channelKeyID, 0, 0, "", "", ttl)
}

func ResponseSessionBindOwned(ctx context.Context, responseID string, channelID, channelKeyID, ownerTokenID, ownerUserID int, rootHash, source string, ttl time.Duration) error {
	responseIDHash := ResponseSessionIDHash(responseID)
	if responseIDHash == "" || channelID == 0 || channelKeyID == 0 {
		return nil
	}
	if ttl <= 0 {
		return nil
	}
	conn := db.GetDB()
	if conn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	source = strings.TrimSpace(source)
	row := model.ResponseSession{
		ResponseIDHash: responseIDHash,
		ChannelID:      channelID,
		ChannelKeyID:   channelKeyID,
		OwnerTokenID:   ownerTokenID,
		OwnerUserID:    ownerUserID,
		RootHash:       rootHash,
		Source:         source,
		ExpiresAt:      now.Add(ttl),
	}
	return conn.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "response_id_hash"}},
		DoUpdates: clause.Assignments(map[string]any{
			"channel_id":     row.ChannelID,
			"channel_key_id": row.ChannelKeyID,
			"owner_token_id": row.OwnerTokenID,
			"owner_user_id":  row.OwnerUserID,
			"root_hash":      row.RootHash,
			"source":         row.Source,
			"transcript_json": gorm.Expr(
				"CASE WHEN owner_token_id = ? AND owner_user_id = ? THEN transcript_json ELSE '' END",
				row.OwnerTokenID,
				row.OwnerUserID,
			),
			"tools_json": gorm.Expr(
				"CASE WHEN owner_token_id = ? AND owner_user_id = ? THEN tools_json ELSE '' END",
				row.OwnerTokenID,
				row.OwnerUserID,
			),
			"expires_at": row.ExpiresAt,
			"updated_at": now,
		}),
	}).Create(&row).Error
}

func ResponseSessionOwner(ctx context.Context, responseID string) (model.ResponseSession, bool, error) {
	responseIDHash := ResponseSessionIDHash(responseID)
	if responseIDHash == "" {
		return model.ResponseSession{}, false, nil
	}
	conn := db.GetDB()
	if conn == nil {
		return model.ResponseSession{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	var row model.ResponseSession
	err := conn.WithContext(ctx).
		Where("response_id_hash = ? AND expires_at > ?", responseIDHash, now).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ResponseSession{}, false, nil
	}
	if err != nil {
		return model.ResponseSession{}, false, err
	}
	if row.ChannelID == 0 || row.ChannelKeyID == 0 {
		return model.ResponseSession{}, false, nil
	}
	return row, true, nil
}

func ResponseSessionBindTranscript(
	ctx context.Context,
	responseID string,
	messages []transformerModel.Message,
	tools []transformerModel.Tool,
	ownerTokenID, ownerUserID int,
	ttl time.Duration,
) error {
	responseIDHash := ResponseSessionIDHash(responseID)
	if responseIDHash == "" || len(messages) == 0 || ttl <= 0 {
		return nil
	}
	conn := db.GetDB()
	if conn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	transcriptBytes, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	toolsBytes, err := json.Marshal(tools)
	if err != nil {
		return err
	}
	return conn.WithContext(ctx).
		Model(&model.ResponseSession{}).
		Where("response_id_hash = ? AND owner_token_id = ? AND owner_user_id = ?", responseIDHash, ownerTokenID, ownerUserID).
		Updates(map[string]any{
			"transcript_json": string(transcriptBytes),
			"tools_json":      string(toolsBytes),
			"expires_at":      time.Now().Add(ttl),
		}).Error
}

func ResponseSessionTranscriptOwned(
	ctx context.Context,
	responseID string,
	ownerTokenID, ownerUserID int,
) (ResponseSessionTranscript, bool, error) {
	row, ok, err := ResponseSessionOwner(ctx, responseID)
	if err != nil || !ok {
		return ResponseSessionTranscript{}, false, err
	}
	ownerMatches := row.OwnerTokenID == 0 && row.OwnerUserID == 0
	if !ownerMatches {
		if row.OwnerTokenID > 0 {
			ownerMatches = row.OwnerTokenID == ownerTokenID
		} else {
			ownerMatches = row.OwnerUserID == ownerUserID
		}
	}
	if !ownerMatches || strings.TrimSpace(row.TranscriptJSON) == "" {
		return ResponseSessionTranscript{}, false, nil
	}
	var transcript ResponseSessionTranscript
	if err := json.Unmarshal([]byte(row.TranscriptJSON), &transcript.Messages); err != nil {
		return ResponseSessionTranscript{}, false, err
	}
	if strings.TrimSpace(row.ToolsJSON) != "" {
		if err := json.Unmarshal([]byte(row.ToolsJSON), &transcript.Tools); err != nil {
			return ResponseSessionTranscript{}, false, err
		}
	}
	return transcript, len(transcript.Messages) > 0, nil
}

// ResponseSessionPruneExpired deletes response-session bindings past their TTL. Driven by a
// periodic task (see task.Init) instead of inline on the bind path, where the DELETE stalled
// whichever request happened to cross the throttle window and — worse — its error was
// swallowed, so a permanently failing prune (locked DB) grew the table invisibly.
// ResponseSessionOwner already filters expired rows on read, so read correctness never
// depended on this running promptly.
func ResponseSessionPruneExpired(ctx context.Context) error {
	conn := db.GetDB()
	if conn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return conn.WithContext(ctx).Where("expires_at <= ?", time.Now()).Delete(&model.ResponseSession{}).Error
}
