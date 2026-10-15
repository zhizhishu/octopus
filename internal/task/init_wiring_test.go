package task

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInitDoesNotWireAutoAudit 静态(AST)断言 Init 不再接线任何会自动打上游的检测路径:
// 既不调用 RegisterAuditFollowUp(回显不一致自动跟进), 也不注册 TaskModelAuditQuick(周期快检)。
// 这是"后台不得自动启动消耗 token 的 behavior 探针"的接线面证据, 与默认值面(DefaultSettings)
// 互为补充——只断言默认 false 而漏接线不算覆盖。
//
// 用 AST 而非运行时调用 Init: Init 会启动后台协程并读设置缓存, 在单测二进制里不该被真跑。
func TestInitDoesNotWireAutoAudit(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	initPath := filepath.Join(filepath.Dir(thisFile), "init.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, initPath, nil, 0)
	if err != nil {
		t.Fatalf("parse init.go: %v", err)
	}

	var initFn *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Init" {
			initFn = fn
			break
		}
	}
	if initFn == nil || initFn.Body == nil {
		t.Fatal("func Init not found in init.go")
	}

	ast.Inspect(initFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "RegisterAuditFollowUp" {
				t.Errorf("Init must not call RegisterAuditFollowUp (auto follow-up disconnected)")
			}
			if fn.Name == "Register" && len(call.Args) > 0 {
				if first, ok := call.Args[0].(*ast.Ident); ok && first.Name == "TaskModelAuditQuick" {
					t.Errorf("Init must not register TaskModelAuditQuick (scheduled quick audit disconnected)")
				}
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "RegisterAuditFollowUp" {
				t.Errorf("Init must not call RegisterAuditFollowUp (auto follow-up disconnected)")
			}
		}
		return true
	})
}
