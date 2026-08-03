package op

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/snowflake"
	"gorm.io/gorm"
)

const relayLogMaxSize = 20
const relayLogMaxSizeNoDB = 100 // 当不保存到数据库时，允许更大的缓存用于实时查询
const bytesPerGB = 1024 * 1024 * 1024

// relayLogTwoPhaseMaxIDs bounds the id list of the two-phase severity-filtered read so the
// id IN(...) fetch stays well under SQLite's bound-parameter cap (999 on old builds, 32766 on
// 3.32+). Interactive log pages are small (page_size<=100 + in-memory cache); larger limits
// (export / deep pages) fall back to the single-query path.
const relayLogTwoPhaseMaxIDs = 900

var relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
var relayLogCacheLock sync.Mutex

var relayLogFlushLock sync.Mutex

var relayLogSubscribers = make(map[chan model.RelayLog]struct{})
var relayLogSubscribersLock sync.RWMutex

type relayLogStreamTokenScope struct {
	UserID       int
	APIKeyID     int
	Endpoint     string
	IsAdmin      bool
	StartTime    int
	EndTime      int
	HasTimeRange bool
}

var relayLogStreamTokens = make(map[string]relayLogStreamTokenScope)
var relayLogStreamTokensLock sync.RWMutex

func RelayLogStreamTokenCreate(scope model.RelayLogScope, isAdmin bool) (string, error) {
	return RelayLogStreamTokenCreateWithTimeRange(scope, isAdmin, nil, nil)
}

func RelayLogStreamTokenCreateWithTimeRange(scope model.RelayLogScope, isAdmin bool, startTime, endTime *int) (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes)
	tokenScope := relayLogStreamTokenScope{UserID: scope.UserID, APIKeyID: scope.APIKeyID, Endpoint: scope.Endpoint, IsAdmin: isAdmin}
	if startTime != nil && endTime != nil {
		tokenScope.StartTime = *startTime
		tokenScope.EndTime = *endTime
		tokenScope.HasTimeRange = true
	}

	relayLogStreamTokensLock.Lock()
	relayLogStreamTokens[token] = tokenScope
	relayLogStreamTokensLock.Unlock()

	return token, nil
}

func RelayLogStreamTokenVerify(token string) (relayLogStreamTokenScope, bool) {
	relayLogStreamTokensLock.RLock()
	scope, ok := relayLogStreamTokens[token]
	relayLogStreamTokensLock.RUnlock()
	return scope, ok
}

func RelayLogStreamTokenRevoke(token string) {
	relayLogStreamTokensLock.Lock()
	delete(relayLogStreamTokens, token)
	relayLogStreamTokensLock.Unlock()
}

func RelayLogSubscribe() chan model.RelayLog {
	ch := make(chan model.RelayLog, 10)
	relayLogSubscribersLock.Lock()
	relayLogSubscribers[ch] = struct{}{}
	relayLogSubscribersLock.Unlock()
	return ch
}

func RelayLogUnsubscribe(ch chan model.RelayLog) {
	relayLogSubscribersLock.Lock()
	delete(relayLogSubscribers, ch)
	relayLogSubscribersLock.Unlock()
	close(ch)
}

func notifySubscribers(relayLog model.RelayLog) {
	relayLogSubscribersLock.RLock()
	defer relayLogSubscribersLock.RUnlock()

	for ch := range relayLogSubscribers {
		select {
		case ch <- relayLog:
		default:
		}
	}
}

func relayLogFlushToDB(ctx context.Context) error {
	relayLogFlushLock.Lock()
	defer relayLogFlushLock.Unlock()

	relayLogCacheLock.Lock()
	if len(relayLogCache) == 0 {
		relayLogCacheLock.Unlock()
		return nil
	}
	batch := make([]model.RelayLog, len(relayLogCache))
	copy(batch, relayLogCache)
	flushedUpto := len(batch)
	relayLogCacheLock.Unlock()

	result := db.GetDB().WithContext(ctx).Create(&batch)
	if result.Error != nil {
		// flush 失败也把这批从缓存排空: 否则一个坏行(如超长字段撞 varchar 上限)会让之后
		// 每次 flush 都带着它重试、永久失败, 而 enabled 分支无淘汰 → 缓存无上限增长。
		// 丢掉这一批(含可能是暂时性 DB 抖动的正常行), 换取落库不被单条坏行永久卡死。
		relayLogCacheLock.Lock()
		if len(relayLogCache) >= flushedUpto {
			relayLogCache = relayLogCache[flushedUpto:]
		} else {
			relayLogCache = relayLogCache[:0]
		}
		relayLogCacheLock.Unlock()
		return result.Error
	}

	relayLogCacheLock.Lock()
	if len(relayLogCache) >= flushedUpto {
		relayLogCache = relayLogCache[flushedUpto:]
	} else {
		relayLogCache = relayLogCache[:0]
	}
	if len(relayLogCache) == 0 {
		relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
	}
	relayLogCacheLock.Unlock()

	return nil
}

