package op

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
	"github.com/zhizhishu/octopus/internal/utils/snowflake"
	"gorm.io/gorm"
)

const relayLogMaxSize = 20
const relayLogMaxSizeNoDB = 100 // 当不保存到数据库时，允许更大的缓存用于实时查询
const bytesPerGB = 1024 * 1024 * 1024

// relayLogPendingMaxSize 是“保存到数据库”时待落库队列的上限。落库改成后台批量刷之后，
// 请求路径只往内存里塞，DB 卡死/变慢时队列会一直涨——封顶后丢最旧的并计数告警，
// 保证内存不被完整请求/响应 body 撑爆(宁可丢审计日志, 也不能拖垮转发)。
const relayLogPendingMaxSize = 500

// relayLogFlushBatchSize 是唤醒后台 flusher 的批量阈值；攒不够则由 relayLogFlushInterval
// 定时兜底，二者取先。
const relayLogFlushBatchSize = relayLogMaxSize

// relayLogFlushInterval 是后台 flusher 的定时兜底间隔(低流量时日志最多晚这么久落库)。
const relayLogFlushInterval = time.Second

// relayLogFlushTimeout 是单次后台批量落库的超时。
const relayLogFlushTimeout = 30 * time.Second

// relayLogDropWarnInterval 限制“落库跟不上、丢最旧”的告警频率：卡死时每请求都会丢一条，
// 不限频会把日志刷爆。
const relayLogDropWarnInterval = 10 * time.Second

// relayLogTwoPhaseMaxIDs bounds the id list of the two-phase severity-filtered read so the
// id IN(...) fetch stays well under SQLite's bound-parameter cap (999 on old builds, 32766 on
// 3.32+). Interactive log pages are small (page_size<=100 + in-memory cache); larger limits
// (export / deep pages) fall back to the single-query path.
const relayLogTwoPhaseMaxIDs = 900

var relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
var relayLogCacheLock sync.Mutex

var relayLogFlushLock sync.Mutex

// relayLogFlushSignal 把“攒够一批了”从请求路径通知给后台 flusher。容量 1 + 非阻塞 send：
// 已有待处理信号时直接丢弃本次通知(flusher 醒来会把当时的整批一起刷)，
// 请求路径永远不会因为落库而阻塞。
var relayLogFlushSignal = make(chan struct{}, 1)

// relayLogFlusherOnce 保证后台 flusher 只启动一次：首次需要落库的写入时惰性启动，
// 未开启持久化的部署(以及单测)不会凭空多一个常驻协程。
var relayLogFlusherOnce sync.Once

// relayLogPendingDropped / relayLogDropWarnAt 由 relayLogCacheLock 保护：
// 累计丢弃条数与上次告警时间。
var relayLogPendingDropped int64
var relayLogDropWarnAt time.Time

// relayLogWriteFailures / relayLogLastWriteFailureAt 同样由 relayLogCacheLock 保护：
// 进程内累计落库失败次数与最近一次失败时间。没有这两个数，运维只能从“列表里少了一条”
// 反推落库出过事——而丢日志和“请求没发生”在界面上长得一模一样。
var relayLogWriteFailures int64
var relayLogLastWriteFailureAt int64

var relayLogSubscribers = make(map[chan model.RelayLog]struct{})
var relayLogSubscribersLock sync.RWMutex

type relayLogStreamTokenScope struct {
	UserID        int
	APIKeyID      int
	Endpoint      string
	Provider      string
	Model         string
	Severity      string
	Search        string
	RetriedOnly   bool
	HideModelTest bool
	// UpstreamModelMismatch 三态筛选: nil=不筛, true/false=只要上游回显不一致/一致的行。
	UpstreamModelMismatch *bool
	IsAdmin               bool
	StartTime             int
	EndTime               int
	HasTimeRange          bool
	createdAt             time.Time
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
	tokenScope := relayLogStreamTokenScope{
		UserID:        scope.UserID,
		APIKeyID:      scope.APIKeyID,
		Endpoint:      scope.Endpoint,
		Provider:      scope.Provider,
		Model:         scope.Model,
		Severity:      scope.Severity,
		Search:        scope.Search,
		RetriedOnly:   scope.RetriedOnly,
		HideModelTest: scope.HideModelTest,

		UpstreamModelMismatch: scope.UpstreamModelMismatch,
		IsAdmin:               isAdmin,
		createdAt:             time.Now(),
	}
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

// RelayLogStreamTokenSweep drops stream tokens minted more than olderThan ago and returns
// how many were removed. Tokens are single-use (revoked on consume), but one created and
// never consumed — the admin opened the log page then closed it before the SSE connected —
// otherwise lingers forever. A minted token is meant to be redeemed within seconds, so a
// generous sweep age reclaims only genuinely-dead tokens.
func RelayLogStreamTokenSweep(olderThan time.Duration) int {
	if olderThan <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-olderThan)
	relayLogStreamTokensLock.Lock()
	removed := 0
	for token, scope := range relayLogStreamTokens {
		if scope.createdAt.Before(cutoff) {
			delete(relayLogStreamTokens, token)
			removed++
		}
	}
	relayLogStreamTokensLock.Unlock()
	return removed
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
	relayLogCacheLock.Unlock()

	result := db.GetDB().WithContext(ctx).Create(&batch)
	if result.Error != nil {
		// flush 失败也把这批从缓存排空: 否则一个坏行(如超长字段撞 varchar 上限)会让之后
		// 每次 flush 都带着它重试、永久失败, 而 enabled 分支的封顶淘汰会替它丢掉更新的日志。
		// 丢掉这一批(含可能是暂时性 DB 抖动的正常行), 换取落库不被单条坏行永久卡死。
		relayLogRemoveFlushed(batch)
		// 计数放在这里而不是各调用方: 后台 flusher 与 RelayLogSaveDBTask 都经此函数落库,
		// 分开记会漏掉其中的失败。
		relayLogCacheLock.Lock()
		relayLogWriteFailures++
		relayLogLastWriteFailureAt = time.Now().Unix()
		relayLogCacheLock.Unlock()
		return result.Error
	}

	// 搜索投影表跟着主表一起补: 少了这一步, 新落库的日志能列表显示却搜不到。
	relayLogSearchIndexUpsert(db.GetDB().WithContext(ctx), batch)
	// 另存图片的引用行：清理任务靠它判断文件还有没有日志在用。
	relayLogImageRefsInsert(db.GetDB().WithContext(ctx), batch)

	relayLogRemoveFlushed(batch)
	return nil
}

// relayLogRemoveFlushed 把已落库(或已放弃)的这一批按 ID 从待落库队列里删掉。
// 不按位置裁剪(旧写法是 relayLogCache[flushedUpto:]): flush 在途期间 relayLogCacheLock
// 是放开的, 队头可能被封顶淘汰或 RelayLogClear 削掉, 按位置裁就会误删还没落库的新日志。
// 顺带重建底层数组, 让已落库日志的 Request/ResponseContent 能被回收。
func relayLogRemoveFlushed(batch []model.RelayLog) {
	flushed := make(map[int64]struct{}, len(batch))
	for _, relayLog := range batch {
		flushed[relayLog.ID] = struct{}{}
	}

	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	if len(relayLogCache) == 0 {
		return
	}
	capHint := len(relayLogCache) - len(flushed)
	if capHint < relayLogMaxSize {
		capHint = relayLogMaxSize
	}
	remaining := make([]model.RelayLog, 0, capHint)
	for _, relayLog := range relayLogCache {
		if _, ok := flushed[relayLog.ID]; ok {
			continue
		}
		remaining = append(remaining, relayLog)
	}
	relayLogCache = remaining
}

// relayLogFlushNotify 非阻塞地唤醒后台 flusher(见 relayLogFlushSignal)。
func relayLogFlushNotify() {
	select {
	case relayLogFlushSignal <- struct{}{}:
	default:
	}
}

// RelayLogFlusherStart 启动后台落库协程(整个进程只启一次), 由启动流程 task.Init 显式调用。
// 刻意不做"首次写入时惰性启动": 一个进程级常驻协程要是从任意 RelayLogAdd 偷偷冒出来,
// 测试二进制里(每个用例各开各的临时 DB)就会出现无人拥有生命周期的后台写库者, 撞别的
// 用例的 DB(readonly/no such table 噪音)甚至在 TempDir 清理瞬间写 WAL 让目录删不掉——
// CI 已实翻过一次。生产路径 cmd/start.go 必经 task.Init, 启动窗口内先入队的日志等
// flusher 起来后一并批量落库; 测试则显式调 relayLogFlushToDB 驱动落库时机。
func RelayLogFlusherStart() {
	relayLogFlusherOnce.Do(func() {
		safe.SafeGo("relay-log-flush-loop", relayLogFlushLoop)
	})
}

