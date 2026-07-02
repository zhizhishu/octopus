package op

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const responseSessionPruneInterval = time.Minute

var responseSessionLastPruneUnix atomic.Int64

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
	return ResponseSessionBindOwned(ctx, responseID, channelID, channelKeyID, 0, 0, "", ttl)
}

func ResponseSessionBindOwned(ctx context.Context, responseID string, channelID, channelKeyID, ownerTokenID, ownerUserID int, rootHash string, ttl time.Duration) error {
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
	maybePruneResponseSessions(ctx, conn, now)
	row := model.ResponseSession{
		ResponseIDHash: responseIDHash,
		ChannelID:      channelID,
		ChannelKeyID:   channelKeyID,
		OwnerTokenID:   ownerTokenID,
		OwnerUserID:    ownerUserID,
		RootHash:       rootHash,
		ExpiresAt:      now.Add(ttl),
	}
	return conn.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "response_id_hash"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"channel_id",
			"channel_key_id",
			"owner_token_id",
			"owner_user_id",
			"root_hash",
			"expires_at",
			"updated_at",
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

func maybePruneResponseSessions(ctx context.Context, conn *gorm.DB, now time.Time) {
	last := responseSessionLastPruneUnix.Load()
	if last != 0 && now.Sub(time.Unix(last, 0)) < responseSessionPruneInterval {
		return
	}
	if !responseSessionLastPruneUnix.CompareAndSwap(last, now.Unix()) {
		return
	}
	_ = conn.WithContext(ctx).Where("expires_at <= ?", now).Delete(&model.ResponseSession{}).Error
}