func RelayLogAdd(ctx context.Context, relayLog model.RelayLog) error {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return err
	}
	maxSize := relayLogMaxSize
	if !enabled {
		maxSize = relayLogMaxSizeNoDB
	}
	relayLog.ID = snowflake.GenerateID()
	if relayLog.Time <= 0 {
		relayLog.Time = time.Now().Unix()
	}

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, relayLog)
	if enabled {
		relayLogCacheLock.Unlock()
		if err := relayLogFlushToDB(ctx); err != nil {
			return err
		}
		go notifySubscribers(relayLog)
		return nil
	}
	if len(relayLogCache) >= maxSize {
		// 如果未启用日志保存，移除最旧的日志，保留最新的日志用于实时查询
		// 重建底层数组而不是 reslice，避免数组持续引用旧日志的 Request/ResponseContent 导致内存无法回收
		keepSize := maxSize / 2
		if len(relayLogCache) > keepSize {
			newCache := make([]model.RelayLog, keepSize, maxSize)
			copy(newCache, relayLogCache[len(relayLogCache)-keepSize:])
			relayLogCache = newCache
		}
	}
	relayLogCacheLock.Unlock()
	go notifySubscribers(relayLog)
	return nil
}

func RelayLogSaveDBTask(ctx context.Context) error {
	log.Debugf("relay log save db task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("relay log save db task finished, save time: %s", time.Since(startTime))
	}()
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return err
	}

	if enabled {
		if err := relayLogFlushToDB(ctx); err != nil {
			return err
		}
		return relayLogCleanup(ctx)
	}

	// 如果未启用日志保存，检查缓存大小，如果超过限制则清理旧日志
	relayLogCacheLock.Lock()
	if len(relayLogCache) > relayLogMaxSizeNoDB {
		keepSize := relayLogMaxSizeNoDB / 2
		newCache := make([]model.RelayLog, keepSize, relayLogMaxSizeNoDB)
		copy(newCache, relayLogCache[len(relayLogCache)-keepSize:])
		relayLogCache = newCache
	}
	relayLogCacheLock.Unlock()

	return nil
}

func relayLogCleanup(ctx context.Context) error {
	keepPeriod, err := SettingGetInt(model.SettingKeyRelayLogKeepPeriod)
	if err != nil {
		return err
	}

	if keepPeriod > 0 {
		cutoffTime := time.Now().Add(-time.Duration(keepPeriod) * 24 * time.Hour).Unix()
		if err := db.GetDB().WithContext(ctx).Where("time < ?", cutoffTime).Delete(&model.RelayLog{}).Error; err != nil {
			return err
		}
	}

	return relayLogCleanupByStorage(ctx)
}

func relayLogMaxStorage() (float64, int64, error) {
	value, err := SettingGetString(model.SettingKeyRelayLogMaxStorageGB)
	if err != nil {
		return 0, 0, err
	}
	maxGB, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, 0, err
	}
	if maxGB <= 0 || math.IsNaN(maxGB) || math.IsInf(maxGB, 0) {
		return 0, 0, nil
	}
	maxBytes := int64(maxGB * bytesPerGB)
	return maxGB, maxBytes, nil
}

func relayLogStorageRows(ctx context.Context, scope *model.RelayLogScope) ([]model.RelayLog, error) {
	var logs []model.RelayLog
	query := db.GetDB().WithContext(ctx).
		Model(&model.RelayLog{}).
		Select("id", "user_id", "api_key_id", "request_endpoint", "request_path", "request_model_name", "request_api_key_name", "channel_name", "actual_model_name", "request_content", "response_content", "error", "attempts")
	query = relayLogApplyScope(query, scope)
	err := query.Order("id DESC").Find(&logs).Error
	return logs, err
}