// relayLogFlushLoop 是常态下唯一的落库者: 被批量阈值信号唤醒, 或每 relayLogFlushInterval
// 定时兜底刷一次(二者取先), 把“每请求一次同步 INSERT”合并成批量写, 请求路径不再等 DB。
// 用自己的 context: 请求侧的 ctx 会随请求结束被取消, 不能拿来做后台落库。
func relayLogFlushLoop() {
	ticker := time.NewTicker(relayLogFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-relayLogFlushSignal:
		case <-ticker.C:
		}

		// 期间可能被关掉持久化: 关掉后就不再落库(与 RelayLogSaveDBTask 同一判据),
		// 缓存交给 RelayLogSaveDBTask 的“不保存”分支按 relayLogMaxSizeNoDB 裁剪。
		enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
		if err != nil || !enabled {
			continue
		}

		flushCtx, cancel := context.WithTimeout(context.Background(), relayLogFlushTimeout)
		err = relayLogFlushToDB(flushCtx)
		cancel()
		if err != nil {
			log.Warnf("relay log async flush failed: %v", err)
		}
	}
}

// relayLogAddPending 把日志放进待落库队列, 必要时唤醒后台 flusher(由 task.Init 经
// RelayLogFlusherStart 启动, 这里绝不偷偷拉起协程——见该函数注释)。
// 队列封顶后丢最旧的: DB 卡死时请求还在源源不断地来, 不淘汰就是无上限吃内存。
func relayLogAddPending(relayLog model.RelayLog) {
	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, relayLog)
	dropped := 0
	warn := false
	if len(relayLogCache) > relayLogPendingMaxSize {
		dropped = len(relayLogCache) - relayLogPendingMaxSize
		// 重建底层数组而不是 reslice, 避免数组继续引用被丢日志的 Request/ResponseContent
		newCache := make([]model.RelayLog, relayLogPendingMaxSize, relayLogPendingMaxSize+relayLogMaxSize)
		copy(newCache, relayLogCache[dropped:])
		relayLogCache = newCache
		relayLogPendingDropped += int64(dropped)
		if time.Since(relayLogDropWarnAt) >= relayLogDropWarnInterval {
			relayLogDropWarnAt = time.Now()
			warn = true
		}
	}
	pending := len(relayLogCache)
	totalDropped := relayLogPendingDropped
	relayLogCacheLock.Unlock()

	if warn {
		log.Warnf("relay log flush is falling behind: dropped %d oldest cached logs (pending cap %d, dropped total %d)", dropped, relayLogPendingMaxSize, totalDropped)
	}
	if pending >= relayLogFlushBatchSize {
		relayLogFlushNotify()
	}
}

// RelayLogAdd 记录一条中继日志。开启持久化时只入内存队列 + 唤醒后台 flusher 批量落库,
// 请求路径上不再有同步 INSERT(整条请求/响应 body 的写入曾把 DB 锁串在转发路径上)。
// ctx 保留是为了调用方签名不变: 落库由后台协程用自己的 context 做, 不能用会被取消的请求 ctx。
func RelayLogAdd(ctx context.Context, relayLog model.RelayLog) error {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return err
	}
	relayLog.ID = snowflake.GenerateID()
	if relayLog.Time <= 0 {
		relayLog.Time = time.Now().Unix()
	}
	// 用户页「IP / 客户端」：只读这条日志已有的入站 IP / UA（纯内存，周期落库），
	// 不看日志保留开关——关了日志也要知道这个人从哪来、用什么客户端。
	UserAccessRecord(relayLog.UserID, relayLog.APIKeyID, relayLog.RequestIP, relayLog.RequestUserAgent, relayLog.Time)
	// 被动观察: 在 ID/Time 归一后、pending/SSE 之前对已脱敏副本做一次纯本地
	// 计算(纯函数, 无外部副作用, 失败以 not_applicable 表达, 不返回业务错误)。
	//
	// 受总开关控制。关掉时两样都不算: 连摘要也不写, 免得界面上出现"关了还有
	// 数据"的矛盾状态。读设置失败按"开"处理——被动检测零成本, 因为一次读配置
	// 抖动就静默停掉检测, 比多算一次更糟。
	if passiveProbeOn() {
		relayLog.PassiveSummary = computePassiveSummary(relayLog)
		// 后端来源证据: 同一个纯本地 pass 里按三源权重表打分（含缺失字段反证）。
		// 零额外请求，只用这条日志已有的 service_tier / cache TTL 拆分字段。
		// 空证据不落库：nil 明确表示"没证据"，比一个全零对象更诚实。
		if src := computePassiveSourceEvidence(relayLog); !src.Empty() {
			relayLog.PassiveSource = &src
		}
		// 被动探针: 同一次纯本地计算里顺带查"这条日志的硬事实对不对得上"(token 加减法、
		// 缓存是否超过输入、字符/token 比值、分词器口径)。零上游请求、只证伪不认证。
		// 失败请求与缺字段一律跳过——"算不出来"不能报成"算出来不对"。
		relayLog.PassiveProbes = computeRelayProbesForLog(relayLog)
	}
	// 桶在入队前就算好。落库时 BeforeCreate 会再算一次（备份导入等不经这里的
	// 路径也盖得住）；这里写上是为了内存里的副本和即将入库的行是同一个值。
	relayLog.Severity = model.ClassifyRelayLogSeverity(relayLog)

	if enabled {
		relayLogAddPending(relayLog)
		// 实时推送保持在 Add 当下就发, 不等落库(日志页/SSE 尾随看到的仍是即时的)
		safe.SafeGo("relay-log-notify", func() { notifySubscribers(relayLog) })
		// Telemetry caches are TTL-bounded (see model_telemetry_cache.go); pushing
		// invalidation here defeats the cache entirely under sustained traffic.
		return nil
	}

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, relayLog)
	if len(relayLogCache) >= relayLogMaxSizeNoDB {
		// 如果未启用日志保存，移除最旧的日志，保留最新的日志用于实时查询
		// 重建底层数组而不是 reslice，避免数组持续引用旧日志的 Request/ResponseContent 导致内存无法回收
		keepSize := relayLogMaxSizeNoDB / 2
		if len(relayLogCache) > keepSize {
			newCache := make([]model.RelayLog, keepSize, relayLogMaxSizeNoDB)
			copy(newCache, relayLogCache[len(relayLogCache)-keepSize:])
			relayLogCache = newCache
		}
	}
	relayLogCacheLock.Unlock()
	safe.SafeGo("relay-log-notify", func() { notifySubscribers(relayLog) })
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
		err := relayLogCleanup(ctx)
		// 日志删完再清扫图片：刚失去最后一条引用的文件这一轮就能看到。
		relayLogImageSweepLogged(ctx, RelayLogImageOrphanGrace)
		return err
	}
	// 不落库时不会再存新图，但以前存的照样要过期、照样受容量上限约束。
	relayLogImageSweepLogged(ctx, RelayLogImageOrphanGrace)

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
		// 按时间分批删，而不是"先把所有过期 id Pluck 进一个切片"。
		//
		// 旧写法在大库上是个陷阱：过期 id 可能有几十万个，一个 []int64 就是几 MB，
		// 之后还要拼成 `id IN (?,?,…)` 的超长语句 —— 参数个数有上限，且整条语句
		// 要在内存里拼出来。分批删的成本只与删除量成正比，内存却是常量。
		if err := relayLogDeleteExpiredBefore(ctx, cutoffTime); err != nil {
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
	// 旧实现在这里把所有日志正文一次性读进内存再倒推要删的 id：生产上几十万行
	// 就是几十 GB 内存，进程被 OOM 杀掉后清理永远跑不完，磁盘只涨不降
	// (设置 10GB、实际 59GB 就是这么来的)。改为全 SQL 聚合 + 分批删除，
	// 内存与数据量无关。详见 log_storage.go。
	//
	// 注意这里刻意只统计一次：relayLogStoredBytes 是全表扫描，在 29GB 的库上
	// 实测约 2 分钟，多算一次就是白烧两分钟。
	before, beforeRows, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		return err
	}
	if err := relayLogCleanupByStorageBounded(ctx); err != nil {
		return err
	}
	after, afterRows, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		return err
	}
	// 用行数判断"是否真的删了东西"，比用字节数更可靠：字节估算受平均值影响，
	// 而行数变化是确定的。
	if afterRows < beforeRows {
		// 确实删掉了行才 VACUUM：只标空闲不还空间，db 文件不会变小，
		// 用户看到的就是"设了上限、日志也少了，文件还是那么大"。
		if vacuumErr := relayLogVacuum(ctx); vacuumErr != nil {
			// 这里用 Debugf 而不是 Warnf：磁盘不够时跳过是**预期行为**，
			// 每 10 分钟报一次 WARN 只会污染日志，让真正的问题淹掉。
			log.Debugf("relay log vacuum skipped/failed (rows deleted %d→%d, bytes %d→%d): %v",
				beforeRows, afterRows, before, after, vacuumErr)
		}
	}

	// 🔴 上面那段只在"本轮删了行"时才回收，有个真实踩到的缺口：
	// 日志只跨 3 天、保留期 30 天 → 每轮一行都删不掉 → afterRows == beforeRows
	// → VACUUM 永远进不去，而库里躺着 11.7 GiB 历史死空间永远不会被回收。
	//
	// 所以回收必须与"这轮删没删"解耦：独立看库自己的空闲页占比。日志删得越干净
	// 这个缺口越不可能被触发——越健康越看不见，正是它藏了这么久的原因。
	// 冷却期在 RelayLogReclaimDeadSpace 内部保证（见 log_storage.go）。
	if err := RelayLogReclaimDeadSpace(ctx); err != nil {
		// 回收失败不该中断清理主流程：空间没还回去不是故障，下次还会再试。
		log.Debugf("relay log dead-space reclaim error: %v", err)
	}
	return nil
}

