package op

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/telemetry"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 用户页「客户端指纹」：每个通过 API Key 鉴权的入站请求，按 (用户, Key, 指纹) 聚合进
// user_client_fingerprints（见 model.UserClientFingerprint）。与 user_access_seen 同一个做法：
// 热路径只动内存，周期任务批量落库；只读入站请求，不碰出站。

const (
	userClientPendingMax     = 5000
	userClientDetailMaxRows  = 200
	userClientHeaderNamesMax = 4096
	userClientNotableMax     = 4096
)

var (
	userClientPendingMu      sync.Mutex
	userClientPending        = make(map[string]*model.UserClientFingerprint)
	userClientPendingDropped int64
	userClientLastPrune      time.Time
)

// UserClientRecord 记一次入站请求的客户端指纹。只动内存。
func UserClientRecord(userID, apiKeyID int, ip string, snap telemetry.Snapshot, at int64) {
	if userID <= 0 || snap.Hash == "" {
		return
	}
	if at <= 0 {
		at = time.Now().Unix()
	}
	ip = normalizeRequestIP(ip)
	key := model.UserClientSeenKey(userID, apiKeyID, snap.Hash)
	notable := userClientNotableJSON(snap.Notable)

	userClientPendingMu.Lock()
	defer userClientPendingMu.Unlock()
	if row, ok := userClientPending[key]; ok {
		row.Requests++
		if at >= row.LastSeen {
			row.LastSeen = at
			row.LastIP = ip
			row.LastPath = snap.Path
			row.Notable = notable
			if snap.Country != "" {
				row.Country = snap.Country
			}
		}
		if at < row.FirstSeen {
			row.FirstSeen = at
		}
		return
	}
	if len(userClientPending) >= userClientPendingMax {
		userClientPendingDropped++
		return
	}
	userClientPending[key] = &model.UserClientFingerprint{
		SeenKey: key, UserID: userID, APIKeyID: apiKeyID, FPHash: snap.Hash,
		ClientName: cutRunes(snap.ClientName, 64), ClientVersion: cutRunes(snap.ClientVersion, 64),
		UserAgent:   cutRunes(snap.UserAgent, 512),
		HeaderNames: cutBytes(strings.Join(snap.HeaderNames, ","), userClientHeaderNamesMax),
		HeaderOrder: snap.OrderKnown, Notable: notable,
		HTTPVersion: cutBytes(snap.HTTPVersion, 16), JA3: cutBytes(snap.JA3, 64), JA4: cutBytes(snap.JA4, 96), JA4H: cutBytes(snap.JA4H, 96),
		ViaProxy: snap.ViaProxy, Country: cutBytes(snap.Country, 8), LastIP: ip, LastPath: cutBytes(snap.Path, 128),
		FirstSeen: at, LastSeen: at, Requests: 1,
	}
}

func userClientNotableJSON(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	size := 2
	for _, k := range keys {
		size += len(k) + len(m[k]) + 6
		if size > userClientNotableMax {
			break
		}
		out[k] = m[k]
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, ','); i > 0 && n == userClientHeaderNamesMax {
		s = s[:i]
	}
	return strings.ToValidUTF8(s, "")
}