func relayLogApproxBytes(relayLog model.RelayLog) int64 {
	size := len(relayLog.RequestModelName) +
		len(relayLog.RequestEndpoint) +
		len(relayLog.RequestPath) +
		len(relayLog.RequestAPIKeyName) +
		len(relayLog.ChannelName) +
		len(relayLog.ActualModelName) +
		len(relayLog.RequestContent) +
		len(relayLog.ResponseContent) +
		len(relayLog.Error) +
		256

	if len(relayLog.Attempts) > 0 {
		if data, err := json.Marshal(relayLog.Attempts); err == nil {
			size += len(data)
		}
	}

	return int64(size)
}

func relayLogCleanupByStorage(ctx context.Context) error {
	_, maxBytes, err := relayLogMaxStorage()
	if err != nil || maxBytes <= 0 {
		return err
	}

	logs, err := relayLogStorageRows(ctx, nil)
	if err != nil {
		return err
	}

	var total int64
	deleteIDs := make([]int64, 0)
	for idx, relayLog := range logs {
		total += relayLogApproxBytes(relayLog)
		if idx == 0 {
			continue
		}
		if total > maxBytes {
			deleteIDs = append(deleteIDs, relayLog.ID)
		}
	}

	if len(deleteIDs) == 0 {
		return nil
	}

	return db.GetDB().WithContext(ctx).Where("id IN ?", deleteIDs).Delete(&model.RelayLog{}).Error
}

func RelayLogStorageInfo(ctx context.Context, scope *model.RelayLogScope) (model.RelayLogStorage, error) {
	logs, err := relayLogStorageRows(ctx, scope)
	if err != nil {
		return model.RelayLogStorage{}, err
	}

	var storedBytes int64
	for _, relayLog := range logs {
		storedBytes += relayLogApproxBytes(relayLog)
	}
	relayLogCacheLock.Lock()
	cachedLogs := make([]model.RelayLog, len(relayLogCache))
	copy(cachedLogs, relayLogCache)
	relayLogCacheLock.Unlock()
	for _, relayLog := range cachedLogs {
		if !relayLogMatchScope(relayLog, scope) {
			continue
		}
		storedBytes += relayLogApproxBytes(relayLog)
	}

	maxGB, maxBytes, err := relayLogMaxStorage()
	if err != nil {
		return model.RelayLogStorage{}, err
	}
	return model.RelayLogStorage{
		StoredBytes: storedBytes,
		MaxBytes:    maxBytes,
		MaxGB:       maxGB,
	}, nil
}

// relayLogExportFormatVersion 标记可移植 NDJSON 导出文件的结构版本。
// 跨设备/重新部署导入时可据此判断兼容性。
const relayLogExportFormatVersion = 1

// RelayLogExportHeader 是 NDJSON 导出文件的首行元数据，使导出自描述、便于备份与迁移。
type RelayLogExportHeader struct {
	Type          string `json:"_export"`
	FormatVersion int    `json:"format_version"`
	GeneratedAt   int64  `json:"generated_at"`
	Redacted      bool   `json:"redacted"`
	StartTime     *int   `json:"start_time,omitempty"`
	EndTime       *int   `json:"end_time,omitempty"`
}

// RelayLogStreamExportNDJSON 以换行分隔 JSON（NDJSON/JSONL）的形式流式导出日志。
// 首行为自描述的元数据头，其后每行一条完整日志（userSummary 为 true 时写脱敏摘要）。
// 相比单个大 JSON 数组，NDJSON 每行可独立解析，更适合增量备份、跨设备迁移与重新部署后导入。
// 该函数与 RelayLogList 共用同一数据源（内存缓存 + 数据库），分页遍历全部匹配日志。
func RelayLogStreamExportNDJSON(ctx context.Context, w io.Writer, startTime, endTime *int, pageSize int, scope *model.RelayLogScope, userSummary bool) error {
	if pageSize < 1 {
		pageSize = 5000
	}
	encoder := json.NewEncoder(w)
	header := RelayLogExportHeader{
		Type:          "octopus.relay_log",
		FormatVersion: relayLogExportFormatVersion,
		GeneratedAt:   time.Now().Unix(),
		Redacted:      userSummary,
		StartTime:     startTime,
		EndTime:       endTime,
	}
	if err := encoder.Encode(header); err != nil {
		return err
	}
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}

	for page := 1; ; page++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		logs, err := RelayLogList(ctx, startTime, endTime, page, pageSize, scope)
		if err != nil {
			return err
		}
		if len(logs) == 0 {
			break
		}
		for _, relayLog := range logs {
			if userSummary {
				if err := encoder.Encode(RelayLogUserSummary(relayLog)); err != nil {
					return err
				}
			} else if err := encoder.Encode(relayLog); err != nil {
				return err
			}
		}
		if flusher, ok := w.(interface{ Flush() }); ok {
			flusher.Flush()
		}
		if len(logs) < pageSize {
			break
		}
	}
	return nil
}

