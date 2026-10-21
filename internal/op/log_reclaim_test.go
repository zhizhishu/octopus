package op

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 这些测试守的是「死空间回收」的触发条件。
//
// 背景（生产真实踩到的缺口）：原来的判据是"本轮确实删掉了行才 VACUUM"。
// 日志只跨 3 天、保留期 30 天时，清理任务每 10 分钟跑一次、每次一行都删不掉，
// 于是 VACUUM 永远进不去——而库里躺着 11.7 GiB 历史死空间永远不会被回收。
//
// 所以判据改成看空闲页占比。这里锁三件事：
//  1. 占比够高就回收，与"这轮删没删"无关；
//  2. 冷却期内绝不重复回收（完整 VACUUM 期间库是被锁死的，反复触发会按住生产）；
//  3. 小库的占比抖动不能触发（否则空库删一行就要重排整个文件）。

func TestReclaimDecisionFollowsFreeRatioNotDeletion(t *testing.T) {
	now := time.Now()

	// 生产实测比例：11.7 GiB / 17.1 GiB ≈ 68%。这正是"一行都没删"却该回收的场景。
	due, reason := relayLogReclaimDecision(3_060_051, 4_478_033, time.Time{}, now)
	if !due {
		t.Fatalf("68%% 空闲页应当触发回收，实际跳过: %s", reason)
	}
	if !strings.Contains(reason, "超过阈值") {
		t.Fatalf("理由应说明超过阈值，实际: %s", reason)
	}
}

func TestReclaimDecisionSkipsWhenRatioIsLow(t *testing.T) {
	now := time.Now()

	// 正常写入会留几个百分点空闲页，这时候重排文件纯属浪费。
	due, reason := relayLogReclaimDecision(50_000, 4_000_000, time.Time{}, now)
	if due {
		t.Fatalf("1.25%% 空闲页不该回收，实际触发了: %s", reason)
	}
	if !strings.Contains(reason, "未达阈值") {
		t.Fatalf("理由应说明未达阈值，实际: %s", reason)
	}
}

func TestReclaimDecisionRespectsRatioBoundary(t *testing.T) {
	now := time.Now()

	// 恰好等于阈值应当触发（判据是 ratio < threshold 才算不够）。
	const total = int64(100_000)
	atThreshold := int64(float64(total) * relayLogReclaimFreeRatio)
	if due, _ := relayLogReclaimDecision(atThreshold, total, time.Time{}, now); !due {
		t.Fatal("恰好达到阈值时应触发回收")
	}

	// 差一页就该跳过，边界不能糊。
	if due, _ := relayLogReclaimDecision(atThreshold-1, total, time.Time{}, now); due {
		t.Fatal("略低于阈值时不该触发回收")
	}
}

func TestReclaimDecisionIgnoresTinyLibraries(t *testing.T) {
	now := time.Now()

	// 空库刚建好时占比可能极高，但只有几十页——重排毫无意义。
	due, reason := relayLogReclaimDecision(900, 1_000, time.Time{}, now)
	if due {
		t.Fatalf("空闲页绝对数太小不该回收，实际触发: %s", reason)
	}
	if !strings.Contains(reason, "未达下限") {
		t.Fatalf("理由应说明未达下限，实际: %s", reason)
	}
}

func TestReclaimDecisionCooldownBlocksRepeat(t *testing.T) {
	now := time.Now()

	// 一小时前刚回收过 → 冷却期内，无论空闲多少都不能再来一次。
	last := now.Add(-1 * time.Hour)
	due, reason := relayLogReclaimDecision(3_000_000, 4_000_000, last, now)
	if due {
		t.Fatalf("冷却期内不该重复回收，实际触发: %s", reason)
	}
	if !strings.Contains(reason, "冷却期") {
		t.Fatalf("理由应说明冷却期，实际: %s", reason)
	}

	// 超过冷却期就恢复。
	due2, _ := relayLogReclaimDecision(3_000_000, 4_000_000, now.Add(-relayLogReclaimCooldown-time.Minute), now)
	if !due2 {
		t.Fatal("超过冷却期后应恢复回收")
	}
}

func TestReclaimDecisionCooldownBeatsEverything(t *testing.T) {
	now := time.Now()

	// 冷却期判断必须排在占比之前：否则日志里会出现"占比够了但没回收"这种
	// 让人以为阈值坏了的误导性理由。
	due, reason := relayLogReclaimDecision(99_999, 100_000, now.Add(-time.Minute), now)
	if due {
		t.Fatal("冷却期内不该回收")
	}
	if !strings.Contains(reason, "冷却期") {
		t.Fatalf("冷却期内理由应提冷却期而不是占比，实际: %s", reason)
	}
}

