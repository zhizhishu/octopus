package migrate

import (
	"go/ast"
	"go/token"
	"strconv"
	"testing"
)

// MySQL 不支持 CREATE INDEX IF NOT EXISTS。一条不加方言区分的字面量若被 Exec
// 直接执行，MySQL 报 1064，错误被日志吞掉之后索引永远没有，计数继续全表扫。
func TestMigration021NoDialectBlindIndexSQL(t *testing.T) {
	file := parseMigrationSource(t, "021.go")
	for _, sql := range execStringLiterals(file) {
		if contains(sql, "CREATE INDEX") && contains(sql, "IF NOT EXISTS") {
			t.Fatalf("021.go 有一条不加方言区分的索引字面量会被直接执行：%q\n"+
				"MySQL 不支持 CREATE INDEX IF NOT EXISTS", sql)
		}
	}
}

func TestMigration021HasMySQLIndexBranch(t *testing.T) {
	file := parseMigrationSource(t, "021.go")
	hasMySQLCase := false
	hasHasIndex := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CaseClause:
			for _, e := range v.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil && s == "mysql" {
						hasMySQLCase = true
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "HasIndex" {
				hasHasIndex = true
			}
		}
		return true
	})
	if !hasMySQLCase {
		t.Fatal("021.go 没有 mysql 方言分支")
	}
	if !hasHasIndex {
		t.Fatal("021.go 的 mysql 分支没有用 HasIndex：AlwaysRun 会在第二次启动重复 CREATE")
	}
}
