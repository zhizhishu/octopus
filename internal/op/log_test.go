package op

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func init() {
	// 单测里不启后台 flusher: 落库时机由用例显式调用 relayLogFlushToDB / RelayLogSaveDBTask
	// 决定(否则定时刷会在用例之间乱动共享的 relayLogCache, 还可能在 t.TempDir 的库关掉后再写)。
	relayLogFlusherOnce.Do(func() {})
}

func setupRelayLogTest(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})

	ctx := context.Background()
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
	if err := RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear logs: %v", err)
	}
	invalidateModelTelemetryCache()
	return ctx
}

func TestRelayLogCleanupByStorageKeepsNewestLog(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000001"); err != nil {
		t.Fatalf("set storage limit: %v", err)
	}

	logs := []model.RelayLog{
		{ID: 1001, Time: 1001, RequestModelName: "oldest", RequestContent: strings.Repeat("a", 700), ResponseContent: strings.Repeat("b", 700)},
		{ID: 1002, Time: 1002, RequestModelName: "middle", RequestContent: strings.Repeat("c", 700), ResponseContent: strings.Repeat("d", 700)},
		{ID: 1003, Time: 1003, RequestModelName: "newest", RequestContent: strings.Repeat("e", 700), ResponseContent: strings.Repeat("f", 700)},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	if err := relayLogCleanupByStorage(ctx); err != nil {
		t.Fatalf("cleanup by storage: %v", err)
	}

	var remaining []model.RelayLog
	if err := db.GetDB().WithContext(ctx).Order("id ASC").Find(&remaining).Error; err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected one log to remain, got %d", len(remaining))
	}
	if remaining[0].ID != 1003 {
		t.Fatalf("expected newest log to remain, got %d", remaining[0].ID)
	}
}

// TestRelayLogListOrdersByIDNotSecondTime 钉住日志列表按 id(snowflake 毫秒) 倒序而不是
// 按秒级 time 倒序。回归的是"一批快速失败的请求整块压在慢成功请求上方"：
// 失败请求几百毫秒就结束、写入 id 连续偏大，慢成功请求跑几十秒后才写入。若排序先比
// 秒级 time，同秒内的行就失去时间分辨力，错误会扎堆顶在最前面。
func TestRelayLogListOrdersByIDNotSecondTime(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log persistence: %v", err)
	}

	// 三条同属一秒(time=5000)但 id 递增: 后写入的 id 更大, 必须排在更前面。
	for _, seed := range []struct {
		id    int64
		model string
	}{
		{5_000_100, "oldest-in-second"},
		{5_000_200, "middle-in-second"},
		{5_000_300, "newest-in-second"},
	} {
		if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
			ID:               seed.id,
			Time:             5000,
			RequestModelName: seed.model,
		}).Error; err != nil {
			t.Fatalf("seed log %d: %v", seed.id, err)
		}
	}

	logs, err := RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("expected 3 logs, got %d", len(logs))
	}

	want := []int64{5_000_300, 5_000_200, 5_000_100}
	for i, wantID := range want {
		if logs[i].ID != wantID {
			t.Fatalf("logs[%d].ID = %d, want %d (full order: %v)", i, logs[i].ID, wantID, relayLogIDs(logs))
		}
	}
}

func relayLogIDs(logs []model.RelayLog) []int64 {
	ids := make([]int64, 0, len(logs))
	for _, l := range logs {
		ids = append(ids, l.ID)
	}
	return ids
}

func TestRelayLogStorageInfoReportsConfiguredLimit(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "1.5"); err != nil {
		t.Fatalf("set storage limit: %v", err)
	}

	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               2001,
		Time:             2001,
		RequestModelName: "audit-model",
		RequestContent:   `{"messages":[{"role":"user","content":"hello"}]}`,
		ResponseContent:  `{"choices":[{"message":{"content":"hi"}}]}`,
	}).Error; err != nil {
		t.Fatalf("create log: %v", err)
	}

	info, err := RelayLogStorageInfo(ctx, nil)
	if err != nil {
		t.Fatalf("storage info: %v", err)
	}
	if info.StoredBytes <= 0 {
		t.Fatalf("expected stored bytes to be positive")
	}
	if info.MaxGB != 1.5 {
		t.Fatalf("expected max gb 1.5, got %v", info.MaxGB)
	}
	if info.MaxBytes <= 0 {
		t.Fatalf("expected max bytes to be positive")
	}
}

