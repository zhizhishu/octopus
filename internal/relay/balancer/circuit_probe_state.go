package balancer

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

)

// ── 熔断探测 · 状态与持久化 ────────────────────────────────────────────────
//
// 这里只放"记住什么"：排期状态、上游等待、真实流量时间戳、连续未恢复计数、
// 当日计数与历史环。判定逻辑在 circuit_probe_scheduler.go。

// probeEntryState 是一段冷却（generation）内的排期状态。generation 一变（新的熔断、
// 冷却被延后、hold 被延长、被重置）整条状态就作废：提前量只挑一次、每个理由的 skip
// 只记一次，都靠它。
type probeEntryState struct {
	generation string
	// due 是本段冷却挑定的探测时刻（= expiry - lead）。零值 = 还没挑（剩余 > 60s）。
	// 只在"剩余进入 [10s,60s]"时写一次，**不靠轮询推进**。
	due time.Time
	// probed 表示这一段冷却已经被领过（避免同代重复领）。
	probed bool
	// skipReasons 记录这一段冷却已经落过哪些 skip 理由（每个理由一条历史，不重复）。
	skipReasons map[string]struct{}
}

// probeWait 是上游要求的等待（Retry-After / 回落 429）。与运营商 hold 分开记：
// hold 的 rule 是人定的策略，wait 的 cause 是上游的口径，两者互不覆盖。
type probeWait struct {
	until time.Time
	cause string
}

// probeRun 是当前在飞的那一次探测。全局只有一条探测车道，所以只需要一个槽。
type probeRun struct {
	id         int64
	target     CircuitProbeTarget
	generation string
	manual     bool
	startedAt  time.Time
}

// probeManualRequest 是排队中的一次手动/审计探测。它把"准入时那一条"的身份一起
// 记下来（generation + writeSeq）：领的时候再比一次，陈旧的就直接出队，绝不发出去。
type probeManualRequest struct {
	target     CircuitProbeTarget
	generation string
	writeSeq   uint64
}

// circuitProbeScheduler 是进程内唯一的探测调度器。
type circuitProbeScheduler struct {
	mu sync.Mutex

	cfg CircuitProbeConfig
	cb  CircuitProbeCallbacks

	// entries 按目标键（与熔断键同构）记住当前冷却段的排期。
	entries map[string]*probeEntryState
	// waits 按 holdKey 的三种作用域记住上游等待。
	waits map[string]probeWait
	// manual 是运营商手动/审计探测队列（优先于自动车道）。
	manual []probeManualRequest
	// due 是本次 tick 算出来"已经到期、可以开跑"的候选（按固定顺序）。
	due []probeCandidate
	// trips 是 hold 行上的探测专用 trip 计数（见 circuit_probe_failure.go）。
	trips map[string]probeTripState

	day           string
	global        CircuitProbeTotals
	channelTotals map[int]CircuitProbeTotals
	history       map[int][]CircuitProbeHistoryRow

	run      *probeRun
	claimSeq int64
}

func newCircuitProbeScheduler() *circuitProbeScheduler {
	return &circuitProbeScheduler{
		entries:       make(map[string]*probeEntryState),
		waits:         make(map[string]probeWait),
		trips:         make(map[string]probeTripState),
		channelTotals: make(map[int]CircuitProbeTotals),
		history:       make(map[int][]CircuitProbeHistoryRow),
	}
}

// circuitProbe 是包级单例：检查点需要能读到它，接线层只需要 Configure + Tick。
var circuitProbe = newCircuitProbeScheduler()

// circuitProbeStreak 记录"连续多少次探测都没恢复"（按确切 channel:key:model）。
// 真实流量直接清零：用户流量在打，说明这个 tuple 不需要我们再探。
// 用 sync.Map 是为了让 BeginRuntimeAttempt 的热路径只付一次 Delete 的代价。
var circuitProbeStreak sync.Map // key: tuple key -> *atomic.Int64

