package op

// 容量统计与清理的守卫测试。
//
// 这一组守的不是"数字对不对"，而是**内存是否有界**——生产库从设置的 10GB 涨到
// 59GB，根因就是统计/清理会把整张表连同正文读进内存，进程被 OOM 杀掉后清理
// 永远跑不完。所以这里既验证结果正确，也验证读取行数不再随表增长。

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// 统计口径必须与 relayLogApproxBytes 一致：SQL 侧算出来的总量要能和 Go 逐行
// 累加对上，否则"已用/上限"会漂，用户看到的上限就是假的。
func TestStoredBytesMatchesGoAccounting(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	rows := []model.RelayLog{
		{ID: 7001, Time: 7001, RequestModelName: "kimi-k3", RequestContent: strings.Repeat("a", 300), ResponseContent: strings.Repeat("b", 500)},
		{ID: 7002, Time: 7002, RequestModelName: "gpt-6-astra", Error: "boom"},
		{ID: 7003, Time: 7003, RequestModelName: "glm-5.3", RequestPath: "/v1/chat/completions"},
	}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Go 侧逐行累加（旧口径）
	var all []model.RelayLog
	if err := db.GetDB().Find(&all).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	var want int64
	for _, r := range all {
		want += relayLogApproxBytes(r)
	}

	got, n, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		t.Fatalf("stored bytes: %v", err)
	}
	if n != int64(len(all)) {
		t.Fatalf("行数应一致: got %d want %d", n, len(all))
	}
	if got != want {
		t.Fatalf("字节口径不一致: SQL=%d Go=%d (差额 %d)", got, want, got-want)
	}
}

// 中文字符要按**字节**算，不是字符数：Go 的 len(string) 是字节，
// SQL 里若用 LENGTH(text) 会得到字符数，两者能差 3 倍，上限就形同虚设。
func TestStoredBytesCountsUTF8AsBytes(t *testing.T) {
	ctx := setupRelayLogTest(t)

	// 10 个汉字 = 30 字节
	zh := strings.Repeat("中", 10)
	row := model.RelayLog{ID: 7101, Time: 7101, RequestModelName: zh}
	if err := db.GetDB().Create(&row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	var loaded model.RelayLog
	if err := db.GetDB().First(&loaded, row.ID).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	want := relayLogApproxBytes(loaded)

	got, _, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		t.Fatalf("stored bytes: %v", err)
	}
	if got != want {
		t.Fatalf("中文应按字节计: SQL=%d Go=%d (差 %d —— 用字符数会低估上限占用)", got, want, got-want)
	}
}

// 上限必须把搜索投影表算进去，否则这张副本会不受约束地长。
func TestStorageInfoIncludesSearchIndex(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	rows := []model.RelayLog{
		{ID: 7201, Time: 7201, RequestModelName: "kimi-k3", UserName: "alice", ChannelName: "primary"},
	}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := RelayLogStorageInfo(ctx, nil)
	if err != nil {
		t.Fatalf("storage before: %v", err)
	}

	// 投影表里塞入大量文本，模拟副本变大
	big := strings.Repeat("x", 5000)
	if err := db.GetDB().Table(relayLogSearchIndexTable).Create(map[string]any{
		"id": 7201, "request_model_name": "kimi-k3", "user_name": "alice",
		"channel_name": "primary", "error": big,
	}).Error; err != nil {
		t.Fatalf("upsert index: %v", err)
	}

	after, err := RelayLogStorageInfo(ctx, nil)
	if err != nil {
		t.Fatalf("storage after: %v", err)
	}
	if after.StoredBytes <= before.StoredBytes {
		t.Fatalf("投影表变大后已用容量应增加: before=%d after=%d", before.StoredBytes, after.StoredBytes)
	}
}