func TestRelayLogAddBackfillsCurrentUnixTime(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
		t.Fatalf("disable relay log persistence: %v", err)
	}
	before := time.Now().Unix()
	if err := RelayLogAdd(ctx, model.RelayLog{RequestModelName: "time-backfill"}); err != nil {
		t.Fatalf("add relay log: %v", err)
	}
	after := time.Now().Unix()

	logs, err := RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected a relay log")
	}
	if logs[0].Time < before || logs[0].Time > after {
		t.Fatalf("expected backfilled current unix time between %d and %d, got %d", before, after, logs[0].Time)
	}
}

func TestRelayLogListScopesAndRedactsUserLogs(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{
		{
			ID:                    3001,
			UserID:                1,
			APIKeyID:              8,
			RequestIP:             "203.0.113.9",
			Time:                  3001,
			RequestEndpoint:       "messages",
			RequestPath:           "/v1/messages",
			RequestModelName:      "claude-opus-4-7[1m]",
			RequestAPIKeyName:     "user-key",
			ChannelId:             4,
			ChannelName:           "Claude-Relay-1",
			ActualModelName:       "claude-opus-4-7[1m]",
			InputTokens:           10,
			OutputTokens:          2,
			CacheHitTokens:        3,
			CacheWriteTokens:      4,
			CacheInputTokens:      13,
			CacheHitRate:          0.3,
			Ftut:                  111,
			UseTime:               222,
			Cost:                  0.123,
			RequestContent:        `{"message":"secret request"}`,
			ResponseContent:       `{"message":"secret response"}`,
			Error:                 "upstream error",
			ErrorCode:             "service_busy",
			ErrorStatus:           503,
			ErrorStrategy:         "internal strategy",
			TotalAttempts:         1,
			AccessPlanSlug:        "vip",
			AccessPlanName:        "VIP",
			RouteProfileName:      "private-route",
			BillingProfileName:    "private-billing",
			BillingModel:          "claude-opus-4-7[1m]",
			BaseInputPrice:        1,
			DefaultMultiplier:     2,
			PromptOverrideSources: []string{"global"},
			Attempts: []model.ChannelAttempt{{
				ChannelID:   1,
				ChannelName: "private-channel",
				Status:      "failed",
			}},
		},
		{
			ID:              3002,
			UserID:          2,
			Time:            3002,
			RequestContent:  `{"message":"other request"}`,
			ResponseContent: `{"message":"other response"}`,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	scope := &model.RelayLogScope{UserID: 1, Redact: true}
	result, err := RelayLogList(ctx, nil, nil, 1, 20, scope)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected one scoped log, got %d", len(result))
	}
	if result[0].UserID != 1 {
		t.Fatalf("expected scoped user log, got user %d", result[0].UserID)
	}
	if result[0].RequestContent != "" || result[0].ResponseContent != "" {
		t.Fatalf("expected request and response content to be redacted")
	}
	if result[0].Error != "[redacted]" {
		t.Fatalf("expected error to be redacted, got %q", result[0].Error)
	}
	if result[0].Attempts != nil {
		t.Fatalf("expected attempts to be redacted")
	}
	if result[0].RequestIP != "" {
		t.Fatalf("expected request ip to be redacted, got %q", result[0].RequestIP)
	}
	summary := RelayLogUserSummary(result[0])
	if summary.RequestModelName != "claude-opus-4-7[1m]" || summary.InputTokens != 10 || summary.CacheHitTokens != 3 {
		t.Fatalf("expected user summary to preserve safe usage fields: %#v", summary)
	}
	// The channel identity must NOT survive into a user-facing summary (a user must
	// not learn which upstream channel served them); the field is removed entirely.
	if summary.ErrorCode != "service_busy" || summary.ErrorStatus != 503 {
		t.Fatalf("expected user summary to preserve safe error status/code: %#v", summary)
	}
	if result[0].RouteProfileName != "private-route" || len(result[0].PromptOverrideSources) != 1 || result[0].BaseInputPrice != 1 {
		t.Fatalf("admin/internal fields should remain in redacted relay log for admin pipeline compatibility")
	}

	publicSummary := RelayLogUserSummary(logs[0])
	if publicSummary.BillingModel != "claude-opus-4-7[1m]" {
		t.Fatalf("expected public summary to preserve safe audit fields: %#v", publicSummary)
	}
}

func TestRelayLogListSinceReturnsScopedLogsOldestFirst(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{
		{ID: 7001, UserID: 1, Time: 7001, RequestModelName: "old"},
		{ID: 7002, UserID: 2, Time: 7002, RequestModelName: "other-user"},
		{ID: 7003, UserID: 1, Time: 7003, RequestModelName: "newer"},
		{ID: 7004, UserID: 1, Time: 7004, RequestModelName: "newest"},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	result, err := RelayLogListSince(ctx, 7001, 10, &model.RelayLogScope{UserID: 1})
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected two scoped replay logs, got %#v", result)
	}
	if result[0].ID != 7003 || result[1].ID != 7004 {
		t.Fatalf("expected replay logs oldest first after since id, got %#v", result)
	}
}

func relayLogCacheLen() int {
	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	return len(relayLogCache)
}

func relayLogDBCount(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var count int64
	if err := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).Count(&count).Error; err != nil {
		t.Fatalf("count db logs: %v", err)
	}
	return count
}