func RelayLogStorageInfo(ctx context.Context, scope *model.RelayLogScope) (model.RelayLogStorage, error) {
	// 旧实现同样要先读全表正文才能算字节数，管理页一问就卡死(实测生产 15s 无响应)。
	// 现在走 SQL 聚合，并额外把搜索投影表的占用算进来。详见 log_storage.go。
	return relayLogStorageInfoBounded(ctx, scope)
}

// RelayLogQueueHealth 是日志落库队列的只读健康快照, 只回答一个问题: 日志系统自己
// 有没有积压或丢记录。没有它时, "日志里查不到"既可能是请求没发生、也可能是落库没跟上,
// 两种完全不同的结论在界面上长得一模一样。
//
// 计数是进程内的: 重启归零。刻意不做持久化——它描述的是"当前进程的落库状态",
// 重启后残留的历史数字反而会被误读成正在发生的故障。
type RelayLogQueueHealth struct {
	// PersistenceEnabled 为 false 表示这是主动设置(只在内存里留最近若干条), 不是故障。
	PersistenceEnabled bool `json:"persistence_enabled"`
	// PendingCount / PendingCapacity 是当前待落库条数与队列上限。
	PendingCount    int `json:"pending_count"`
	PendingCapacity int `json:"pending_capacity"`
	// DroppedTotal 是落库跟不上时被丢掉的条数(丢最旧), 只增不减。
	DroppedTotal int64 `json:"dropped_total"`
	// WriteFailureTotal / LastWriteFailureAt 是落库失败次数与最近失败时间(Unix 秒, 0 表示本次进程内没失败过)。
	WriteFailureTotal  int64 `json:"write_failure_total"`
	LastWriteFailureAt int64 `json:"last_write_failure_at"`
}