// circuitProbeTraffic 记录真实流量的时间戳（BeginRuntimeAttempt 写入，探测不写）。
// 这份时间戳不进检查点：重启后重新观察，超过 probeTrafficRetention 就机会式丢弃。
var circuitProbeTraffic sync.Map // key: tuple key -> time.Time

// ResetCircuitProbe 丢掉全部探测状态（含注入的口径与回调）。生产代码不需要它，
// 接线层的重启/测试用它把内存清干净。
func ResetCircuitProbe() {
	circuitProbe = newCircuitProbeScheduler()
	circuitProbeStreak = sync.Map{}
	circuitProbeTraffic = sync.Map{}
}

// ConfigureCircuitProbe 刷新口径与回调。接线层每次 tick 前调一次即可（便宜）。
func ConfigureCircuitProbe(cfg CircuitProbeConfig, cb CircuitProbeCallbacks) {
	s := circuitProbe
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.cb = cb
}

// nowLocked 取当前时间。调用方必须持有 s.mu。
func (s *circuitProbeScheduler) nowLocked() time.Time {
	if s.cb.Now != nil {
		return s.cb.Now()
	}
	return time.Now()
}

// now 取当前时间。
func (s *circuitProbeScheduler) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nowLocked()
}

// configSnapshot 取一份口径 + 回调。回调本身是接线层的函数值，读出来之后在锁外调用，
// 避免接线层的查询反过来进本包时自锁。
func (s *circuitProbeScheduler) configSnapshot() (CircuitProbeConfig, CircuitProbeCallbacks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.cb
}

// ── 日切与计数 ───────────────────────────────────────────────────────────

// circuitProbeDayZone 是计数日界所在的时区：UTC+8 的零点翻篇。
var circuitProbeDayZone = time.FixedZone("UTC+8", 8*60*60)

// circuitProbeDay 返回 now 所在的 UTC+8 日历日。
func circuitProbeDay(now time.Time) string {
	return now.In(circuitProbeDayZone).Format("2006-01-02")
}

// rollDailyLocked 在跨过 UTC+8 零点时清零全局与各渠道的当日计数（历史环不清：
// 那是最近 20 次观测，不是当日额度）。
func (s *circuitProbeScheduler) rollDailyLocked(now time.Time) {
	day := circuitProbeDay(now)
	if s.day == day {
		return
	}
	s.day = day
	s.global = CircuitProbeTotals{}
	s.channelTotals = make(map[int]CircuitProbeTotals)
}

// countLocked 给渠道与全局同时记一笔。
func (s *circuitProbeScheduler) countLocked(channelID int, apply func(*CircuitProbeTotals)) {
	totals := s.channelTotals[channelID]
	apply(&totals)
	s.channelTotals[channelID] = totals
	apply(&s.global)
}

// cappedLocked 判断当日额度是否已经用尽（全局与渠道两道都算，手动探测也不豁免）。
// 渠道级覆写 DailyCap>0 时用覆写值，否则沿用全局 ChannelDailyCap。
func (s *circuitProbeScheduler) cappedLocked(cfg CircuitProbeConfig, overrides CircuitProbeChannelOverrides, channelID int) bool {
	if cfg.GlobalDailyCap > 0 && s.global.Used >= cfg.GlobalDailyCap {
		return true
	}
	channelCap := cfg.channelDailyCapFor(overrides)
	if channelCap > 0 && s.channelTotals[channelID].Used >= channelCap {
		return true
	}
	return false
}

// ── 上游等待 ─────────────────────────────────────────────────────────────

// circuitProbeWaitKeys 返回一次上游等待要盖住的作用域：确切 key+model，以及整把 key。
// 429 是 per-key 的限流信号，所以只到 key 为止，不冻住整个渠道的其它 key。
// keyID<=0 是渠道级信号：只盖渠道级那一条，不碰任何单 key。
func circuitProbeWaitKeys(channelID, keyID int, model string) []string {
	if keyID <= 0 {
		return []string{holdKey(channelID, 0, holdAnyModel)}
	}
	keys := make([]string, 0, 2)
	if strings.TrimSpace(model) != "" {
		keys = append(keys, holdKey(channelID, keyID, strings.TrimSpace(model)))
	}
	return append(keys, holdKey(channelID, keyID, holdAnyModel))
}

