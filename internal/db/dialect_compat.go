package db

import (
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// dropMySQLTextDefaults removes literal defaults from TEXT/BLOB/JSON columns for MySQL,
// which rejects them (Error 1101), so a model tagged `type:text;default:”` could not be
// created there. The tags stay untouched on purpose: changing them would make AutoMigrate
// rewrite the column on existing SQLite/Postgres databases. GORM still writes the Go zero
// value on insert, so stored values do not change.
func dropMySQLTextDefaults(db *gorm.DB, models []any) error {
	if db.Dialector == nil || db.Dialector.Name() != "mysql" {
		return nil
	}
	for _, m := range models {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(m); err != nil {
			return err
		}
		dropTextDefaults(stmt.Schema)
	}
	return nil
}

func dropTextDefaults(s *schema.Schema) {
	for _, f := range s.Fields {
		if f.HasDefaultValue && mysqlTypeRejectsDefault(string(f.DataType)) {
			f.HasDefaultValue = false
			f.DefaultValue = ""
			f.DefaultValueInterface = nil
		}
	}
}

func mysqlTypeRejectsDefault(dataType string) bool {
	t := strings.ToLower(strings.TrimSpace(dataType))
	for _, prefix := range []string{"tinytext", "text", "mediumtext", "longtext", "tinyblob", "blob", "mediumblob", "longblob", "json", "geometry"} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}