// RelayLogList 查询日志列表，支持可选的时间范围过滤
// startTime 和 endTime 为 nil 时表示不限制时间范围
func RelayLogList(ctx context.Context, startTime, endTime *int, page, pageSize int, scope *model.RelayLogScope) ([]model.RelayLog, error) {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return nil, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	hasTimeFilter := startTime != nil && endTime != nil

	// 获取缓存中符合条件的日志
	relayLogCacheLock.Lock()
	var cachedLogs []model.RelayLog
	for _, log := range relayLogCache {
		if !relayLogMatchScope(log, scope) {
			continue
		}
		if hasTimeFilter {
			if log.Time >= int64(*startTime) && log.Time <= int64(*endTime) {
				cachedLogs = append(cachedLogs, log)
			}
		} else {
			cachedLogs = append(cachedLogs, log)
		}
	}
	relayLogCacheLock.Unlock()

	// 反转缓存日志顺序（原本新的在末尾，反转后新的在前面，方便分页）
	for i, j := 0, len(cachedLogs)-1; i < j; i, j = i+1, j-1 {
		cachedLogs[i], cachedLogs[j] = cachedLogs[j], cachedLogs[i]
	}

	offset := (page - 1) * pageSize

	result := make([]model.RelayLog, 0, len(cachedLogs)+pageSize)
	result = append(result, cachedLogs...)

	// 如果启用了日志保存，读取足够覆盖当前页的 DB 日志，再与内存缓存合并去重后统一分页。
	// 不能先按 cacheCount 偏移 DB；刷新/flush 后 cacheCount 会变，页面上的旧日志就会“消失”。
	if enabled {
		dbLimit := offset + pageSize + len(cachedLogs)
		if dbLimit < pageSize {
			dbLimit = pageSize
		}
		if dbLimit > 0 {
			query := db.GetDB().WithContext(ctx)
			if hasTimeFilter {
				query = query.Where("time >= ? AND time <= ?", *startTime, *endTime)
			}
			query = relayLogApplyScope(query, scope)

			if dbLimit <= relayLogTwoPhaseMaxIDs {
				// Two-phase read for interactive pages: first pluck ONLY the ids that match the
				// filter (ORDER+LIMIT), then fetch the full rows for just those ids. A
				// severity-filtered list (WHERE on the non-indexed error/total_attempts predicate)
				// walks past many non-matching rows to collect a page; doing that walk with
				// SELECT * pulls each walked row's large request_content/response_content (overflow
				// pages) off disk — the cause of the ~64s severity=success page. Plucking id only
				// skips those overflow reads during the walk; the full-row fetch touches just the
				// matching rows. Order is reapplied by the caller's stable sort below, so the id-IN
				// fetch need not preserve it. Guarded by relayLogTwoPhaseMaxIDs so the IN(...) list
				// never approaches SQLite's bound-parameter cap.
				// Phase 1 (pluck ids) + phase 2 (fetch rows) run in ONE read transaction so they
				// see a single consistent snapshot; without it a concurrent delete between them
				// could underfill the page (and make an export loop mistake the short page for EOF).
				txErr := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					idQuery := tx.Model(&model.RelayLog{})
					if hasTimeFilter {
						idQuery = idQuery.Where("time >= ? AND time <= ?", *startTime, *endTime)
					}
					idQuery = relayLogApplyScope(idQuery, scope)
					var ids []int64
					if err := idQuery.Order("time DESC, id DESC").Limit(dbLimit).Pluck("id", &ids).Error; err != nil {
						return err
					}
					if len(ids) > 0 {
						var dbLogs []model.RelayLog
						if err := tx.Where("id IN ?", ids).Find(&dbLogs).Error; err != nil {
							return err
						}
						result = append(result, dbLogs...)
					}
					return nil
				})
				if txErr != nil {
					return nil, txErr
				}
			} else {
				// Large limits (export / deep pages) would make the id IN(...) list exceed
				// SQLite's parameter cap; keep the original single-query path (export streams
				// everything and is not the interactive-page latency case the two-phase targets).
				var dbLogs []model.RelayLog
				if err := query.Order("time DESC, id DESC").Limit(dbLimit).Find(&dbLogs).Error; err != nil {
					return nil, err
				}
				result = append(result, dbLogs...)
			}
		}
	}

	result = relayLogDedupe(result)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Time != result[j].Time {
			return result[i].Time > result[j].Time
		}
		return result[i].ID > result[j].ID
	})
	if offset >= len(result) {
		result = nil
	} else {
		end := offset + pageSize
		if end > len(result) {
			end = len(result)
		}
		result = result[offset:end]
	}
	if scope != nil && scope.Redact {
		for i := range result {
			result[i] = RelayLogRedact(result[i])
		}
	}

	return result, nil
}