// RelayLogQueueHealthGet 读取上面的快照。只读内存计数, 不碰 DB、不触发落库,
// 因此可以在管理页随便轮询。
func RelayLogQueueHealthGet() (RelayLogQueueHealth, error) {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return RelayLogQueueHealth{}, err
	}

	relayLogCacheLock.Lock()
	health := RelayLogQueueHealth{
		PersistenceEnabled: enabled,
		PendingCount:       len(relayLogCache),
		DroppedTotal:       relayLogPendingDropped,
		WriteFailureTotal:  relayLogWriteFailures,
		LastWriteFailureAt: relayLogLastWriteFailureAt,
	}
	relayLogCacheLock.Unlock()

	// 关掉持久化时队列按 relayLogMaxSizeNoDB 走"只留最近几条"的裁剪, 上限不是待落库上限。
	if enabled {
		health.PendingCapacity = relayLogPendingMaxSize
	} else {
		health.PendingCapacity = relayLogMaxSizeNoDB
	}
	return health, nil
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
		// 非摘要导出（include_content=1，仅管理员）必须真的把那两列取出来，
		// 否则导出的 NDJSON 里正文恒为空，而界面承诺的是"完整正文"。
		// 复制一份 scope 而不是就地把调用方的改掉：RelayLogStreamExportNDJSON
		// 的 scope 可能被上层复用，就地修改会留下隐蔽副作用。
		exportScope := model.RelayLogScope{}
		if scope != nil {
			exportScope = *scope
		}
		exportScope.IncludeContent = !userSummary
		logs, err := RelayLogList(ctx, startTime, endTime, page, pageSize, &exportScope)
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
	// 导出「完整正文」时才取那两列；列表页照旧 omitting，避免把几十 KB/行的正文
	// 拉进内存又立刻清掉。见 model.RelayLogScope.IncludeContent 的说明。
	keepContent := scope != nil && scope.IncludeContent
	contentOmit := []string{"request_content", "response_content", "upstream_request_body", "model_audit_report"}
	if keepContent {
		contentOmit = []string{"model_audit_report"}
	}
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

	// windowed: 大 limit 分支已经直接算出了这一页(见 relayLogFetchWindow), 下面不再合并/排序/切片。
	windowed := false

	// 如果启用了日志保存，读取足够覆盖当前页的 DB 日志，再与内存缓存合并去重后统一分页。
	// 不能先按 cacheCount 偏移 DB；刷新/flush 后 cacheCount 会变，页面上的旧日志就会“消失”。
	if enabled {
		dbLimit := offset + pageSize + len(cachedLogs)
		if dbLimit < pageSize {
			dbLimit = pageSize
		}
		if dbLimit > 0 {
			if dbLimit <= relayLogTwoPhaseMaxIDs {
				// Two-phase read for interactive pages: first pluck ONLY the ids that match the
				// filter (ORDER+LIMIT), then fetch the full rows for just those ids. A
				// severity-filtered list used to WHERE on TRIM(error)/total_attempts, which sits
				// after the big TEXT bodies, so the walk pulled overflow pages for every row it
				// skipped — the ~64s severity=success page. The persisted severity column (and
				// idx_relay_logs_severity) lets that filter stay on the index; plucking id only
				// still skips body reads for whatever the planner does have to visit. The
				// full-row fetch touches just the matching rows. Order is reapplied by the
				// caller's stable sort below, so the id-IN fetch need not preserve it. Guarded
				// by relayLogTwoPhaseMaxIDs so the IN(...) list never approaches SQLite's
				// bound-parameter cap.
				// Phase 1 (pluck ids) + phase 2 (fetch rows) run in ONE read transaction so they
				// see a single consistent snapshot; without it a concurrent delete between them
				// could underfill the page (and make an export loop mistake the short page for EOF).
				txErr := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					idQuery := tx.Model(&model.RelayLog{})
					if hasTimeFilter {
						idQuery = idQuery.Where("time >= ? AND time <= ?", *startTime, *endTime)
					}
					idQuery = relayLogApplyScopeBase(idQuery, scope)
					// 搜索时: 先让投影表按关键词取一批 id, 再回主表按 id 取行。
					//
					// 三条实测事实决定了这个写法(2 万行, 命中 1/10):
					//   主表 LIKE + LIMIT 20            命中 1.5ms / 无命中 261ms
					//   窄表 LIKE + LIMIT 20            命中 1.4ms / 无命中  98ms
					//   主表 id IN (窄表子查询) + LIMIT   85ms(两种场景都慢)
					//
					// 结论: 慢的不是"主表"而是"扫多少行"。带 LIMIT 的 LIKE 命中就停,
					// 所以命中场景两者一样快; 真正省的是**搜不到时**少扫 2.7 倍。
					// 而 id IN (子查询) 会被 SQLite 对每个候选行重跑子查询, 必须避开。
					//
					// 因此: 把 LIKE 放进带 ORDER+LIMIT 的投影表查询(能早停),
					// 再把它给出的 id 作为普通 IN 列表交给主表。
					var ids []int64
					// 按 id 排序而不是 time: id 由 snowflake.GenerateID 发号, 本身就是严格单调
					// 递增的毫秒时间戳, 而 time 只有秒级精度。同一秒内 time 相等会让排序退化,
					// 快速失败的请求(几百毫秒就结束)拿到连续大 id 后会整批压在慢成功请求上方。
					search := ""
					if scope != nil {
						search = strings.TrimSpace(scope.Search)
					}
					if search != "" && relayLogSearchReady() {
						newIDQuery := func() *gorm.DB {
							q := tx.Model(&model.RelayLog{})
							if hasTimeFilter {
								q = q.Where("time >= ? AND time <= ?", *startTime, *endTime)
							}
							return relayLogApplyScopeBase(q, scope)
						}
						found, err := relayLogSearchTopIDs(ctx, newIDQuery, search, dbLimit)
						if err != nil {
							return err
						}
						ids = found
					} else {
						// 没有搜索词, 或投影表不可用(迁移未完成/被删): 搜索条件在
						// 这两种情况下都必须仍然生效 —— 早先这里漏了 else 里的搜索,
						// 导致"投影表一缺, 搜索就被静默忽略、返回全部日志"。
						if search != "" {
							idQuery = relayLogApplySearchOnMainTable(idQuery, search)
						}
						if err := idQuery.Order("id DESC").Limit(dbLimit).Pluck("id", &ids).Error; err != nil {
							return err
						}
					}
					if len(ids) > 0 {
						var dbLogs []model.RelayLog
						if err := tx.Where("id IN ?", ids).Omit(contentOmit...).Find(&dbLogs).Error; err != nil {
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
				// 大 limit（导出 / 深翻页）不能用 id IN(...) 两段式：id 列表会超过
				// SQLite 的参数上限，所以走单查询。
				//
				// 🔴 但**搜索条件必须照样生效**。这里曾经直接 `query.Order(...).Find()`，
				// 而 relayLogApplyScopeBase 只管时间和用户范围、不管搜索词 —— 于是
				// "搜索结果页点导出"导出的是**全库日志**：用户以为拿到的是自己筛出来的
				// 那几十条，实际拿到全部记录（含别人的、含他没筛的时间段）。这既是数据
				// 泄露，也让导出功能本身失去意义。
				//
				// 判据与交互页保持同一套：能用投影窄表就先算命中 id，再拿 id 集合过滤主表；
				// 投影表不可用（迁移未完成/被删）时退回主表 LIKE。
				//
				// 这里不能像以前那样先把投影表的全部命中 id Pluck 出来再 `id IN (列表)`:
				// 宽泛的搜索词(比如 "gpt")在大库上命中几十万行, 列表一长就超过 SQLite
				// 的绑定参数上限(32766), 导出直接报 "too many SQL variables"。
				// 与计数共用 relayLogApplySearch: 投影表走子查询, 回填未完成时游标之后
				// 那段补主表 LIKE, 投影表不可用时整段主表 LIKE。
				build := func(base *gorm.DB) *gorm.DB {
					q := base.Model(&model.RelayLog{})
					if hasTimeFilter {
						q = q.Where("time >= ? AND time <= ?", *startTime, *endTime)
					}
					q = relayLogApplyScopeBase(q, scope)
					if scope != nil && strings.TrimSpace(scope.Search) != "" {
						q = relayLogApplySearch(q, scope.Search)
					}
					return q
				}
				windowRows, err := relayLogFetchWindow(ctx, build, result, dbLimit, offset, pageSize, contentOmit)
				if err != nil {
					return nil, err
				}
				result = windowRows
				windowed = true
			}
		}
	}

	if !windowed {
		result = relayLogDedupe(result)
		sort.SliceStable(result, relayLogPageLess(result))
		if offset >= len(result) {
			result = nil
		} else {
			end := offset + pageSize
			if end > len(result) {
				end = len(result)
			}
			result = result[offset:end]
		}
	}
	if !keepContent {
		for i := range result {
			result[i] = RelayLogOmitContent(result[i])
		}
	}
	if scope != nil && scope.Redact {
		for i := range result {
			result[i] = RelayLogRedact(result[i])
		}
	}

	return result, nil
}

// relayLogPageLess 是列表的最终排序: time 降序, 同秒内 id 降序。
func relayLogPageLess(logs []model.RelayLog) func(i, j int) bool {
	return func(i, j int) bool {
		if logs[i].Time != logs[j].Time {
			return logs[i].Time > logs[j].Time
		}
		return logs[i].ID > logs[j].ID
	}
}

// relayLogFetchWindow 是大 limit(深翻页 / 导出)分支: 结果与"把前 dbLimit 行整行读出来、
// 和缓存合并去重、排序、切出 [offset, offset+pageSize)"逐行一致, 但只为**这一页**读整行。
//
// 以前这里把前 dbLimit 行的全部列(除正文外还有 attempts / passive_* 等 JSON)读进内存再切片:
// 16 万行的库上翻到第 8000 页(每页 20)要 3.9s、分配 1.3GB, 而页面只要其中 20 行。
// 现在先只取 (id, time) 这两列定位窗口, 再按 id 取这一页的整行。排序键、去重规则
// (缓存在前、缓存副本优先、id 为 0 不去重)和稳定排序的初始顺序都与原来相同。
//
// 两步在同一个读事务里: 中间若有并发删除, 页面不会少行(导出循环会把短页当成结尾)。
func relayLogFetchWindow(ctx context.Context, build func(*gorm.DB) *gorm.DB, cached []model.RelayLog, dbLimit, offset, pageSize int, omit []string) ([]model.RelayLog, error) {
	type pageKey struct {
		ID   int64
		Time int64
	}
	type entry struct {
		id, time int64
		cacheIdx int // >= 0: 来自缓存; -1: 来自数据库
	}
	var out []model.RelayLog
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var keys []pageKey
		if err := build(tx).Select("id, time").Order("id DESC").Limit(dbLimit).Scan(&keys).Error; err != nil {
			return err
		}
		entries := make([]entry, 0, len(cached)+len(keys))
		seen := make(map[int64]bool, len(cached)+len(keys))
		keep := func(id int64) bool {
			if id == 0 {
				return true
			}
			if seen[id] {
				return false
			}
			seen[id] = true
			return true
		}
		for i := range cached {
			if keep(cached[i].ID) {
				entries = append(entries, entry{id: cached[i].ID, time: cached[i].Time, cacheIdx: i})
			}
		}
		for _, k := range keys {
			if keep(k.ID) {
				entries = append(entries, entry{id: k.ID, time: k.Time, cacheIdx: -1})
			}
		}
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].time != entries[j].time {
				return entries[i].time > entries[j].time
			}
			return entries[i].id > entries[j].id
		})
		if offset >= len(entries) {
			return nil
		}
		end := offset + pageSize
		if end > len(entries) {
			end = len(entries)
		}
		window := entries[offset:end]

		ids := make([]int64, 0, len(window))
		for _, e := range window {
			if e.cacheIdx < 0 {
				ids = append(ids, e.id)
			}
		}
		rows := make(map[int64]model.RelayLog, len(ids))
		for start := 0; start < len(ids); start += relayLogTwoPhaseMaxIDs {
			stop := start + relayLogTwoPhaseMaxIDs
			if stop > len(ids) {
				stop = len(ids)
			}
			var chunk []model.RelayLog
			if err := tx.Where("id IN ?", ids[start:stop]).Omit(omit...).Find(&chunk).Error; err != nil {
				return err
			}
			for _, l := range chunk {
				rows[l.ID] = l
			}
		}
		out = make([]model.RelayLog, 0, len(window))
		for _, e := range window {
			if e.cacheIdx >= 0 {
				out = append(out, cached[e.cacheIdx])
				continue
			}
			if l, ok := rows[e.id]; ok {
				out = append(out, l)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
// independent of the current page.
//
// DB 路径一条查询算齐三桶，谓词与列表过滤共用 relayLogSeverityBucketSQL：
//   - 行都已回填：只碰 severity。SQLite 会选 idx_relay_logs_time_severity 做覆盖
//     扫描，不读正文溢出页；
//   - 还有 severity=0：已回填的行用列，没回填的行用旧的 TRIM 条件，回填中途
//     数字与改列之前一致；
//
// 内存路径仍按 relayLogSeverityValue 归桶。
//
// 每条日志恰好归入一个桶，因此 total 恒等于 success+warn+error（两条路径都成立）。
func RelayLogSeverityCounts(ctx context.Context, startTime, endTime *int, scope *model.RelayLogScope) (model.RelayLogSeverityCounts, error) {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return model.RelayLogSeverityCounts{}, err
	}

	hasTimeFilter := startTime != nil && endTime != nil

	if enabled {
		query := db.GetDB().WithContext(ctx).Model(&model.RelayLog{})
		// 一次探测同时喂给 WHERE（scope 带了 severity 时）和 SUM(CASE)。
		// 不能让 ApplyScope 再探一次：回填刚好跑完时两边会一个走列、一个走 TRIM。
		// scope 没带 severity 也要探——SELECT 的三桶表达式用得着，列表路径则不用。
		mode := relayLogSeveritySQLModeOf(query)
		if hasTimeFilter {
			query = query.Where("time >= ? AND time <= ?", *startTime, *endTime)
		}
		query = relayLogApplyScopeMode(query, scope, mode)

		// 三桶与列表的 severity 过滤共用同一个谓词，不会各写一套。
		// 不写 INDEXED BY：fast 路径只引用 time/severity 时，SQLite 自己会选
		// idx_relay_logs_time_severity 做覆盖扫描（见 EXPLAIN 测试）。钉死这条
		// 索引会让「搜索 id IN (...)」的计数也被迫按时间扫一遍。
		// fast / hybrid 下每个 CASE 的 WHEN 互斥且并集为全集，SUM 即 total。
		sel, args := relayLogSeverityCountSelect(mode)
		var row struct {
			Total        int64
			SuccessError int64
			Warn         int64
			SuccessOK    int64
		}
		err := query.Select(sel, args...).Scan(&row).Error
		if err != nil {
			return model.RelayLogSeverityCounts{}, err
		}
		return model.RelayLogSeverityCounts{
			Total:   row.Total,
			Success: row.SuccessOK,
			Warn:    row.Warn,
			Error:   row.SuccessError,
		}, nil
	}

	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()

	var counts model.RelayLogSeverityCounts
	for _, relayLog := range relayLogCache {
		if !relayLogMatchScope(relayLog, scope) {
			continue
		}
		if hasTimeFilter && (relayLog.Time < int64(*startTime) || relayLog.Time > int64(*endTime)) {
			continue
		}
		counts.Total++
		switch relayLogSeverityValue(relayLog) {
		case "success":
			counts.Success++
		case "warn":
			counts.Warn++
		default:
			counts.Error++
		}
	}
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
		if err := query.Order("id ASC").Limit(limit+len(result)).Omit("request_content", "response_content", "model_audit_report").Find(&dbLogs).Error; err != nil {
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
	for i := range result {
		result[i] = RelayLogOmitContent(result[i])
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
	// 清空/按 scope 删除前, 先用同一套条件把投影表对应行删掉。
	// 条件里只有 id/channel_id 之类投影表也有的列时才能直接下推; 含 time 的
	// scope 条件先查 id(与上面按时间清理同一策略)。
	var clearIDs []int64
	if err := relayLogApplyScope(db.GetDB().WithContext(ctx).Model(&model.RelayLog{}), scope).
		Pluck("id", &clearIDs).Error; err == nil {
		// 投影删失败就整条中止，绝不继续删主表：否则这些投影行会变成永久孤儿，
		// 保留期与容量两条判据都只看主表，谁也管不到它们。
		// 清空是用户显式动作，失败要如实报错让人重试，而不是装作删干净了。
		if err := relayLogSearchIndexDeleteByIDs(db.GetDB().WithContext(ctx), clearIDs); err != nil {
			return fmt.Errorf("清空日志投影表失败，已中止以免产生无法清理的孤儿行: %w", err)
		}
		relayLogImageRefsDeleteByLogIDs(db.GetDB().WithContext(ctx), clearIDs)
	} else {
		// 条件无法下推时改走整表清空投影。
		//
		// 注意这里以前写作"整表重建"，实际是"清空"——投影行不会自动从主表补回来，
		// 只能等下次启动的迁移续填。所以清空失败必须中止，不能吞掉：
		// 万一投影清空了而主表只删了一部分，那部分主表的行就再也搜不到了。
		if err := db.GetDB().WithContext(ctx).Where("1 = 1").
			Delete(&model.RelayLogSearchIndex{}).Error; err != nil {
			return fmt.Errorf("清空日志投影表失败，已中止以免搜索结果与主表不一致: %w", err)
		}
	}

	if err := query.Delete(&model.RelayLog{}).Error; err != nil {
		return err
	}
	invalidateModelTelemetryCache()
	// 清空是用户想立刻腾出空间：没了引用的图片马上删（只给正在落库的留一分钟）。
	// 清扫开头会删掉指向已不存在日志的引用行，上面 Pluck 失败的分支也一样兜得住。
	relayLogImageSweepLogged(ctx, relayLogImageClearGrace)
	return nil
}

func RelayLogRedact(relayLog model.RelayLog) model.RelayLog {
	relayLog.RequestContent = ""
	relayLog.ResponseContent = ""
	// UpstreamRequestBody 是完整出站请求体（含 system/messages/tools），
	// 与 RequestContent 同一敏感级别——受限 key / 非管理员读取时必须一起清掉。
	relayLog.UpstreamRequestBody = ""
	relayLog.RequestIP = ""
	relayLog.Attempts = nil
	if relayLog.Error != "" {
		relayLog.Error = "[redacted]"
	}
	return relayLog
}

// RelayLogOmitContent clears bulky request/response bodies for list/stream
// payloads. Interactive pages only need metadata; full bodies load via
// RelayLogGetByID when an admin opens a detail dialog.
func RelayLogOmitContent(relayLog model.RelayLog) model.RelayLog {
	relayLog.RequestContent = ""
	relayLog.ResponseContent = ""
	// 真实出站请求体与 RequestContent 同级体量（上限 128KB）：列表/实时流只给
	// 元数据，正文留给详情弹窗，否则列表每行最多多背 128KB、导出可达 GB 级。
	relayLog.UpstreamRequestBody = ""
	return relayLog
}

// RelayLogGetByID loads one relay log by id for the admin detail dialog.
func RelayLogGetByID(ctx context.Context, id int64, scope *model.RelayLogScope) (*model.RelayLog, error) {
	if id == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	relayLogCacheLock.Lock()
	for i := range relayLogCache {
		if relayLogCache[i].ID == id {
			item := relayLogCache[i]
			relayLogCacheLock.Unlock()
			if !relayLogMatchScope(item, scope) {
				return nil, gorm.ErrRecordNotFound
			}
			if scope != nil && scope.Redact {
				item = RelayLogRedact(item)
			}
			return &item, nil
		}
	}
	relayLogCacheLock.Unlock()
	var item model.RelayLog
	query := db.GetDB().WithContext(ctx).Where("id = ?", id)
	query = relayLogApplyScope(query, scope)
	if err := query.First(&item).Error; err != nil {
		return nil, err
	}
	if scope != nil && scope.Redact {
		item = RelayLogRedact(item)
	}
	return &item, nil
}

// RelayLogMarkAudited 把一次行为审计的结论钉回触发它的那一条日志行(先到先得)。
// 实现为单条 UPDATE, WHERE 限制 verdict 仍为空(NULL/" = 未检测): 被影响行数为 0 说明
// 已有更早的结论占了位(或行不存在), 视为"已有结论，不覆盖"，返回 nil 而不是报错。
// 语义与 behavior.Verdict 对齐: none/unknown/low/medium/high; reportJSON 是 behavior.ViewReport 的 JSON。
func RelayLogMarkAudited(ctx context.Context, logID int64, verdict string, score, findingN int, trigger, reportJSON string) error {
	if logID <= 0 {
		return nil
	}
	result := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).
		Where("id = ? AND (model_audit_verdict IS NULL OR model_audit_verdict = '')", logID).
		Updates(map[string]any{
			"model_audit_verdict":   verdict,
			"model_audit_score":     score,
			"model_audit_finding_n": findingN,
			"model_audit_trigger":   trigger,
			"model_audit_report":    reportJSON,
		})
	if result.Error != nil {
		return result.Error
	}
	// 库里改了，内存缓存里的同一行也必须跟着改 —— 否则详情接口会返回"没检测过"。
	//
	// 🔴 为什么这不是可有可无的优化：RelayLogGetByID **先扫 relayLogCache，命中就
	// 直接返回**（那条路径根本不碰数据库）。一段检测流程的实际顺序是
	// 「日志刚产生 → 进缓存 → 检测 → MarkAudited 写库」，此时这行**必然**还在缓存里，
	// 于是详情接口拿到的是没有 verdict、没有 report 的旧结构。
	// 用户看到的就是：点了检测，转圈结束，弹窗里徽标和明细还是空的；
	// 要等这行被挤出缓存（默认上限内可能几小时）或进程重启才会出现。
	//
	// 这里只在 RowsAffected>0 时同步：0 表示"库里已有更早的结论占位"，此时以库里
	// 的旧结论为准，缓存不能拿新值去覆盖它 —— 那会破坏"先到先得"语义。
	if result.RowsAffected > 0 {
		relayLogApplyAuditToCache(logID, verdict, score, findingN, trigger, reportJSON)
	}
	return nil
}

// relayLogApplyAuditToCache 把一次检测结论同步到内存缓存里的对应行。
//
// 只动缓存里那一份副本，不碰数据库（调用方已经写过了）。找不到就什么都不做：
// 说明这行不在缓存里，下次读取自然会走数据库拿到新值。
func relayLogApplyAuditToCache(logID int64, verdict string, score, findingN int, trigger, reportJSON string) {
	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	for i := range relayLogCache {
		if relayLogCache[i].ID != logID {
			continue
		}
		relayLogCache[i].ModelAuditVerdict = verdict
		relayLogCache[i].ModelAuditScore = score
		relayLogCache[i].ModelAuditFindingN = findingN
		relayLogCache[i].ModelAuditTrigger = trigger
		relayLogCache[i].ModelAuditReport = reportJSON
		return
	}
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
		RequestUserAgent:  relayLog.RequestUserAgent,
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
		BaseInputPrice:     relayLog.BaseInputPrice,
		BaseOutputPrice:    relayLog.BaseOutputPrice,
	}
}

// relayLogErrorSQLCond is the legacy SQL predicate for an "error" severity log.
// It is only used for rows whose persisted severity is still 0 (backfill has not
// reached them). Classified rows — and every new write — use
// model.ClassifyRelayLogSeverity instead, which is Go's TrimSpace and matches
// the web getRelayLogSeverity. SQL TRIM strips spaces only, so this predicate
// is NOT identical to TrimSpace for tab/newline; that gap closes as each row
// is backfilled, and it never applied to strings the relay actually stores
// (those already passed TrimSpace on the way in).
// COALESCE guards nullable columns: NOT (NULL OR ...) is NULL, which would
// silently drop such rows from the warn/success filters and undercount them.
const relayLogErrorSQLCond = "(TRIM(COALESCE(error,'')) <> '' OR TRIM(COALESCE(error_code,'')) <> '' OR COALESCE(error_status,0) >= 400)"

// relayLogIsError reports whether a log counts as an error (the request failed).
func relayLogIsError(relayLog model.RelayLog) bool {
	return model.ClassifyRelayLogSeverity(relayLog) == model.RelayLogSeverityError
}

// relayLogSeverityValue classifies a log as "error" / "warn" / "success".
//   - error:   the request failed.
//   - warn:    it ultimately succeeded but took more than one channel attempt
//     (a retry/failover happened — TotalAttempts counts real attempts, i.e.
//     len(attempts) minus race losers marked AttemptMsgRacedOut).
//   - success: first-attempt success.
//
// Mirror of the web getRelayLogSeverity rule so client display and server filter
// agree. Every log maps to exactly one bucket, so the three counts sum to total.
func relayLogSeverityValue(relayLog model.RelayLog) string {
	return model.RelayLogSeverityLabel(model.ClassifyRelayLogSeverity(relayLog))
}

// RelayLogSeverityValue is the shared success/warn/error bucket used by list,
// count, cache, and the live SSE tail so a streamed row cannot bypass the
// filter the page asked for.
func RelayLogSeverityValue(relayLog model.RelayLog) string {
	return relayLogSeverityValue(relayLog)
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
	if scope.Provider != "" && !relayLogProviderMatches(relayLog, scope.Provider) {
		return false
	}
	if scope.Model != "" && !relayLogModelMatches(relayLog, scope.Model) {
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
	// 三态: nil 的行(上游没声明模型名)两个桶都不进, 不是"一致"也不是"不一致"。
	if scope.UpstreamModelMismatch != nil {
		if relayLog.UpstreamModelMismatch == nil || *relayLog.UpstreamModelMismatch != *scope.UpstreamModelMismatch {
			return false
		}
	}
	if scope.Search != "" && !relayLogSearchMatches(relayLog, scope.Search) {
		return false
	}
	return true
}

func relayLogSearchMatches(relayLog model.RelayLog, search string) bool {
	q := strings.ToLower(strings.TrimSpace(search))
	if q == "" {
		return true
	}

	if num, err := strconv.ParseInt(q, 10, 64); err == nil && num > 0 {
		if relayLog.ID == num || int64(relayLog.ChannelId) == num {
			return true
		}
	}

	if strings.Contains(strings.ToLower(relayLog.UserName), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.RequestAPIKeyName), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.RequestModelName), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.ActualModelName), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.UpstreamResponseModel), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.ChannelName), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.RequestEndpoint), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.RequestPath), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.SessionKey), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.Error), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.ErrorCode), q) {
		return true
	}
	if strings.Contains(strings.ToLower(relayLog.RequestUserAgent), q) {
		return true
	}
	return false
}

