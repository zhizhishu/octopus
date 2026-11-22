package db

import (
	"os"
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm/schema"
)

func TestDropTextDefaultsOnlyTouchesTextColumns(t *testing.T) {
	s, err := schema.Parse(&model.RelayLog{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := s.FieldsByDBName["model_audit_report"]
	if report == nil || !report.HasDefaultValue {
		t.Fatalf("model_audit_report should carry a default before the MySQL adjustment")
	}
	before := map[string]bool{}
	for _, f := range s.Fields {
		before[f.DBName] = f.HasDefaultValue
	}
	dropTextDefaults(s)
	for _, f := range s.Fields {
		isText := mysqlTypeRejectsDefault(string(f.DataType))
		if isText && f.HasDefaultValue {
			t.Fatalf("%s keeps a default MySQL rejects", f.DBName)
		}
		if !isText && f.HasDefaultValue != before[f.DBName] {
			t.Fatalf("%s default changed", f.DBName)
		}
	}
}

// MySQL cannot index a TEXT column without a key length; the redeem code needs a size.
func TestRedeemCodeIsSizedForMySQLIndex(t *testing.T) {
	s, err := schema.Parse(&model.RedeemCode{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if size := s.FieldsByDBName["code"].Size; size <= 0 || size > 191 {
		t.Fatalf("redeem_codes.code size = %d, want 1..191", size)
	}
}

// Runs only when OCT_TEST_DB_TYPE / OCT_TEST_DB_DSN point at an external MySQL or
// Postgres database: the schema must migrate from empty, migrate again unchanged, and
// the pool must stay usable right after InitDB.
func TestInitDBOnExternalDatabase(t *testing.T) {
	dbType := os.Getenv("OCT_TEST_DB_TYPE")
	if dbType == "" {
		t.Skip("OCT_TEST_DB_TYPE not set")
	}
	for round := 1; round <= 2; round++ {
		if err := InitDB(dbType, os.Getenv("OCT_TEST_DB_DSN"), false); err != nil {
			t.Fatalf("InitDB round %d: %v", round, err)
		}
		var settings []model.Setting
		if err := GetDB().Find(&settings).Error; err != nil {
			t.Fatalf("query after InitDB round %d: %v", round, err)
		}
		if err := GetDB().Create(&model.RedeemCode{Code: "external-db-check"}).Error; err != nil && round == 1 {
			t.Fatalf("insert redeem code: %v", err)
		}
		if err := Close(); err != nil {
			t.Fatalf("close round %d: %v", round, err)
		}
	}
}