func RelayLogCount(ctx context.Context, startTime, endTime *int, scope *model.RelayLogScope) (int64, error) {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return 0, err
	}

	hasTimeFilter := startTime != nil && endTime != nil

	if enabled {
		query := db.GetDB().WithContext(ctx).Model(&model.RelayLog{})
		if hasTimeFilter {
			query = query.Where("time >= ? AND time <= ?", *startTime, *endTime)
		}
		query = relayLogApplyScope(query, scope)

		var total int64
		if err := query.Count(&total).Error; err != nil {
			return 0, err
		}
		return total, nil
	}

	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()

	var total int64
	for _, log := range relayLogCache {
		if !relayLogMatchScope(log, scope) {
			continue
		}
		if hasTimeFilter && (log.Time < int64(*startTime) || log.Time > int64(*endTime)) {
			continue
		}
		total++
	}

	return total, nil
}

// RelayLogSeverityCounts returns the total plus per-severity counts for the given
// filters, so the client can render accurate global badges (成功 / Warn / Error)
// independent of the current page. It reuses RelayLogCount per severity (so both
// the DB and the in-memory cache paths, and the persistence toggle, stay covered);
// every log classifies into exactly one severity, hence total is their sum.
func RelayLogSeverityCounts(ctx context.Context, startTime, endTime *int, scope *model.RelayLogScope) (model.RelayLogSeverityCounts, error) {
	base := model.RelayLogScope{}
	if scope != nil {
		base = *scope
	}

	var counts model.RelayLogSeverityCounts
	for _, severity := range []string{"success", "warn", "error"} {
		scoped := base
		scoped.Severity = severity
		count, err := RelayLogCount(ctx, startTime, endTime, &scoped)
		if err != nil {
			return model.RelayLogSeverityCounts{}, err
		}
		switch severity {
		case "success":
			counts.Success = count
		case "warn":
			counts.Warn = count
		case "error":
			counts.Error = count
		}
	}
	counts.Total = counts.Success + counts.Warn + counts.Error
	return counts, nil
}

func relayLogDedupe(logs []model.RelayLog) []model.RelayLog {
	if len(logs) < 2 {
		return logs
	}
	seen := make(map[int64]bool, len(logs))
	result := logs[:0]
	for _, relayLog := range logs {
		if relayLog.ID != 0 {
			if seen[relayLog.ID] {
				continue
			}
			seen[relayLog.ID] = true
		}
		result = append(result, relayLog)
	}
	return result
}

func RelayLogListSince(ctx context.Context, sinceID int64, limit int, scope *model.RelayLogScope) ([]model.RelayLog, error) {
	return RelayLogListSinceRange(ctx, sinceID, limit, scope, nil, nil)
}

func RelayLogListSinceRange(ctx context.Context, sinceID int64, limit int, scope *model.RelayLogScope, startTime, endTime *int) ([]model.RelayLog, error) {
	if sinceID <= 0 {
		return nil, nil
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	hasTimeFilter := startTime != nil && endTime != nil
	matchTime := func(relayLog model.RelayLog) bool {
		if !hasTimeFilter {
			return true
		}
		return relayLog.Time >= int64(*startTime) && relayLog.Time <= int64(*endTime)
	}

	result := make([]model.RelayLog, 0, limit)
	relayLogCacheLock.Lock()
	for _, relayLog := range relayLogCache {
		if relayLog.ID <= sinceID || !relayLogMatchScope(relayLog, scope) || !matchTime(relayLog) {
			continue
		}
		result = append(result, relayLog)
	}
	relayLogCacheLock.Unlock()

	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return nil, err
	}
	if enabled {
		query := db.GetDB().WithContext(ctx).Where("id > ?", sinceID)
		if hasTimeFilter {
			query = query.Where("time >= ? AND time <= ?", *startTime, *endTime)
		}
		query = relayLogApplyScope(query, scope)

		var dbLogs []model.RelayLog
		if err := query.Order("id ASC").Limit(limit + len(result)).Find(&dbLogs).Error; err != nil {
			return nil, err
		}
		result = append(result, dbLogs...)
	}

	result = relayLogDedupe(result)
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	if scope != nil && scope.Redact {
		for i := range result {
			result[i] = RelayLogRedact(result[i])
		}
	}
	return result, nil
}