// UserClientSaveDB 把内存里攒的指纹合进表，并每小时清一次：90 天没再出现的、每个 (用户, Key) 超过上限的。
func UserClientSaveDB(ctx context.Context) error {
	userClientPendingMu.Lock()
	pending := userClientPending
	userClientPending = make(map[string]*model.UserClientFingerprint, len(pending))
	dropped := userClientPendingDropped
	userClientPendingDropped = 0
	userClientPendingMu.Unlock()
	if dropped > 0 {
		log.Warnf("user client fingerprint: dropped %d new fingerprints (pending cap %d)", dropped, userClientPendingMax)
	}
	var firstErr error
	for _, row := range pending {
		if err := userClientUpsert(ctx, *row); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to save user client fingerprint: %w", err)
		}
	}
	if time.Since(userClientLastPrune) >= userAccessPruneEvery {
		userClientLastPrune = time.Now()
		if err := userClientPrune(ctx, time.Now()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// userClientUpsert：先 UPDATE 再 INSERT，撞唯一键再 UPDATE 一次（同 userAccessUpsert）。
// 「最近」那几列放在 last_seen 之前赋值：MySQL 的单表 UPDATE 从左到右用已更新的值，
// 先赋 last_ip 等列才能拿旧的 last_seen 比较；PostgreSQL / SQLite 本来就用旧值。
func userClientUpsert(ctx context.Context, row model.UserClientFingerprint) error {
	update := func() (int64, error) {
		res := db.GetDB().WithContext(ctx).Exec(
			"UPDATE user_client_fingerprints SET "+
				"last_ip = CASE WHEN last_seen <= ? THEN ? ELSE last_ip END, "+
				"last_path = CASE WHEN last_seen <= ? THEN ? ELSE last_path END, "+
				"notable = CASE WHEN last_seen <= ? THEN ? ELSE notable END, "+
				"country = CASE WHEN last_seen <= ? AND ? <> '' THEN ? ELSE country END, "+
				"requests = requests + ?, "+
				"first_seen = CASE WHEN first_seen > ? THEN ? ELSE first_seen END, "+
				"last_seen = CASE WHEN last_seen < ? THEN ? ELSE last_seen END "+
				"WHERE seen_key = ?",
			row.LastSeen, row.LastIP,
			row.LastSeen, row.LastPath,
			row.LastSeen, row.Notable,
			row.LastSeen, row.Country, row.Country,
			row.Requests,
			row.FirstSeen, row.FirstSeen,
			row.LastSeen, row.LastSeen,
			row.SeenKey,
		)
		return res.RowsAffected, res.Error
	}
	n, err := update()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	row.ID = 0
	if err := db.GetDB().WithContext(ctx).Create(&row).Error; err != nil {
		if n2, err2 := update(); err2 == nil && n2 > 0 {
			return nil
		}
		return err
	}
	return nil
}

func userClientPrune(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-UserAccessKeepDays * 24 * time.Hour).Unix()
	if err := db.GetDB().WithContext(ctx).Where("last_seen < ?", cutoff).Delete(&model.UserClientFingerprint{}).Error; err != nil {
		return fmt.Errorf("failed to prune user client fingerprints: %w", err)
	}
	type over struct {
		UserID   int
		APIKeyID int
		N        int64
	}
	var overs []over
	if err := db.GetDB().WithContext(ctx).Model(&model.UserClientFingerprint{}).
		Select("user_id, api_key_id, COUNT(*) AS n").Group("user_id, api_key_id").
		Having("COUNT(*) > ?", model.UserClientPerKeyMax).Scan(&overs).Error; err != nil {
		return fmt.Errorf("failed to count user client fingerprints: %w", err)
	}
	for _, o := range overs {
		var keep []int64
		if err := db.GetDB().WithContext(ctx).Model(&model.UserClientFingerprint{}).
			Where("user_id = ? AND api_key_id = ?", o.UserID, o.APIKeyID).
			Order("last_seen DESC, id DESC").Limit(model.UserClientPerKeyMax).Pluck("id", &keep).Error; err != nil {
			return fmt.Errorf("failed to list user client fingerprints: %w", err)
		}
		if len(keep) == 0 {
			continue
		}
		if err := db.GetDB().WithContext(ctx).
			Where("user_id = ? AND api_key_id = ? AND id NOT IN ?", o.UserID, o.APIKeyID, keep).
			Delete(&model.UserClientFingerprint{}).Error; err != nil {
			return fmt.Errorf("failed to trim user client fingerprints: %w", err)
		}
	}
	return nil
}

// UserClientDetailGet 给用户抽屉「客户端指纹」页。userID > 0 看这个人（apiKeyID > 0 时再按 Key 筛）；
// userID = 0 且 apiKeyID > 0 时只按 Key 看（给管理员自己名下的访客 Key 之类用）。
func UserClientDetailGet(ctx context.Context, userID, apiKeyID int) (model.UserClientDetail, error) {
	if userID <= 0 && apiKeyID <= 0 {
		return model.UserClientDetail{}, fmt.Errorf("invalid user id")
	}
	if userID > 0 {
		if _, err := UserGet(userID); err != nil {
			return model.UserClientDetail{}, err
		}
	}
	q := db.GetDB().WithContext(ctx).Model(&model.UserClientFingerprint{})
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	} else {
		q = q.Where("api_key_id = ?", apiKeyID)
	}
	var rows []model.UserClientFingerprint
	if err := q.Order("last_seen DESC").Limit(userClientDetailMaxRows).Find(&rows).Error; err != nil {
		return model.UserClientDetail{}, err
	}
	rows = userClientMergePending(rows, func(r model.UserClientFingerprint) bool {
		if userID > 0 {
			return r.UserID == userID
		}
		return r.APIKeyID == apiKeyID
	})

	out := model.UserClientDetail{
		UserID: userID, APIKeyID: apiKeyID, KeepDays: UserAccessKeepDays,
		PerKeyMax: model.UserClientPerKeyMax, TLSTerminated: false,
	}
	keys := map[int]*model.UserClientKeyInfo{}
	for _, r := range rows {
		k := keys[r.APIKeyID]
		if k == nil {
			k = &model.UserClientKeyInfo{APIKeyID: r.APIKeyID, APIKeyName: userAccessKeyName(r.APIKeyID)}
			keys[r.APIKeyID] = k
		}
		k.Fingerprints++
		k.Requests += r.Requests
		if r.LastSeen > k.LastSeen {
			k.LastSeen = r.LastSeen
		}
	}
	out.Keys = make([]model.UserClientKeyInfo, 0, len(keys))
	for _, k := range keys {
		out.Keys = append(out.Keys, *k)
	}
	sort.Slice(out.Keys, func(i, j int) bool {
		if out.Keys[i].LastSeen != out.Keys[j].LastSeen {
			return out.Keys[i].LastSeen > out.Keys[j].LastSeen
		}
		return out.Keys[i].APIKeyID < out.Keys[j].APIKeyID
	})
	out.Fingerprints = make([]model.UserClientFingerprint, 0, len(rows))
	for _, r := range rows {
		if apiKeyID > 0 && r.APIKeyID != apiKeyID {
			continue
		}
		out.Fingerprints = append(out.Fingerprints, r)
	}
	sort.Slice(out.Fingerprints, func(i, j int) bool {
		a, b := out.Fingerprints[i], out.Fingerprints[j]
		if a.LastSeen != b.LastSeen {
			return a.LastSeen > b.LastSeen
		}
		return a.SeenKey < b.SeenKey
	})
	return out, nil
}