// noteCircuitProbeUpstreamWait 把**真实转发**撞到的上游等待转给探测车道。
//
// 为什么需要它：真实上游给的 Retry-After 从来只进 RecordRuntimeFailure 的软冷却，
// 探测调度看不见它就会在冷却期间继续对同一个 key 出题。这里把上游口径原样记进探测
// 等待表：
//   - 有 Retry-After：按原值记，**不设上限**（这是上游自己的口径，不是我们的猜测；
//     真实转发的软冷却仍然按既有的 boundedRetryAfterCooldown 收敛，两边互不影响）；
//   - 429 但没给 Retry-After（回落链路常见形态）：按真实转发那套**指数冷却**算
//     （5s 起步、随连续 429 倍增、封顶 maxRuntimeCooldown），所以可以超过固定的
//     probeFallback429Wait；成因记成 fallback_429，与 retry_after 分开看得见。
//
// 调用时不得持有调度器锁：ApplyCircuitProbeWait 自己会取 s.mu。
func noteCircuitProbeUpstreamWait(channelID, keyID int, modelName string, statusCode int, retryAfter time.Duration) {
	switch {
	case retryAfter > 0:
		ApplyCircuitProbeWait(channelID, keyID, modelName, CircuitProbeWaitCauseRetryAfter, retryAfter)
	case statusCode == http.StatusTooManyRequests:
		ApplyCircuitProbeWait(channelID, keyID, modelName, CircuitProbeWaitCauseFallback429, probeFallback429WaitFor(channelID, keyID, modelName))
	}
}

// probeFallback429WaitFor 算"429 但没 Retry-After"时探测车道要等多久。
// 用真实转发同一套指数冷却（runtimeCooldownForStatus，按该 key 当前连续失败数 +1）；
// 算不出来（<=0）才退回固定兜底值。
func probeFallback429WaitFor(channelID, keyID int, modelName string) time.Duration {
	failures := SnapshotKeyRuntime(channelID, keyID, modelName).ConsecutiveFailures + 1
	wait := runtimeCooldownForStatus(http.StatusTooManyRequests, failures)
	if wait <= 0 {
		return probeFallback429Wait
	}
	return wait
}

// ApplyCircuitProbeWait 记下上游要求的等待（Retry-After / 回落 429）。等待只延长不缩短，
// 并且原样保留成因（长等待也照记，不设上限——这是上游自己的口径，不是我们的猜测）。
func ApplyCircuitProbeWait(channelID, keyID int, model, cause string, d time.Duration) {
	if d <= 0 || channelID <= 0 {
		return
	}
	cause = strings.TrimSpace(cause)
	if cause == "" {
		cause = CircuitProbeWaitCauseRetryAfter
	}
	s := circuitProbe
	s.mu.Lock()
	defer s.mu.Unlock()
	until := s.nowLocked().Add(d)
	for _, key := range circuitProbeWaitKeys(channelID, keyID, model) {
		if existing, ok := s.waits[key]; ok && existing.until.After(until) {
			continue
		}
		s.waits[key] = probeWait{until: until, cause: cause}
	}
}

// waitLocked 取覆盖 channel/key/model 的最长一条上游等待（顺带清掉已过期的）。
func (s *circuitProbeScheduler) waitLocked(now time.Time, channelID, keyID int, model string) (probeWait, bool) {
	var longest probeWait
	found := false
	for _, key := range []string{
		holdKey(channelID, keyID, model),
		holdKey(channelID, keyID, holdAnyModel),
		holdKey(channelID, 0, holdAnyModel),
	} {
		wait, ok := s.waits[key]
		if !ok {
			continue
		}
		if !wait.until.After(now) {
			delete(s.waits, key)
			continue
		}
		if !found || wait.until.After(longest.until) {
			longest = wait
			found = true
		}
	}
	return longest, found
}

