package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// userAccessBackfillDays：从最近多少天的 relay_logs 回填 user_access_seen。
const userAccessBackfillDays = 30

// userAccessBackfillBudget：回填最多花多久。超时就算了（不报错，表会从实时请求慢慢长出来），
// 免得一张很大的日志表拖慢升级后的第一次启动。
const userAccessBackfillBudget = 20 * time.Second

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 25,
		Up:      migrateBackfillUserAccessSeen,
	})
}

type userAccessBackfillRow struct {
	UserID    int
	APIKeyID  int
	IP        string
	UserAgent string
	FirstSeen int64
	LastSeen  int64
	Requests  int64
}

// migrateBackfillUserAccessSeen 用已有日志里的 request_ip / request_user_agent 给新表
// user_access_seen 填一份初始数据，升级后用户页立刻就有「IP / 客户端」可看，
// 「这个 IP 以前见过没有」也有历史可比。只读 relay_logs，不改它。
// 幂等：撞到已有的 seen_key 直接跳过。
func migrateBackfillUserAccessSeen(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable(&model.RelayLog{}) || !db.Migrator().HasTable(&model.UserAccessSeen{}) {
		return nil
	}
	for _, col := range []string{"request_ip", "request_user_agent", "user_id", "api_key_id"} {
		if !db.Migrator().HasColumn(&model.RelayLog{}, col) {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), userAccessBackfillBudget)
	defer cancel()
	since := time.Now().AddDate(0, 0, -userAccessBackfillDays).Unix()

	var rows []userAccessBackfillRow
	err := db.WithContext(ctx).Model(&model.RelayLog{}).
		Select("user_id, api_key_id, request_ip AS ip, COALESCE(request_user_agent, '') AS user_agent, "+
			"MIN(time) AS first_seen, MAX(time) AS last_seen, COUNT(*) AS requests").
		Where("time >= ? AND user_id > 0 AND request_ip IS NOT NULL AND request_ip <> ''", since).
		Group("user_id, api_key_id, request_ip, COALESCE(request_user_agent, '')").
		Scan(&rows).Error
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			log.Warnf("migration 025: user access backfill skipped after %s (relay_logs too large); it fills from new requests", userAccessBackfillBudget)
			return nil
		}
		return fmt.Errorf("read relay_logs for user access backfill: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	merged := make(map[string]*model.UserAccessSeen, len(rows))
	for _, r := range rows {
		ip := strings.TrimSpace(r.IP)
		if len(ip) > 128 {
			ip = ip[:128]
		}
		ua := strings.TrimSpace(r.UserAgent)
		if rs := []rune(ua); len(rs) > 512 {
			ua = string(rs[:512])
		}
		if ip == "" {
			continue
		}
		key := model.UserAccessSeenKey(r.UserID, r.APIKeyID, ip, ua)
		if m, ok := merged[key]; ok {
			m.Requests += r.Requests
			if r.FirstSeen < m.FirstSeen {
				m.FirstSeen = r.FirstSeen
			}
			if r.LastSeen > m.LastSeen {
				m.LastSeen = r.LastSeen
			}
			continue
		}
		merged[key] = &model.UserAccessSeen{
			SeenKey: key, UserID: r.UserID, APIKeyID: r.APIKeyID, IP: ip, UserAgent: ua,
			FirstSeen: r.FirstSeen, LastSeen: r.LastSeen, Requests: r.Requests,
		}
	}
	batch := make([]model.UserAccessSeen, 0, 200)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "seen_key"}}, DoNothing: true}).
			CreateInBatches(batch, 100).Error
		batch = batch[:0]
		return err
	}
	for _, m := range merged {
		batch = append(batch, *m)
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return fmt.Errorf("write user access backfill: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return fmt.Errorf("write user access backfill: %w", err)
	}
	log.Infof("migration 025: backfilled %d user ip/client combos from the last %d days of logs", len(merged), userAccessBackfillDays)
	return nil
}