// 开启持久化时, RelayLogAdd 只入内存待落库队列(请求路径上没有同步 INSERT),
// 真正的落库由后台 flusher / 周期任务做。
func TestRelayLogAddQueuesInsteadOfWritingSynchronously(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log persistence: %v", err)
	}

	if err := RelayLogAdd(ctx, model.RelayLog{RequestModelName: "async-queued"}); err != nil {
		t.Fatalf("add relay log: %v", err)
	}
	if got := relayLogDBCount(t, ctx); got != 0 {
		t.Fatalf("expected no synchronous db write from RelayLogAdd, got %d rows", got)
	}
	if got := relayLogCacheLen(); got != 1 {
		t.Fatalf("expected one pending cached log, got %d", got)
	}

	// 未落库也要能被日志页读到(缓存 + DB 合并读)
	logs, err := RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 || logs[0].RequestModelName != "async-queued" {
		t.Fatalf("expected pending log to be visible before flush, got %#v", logs)
	}

	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatalf("flush relay logs: %v", err)
	}
	if got := relayLogDBCount(t, ctx); got != 1 {
		t.Fatalf("expected one row after flush, got %d", got)
	}
	if got := relayLogCacheLen(); got != 0 {
		t.Fatalf("expected pending queue drained after flush, got %d", got)
	}
}

// 攒够 relayLogFlushBatchSize 就唤醒后台 flusher(信号非阻塞、容量 1)。
func TestRelayLogAddSignalsFlusherOnBatchThreshold(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log persistence: %v", err)
	}
	select { // 清掉之前用例可能留下的信号
	case <-relayLogFlushSignal:
	default:
	}

	for i := 0; i < relayLogFlushBatchSize-1; i++ {
		if err := RelayLogAdd(ctx, model.RelayLog{RequestModelName: "batch-" + strconv.Itoa(i)}); err != nil {
			t.Fatalf("add relay log %d: %v", i, err)
		}
	}
	if len(relayLogFlushSignal) != 0 {
		t.Fatalf("expected no flush signal below batch threshold")
	}

	if err := RelayLogAdd(ctx, model.RelayLog{RequestModelName: "batch-threshold"}); err != nil {
		t.Fatalf("add threshold relay log: %v", err)
	}
	if len(relayLogFlushSignal) != 1 {
		t.Fatalf("expected a flush signal once the batch threshold is reached")
	}

	select { // 归位, 不留给后面的用例
	case <-relayLogFlushSignal:
	default:
	}
}

// DB 卡住(这里用“不刷”模拟)时待落库队列封顶, 丢最旧的并计数, 内存不无限涨。
func TestRelayLogPendingQueueCapsAndDropsOldest(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log persistence: %v", err)
	}
	relayLogCacheLock.Lock()
	droppedBefore := relayLogPendingDropped
	relayLogCacheLock.Unlock()

	overflow := 2
	for i := 0; i < relayLogPendingMaxSize+overflow; i++ {
		if err := RelayLogAdd(ctx, model.RelayLog{RequestModelName: "pending-" + strconv.Itoa(i)}); err != nil {
			t.Fatalf("add relay log %d: %v", i, err)
		}
	}

	relayLogCacheLock.Lock()
	cached := len(relayLogCache)
	oldest := relayLogCache[0].RequestModelName
	droppedAfter := relayLogPendingDropped
	relayLogCacheLock.Unlock()

	if cached != relayLogPendingMaxSize {
		t.Fatalf("expected pending queue capped at %d, got %d", relayLogPendingMaxSize, cached)
	}
	if droppedAfter-droppedBefore != int64(overflow) {
		t.Fatalf("expected %d dropped logs counted, got %d", overflow, droppedAfter-droppedBefore)
	}
	if oldest != "pending-"+strconv.Itoa(overflow) {
		t.Fatalf("expected the oldest logs to be dropped first, oldest kept is %q", oldest)
	}

	relayLogCacheLock.Lock()
	relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
	relayLogCacheLock.Unlock()
}