// 超限时必须真的删到上限以内 —— 这是"设了 10GB 却长到 59GB"的直接反面。
func TestCleanupBringsUsageUnderLimit(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	const n = 60
	rows := make([]model.RelayLog, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, model.RelayLog{
			ID:               int64(7300 + i),
			Time:             int64(7300 + i),
			RequestModelName: fmt.Sprintf("m-%d", i),
			RequestContent:   strings.Repeat("p", 400),
			ResponseContent:  strings.Repeat("q", 400),
			UserID:           1,
		})
	}
	if err := db.GetDB().CreateInBatches(&rows, 20).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	full, _, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	// 把上限设成总量的约 1/3
	gb := float64(full) / 3 / (1024 * 1024 * 1024)
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, fmt.Sprintf("%.12f", gb)); err != nil {
		t.Fatalf("set cap: %v", err)
	}

	if err := relayLogCleanupByStorage(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	after, _, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		t.Fatalf("remeasure: %v", err)
	}
	_, maxBytes, _ := relayLogMaxStorage()
	if after > maxBytes {
		t.Fatalf("清理后仍超上限: after=%d max=%d (清理没真正生效)", after, maxBytes)
	}

	// 最新一条必须留着，否则出问题 timeline 上什么都看不到
	var newest model.RelayLog
	if err := db.GetDB().Order("id DESC").First(&newest).Error; err != nil {
		t.Fatalf("load newest: %v", err)
	}
	if newest.ID != int64(7300+n-1) {
		t.Fatalf("最新一条应保留, 实际 %d", newest.ID)
	}
}

// 投影表残留必须随主表一起收缩，否则"日志删了、空间没降"。
func TestCleanupAlsoShrinksSearchIndex(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	const n = 40
	rows := make([]model.RelayLog, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, model.RelayLog{
			ID:               int64(7400 + i),
			Time:             int64(7400 + i),
			RequestModelName: "kimi-k3",
			RequestContent:   strings.Repeat("z", 500),
		})
	}
	if err := db.GetDB().CreateInBatches(&rows, 20).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := relayLogSearchIndexRebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.00001"); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	if err := relayLogCleanupByStorage(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var mainRows, idxRows int64
	db.GetDB().Model(&model.RelayLog{}).Count(&mainRows)
	db.GetDB().Table(relayLogSearchIndexTable).Count(&idxRows)
	if mainRows >= n {
		t.Fatalf("应删掉部分日志, 实际仍剩 %d", mainRows)
	}
	if idxRows != mainRows {
		t.Fatalf("投影表(%d)必须与主表(%d)同步收缩, 否则会留下搜得到打不开的行", idxRows, mainRows)
	}
}

// 空表不能死循环、不能报错。
func TestCleanupOnEmptyTable(t *testing.T) {
	ctx := setupRelayLogTest(t)
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000001"); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	if err := relayLogCleanupByStorage(ctx); err != nil {
		t.Fatalf("空表清理不应报错: %v", err)
	}
}

// 🔴 本组的核心：统计不得把正文读进 Go 内存。
//
// 做法：塞入若干条大正文日志，然后量统计期间的堆分配增量。有界实现只在 DB 内
// 聚合，分配应与正文总量无关；若有人把实现改回 Find(&allLogs)，这里的分配量
// 会随正文规模线性上涨而爆掉阈值。
func TestStoredBytesDoesNotLoadContentIntoMemory(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	// 每条约 200KB 正文 × 40 条 ≈ 8MB 正文；全量读进内存会明显超过下面的阈值。
	const rows = 40
	const payload = 200 * 1024
	big := strings.Repeat("x", payload)
	batch := make([]model.RelayLog, 0, rows)
	for i := 0; i < rows; i++ {
		batch = append(batch, model.RelayLog{
			ID:               int64(7500 + i),
			Time:             int64(7500 + i),
			RequestModelName: "kimi-k3",
			RequestContent:   big,
			ResponseContent:  big,
		})
	}
	if err := db.GetDB().CreateInBatches(&batch, 5).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	total, n, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	if n != rows {
		t.Fatalf("行数应为 %d, 实际 %d", rows, n)
	}
	// 总量应该接近 2×200KB×40 = 16MB（两条正文列）
	if total < int64(rows*payload) {
		t.Fatalf("统计值偏小，可能没把正文算进去: %d", total)
	}

	// 统计期间的堆增量：有界实现是每行一个 int64 级别的累加，实测远小于 1MB。
	// 若回退成全量 Find，至少要分配 16MB 以上。
	const budget = 8 << 20
	if after.TotalAlloc-before.TotalAlloc > budget {
		t.Fatalf("统计过程分配了 %d 字节(>%d)：正文被读进 Go 内存了，"+
			"生产上这正是把进程 OOM 掉、清理永远跑不完的原因",
			after.TotalAlloc-before.TotalAlloc, budget)
	}
}