// relayLogModelMatches checks if the requested model or actual model equals the filter model (case-insensitive).
func relayLogModelMatches(relayLog model.RelayLog, filterModel string) bool {
	target := strings.ToLower(strings.TrimSpace(filterModel))
	if target == "" {
		return true
	}
	return strings.ToLower(strings.TrimSpace(relayLog.RequestModelName)) == target ||
		strings.ToLower(strings.TrimSpace(relayLog.ActualModelName)) == target
}

// ProviderTaxonomyPrefixes maps unified provider IDs to their canonical model name prefix/substring markers.
// Kept in sync with internal/price.Provider and web/src/lib/model-icons.tsx.
var ProviderTaxonomyPrefixes = map[string][]string{
	"openai":     {"gpt", "o1", "o3", "o4", "chatgpt", "text-embedding", "dall-e", "whisper", "tts"},
	"anthropic":  {"claude"},
	"google":     {"gemini", "gemma", "palm"},
	"deepseek":   {"deepseek"},
	"xai":        {"grok"},
	"alibaba":    {"qwen", "qwq", "wanx"},
	"zhipuai":    {"glm", "chatglm", "zhipu", "z-ai", "ox-", "ox_", "oxalpha", "niulai"},
	"minimax":    {"minimax", "abab"},
	"moonshotai": {"moonshot", "kimi"},
	"mistral":    {"mistral", "mixtral", "codestral", "pixtral"},
	"meta":       {"llama", "meta-llama"},
	"bytedance":  {"doubao", "skylark"},
	"baidu":      {"ernie", "wenxin"},
	"tencent":    {"hunyuan"},
	"cohere":     {"cohere", "command"},
	"microsoft":  {"phi-", "wizardlm"},
}