// pruneWaitsLocked 清掉已过期的上游等待，避免长期运行的进程攒垃圾。
func (s *circuitProbeScheduler) pruneWaitsLocked(now time.Time) {
	for key, wait := range s.waits {
		if !wait.until.After(now) {
			delete(s.waits, key)
		}
	}
}

// CircuitProbeWaitRemaining 返回覆盖 channel/key/model 的上游等待剩余时间与成因。
func CircuitProbeWaitRemaining(channelID, keyID int, model string) (time.Duration, string) {
	s := circuitProbe
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked()
	wait, ok := s.waitLocked(now, channelID, keyID, model)
	if !ok {
		return 0, ""
	}
	return wait.until.Sub(now), wait.cause
}

// ── 真实流量 ─────────────────────────────────────────────────────────────

// noteCircuitProbeRealTraffic 记下一次真实流量（真实转发路径，由 BeginRuntimeAttempt
// 调用）。它让该 tuple 在 probeTrafficSkipWindow 内不被自动探测，并清零连续未恢复计数。
func noteCircuitProbeRealTraffic(channelID, keyID int, modelName string, at time.Time) {
	if channelID <= 0 || keyID <= 0 {
		return
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return
	}
	tuple := circuitKey(channelID, keyID, modelName)
	circuitProbeTraffic.Store(tuple, at)
	circuitProbeStreak.Delete(tuple)
}

// NoteCircuitProbeRealTraffic 是 noteCircuitProbeRealTraffic 的导出形态：真实转发已经在
// BeginRuntimeAttempt 里自动记账，接线层只有在别的路径（比如自建 attempt）里需要时再调。
func NoteCircuitProbeRealTraffic(channelID, keyID int, modelName string) {
	noteCircuitProbeRealTraffic(channelID, keyID, modelName, time.Now())
}

// probeRealTrafficAt 取该 tuple 最近一次真实流量的时间。
func probeRealTrafficAt(tuple string) (time.Time, bool) {
	value, ok := circuitProbeTraffic.Load(tuple)
	if !ok {
		return time.Time{}, false
	}
	at, ok := value.(time.Time)
	return at, ok
}

// cleanupCircuitProbeTraffic 机会式丢掉落灰的真实流量时间戳（超过保留期）。
func cleanupCircuitProbeTraffic(now time.Time) {
	circuitProbeTraffic.Range(func(key, value any) bool {
		at, ok := value.(time.Time)
		if !ok || now.Sub(at) > probeTrafficRetention {
			circuitProbeTraffic.Delete(key)
		}
		return true
	})
}

// ── 连续未恢复计数 ───────────────────────────────────────────────────────

// probeStreakValue 取该 tuple 的连续未恢复次数。
func probeStreakValue(tuple string) int {
	value, ok := circuitProbeStreak.Load(tuple)
	if !ok {
		return 0
	}
	counter, ok := value.(*atomic.Int64)
	if !ok {
		return 0
	}
	return int(counter.Load())
}

// incProbeStreak 记一次未恢复，返回累计次数。
func incProbeStreak(tuple string) int {
	counter, _ := circuitProbeStreak.LoadOrStore(tuple, newProbeStreakCounter(0))
	return int(counter.(*atomic.Int64).Add(1))
}

// resetProbeStreak 清零该 tuple 的连续未恢复次数。
func resetProbeStreak(tuple string) {
	circuitProbeStreak.Delete(tuple)
}

func newProbeStreakCounter(value int64) *atomic.Int64 {
	counter := &atomic.Int64{}
	counter.Store(value)
	return counter
}

// ── 安全文本（历史落盘口径）─────────────────────────────────────────────