// 落库完成后只移除“这一批”(按 ID), 不按位置裁剪: flush 在途期间队头可能被封顶淘汰或
// RelayLogClear 削掉, 按位置裁会误删还没落库的新日志。
func TestRelayLogRemoveFlushedKeepsUnflushedEntries(t *testing.T) {
	setupRelayLogTest(t)

	flushedBatch := []model.RelayLog{
		{ID: 6001, Time: 6001, RequestModelName: "already-dropped-head"},
		{ID: 6002, Time: 6002, RequestModelName: "flushed"},
	}

	relayLogCacheLock.Lock()
	// 队头 6001 在 flush 在途时已被削掉, 队尾 6003 是 flush 之后新写入的
	relayLogCache = append(relayLogCache[:0],
		model.RelayLog{ID: 6002, Time: 6002, RequestModelName: "flushed"},
		model.RelayLog{ID: 6003, Time: 6003, RequestModelName: "added-during-flush"},
	)
	relayLogCacheLock.Unlock()

	relayLogRemoveFlushed(flushedBatch)

	relayLogCacheLock.Lock()
	remaining := make([]model.RelayLog, len(relayLogCache))
	copy(remaining, relayLogCache)
	relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
	relayLogCacheLock.Unlock()

	if len(remaining) != 1 || remaining[0].ID != 6003 {
		t.Fatalf("expected only the not-yet-flushed log to remain, got %#v", remaining)
	}
}

func TestRelayLogListDedupesCacheAndDBOverlap(t *testing.T) {
	ctx := setupRelayLogTest(t)

	duplicate := model.RelayLog{ID: 8001, Time: 8001, RequestModelName: "duplicate"}
	if err := db.GetDB().WithContext(ctx).Create(&duplicate).Error; err != nil {
		t.Fatalf("create db log: %v", err)
	}

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, duplicate)
	relayLogCacheLock.Unlock()

	result, err := RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected duplicate cache/db log to appear once, got %#v", result)
	}
	if result[0].ID != duplicate.ID {
		t.Fatalf("expected duplicate log id %d, got %d", duplicate.ID, result[0].ID)
	}
}

func TestRelayLogListPaginatesAfterMergingCacheAndDB(t *testing.T) {
	ctx := setupRelayLogTest(t)

	dbLogs := []model.RelayLog{
		{ID: 9001, Time: 9001, RequestModelName: "oldest"},
		{ID: 9002, Time: 9002, RequestModelName: "older"},
		{ID: 9003, Time: 9003, RequestModelName: "middle"},
		{ID: 9004, Time: 9004, RequestModelName: "newer"},
		{ID: 9005, Time: 9005, RequestModelName: "newest"},
	}
	if err := db.GetDB().WithContext(ctx).Create(&dbLogs).Error; err != nil {
		t.Fatalf("create db logs: %v", err)
	}

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, dbLogs[3], dbLogs[4])
	relayLogCacheLock.Unlock()

	result, err := RelayLogList(ctx, nil, nil, 2, 2, nil)
	if err != nil {
		t.Fatalf("list relay logs with cache: %v", err)
	}
	if len(result) != 2 || result[0].ID != 9003 || result[1].ID != 9002 {
		t.Fatalf("expected stable second page after cache/db merge, got %#v", result)
	}

	relayLogCacheLock.Lock()
	relayLogCache = relayLogCache[:0]
	relayLogCacheLock.Unlock()

	result, err = RelayLogList(ctx, nil, nil, 2, 2, nil)
	if err != nil {
		t.Fatalf("list relay logs after refresh: %v", err)
	}
	if len(result) != 2 || result[0].ID != 9003 || result[1].ID != 9002 {
		t.Fatalf("expected same second page after cache clears, got %#v", result)
	}
}

func TestRelayLogListOmitsContentBodies(t *testing.T) {
	ctx := setupRelayLogTest(t)
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable keep: %v", err)
	}

	big := strings.Repeat("X", 20000)
	item := model.RelayLog{
		ID:               91001,
		Time:             time.Now().Unix(),
		RequestEndpoint:  "responses",
		RequestModelName: "glm-5.2",
		ActualModelName:  "glm-5.2",
		RequestContent:   big,
		ResponseContent:  big,
	}
	if err := RelayLogAdd(ctx, item); err != nil {
		t.Fatalf("add log: %v", err)
	}
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatalf("flush log: %v", err)
	}

	logs, err := RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected logs")
	}
	if logs[0].RequestContent != "" || logs[0].ResponseContent != "" {
		t.Fatalf("list must omit bodies, got req=%d resp=%d", len(logs[0].RequestContent), len(logs[0].ResponseContent))
	}
	detail, err := RelayLogGetByID(ctx, logs[0].ID, nil)
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	if detail.RequestContent != big || detail.ResponseContent != big {
		t.Fatalf("detail must keep bodies")
	}
}