// IsKnownProvider returns true if the given provider ID is recognized in the taxonomy.
func IsKnownProvider(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return false
	}
	_, ok := ProviderTaxonomyPrefixes[p]
	return ok
}

// normalizeModelForTaxonomy returns the path-prefix (if any) and basename for a model name.
// E.g. "openai/gpt-4o" -> pathProvider: "openai", baseModel: "gpt-4o"
// E.g. "claude-3-5-sonnet" -> pathProvider: "", baseModel: "claude-3-5-sonnet"
func normalizeModelForTaxonomy(name string) (pathProvider string, baseModel string) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return "", ""
	}
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		pathProvider = lower[:idx]
		if idx+1 < len(lower) {
			baseModel = lower[idx+1:]
		}
		return pathProvider, baseModel
	}
	return "", lower
}

// modelMatchesProviderPrefixes returns true if modelName matches the given provider taxonomy.
// It checks both the canonical provider prefix/path and the basename prefixes.
// Unknown providers return false.
func modelMatchesProviderPrefixes(modelName, provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return false
	}
	prefixes, ok := ProviderTaxonomyPrefixes[p]
	if !ok {
		return false
	}

	pathProv, baseModel := normalizeModelForTaxonomy(modelName)
	if baseModel == "" && pathProv == "" {
		return false
	}

	// 1. If model has a path provider prefix (e.g. "openai/gpt-4" or "meta-llama/Llama-3"),
	// match if path provider equals or ends with the provider key.
	if pathProv != "" {
		if pathProv == p || strings.HasSuffix(pathProv, "/"+p) || strings.HasPrefix(pathProv, p+"-") || strings.HasPrefix(pathProv, p+"_") {
			return true
		}
		if p == "meta" && (pathProv == "meta-llama" || strings.HasPrefix(pathProv, "meta-")) {
			return true
		}
	}

	// 2. Match basename against controlled prefixes.
	for _, prefix := range prefixes {
		if strings.HasPrefix(baseModel, prefix) {
			return true
		}
	}
	return false
}

// RelayLogProviderMatches checks if either the request model or actual model belongs to the given provider taxonomy.
func RelayLogProviderMatches(relayLog model.RelayLog, provider string) bool {
	if strings.TrimSpace(provider) == "" {
		return true
	}
	return modelMatchesProviderPrefixes(relayLog.ActualModelName, provider) ||
		modelMatchesProviderPrefixes(relayLog.RequestModelName, provider)
}