// probeSafeAnswer 只留答案里的第一段数字。上游正文（散文、错误体、可能的凭据）
// 一律不落盘：历史里能出现的只有"模型答了哪个整数"。
func probeSafeAnswer(answer string) string {
	var builder strings.Builder
	for _, r := range answer {
		if r >= '0' && r <= '9' {
			if builder.Len() >= probeAnswerLimit {
				break
			}
			builder.WriteRune(r)
			continue
		}
		if builder.Len() > 0 {
			break
		}
	}
	return builder.String()
}

// probePromptSymbols 是题面里允许出现的数学符号与常见中英标点（字母/数字/空白另算）。
const probePromptSymbols = "+-−×*·÷=/?.,:;!()[]？。，、：；！（）【】「」"

// probeSafePrompt 把一道题面收敛成"可以直接落盘展示的形态"。
//
// 题面是**第一方**生成的（modeltest 出题，接线层搬运），模板可能带中文、可能带可选
// 英文，运算符也可能是 + - − × * x 的任意一种。所以这里**不能**按算术符号白名单裁字：
// 那样会把新模板裁成半个数字甚至空白，历史里就看不到当时到底问了什么。
//
// 这里做的是"收敛 + 校验"，不是"裁剪"：
//  1. 先过一遍既有的 Secrets 脱敏（A 已经把所选 key 抹掉，这里只做纵深防御）；
//  2. 折叠空白，按 **rune** 截断到 probePromptLimit（不切半个字）；
//  3. 逐字校验：只允许字母/数字/空白与上面那一个小符号集。出现任何别的字符
//     （控制符、emoji、引号、@ $ # & | \ < > % ^ ~ 等）就**整条丢掉**，而不是
//     偷偷裁成半句话——历史里宁可不留题面，也不留半句可疑文本。
func probeSafePrompt(prompt string) string {
	prompt = strings.Join(strings.Fields(prompt), " ")
	if prompt == "" {
		return ""
	}
	runes := []rune(prompt)
	if len(runes) > probePromptLimit {
		runes = runes[:probePromptLimit]
	}
	for _, r := range runes {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsSpace(r):
			continue
		case strings.ContainsRune(probePromptSymbols, r):
			continue
		default:
			return ""
		}
	}
	return strings.TrimSpace(string(runes))
}

// ── 检查点（接在既有 circuit_state.json 上）──────────────────────────────
//
// 检查点版本与既有字段一个都不动：只在顶层加一个可选的 "probe" 对象。旧文件没有它，
// 读出来就是 nil（向后兼容）；新文件多出来的键对旧读取器只是未知键，encoding/json
// 默认忽略（向前兼容）。