func RelayLogClear(ctx context.Context, scope *model.RelayLogScope) error {
	relayLogCacheLock.Lock()
	if scope == nil || (scope.UserID == 0 && scope.APIKeyID == 0) {
		relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
	} else {
		filtered := relayLogCache[:0]
		for _, log := range relayLogCache {
			if !relayLogMatchScope(log, scope) {
				filtered = append(filtered, log)
			}
		}
		relayLogCache = filtered
	}
	relayLogCacheLock.Unlock()

	query := db.GetDB().WithContext(ctx)
	query = relayLogApplyScope(query, scope)
	if scope == nil || (scope.UserID == 0 && scope.APIKeyID == 0) {
		query = query.Where("1 = 1")
	}
	return query.Delete(&model.RelayLog{}).Error
}

func RelayLogRedact(relayLog model.RelayLog) model.RelayLog {
	relayLog.RequestContent = ""
	relayLog.ResponseContent = ""
	relayLog.RequestIP = ""
	relayLog.Attempts = nil
	if relayLog.Error != "" {
		relayLog.Error = "[redacted]"
	}
	return relayLog
}

func RelayLogUserSummary(relayLog model.RelayLog) model.RelayLogUserSummary {
	return model.RelayLogUserSummary{
		ID:                relayLog.ID,
		UserID:            relayLog.UserID,
		APIKeyID:          relayLog.APIKeyID,
		Time:              relayLog.Time,
		RequestEndpoint:   relayLog.RequestEndpoint,
		RequestPath:       relayLog.RequestPath,
		RequestModelName:  relayLog.RequestModelName,
		RequestAPIKeyName: relayLog.RequestAPIKeyName,
		// ChannelId / ChannelName are intentionally NOT copied: a normal user's log
		// view must not reveal which upstream channel served the request (the channel
		// label can carry the upstream's identity). Users see the model names
		// (request/actual) instead; admins keep the full RelayLog with channel info.
		ActualModelName:    relayLog.ActualModelName,
		InputTokens:        relayLog.InputTokens,
		OutputTokens:       relayLog.OutputTokens,
		CacheHitTokens:     relayLog.CacheHitTokens,
		CacheWriteTokens:   relayLog.CacheWriteTokens,
		CacheInputTokens:   relayLog.CacheInputTokens,
		CacheHitRate:       relayLog.CacheHitRate,
		Ftut:               relayLog.Ftut,
		UseTime:            relayLog.UseTime,
		Cost:               relayLog.Cost,
		ErrorCode:          relayLog.ErrorCode,
		ErrorStatus:        relayLog.ErrorStatus,
		TotalAttempts:      relayLog.TotalAttempts,
		SessionKey:         relayLog.SessionKey,
		SessionSource:      relayLog.SessionSource,
		RouteStickyHit:     relayLog.RouteStickyHit,
		IsStream:           relayLog.IsStream,
		UsageSource:        relayLog.UsageSource,
		UsageMissingReason: relayLog.UsageMissingReason,
		AccessPlanSlug:     relayLog.AccessPlanSlug,
		AccessPlanName:     relayLog.AccessPlanName,
		BillingModel:       relayLog.BillingModel,
	}
}

// relayLogErrorSQLCond is the SQL predicate for an "error" severity log. It is
// kept in lockstep with relayLogIsError (Go) and getRelayLogSeverity (web) so the
// server-side severity filter, the severity counts, and the client badges never
// diverge. A request "failed" when it carries an error message/code or a >=400
// upstream status.
// COALESCE guards against nullable columns: NOT (NULL OR ...) is NULL, which would
// silently drop such rows from the warn/success filters and undercount them.
const relayLogErrorSQLCond = "(COALESCE(error,'') <> '' OR COALESCE(error_code,'') <> '' OR COALESCE(error_status,0) >= 400)"