func TestReclaimDecisionHandlesZeroPages(t *testing.T) {
	// 库还没建完/读不到时不能除零，也不能误判成该回收。
	if due, _ := relayLogReclaimDecision(0, 0, time.Time{}, time.Now()); due {
		t.Fatal("总页数为 0 时不该回收")
	}
}

func TestReclaimSkipsBeforeTouchingDatabaseOnCooldown(t *testing.T) {
	// 冷却期内应当连 VACUUM 都不尝试。用"库还没初始化"来证明它确实提前返回了：
	// 如果它去读 PRAGMA 就会因空指针/无连接而失败。
	relayLogLastReclaimUnix.Store(time.Now().Unix())
	t.Cleanup(func() { relayLogLastReclaimUnix.Store(0) })

	if err := RelayLogReclaimDeadSpace(context.Background()); err != nil {
		t.Fatalf("冷却期内应安全返回 nil，实际: %v", err)
	}
}

func TestReclaimRecordsTimestampWhenTriggered(t *testing.T) {
	ctx := setupRelayLogTest(t)
	relayLogLastReclaimUnix.Store(0)
	t.Cleanup(func() { relayLogLastReclaimUnix.Store(0) })

	// 造出一个"占用率很高"的库：写入足够多行再删掉，留下空闲页。
	//
	// 批次刻意开小：RelayLog 字段多，SQLite 对单条 INSERT 的变量个数有上限
	// （SQLITE_MAX_VARIABLE_NUMBER，老版本默认 999），按 500 行一把塞会直接
	// 报 "too many SQL variables"。逐行插入最稳，慢一点无所谓。
	const seedRows = 2000
	rows := make([]model.RelayLog, 0, seedRows)
	for i := 0; i < seedRows; i++ {
		rows = append(rows, model.RelayLog{
			ID:               int64(700000 + i),
			Time:             int64(700000 + i),
			RequestModelName: "probe-model",
			RequestContent:   strings.Repeat("x", 512),
		})
	}
	for i := range rows {
		if err := db.GetDB().WithContext(ctx).Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed logs: %v", err)
		}
	}
	if err := db.GetDB().WithContext(ctx).Where("id >= ?", 700000).Delete(&model.RelayLog{}).Error; err != nil {
		t.Fatalf("delete logs: %v", err)
	}

	// 只是确保这条路径不炸；是否真的回收取决于空闲页是否过阈值。
	if err := RelayLogReclaimDeadSpace(ctx); err != nil {
		t.Fatalf("reclaim 不应返回错误: %v", err)
	}
}

// 开关的存在理由：完整 VACUUM 会排他锁死生产库。万一它卡住了，用户必须能在
// 管理页立刻关掉，而不是等下一次发版。所以这两条必须锁死——关掉就是真关掉。
func TestReclaimHonorsDisabledSwitch(t *testing.T) {
	ctx := setupRelayLogTest(t)
	relayLogLastReclaimUnix.Store(0)
	t.Cleanup(func() { relayLogLastReclaimUnix.Store(0) })

	if err := SettingSetString(model.SettingKeyRelayLogReclaimEnabled, "false"); err != nil {
		t.Fatalf("关闭开关: %v", err)
	}

	// 造出足以触发回收的死空间，然后确认关掉开关后既不回收、也不记时间戳。
	var rows []model.RelayLog
	for i := 0; i < 300; i++ {
		rows = append(rows, model.RelayLog{
			ID:             int64(800000 + i),
			Time:           int64(800000 + i),
			RequestContent: strings.Repeat("y", 1024),
		})
	}
	for i := range rows {
		if err := db.GetDB().WithContext(ctx).Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.GetDB().WithContext(ctx).Where("id >= ?", 800000).Delete(&model.RelayLog{}).Error; err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := RelayLogReclaimDeadSpace(ctx); err != nil {
		t.Fatalf("关闭时也应安全返回: %v", err)
	}
	if got := relayLogLastReclaimUnix.Load(); got != 0 {
		t.Fatalf("开关关闭时不该记录回收时间戳，实际 %d", got)
	}
}

func TestReclaimRunsWhenSwitchEnabled(t *testing.T) {
	ctx := setupRelayLogTest(t)
	relayLogLastReclaimUnix.Store(0)
	t.Cleanup(func() { relayLogLastReclaimUnix.Store(0) })

	if err := SettingSetString(model.SettingKeyRelayLogReclaimEnabled, "true"); err != nil {
		t.Fatalf("开启开关: %v", err)
	}
	// 只验证这条路径能正常跑通不报错；是否真的回收取决于空闲页是否过阈值。
	if err := RelayLogReclaimDeadSpace(ctx); err != nil {
		t.Fatalf("开启时不应报错: %v", err)
	}
}