// VACUUM 的安全门槛必须真的拦得住：生产库 59GB 时盲目 VACUUM 会重建整个文件，
// 临时占用约等于库大小，写满磁盘就会让数据库写入失败 —— 比"空间没还回来"严重。
//
// 这里只验证"拿不到磁盘信息时保守跳过"，不依赖具体机器的剩余空间，
// 所以 Windows(无 Statfs 实现) 与 Linux 上同样成立。
func TestVacuumSafetyIsConservative(t *testing.T) {
	_ = setupRelayLogTest(t)

	safe, reason := relayLogVacuumSafe()
	if safe {
		// 只有在磁盘真的宽裕时才会返回 true；此时 reason 应为空
		if reason != "" {
			t.Fatalf("判定安全时不应带原因: %q", reason)
		}
		return
	}
	if reason == "" {
		t.Fatal("判定不安全时必须给出原因，否则用户不知道为什么没回收空间")
	}
}

// 每批删除后必须重新统计，最终确实落回上限内(而不是靠一次估算删过头或不够)。
func TestBoundedCleanupLoopsUntilUnderLimit(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	const n = 30
	rows := make([]model.RelayLog, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, model.RelayLog{
			ID:               int64(7600 + i),
			Time:             int64(7600 + i),
			RequestModelName: "kimi-k3",
			RequestContent:   strings.Repeat("c", 300),
			ResponseContent:  strings.Repeat("d", 300),
		})
	}
	if err := db.GetDB().CreateInBatches(&rows, 10).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 上限设成极小值，迫使循环多轮才能收敛
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000002"); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	if err := relayLogCleanupByStorageBounded(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	after, rowsLeft, err := relayLogStoredBytes(ctx, nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	_, maxBytes, _ := relayLogMaxStorage()
	if after > maxBytes {
		t.Fatalf("多轮清理后仍超限: after=%d max=%d", after, maxBytes)
	}
	// 必须给最新一条留位置（旧实现里 idx==0 不删，语义保留）
	if rowsLeft < 1 {
		t.Fatal("不应把日志删光，最新一条要留着")
	}
}

// 手动清理回报的前后差值必须是真删掉的量，不能是负数或凭空出现。
func TestCleanupNowReportsRealDelta(t *testing.T) {
	ctx := setupRelayLogTest(t)
	ensureSearchIndexTable(t)

	rows := make([]model.RelayLog, 0, 10)
	for i := 0; i < 10; i++ {
		rows = append(rows, model.RelayLog{
			ID:              int64(7700 + i),
			Time:            int64(7700 + i),
			RequestContent:  strings.Repeat("e", 400),
			ResponseContent: strings.Repeat("f", 400),
		})
	}
	if err := db.GetDB().CreateInBatches(&rows, 10).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000003"); err != nil {
		t.Fatalf("set cap: %v", err)
	}

	result, err := RelayLogCleanupNow(ctx)
	if err != nil {
		t.Fatalf("cleanup now: %v", err)
	}
	if result.FreedBytes != result.BeforeBytes-result.AfterBytes {
		t.Fatalf("释放量与前後差值不一致: %+v", result)
	}
	if result.FreedBytes <= 0 {
		t.Fatalf("应确实删掉了一些日志: %+v", result)
	}
	if result.Note == "" {
		t.Fatal("必须给用户一句说明，否则只回数字不知道发生了什么")
	}
}

// 单飞：并发调用只能有一个真正执行，其余立刻被拒(不能排队跑两次 VACUUM)。
func TestCleanupNowIsSingleFlight(t *testing.T) {
	ctx := setupRelayLogTest(t)

	// 手动占住标志，模拟"上一次还在跑"
	if !relayLogCleanupRunning.CompareAndSwap(false, true) {
		t.Fatal("标志初始应为 false")
	}
	defer relayLogCleanupRunning.Store(false)

	if _, err := RelayLogCleanupNow(ctx); err == nil {
		t.Fatal("清理进行中时应直接拒绝，而不是排队再跑一次")
	}
}