type checkpointProbeCounters struct {
	Used      int `json:"used"`
	Recovered int `json:"recovered"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type checkpointProbeChannel struct {
	ChannelID int `json:"channel_id"`
	Used      int `json:"used"`
	Recovered int `json:"recovered"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type checkpointProbeStreak struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type checkpointProbeHistory struct {
	ChannelID  int    `json:"channel_id"`
	Timestamp  string `json:"timestamp"`
	Model      string `json:"model"`
	KeyID      int    `json:"key_id"`
	Prompt     string `json:"prompt"`
	Answer     string `json:"answer"`
	DurationMS int64  `json:"duration_ms"`
	Result     string `json:"result"`
}

type checkpointProbe struct {
	Day      string                   `json:"day,omitempty"`
	Global   checkpointProbeCounters  `json:"global"`
	Channels []checkpointProbeChannel `json:"channels,omitempty"`
	Streaks  []checkpointProbeStreak  `json:"streaks,omitempty"`
	History  []checkpointProbeHistory `json:"history,omitempty"`
}

// captureProbeCheckpoint 抓一份探测状态快照。没有任何值得留的东西时返回 nil，这样
// "内容没变就不重写"的既有判定不会被一个空对象打乱；快照里没有任何随当前时间变化的
// 字段（只有历史行自己的时间戳），所以两次无变化保存的字节是一定相同的。
func captureProbeCheckpoint() *checkpointProbe {
	s := circuitProbe
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := checkpointProbe{
		Day: s.day,
		Global: checkpointProbeCounters{
			Used:      s.global.Used,
			Recovered: s.global.Recovered,
			Failed:    s.global.Failed,
			Skipped:   s.global.Skipped,
		},
	}
	channelIDs := make([]int, 0, len(s.channelTotals))
	for channelID := range s.channelTotals {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Ints(channelIDs)
	for _, channelID := range channelIDs {
		totals := s.channelTotals[channelID]
		cp.Channels = append(cp.Channels, checkpointProbeChannel{
			ChannelID: channelID,
			Used:      totals.Used,
			Recovered: totals.Recovered,
			Failed:    totals.Failed,
			Skipped:   totals.Skipped,
		})
	}
	circuitProbeStreak.Range(func(key, value any) bool {
		tuple, ok := key.(string)
		if !ok {
			return true
		}
		counter, ok := value.(*atomic.Int64)
		if !ok {
			return true
		}
		if count := int(counter.Load()); count > 0 {
			cp.Streaks = append(cp.Streaks, checkpointProbeStreak{Key: tuple, Count: count})
		}
		return true
	})
	sort.Slice(cp.Streaks, func(i, j int) bool { return cp.Streaks[i].Key < cp.Streaks[j].Key })

	historyChannels := make([]int, 0, len(s.history))
	for channelID := range s.history {
		historyChannels = append(historyChannels, channelID)
	}
	sort.Ints(historyChannels)
	for _, channelID := range historyChannels {
		for _, row := range s.history[channelID] {
			cp.History = append(cp.History, checkpointProbeHistory{
				ChannelID:  channelID,
				Timestamp:  row.Timestamp,
				Model:      row.Model,
				KeyID:      row.KeyID,
				Prompt:     row.Prompt,
				Answer:     row.Answer,
				DurationMS: row.DurationMS,
				Result:     row.Result,
			})
		}
	}

	if cp.Global == (checkpointProbeCounters{}) && len(cp.Channels) == 0 && len(cp.Streaks) == 0 && len(cp.History) == 0 {
		return nil
	}
	return &cp
}

// restoreProbeCheckpoint 把检查点里的计数/连续计数/历史装回内存。日期不对的计数会在
// 下一次 tick 的日切里被清掉（那时才知道"今天"是哪天），所以这里不做时间判断。
func restoreProbeCheckpoint(cp *checkpointProbe) {
	if cp == nil {
		return
	}
	s := circuitProbe
	s.mu.Lock()
	defer s.mu.Unlock()

	s.day = cp.Day
	s.global = CircuitProbeTotals{
		Used:      cp.Global.Used,
		Recovered: cp.Global.Recovered,
		Failed:    cp.Global.Failed,
		Skipped:   cp.Global.Skipped,
	}
	for _, channel := range cp.Channels {
		if channel.ChannelID <= 0 {
			continue
		}
		s.channelTotals[channel.ChannelID] = CircuitProbeTotals{
			Used:      channel.Used,
			Recovered: channel.Recovered,
			Failed:    channel.Failed,
			Skipped:   channel.Skipped,
		}
	}
	for _, streak := range cp.Streaks {
		if streak.Count <= 0 || strings.TrimSpace(streak.Key) == "" {
			continue
		}
		circuitProbeStreak.Store(streak.Key, newProbeStreakCounter(int64(streak.Count)))
	}
	for _, row := range cp.History {
		if row.ChannelID <= 0 {
			continue
		}
		existing := s.history[row.ChannelID]
		if len(existing) >= probeHistoryRingSize {
			continue
		}
		s.history[row.ChannelID] = append(existing, CircuitProbeHistoryRow{
			Timestamp:  row.Timestamp,
			Model:      row.Model,
			KeyID:      row.KeyID,
			Prompt:     row.Prompt,
			Answer:     row.Answer,
			DurationMS: row.DurationMS,
			Result:     row.Result,
		})
	}
}