func relayLogProviderMatches(relayLog model.RelayLog, provider string) bool {
	return RelayLogProviderMatches(relayLog, provider)
}

// relayLogEndpointMatches reports whether a stored request_endpoint belongs to
// the requested endpoint family: an exact match, or a "<family>_<variant>" form.
// The relay stores variant endpoints as "<family>_<sub>" (e.g. gemini →
// gemini_generate_content / gemini_stream_generate_content, images →
// images_generations, videos → videos_poll), so the log filter matches by
// family rather than requiring the caller to know every stored variant.
// RelayLogEndpointMatches reports whether a stored request_endpoint belongs to
// the requested endpoint family: an exact match, or a "<family>_<variant>" form.
func RelayLogEndpointMatches(stored, filter string) bool {
	return relayLogEndpointMatches(stored, filter)
}

func relayLogEndpointMatches(stored, filter string) bool {
	return stored == filter || strings.HasPrefix(stored, filter+"_")
}

// escapeLogEndpointLike escapes LIKE metacharacters so an endpoint family prefix
// is matched literally (backslash is declared as the ESCAPE char in the query).
func escapeLogEndpointLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// relayLogModelFilterCols are the model-name columns referenced by the provider LIKE
// predicates. They are TRIMmed because normalizeModelForTaxonomy (the Go/web side of the
// same rule) applies TrimSpace before matching, so a stored name carrying padding
// (" gpt-4o ") is the openai provider on those paths; comparing the raw column made the
// identical row invisible to the DB path.
var relayLogModelFilterCols = []string{"LOWER(TRIM(actual_model_name))", "LOWER(TRIM(request_model_name))"}

func relayLogApplyScopeRaw(query *gorm.DB, scope *model.RelayLogScope, mode relayLogSeveritySQLMode) *gorm.DB {
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
	if scope.Model != "" {
		m := strings.ToLower(strings.TrimSpace(scope.Model))
		// TRIM the columns too: relayLogModelMatches trims both sides before comparing,
		// so a row whose stored model name carries padding (" gpt-4o ") is a match on the
		// cache/SSE path. Comparing LOWER(col) here without TRIM made the same row
		// invisible to the DB path — a filter that "works until the row moves from the
		// in-memory cache into the database".
		query = query.Where("LOWER(TRIM(request_model_name)) = ? OR LOWER(TRIM(actual_model_name)) = ?", m, m)
	}
	if scope.Provider != "" {
		providerKey := strings.ToLower(strings.TrimSpace(scope.Provider))
		prefixes, ok := ProviderTaxonomyPrefixes[providerKey]
		if !ok {
			// Unknown provider: reject/match nothing.
			query = query.Where("1 = 0")
		} else {
			var conditions []string
			var args []interface{}

			// Model basename prefix conditions (e.g. "gpt%", "%/gpt%")
			for _, prefix := range prefixes {
				escaped := escapeLogEndpointLike(prefix)
				patternStart := escaped + "%"
				patternSlash := "%/" + escaped + "%"
				for _, col := range relayLogModelFilterCols {
					conditions = append(conditions, col+" LIKE ? ESCAPE '\\'", col+" LIKE ? ESCAPE '\\'")
					args = append(args, patternStart, patternSlash)
				}
			}

			// Provider path prefix conditions (e.g. "openai/%" or "openai-%/" or "openai_%/").
			// The trailing "/" is required: modelMatchesProviderPrefixes only accepts a
			// "<provider>-"/"<provider>_" token as a *path* prefix (the segment before a
			// slash), so a bare basename like "openai_gpt" is NOT the openai provider. The
			// meta special case below already spells the path form ("meta-%/%"); the generic
			// hyphen/underscore patterns used to omit it and over-matched bare basenames.
			// Kept in sync with modelMatchesProviderPrefixes (memory/SSE path).
			escapedProv := escapeLogEndpointLike(providerKey)
			provSlash := escapedProv + "/%"
			provSubSlash := "%/" + escapedProv + "/%"
			provHyphen := escapedProv + "-%/"
			provUnderscore := escapedProv + "_%/"
			for _, col := range relayLogModelFilterCols {
				conditions = append(conditions,
					col+" LIKE ? ESCAPE '\\'",
					col+" LIKE ? ESCAPE '\\'",
					col+" LIKE ? ESCAPE '\\'",
					col+" LIKE ? ESCAPE '\\'",
				)
				args = append(args, provSlash, provSubSlash, provHyphen, provUnderscore)
			}
			if providerKey == "meta" {
				for _, col := range relayLogModelFilterCols {
					conditions = append(conditions, col+" LIKE ? ESCAPE '\\'", col+" LIKE ? ESCAPE '\\'")
					args = append(args, "meta-llama/%", "meta-%/%")
				}
			}

			if len(conditions) > 0 {
				query = query.Where(strings.Join(conditions, " OR "), args...)
			}
		}
	}
	if scope.Severity != "" {
		expr, args := relayLogSeverityBucketSQL(mode, scope.Severity)
		query = query.Where(expr, args...)
	}
	if scope.RetriedOnly {
		// 不写 COALESCE(total_attempts,0) > 1: 那会把列变成表达式, 索引直接失效
		// (实测 10 万行 990ms → 有索引后 10ms 量级)。语义完全等价 ——
		// SQL 里 NULL > 1 为 NULL 即不匹配, 而 Go 侧 relayLogMatchScope 用的是
		// TotalAttempts <= 1 排除, NULL 读出来是 0, 同样不匹配。
		query = query.Where("total_attempts > 1")
	}
	if scope.HideModelTest {
		// Exclude channel-test probe endpoints ("model_test", "model_test_responses", …).
		// The underscore is escaped so it matches literally, not as a LIKE wildcard.
		query = query.Where(`request_endpoint NOT LIKE ? ESCAPE '\'`, `model\_test%`)
	}
	if scope.UpstreamModelMismatch != nil {
		// Explicit IS NOT NULL keeps the third state (upstream declared nothing) out of
		// both buckets — a plain `= ?` comparison would already drop NULLs, but stating it
		// makes the tri-state contract obvious to the next reader.
		query = query.Where("upstream_model_mismatch IS NOT NULL AND upstream_model_mismatch = ?", *scope.UpstreamModelMismatch)
	}
	return query
}

// relayLogApplyScope 应用全部 scope 条件, 含关键词搜索。
//
// 用于计数 / 导出 / 严重程度统计这类"本来就要全量扫"的路径:
// 它们没有 LIMIT 可早停, 直接让 SQL 用窄表子查询取交集最省。
func relayLogApplyScope(query *gorm.DB, scope *model.RelayLogScope) *gorm.DB {
	return relayLogApplyScopeMode(query, scope, relayLogSeveritySQLModeForScope(query, scope))
}

// relayLogApplyScopeMode 与 relayLogApplyScope 相同，但严重程度谓词用调用方已经
// 定好的 mode。计数把 WHERE 和 SUM(CASE) 放在同一条 SQL 里，两边必须是同一次
// 探测的结果，不能各探一次（回填刚好跑完时会一个走列、一个走 TRIM）。
func relayLogApplyScopeMode(query *gorm.DB, scope *model.RelayLogScope, mode relayLogSeveritySQLMode) *gorm.DB {
	query = relayLogApplyScopeRaw(query, scope, mode)
	if scope == nil {
		return query
	}
	return relayLogApplySearch(query, scope.Search)
}

// relayLogApplyScopeBase 只应用 scope 里那些"主表自身能判断"的条件, 不含关键词搜索。
//
// 为什么需要这个变体: 列表路径的搜索改用"窄表先取 id + 主表按 id 取行"的两步写法
// (见 RelayLogList), 如果这里再把搜索套一遍, 就变成"慢的 id IN (子查询) 再叠一次
// 快的两步查询"——两边重复干活, 实测反而比优化前更慢(97ms vs 1.5ms)。
// 计数/导出等没有 LIMIT、也无需早停的路径仍走 relayLogApplyScope。
func relayLogApplyScopeBase(query *gorm.DB, scope *model.RelayLogScope) *gorm.DB {
	return relayLogApplyScopeRaw(query, scope, relayLogSeveritySQLModeForScope(query, scope))
}

// relayLogSearchIndexTable 是搜索投影表名(见 model.RelayLogSearchIndex)。
const relayLogSearchIndexTable = "relay_log_search"