func userClientMergePending(rows []model.UserClientFingerprint, keep func(model.UserClientFingerprint) bool) []model.UserClientFingerprint {
	idx := make(map[string]int, len(rows))
	for i, r := range rows {
		idx[r.SeenKey] = i
	}
	userClientPendingMu.Lock()
	defer userClientPendingMu.Unlock()
	for key, p := range userClientPending {
		if !keep(*p) {
			continue
		}
		if i, ok := idx[key]; ok {
			r := &rows[i]
			r.Requests += p.Requests
			if p.LastSeen >= r.LastSeen {
				r.LastSeen, r.LastIP, r.LastPath, r.Notable = p.LastSeen, p.LastIP, p.LastPath, p.Notable
				if p.Country != "" {
					r.Country = p.Country
				}
			}
			if p.FirstSeen < r.FirstSeen {
				r.FirstSeen = p.FirstSeen
			}
			continue
		}
		idx[key] = len(rows)
		rows = append(rows, *p)
	}
	return rows
}

// userClientForgetPending 删用户时一并清掉还没落库的指纹。
func userClientForgetPending(userID int) {
	userClientPendingMu.Lock()
	defer userClientPendingMu.Unlock()
	for k, p := range userClientPending {
		if p.UserID == userID {
			delete(userClientPending, k)
		}
	}
}
