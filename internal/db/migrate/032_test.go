package migrate

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 旧库（没有 png_size 列，或加了但留着 NULL）→ 032：列补齐、存量行收敛成 0；
// 已有非 0 值（已经存过 PNG 的行）不动；跑两次结果一样。
func TestMigration032CreativePNGSizeConvergesToZero(t *testing.T) {
	db := new026DB(t)
	// 老表的形状：鹈鹕表在，但 PNG 两列根本没有——升级路径上最坏的情况
	//（AutoMigrate 还没跑过 / 或跑过但方言没把 DEFAULT 真的落到存量行上）。
	if err := db.Exec(`CREATE TABLE model_audit_creatives (
		id integer PRIMARY KEY,
		round_id text,
		channel_id integer,
		model text,
		kind text,
		prompt text,
		status text,
		pinned boolean,
		created_at datetime
	)`).Error; err != nil {
		t.Fatalf("create legacy creatives: %v", err)
	}
	for _, ddl := range []string{
		`INSERT INTO model_audit_creatives (id, round_id, channel_id, model, kind, prompt, status, pinned, created_at)
		 VALUES (1,'r1',1,'m','pelican','p','done',0,'2026-01-01 00:00:00')`,
		`INSERT INTO model_audit_creatives (id, round_id, channel_id, model, kind, prompt, status, pinned, created_at)
		 VALUES (2,'r1',1,'m2','pelican','p','done',0,'2026-01-01 00:00:01')`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	if err := migrateCreativePNGColumns(db); err != nil {
		t.Fatal(err)
	}

	m := db.Migrator()
	if !m.HasColumn(&model.ModelAuditCreative{}, "PNGSize") {
		t.Fatal("migration 032 must add the png_size column")
	}
	var nullSizes int64
	if err := db.Table("model_audit_creatives").Where("png_size IS NULL").Count(&nullSizes).Error; err != nil {
		t.Fatalf("count null png_size: %v", err)
	}
	if nullSizes != 0 {
		t.Fatalf("%d row(s) still hold NULL png_size", nullSizes)
	}
	readSizes := func() []int {
		t.Helper()
		var rows []struct {
			ID      int
			PNGSize int `gorm:"column:png_size"`
		}
		if err := db.Table("model_audit_creatives").Select("id, png_size").Order("id").Find(&rows).Error; err != nil {
			t.Fatalf("read back: %v", err)
		}
		out := make([]int, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.PNGSize)
		}
		return out
	}
	if got := readSizes(); len(got) != 2 || got[0] != 0 || got[1] != 0 {
		t.Fatalf("png_size after migration = %v, want [0 0]", got)
	}

	// 已经存过 PNG 的行再跑一次不许清零（防重复上传靠非 0 值识别「已有 PNG」）。
	if err := db.Exec(`UPDATE model_audit_creatives SET png_size = 4096 WHERE id = 1`).Error; err != nil {
		t.Fatalf("seed stored png: %v", err)
	}
	if err := migrateCreativePNGColumns(db); err != nil {
		t.Fatal(err)
	}
	if got := readSizes(); len(got) != 2 || got[0] != 4096 || got[1] != 0 {
		t.Fatalf("rerun must keep stored sizes, got %v want [4096 0]", got)
	}

	// 表都不在（还没建过鹈鹕表的老库）：直接成功，不炸。
	fresh := new026DB(t)
	if err := migrateCreativePNGColumns(fresh); err != nil {
		t.Fatalf("no table must be a no-op, got %v", err)
	}
}
