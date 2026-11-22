package op

import (
	"os"

	"github.com/zhizhishu/octopus/internal/db"
)

// initTestDB uses SQLite unless OCT_TEST_DB_TYPE / OCT_TEST_DB_DSN point the tests at an
// external MySQL or Postgres database.
func initTestDB(sqlitePath string) error {
	if dbType := os.Getenv("OCT_TEST_DB_TYPE"); dbType != "" {
		return db.InitDB(dbType, os.Getenv("OCT_TEST_DB_DSN"), false)
	}
	return db.InitDB("sqlite", sqlitePath, false)
}
