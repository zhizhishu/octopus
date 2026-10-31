package migrate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// key 是 MySQL 保留字：按 key 查 settings 必须让 GORM 按方言给列名加引号。
// 不连库（DryRun），只看生成的 SQL。
func TestSettingKeyIsQuotedPerDialect(t *testing.T) {
	cases := map[string]struct {
		dialector gorm.Dialector
		want      string
	}{
		"mysql":    {mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/x", SkipInitializeWithVersion: true}), "`key` = ?"},
		"postgres": {postgres.New(postgres.Config{DSN: "host=127.0.0.1 port=1 user=u dbname=x sslmode=disable"}), `"key" = $`},
	}
	for name, tc := range cases {
		gdb, err := gorm.Open(tc.dialector, &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
		if err != nil {
			t.Fatalf("%s: open dry-run: %v", name, err)
		}
		var rows []model.Setting
		stmt := gdb.Where(model.SettingKeyIs(model.SettingKeyPromptOverrideMode)).Find(&rows).Statement
		sql := stmt.SQL.String()
		if !strings.Contains(sql, tc.want) {
			t.Fatalf("%s: SQL %q does not quote the key column (want %q)", name, sql, tc.want)
		}
		upd := gdb.Model(&model.Setting{}).Where(model.SettingKeyIs("k")).Where("value = ?", "old").Update("value", "new")
		if !strings.Contains(upd.Statement.SQL.String(), tc.want) {
			t.Fatalf("%s: update SQL %q does not quote the key column (err=%v)", name, upd.Statement.SQL.String(), upd.Error)
		}
	}
}

// 防回归：迁移和 settings 读写里不许再出现裸写的 "key = ?"（SQLite 测试发现不了，MySQL 上直接 1064）。
func TestNoBareSettingKeyLiteral(t *testing.T) {
	bare := regexp.MustCompile("[\"`(, ]key (=|IN|in) \\?")
	files, _ := filepath.Glob("*.go")
	files = append(files, "../../op/setting.go", "../../migration/newapi/migration.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "//") {
				continue
			}
			if bare.MatchString(line) {
				t.Fatalf("%s:%d bare \"key = ?\" breaks on MySQL; use model.SettingKeyIs: %s", f, i+1, trim)
			}
		}
	}
}
