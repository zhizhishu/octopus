package db

import (
	"fmt"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/db/migrate"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var db *gorm.DB

func InitDB(dbType, dsn string, debug bool) error {
	var err error
	// SkipDefaultTransaction: GORM otherwise wraps every single Create/Update/Delete in
	// its own BEGIN...COMMIT. On SQLite (single-writer) with a large connection pool this
	// makes concurrent per-request writes (relay log / stats / ip / usage / session) all
	// issue BEGIN at once, which collides as "cannot start a transaction within a
	// transaction" and holds the write lock long enough to cascade into database-is-locked
	// (SQLITE_BUSY) across every log/stats write AND the log-list reads. Single statements
	// are already atomic, so the wrapper buys nothing here; skipping it turns each write
	// into one short bare statement. Explicit Transaction()/Begin() calls (admin CRUD) keep
	// their own transactions and are unaffected. Harmless for MySQL/Postgres.
	// CreateBatchSize caps how many rows GORM binds per INSERT on a batch Create. The
	// async relay-log flusher can hand Create() the whole pending queue (up to 500 rows);
	// RelayLog has ~60 columns, so an unbounded batch would bind ~30k parameters — perilously
	// close to SQLite's default 32766 bound-variable cap, i.e. one poison-scale flush away
	// from every full-queue write failing with "too many SQL variables" exactly when the DB
	// is already behind. Splitting at 100 rows/statement keeps every INSERT well under the cap
	// (and under MySQL's max_allowed_packet) with no behaviour change to callers.
	gormConfig := gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true, CreateBatchSize: 100}
	if debug {
		gormConfig.Logger = logger.Default.LogMode(logger.Info)
	}

	switch dbType {
	case "sqlite":
		db, err = initSQLite(dsn, &gormConfig)
	case "mysql":
		db, err = initMySQL(dsn, &gormConfig)
	case "postgres", "postgresql":
		db, err = initPostgres(dsn, &gormConfig)
	default:
		return fmt.Errorf("unsupported database type: %s", dbType)
	}

	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}

	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)
	sqlDB.SetConnMaxIdleTime(10 * time.Minute)

	if err := migrate.BeforeAutoMigrate(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.UserCheckIn{},
		&model.Channel{},
		&model.ChannelKey{},
		&model.FingerprintProfile{},
		&model.Group{},
		&model.GroupItem{},
		&model.AccessRouteProfile{},
		&model.AccessRouteRule{},
		&model.AccessRouteTarget{},
		&model.AccessBillingProfile{},
		&model.AccessBillingModelRule{},
		&model.AccessPlan{},
		&model.APIKeyAccessPlan{},
		&model.UserAccessPlan{},
		&model.LLMInfo{},
		&model.APIKey{},
		&model.RedeemCode{},
		&model.Setting{},
		&model.StatsTotal{},
		&model.StatsDaily{},
		&model.StatsHourly{},
		&model.StatsModel{},
		&model.StatsChannel{},
		&model.StatsAPIKey{},
		&model.RelayLog{},
		&model.ResponseSession{},
		&migrate.MigrationRecord{},
	); err != nil {
		return err
	}
	if err := migrate.AfterAutoMigrate(db); err != nil {
		return err
	}
	// Postgres: schema changes during migrations can invalidate cached prepared plans
	// (e.g. "cached plan must not change result type"). Clear them.
	if db.Dialector != nil && db.Dialector.Name() == "postgres" {
		db.Exec("DEALLOCATE ALL")
		db.Exec("DISCARD ALL")
	}
	return nil
}

func initSQLite(path string, config *gorm.Config) (*gorm.DB, error) {
	// The driver is glebarez/sqlite (modernc, pure-Go), NOT mattn/go-sqlite3. modernc only
	// honours the "_pragma=name(value)" DSN dialect — it silently ignores the mattn-style
	// "_journal_mode=WAL" / "_synchronous=NORMAL" keys (url.ParseQuery keeps them but the
	// driver's applyQueryParams only reads q["_pragma"]). This file used the mattn dialect,
	// so for a long time ONLY busy_timeout was actually in effect (the driver hard-codes that
	// one default) and WAL was NEVER on — the DB ran in rollback-journal mode where a writer
	// blocks every reader and vice-versa, serialising the whole request hot path. Switch to
	// the dialect the driver reads. (internal/migration/newapi already used the correct form.)
	//
	//   journal_mode(WAL): readers stop blocking the writer — the point of this fix.
	//   synchronous(NORMAL): safe under WAL, far fewer fsyncs than the FULL default.
	//   busy_timeout / cache_size / mmap_size: same intent as before, now actually applied.
	//
	// Deliberately NOT set: foreign_keys. This DB has run with FK enforcement OFF since day
	// one; turning it ON here could start rejecting writes against any pre-existing orphaned
	// row — a behaviour change unrelated to the perf fix, so it stays a separate, testable
	// decision. auto_vacuum / locking_mode dropped too: a DSN pragma cannot change auto_vacuum
	// without a full VACUUM, and locking_mode NORMAL is already the default (EXCLUSIVE would
	// defeat WAL's multi-reader benefit).
	params := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=cache_size(10000)",
		"_pragma=mmap_size(268435456)",
	}
	return gorm.Open(sqlite.Open(path+"?"+strings.Join(params, "&")), config)
}

func initMySQL(dsn string, config *gorm.Config) (*gorm.DB, error) {
	// DSN 格式: user:password@tcp(host:port)/dbname?charset=utf8mb4&parseTime=True&loc=Local
	if !strings.Contains(dsn, "?") {
		dsn += "?charset=utf8mb4&parseTime=True&loc=Local"
	}
	return gorm.Open(mysql.Open(dsn), config)
}

func initPostgres(dsn string, config *gorm.Config) (*gorm.DB, error) {
	// DSN 格式: host=localhost user=postgres password=xxx dbname=octopus port=5432 sslmode=disable
	return gorm.Open(postgres.Open(dsn), config)
}

func Close() error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func GetDB() *gorm.DB {
	return db
}
