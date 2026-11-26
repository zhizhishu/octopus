package migrate

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 旧库（没有 queue_size / queue_timeout_ms 两列，或加了但留着 NULL）→ 029：
// 两列补齐、存量行都收敛成 0；运营商自己配过的非 0 值不动；跑两次结果一样。
func TestMigration029ChannelQueueSettingsDefaultsZero(t *testing.T) {
	db := new026DB(t)
	// 老表的形状：渠道表存在，但排队两列根本没有。这正是升级路径上最坏的情况
	//（AutoMigrate 还没跑过 / 或者跑过但方言没带 DEFAULT）。
	if err := db.Exec(`CREATE TABLE channels (id integer PRIMARY KEY, name text, max_concurrent integer, rpm_limit integer)`).Error; err != nil {
		t.Fatalf("create legacy channels: %v", err)
	}
	for _, ddl := range []string{
		`INSERT INTO channels (id, name, max_concurrent, rpm_limit) VALUES (1,'a',4,0)`,
		`INSERT INTO channels (id, name, max_concurrent, rpm_limit) VALUES (2,'b',0,60)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	if err := migrateChannelQueueSettings(db); err != nil {
		t.Fatal(err)
	}

	// 补出来的两列必须真的在，且存量行读回来是 0（不是「读出来碰巧是 0」的 NULL）。
	m := db.Migrator()
	for _, field := range []string{"QueueSize", "QueueTimeoutMs"} {
		if !m.HasColumn(&model.Channel{}, field) {
			t.Fatalf("migration 029 must add column for %s", field)
		}
	}
	var nullQueues int64
	if err := db.Table("channels").Where("queue_size IS NULL").Count(&nullQueues).Error; err != nil {
		t.Fatalf("count null queue_size: %v", err)
	}
	if nullQueues != 0 {
		t.Fatalf("%d row(s) still hold NULL queue_size", nullQueues)
	}
	var nullTimeouts int64
	if err := db.Table("channels").Where("queue_timeout_ms IS NULL").Count(&nullTimeouts).Error; err != nil {
		t.Fatalf("count null queue_timeout_ms: %v", err)
	}
	if nullTimeouts != 0 {
		t.Fatalf("%d row(s) still hold NULL queue_timeout_ms", nullTimeouts)
	}

	type queueRow struct {
		ID             int
		QueueSize      int
		QueueTimeoutMs int
	}
	readRows := func() []queueRow {
		t.Helper()
		var rows []queueRow
		if err := db.Table("channels").
			Select("id, queue_size, queue_timeout_ms").
			Order("id").Find(&rows).Error; err != nil {
			t.Fatalf("read back: %v", err)
		}
		return rows
	}
	if got, want := readRows(), []queueRow{{1, 0, 0}, {2, 0, 0}}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("stored channels = %#v, want %#v (存量渠道必须不排队、用默认等待)", got, want)
	}

	// 运营商后来真的配了排队：再跑一次不许把它清零。
	if err := db.Exec(`UPDATE channels SET queue_size = 5, queue_timeout_ms = 1500 WHERE id = 1`).Error; err != nil {
		t.Fatalf("operator configure queue: %v", err)
	}
	if err := migrateChannelQueueSettings(db); err != nil {
		t.Fatal(err)
	}
	if got, want := readRows(), []queueRow{{1, 5, 1500}, {2, 0, 0}}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rerun must be idempotent and keep operator values, got %#v want %#v", got, want)
	}
}

// 全新库：AutoMigrate 建好带 DEFAULT 0 的两列之后，029 是个空转的幂等步骤；
// 升级前就存在、从没写过排队字段的渠道读回来必须是不排队 + 默认等待。
func TestMigration029FreshSchemaStaysZero(t *testing.T) {
	db := new026DB(t)
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	if err := db.Exec(`INSERT INTO channels (id, name, type) VALUES (1,'legacy',0)`).Error; err != nil {
		t.Fatalf("insert legacy channel: %v", err)
	}
	if err := migrateChannelQueueSettings(db); err != nil {
		t.Fatal(err)
	}
	if err := migrateChannelQueueSettings(db); err != nil {
		t.Fatal(err)
	}
	var got model.Channel
	if err := db.First(&got, 1).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.QueueSize != 0 || got.QueueTimeoutMs != 0 {
		t.Fatalf("a channel that predates the queue knobs must not queue: queue_size=%d queue_timeout_ms=%d", got.QueueSize, got.QueueTimeoutMs)
	}
}

// 同一张表已经有值（非 NULL）时，029 必须一个字节都不改——尤其是 0 与「未配置」在语义上
// 不同：0 是「不排队」，把它写成别的值就等于替运营商做了决定。
func TestMigration029KeepsExistingValues(t *testing.T) {
	db := new026DB(t)
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	if err := db.Exec(`INSERT INTO channels (id, name, type, max_concurrent, queue_size, queue_timeout_ms) VALUES (1,'configured',0,3,7,2500)`).Error; err != nil {
		t.Fatalf("insert configured channel: %v", err)
	}
	if err := db.Exec(`INSERT INTO channels (id, name, type, max_concurrent, queue_size, queue_timeout_ms) VALUES (2,'off',0,0,0,0)`).Error; err != nil {
		t.Fatalf("insert queue-off channel: %v", err)
	}
	if err := migrateChannelQueueSettings(db); err != nil {
		t.Fatal(err)
	}
	var configured model.Channel
	if err := db.First(&configured, 1).Error; err != nil {
		t.Fatalf("read back configured: %v", err)
	}
	if configured.QueueSize != 7 || configured.QueueTimeoutMs != 2500 {
		t.Fatalf("migration must not touch configured values, got queue_size=%d queue_timeout_ms=%d", configured.QueueSize, configured.QueueTimeoutMs)
	}
	var off model.Channel
	if err := db.First(&off, 2).Error; err != nil {
		t.Fatalf("read back off: %v", err)
	}
	if off.QueueSize != 0 || off.QueueTimeoutMs != 0 {
		t.Fatalf("migration must keep explicit 0 (不排队), got queue_size=%d queue_timeout_ms=%d", off.QueueSize, off.QueueTimeoutMs)
	}
}
