package migrate

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 旧库（没有 probe_enabled 列、或者加了但留着空值）→ 027：存量行一律收敛成 false；
// 已经是 true 的行不动；跑两次结果一样。
func TestMigration027ChannelProbeEnabledDefaultsOff(t *testing.T) {
	db := new026DB(t)
	// 老表的形状：列可以是空的（没有 DEFAULT，也没有 NOT NULL）。这是最坏情况，
	// 也是"读出来碰巧是 false"和"库里确实写着 false"之间的差别所在。
	if err := db.Exec(`CREATE TABLE channels (id integer PRIMARY KEY, name text, probe_enabled boolean)`).Error; err != nil {
		t.Fatalf("create legacy channels: %v", err)
	}
	for _, ddl := range []string{
		`INSERT INTO channels (id, name, probe_enabled) VALUES (1,'a',NULL)`,
		`INSERT INTO channels (id, name, probe_enabled) VALUES (2,'b',1)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	if err := migrateChannelProbeEnabledOff(db); err != nil {
		t.Fatal(err)
	}
	// 运营商后来手动打开了 2 号渠道；再跑一次不许把它关回去。
	if err := db.Exec(`UPDATE channels SET probe_enabled = 1 WHERE id = 2`).Error; err != nil {
		t.Fatalf("operator enable: %v", err)
	}
	if err := migrateChannelProbeEnabledOff(db); err != nil {
		t.Fatal(err)
	}

	type row struct {
		ID      int
		Enabled bool
	}
	var rows []row
	if err := db.Table("channels").Select("id, probe_enabled AS enabled").Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := []row{{1, false}, {2, true}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %#v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %#v, want %#v", i+1, rows[i], want[i])
		}
	}

	// 空值必须真的没了 —— 不能被"读成 false"掩盖。
	var nullable int64
	if err := db.Table("channels").Where("probe_enabled IS NULL").Count(&nullable).Error; err != nil {
		t.Fatalf("count null: %v", err)
	}
	if nullable != 0 {
		t.Fatalf("%d row(s) still hold NULL probe_enabled", nullable)
	}
}

// 全新库：AutoMigrate 之后新渠道默认关闭，027 是个空转的幂等步骤。
func TestMigration027FreshSchemaStaysOff(t *testing.T) {
	db := new026DB(t)
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	// 存量渠道（升级前就存在、字段从未被写过）用零值插入。
	if err := db.Exec(`INSERT INTO channels (id, name, type) VALUES (1,'legacy',0)`).Error; err != nil {
		t.Fatalf("insert legacy channel: %v", err)
	}
	if err := migrateChannelProbeEnabledOff(db); err != nil {
		t.Fatal(err)
	}
	var got model.Channel
	if err := db.First(&got, 1).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.ProbeEnabled {
		t.Fatal("a channel that predates the switch must not have automatic probing enabled")
	}
}