// relayLogIsError reports whether a log counts as an error (the request failed).
func relayLogIsError(relayLog model.RelayLog) bool {
	return strings.TrimSpace(relayLog.Error) != "" ||
		strings.TrimSpace(relayLog.ErrorCode) != "" ||
		relayLog.ErrorStatus >= 400
}

// relayLogSeverityValue classifies a log as "error" / "warn" / "success".
//   - error:   the request failed.
//   - warn:    it ultimately succeeded but took more than one channel attempt
//     (a retry/failover happened — TotalAttempts is written as len(attempts)).
//   - success: first-attempt success.
//
// Mirror of the web getRelayLogSeverity rule so client display and server filter
// agree. Every log maps to exactly one bucket, so the three counts sum to total.
func relayLogSeverityValue(relayLog model.RelayLog) string {
	if relayLogIsError(relayLog) {
		return "error"
	}
	if relayLog.TotalAttempts > 1 {
		return "warn"
	}
	return "success"
}

func relayLogMatchScope(relayLog model.RelayLog, scope *model.RelayLogScope) bool {
	if scope == nil {
		return true
	}
	if scope.UserID > 0 && relayLog.UserID != scope.UserID {
		return false
	}
	if scope.APIKeyID > 0 && relayLog.APIKeyID != scope.APIKeyID {
		return false
	}
	if scope.Endpoint != "" && !relayLogEndpointMatches(relayLog.RequestEndpoint, scope.Endpoint) {
		return false
	}
	if scope.Severity != "" && relayLogSeverityValue(relayLog) != scope.Severity {
		return false
	}
	if scope.RetriedOnly && relayLog.TotalAttempts <= 1 {
		return false
	}
	if scope.HideModelTest && strings.HasPrefix(relayLog.RequestEndpoint, "model_test") {
		return false
	}
	return true
}

// relayLogEndpointMatches reports whether a stored request_endpoint belongs to
// the requested endpoint family: an exact match, or a "<family>_<variant>" form.
// The relay stores variant endpoints as "<family>_<sub>" (e.g. gemini →
// gemini_generate_content / gemini_stream_generate_content, images →
// images_generations, videos → videos_poll), so the log filter matches by
// family rather than requiring the caller to know every stored variant.
func relayLogEndpointMatches(stored, filter string) bool {
	return stored == filter || strings.HasPrefix(stored, filter+"_")
}

// escapeLogEndpointLike escapes LIKE metacharacters so an endpoint family prefix
// is matched literally (backslash is declared as the ESCAPE char in the query).
func escapeLogEndpointLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func relayLogApplyScope(query *gorm.DB, scope *model.RelayLogScope) *gorm.DB {
	if scope == nil {
		return query
	}
	if scope.UserID > 0 {
		query = query.Where("user_id = ?", scope.UserID)
	}
	if scope.APIKeyID > 0 {
		query = query.Where("api_key_id = ?", scope.APIKeyID)
	}
	if scope.Endpoint != "" {
		// Family match: exact, or "<family>_<variant>" (e.g. filter "gemini"
		// also catches "gemini_generate_content"; "images" catches
		// "images_generations"; "videos" catches "videos_poll"). Kept in sync
		// with relayLogEndpointMatches (memory path) and the SSE tail path.
		query = query.Where("request_endpoint = ? OR request_endpoint LIKE ? ESCAPE '\\'", scope.Endpoint, escapeLogEndpointLike(scope.Endpoint)+`\_%`)
	}
	switch scope.Severity {
	case "error":
		query = query.Where(relayLogErrorSQLCond)
	case "warn":
		query = query.Where("NOT " + relayLogErrorSQLCond + " AND COALESCE(total_attempts,0) > 1")
	case "success":
		query = query.Where("NOT " + relayLogErrorSQLCond + " AND COALESCE(total_attempts,0) <= 1")
	}
	if scope.RetriedOnly {
		query = query.Where("COALESCE(total_attempts,0) > 1")
	}
	if scope.HideModelTest {
		// Exclude channel-test probe endpoints ("model_test", "model_test_responses", …).
		// The underscore is escaped so it matches literally, not as a LIKE wildcard.
		query = query.Where(`request_endpoint NOT LIKE ? ESCAPE '\'`, `model\_test%`)
	}
	return query
}