// relayLogApplySearch 把搜索词落到 SQL 上。
//
// 为什么不直接用子查询(id IN (SELECT id FROM relay_log_search ...)): 实测那样会把
// "命中就早停"的优势弄丢。
//
//	写法                          命中 10%%    无结果
//	主表 LIKE + LIMIT 20          1.1ms        616ms
//	子查询 id IN (窄表)            206ms        227ms
//	仅查窄表 + LIMIT 20            0.4ms        206ms
//
// 原因: 加 LIMIT 的查询找到够一页就停, 命中率高时只扫几百行; 而子查询必须先把
// 所有匹配 id 算出来(可能上万条)才能给外层用, 于是每次都走满全表。
//
// 所以这里选了"只查窄表"这条路: 扫描便宜(不碰 request_content/response_content),
// 又能带着 LIMIT 早停。唯一的代价是搜索命中的行不保证都在窄表里(迁移没跑完、
// 或历史缺行), 因此调用方在"窄表给不满一页"时会再用主表 LIKE 补一次。
// 最多两次查询, 且第二次只在真的缺行时发生。
const relayLogSearchLikeCond = "LOWER(user_name) LIKE ? ESCAPE '\\' OR LOWER(request_api_key_name) LIKE ? ESCAPE '\\' OR LOWER(request_model_name) LIKE ? ESCAPE '\\' OR LOWER(actual_model_name) LIKE ? ESCAPE '\\' OR LOWER(upstream_response_model) LIKE ? ESCAPE '\\' OR LOWER(channel_name) LIKE ? ESCAPE '\\' OR LOWER(request_endpoint) LIKE ? ESCAPE '\\' OR LOWER(request_path) LIKE ? ESCAPE '\\' OR LOWER(session_key) LIKE ? ESCAPE '\\' OR LOWER(error) LIKE ? ESCAPE '\\' OR LOWER(error_code) LIKE ? ESCAPE '\\' OR LOWER(request_user_agent) LIKE ? ESCAPE '\\'"

// relayLogSearchCond 返回 12 列 LIKE(数字时再加 id / channel_id 精确匹配)的条件与参数。
// 投影表与主表列名相同, 两边共用这一份, 保证两条路径的匹配规则逐字一致。
func relayLogSearchCond(trimmed string) (string, []any) {
	pattern := "%" + escapeLogEndpointLike(strings.ToLower(trimmed)) + "%"
	args := make([]any, 0, 14)
	cond := relayLogSearchLikeCond
	if num, err := strconv.ParseInt(trimmed, 10, 64); err == nil && num > 0 {
		// 数字既可能是日志 id 也可能是渠道号, 两者在投影表里都有列。
		cond = "id = ? OR channel_id = ? OR (" + relayLogSearchLikeCond + ")"
		args = append(args, num, int(num))
	}
	for i := 0; i < 12; i++ {
		args = append(args, pattern)
	}
	return cond, args
}

// relayLogSearchIndexQuery 返回"在投影表里按关键词匹配"的查询, 由调用方决定
// 取多少条、怎么排序。
func relayLogSearchIndexQuery(search string) *gorm.DB {
	cond, args := relayLogSearchCond(strings.TrimSpace(search))
	return db.GetDB().Table(relayLogSearchIndexTable).Where(cond, args...)
}

// relayLogSearchReady 报告投影表是否可用于加速。
func relayLogSearchReady() bool {
	return db.GetDB().Migrator().HasTable(relayLogSearchIndexTable)
}

// relayLogApplySearchOnMainTable 是退路(投影表不可用): 与历史行为逐字一致。
func relayLogApplySearchOnMainTable(query *gorm.DB, trimmed string) *gorm.DB {
	cond, args := relayLogSearchCond(trimmed)
	return query.Where(cond, args...)
}

// relayLogSearchMaxIndexRounds 是列表搜索在投影表上最多翻几轮候选。
//
// 投影表没有 user_id / time / severity 等列, 候选 id 要回主表按范围再过滤一次。
// 范围很窄时(普通用户只看自己的日志、选了时间段), 最新那一批命中可能全被过滤掉。
// 翻到这么多轮还凑不满一页, 剩下的直接交给主表 LIKE(带范围、带 LIMIT 早停)。
const relayLogSearchMaxIndexRounds = 8

// relayLogSearchTopIDs 返回满足搜索词 + 范围的最新 limit 个 id(降序)。
//
// 结果与"主表上 LIKE + 范围 + ORDER BY id DESC LIMIT limit"逐行一致, 只是尽量
// 让扫描落在窄的投影表上:
//
//  1. 投影表按关键词取一批候选(id 降序, 带 LIMIT 能早停), 回主表按范围过滤;
//     凑不满就以上一批最小 id 为界继续翻, 最多 relayLogSearchMaxIndexRounds 轮,
//     之后余下的区间交给主表 LIKE。修复前只取一轮: 最新那批命中若都属于别的用户/
//     别的时间段, 页面直接显示"没有匹配"。
//  2. 投影表回填未完成时, 它只对 id <= cursor 负责; 大于游标的那段再在主表上 LIKE
//     一次并合并。修复前只要投影表给出了 >0 个命中就只用它们, 回填期间只在主表里的
//     老日志被静默漏掉(补表那段时间新日志也会命中, 所以这是常态而非边角)。
func relayLogSearchTopIDs(ctx context.Context, newIDQuery func() *gorm.DB, search string, limit int) ([]int64, error) {
	if limit <= 0 {
		return nil, nil
	}
	complete, cursor := relayLogSearchIndexState(ctx)

	seen := make(map[int64]struct{}, limit)
	out := make([]int64, 0, limit)
	add := func(ids []int64) {
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}

	if !complete {
		var tail []int64
		tq := relayLogApplySearchOnMainTable(newIDQuery(), search)
		if cursor > 0 {
			tq = tq.Where("id > ?", cursor)
		}
		if err := tq.Order("id DESC").Limit(limit).Pluck("id", &tail).Error; err != nil {
			return nil, err
		}
		add(tail)
	}

	batch := limit
	if batch < 100 {
		batch = 100
	}
	if batch > relayLogTwoPhaseMaxIDs {
		batch = relayLogTwoPhaseMaxIDs
	}
	fromIndex := 0
	var before int64 // 0 = 不设上界
	for round := 0; fromIndex < limit; round++ {
		if round >= relayLogSearchMaxIndexRounds {
			// 投影表翻够了还凑不满: 余下的区间(id < before)整段交给主表 LIKE。
			var rest []int64
			if err := relayLogApplySearchOnMainTable(newIDQuery(), search).
				Where("id < ?", before).
				Order("id DESC").Limit(limit-fromIndex).Pluck("id", &rest).Error; err != nil {
				return nil, err
			}
			add(rest)
			break
		}
		q := relayLogSearchIndexQuery(search).Select("id")
		if before > 0 {
			q = q.Where("id < ?", before)
		}
		var cand []int64
		if err := q.Order("id DESC").Limit(batch).Pluck("id", &cand).Error; err != nil {
			return nil, err
		}
		if len(cand) == 0 {
			break
		}
		// 投影表给的是候选; time/severity/用户等条件只有主表有, 用 id 列表再过滤一次。
		var filtered []int64
		if err := newIDQuery().Where("id IN ?", cand).
			Order("id DESC").Limit(limit).Pluck("id", &filtered).Error; err != nil {
			return nil, err
		}
		fromIndex += len(filtered)
		add(filtered)
		before = cand[len(cand)-1]
		if len(cand) < batch {
			break // 投影表里已经没有更多命中
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// relayLogApplySearch 用于 Count / SeverityCounts / 导出这类"要全量结果"的场景:
// 这些地方本来就没有 LIMIT, 早停无从谈起, 直接走窄表取子查询更省。
//
// 投影表回填未完成时只信它 id <= cursor 的那段, 游标之后的行在主表上 LIKE。
// 修复前这里无条件 id IN (投影表子查询): 补表期间徽章/总数少算老日志。
func relayLogApplySearch(query *gorm.DB, search string) *gorm.DB {
	trimmed := strings.TrimSpace(search)
	if trimmed == "" {
		return query
	}
	if !relayLogSearchReady() {
		return relayLogApplySearchOnMainTable(query, trimmed)
	}
	ctx := context.Background()
	if query.Statement != nil && query.Statement.Context != nil {
		ctx = query.Statement.Context
	}
	sub := relayLogSearchIndexQuery(trimmed).Select("id")
	complete, cursor := relayLogSearchIndexState(ctx)
	if complete {
		return query.Where("id IN (?)", sub)
	}
	if cursor <= 0 {
		// 投影表对任何 id 都还不担保: 整段走主表 LIKE(与投影表缺失时同一条退路)。
		return relayLogApplySearchOnMainTable(query, trimmed)
	}
	cond, args := relayLogSearchCond(trimmed)
	return query.Where("id IN (?) OR (id > ? AND ("+cond+"))", append([]any{sub, cursor}, args...)...)
}
