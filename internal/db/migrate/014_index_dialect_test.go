package migrate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// MySQL 从来没有给 `CREATE INDEX` 加过 `IF NOT EXISTS`（MariaDB 有，MySQL 没有）。
//
// 原来 014.go 里的 `_ = db.Exec("CREATE INDEX IF NOT EXISTS idx_... ON relay_logs(...)")`
// 在 MySQL 上必然报 1064 语法错，而错误被 `_ =` 吞掉 —— 索引永远建不上，也永远
// 没人知道，两个谓词继续全表扫（10 万行实测 847ms）。本迁移标了 AlwaysRun，
// 每轮启动还会再报一次同样的错。
//
// 这些用例走 AST（不是 grep 源文本）：注释里提到这句 SQL 是正常的（说明历史），
// 只有真的作为**执行参数**出现才算违规。

// parseMigrationSource 解析指定的迁移源文件。
func parseMigrationSource(t *testing.T, name string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

// execStringLiterals 收集所有 `....Exec("...")` 调用里的第一个字符串字面量。
func execStringLiterals(file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Exec" && sel.Sel.Name != "Raw") {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, s)
		}
		return true
	})
	return out
}

// 对方言不加区分的 CREATE INDEX IF NOT EXISTS 不许再直接执行。
func TestMigration014NoDialectBlindIndexSQL(t *testing.T) {
	file := parseMigrationSource(t, "014.go")
	for _, sql := range execStringLiterals(file) {
		if contains(sql, "CREATE INDEX") && contains(sql, "IF NOT EXISTS") {
			t.Fatalf("014.go 仍有一条不加方言区分的索引语句会被直接执行：%q\n"+
				"MySQL 不支持 CREATE INDEX IF NOT EXISTS，会报 1064；"+
				"错误被吞掉后索引永远建不上，两个谓词继续全表扫", sql)
		}
	}
}

// mysql 分支必须存在：MySQL 只能靠 HasIndex 判断后再裸 CREATE。
func TestMigration014HasMySQLIndexBranch(t *testing.T) {
	file := parseMigrationSource(t, "014.go")
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
		t.Fatal("014.go 没有 mysql 方言分支：MySQL 不支持 CREATE INDEX IF NOT EXISTS，" +
			"必须单独处理")
	}
	if !hasHasIndex {
		t.Fatal("014.go 的 mysql 分支没有用 HasIndex 判存在性：本迁移是 AlwaysRun，" +
			"每轮启动都会重复 CREATE，第二次报 duplicate key name 并被吞掉")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
