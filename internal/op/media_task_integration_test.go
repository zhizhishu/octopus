package op

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 三库语法 smoke：派生表别名/top-N 下推/COUNT 分表在 SQLite 之外的 MySQL、Postgres 也必须成立。
// 默认跳过；只有显式提供 DSN 才连 Docker 库跑，避免普通单测依赖本机服务。
func TestMediaTaskListMySQLPostgresSyntax(t *testing.T) {
	cases := []struct{ name, typ, env string }{
		{"mysql", "mysql", "OCTOPUS_TEST_MYSQL_DSN"},
		{"postgres", "postgres", "OCTOPUS_TEST_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip(tc.env + " not set")
			}
			if err := db.InitDB(tc.typ, dsn, false); err != nil {
				t.Fatalf("init %s: %v", tc.name, err)
			}
			t.Cleanup(func() { _ = db.Close() })
			ctx := context.Background()
			now := time.Now().Unix()
			if err := db.GetDB().Create(&model.ImageTask{TaskID: "itx", CreatedAt: now, ExpiresAt: now + 60, Status: model.ImageTaskStatusCompleted, OwnerUserID: 7, Model: "im"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.GetDB().Create(&model.MidjourneyTask{CreatedAt: now - 1, UpstreamTaskID: "mjx", OwnerUserID: 7, Model: "mj"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.GetDB().Create(&model.VideoTask{CreatedAt: now - 2, UpstreamTaskID: "vix", Status: model.VideoTaskSuccess, OwnerUserID: 7, Model: "vi"}).Error; err != nil {
				t.Fatal(err)
			}
			rows, total, err := MediaTaskListByOwner(ctx, MediaTaskListFilter{OwnerUserID: 7, Page: 1, PageSize: 2})
			if err != nil {
				t.Fatalf("list %s: %v", tc.name, err)
			}
			if total != 3 || len(rows) != 2 {
				t.Fatalf("total=%d len=%d rows=%v", total, len(rows), rows)
			}
			if rows[0].TaskID != "itx" || rows[1].TaskID != "mjx" {
				t.Fatalf("order rows=%v", rows)
			}
		})
	}
}
