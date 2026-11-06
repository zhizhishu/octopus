package task

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInitWiresAutoAuditBehindRuntimeSwitches 静态(AST)断言 Init **接线了**两条会花钱的
// 检测路径，且接线方式仍然是"运行时由开关挡住"：
//   - 注册 TaskModelAuditQuick（周期抽检），间隔用 scheduledAuditInterval，且 runOnStart=false
//     （启动那一刻不该打上游）；
//   - 调用 RegisterAuditFollowUp（可疑日志跟查）。
//
// 为什么这条断言的形式变了：上一期为了"后台绝不自动打上游"，把两条路径整段拆掉了——
// 那样一来开关再打开也什么都不会发生，开关本身成了摆设。现在接回来，但真正的闸门在
// 运行时：ModelAuditQuickTask 第一件事读 model_audit_enabled，跟查回调先过
// model_audit_enabled + model_audit_followup_enabled 两道明确 true（见 op.AuditFollowUpTrigger），
// 且每次真发题还要扣当日额度。"接线了但开关关着时零入队、零上游请求"由
// audit_test.go 的运行时用例证明，这里只证明接线面本身。
//
// 用 AST 而非运行时调用 Init：Init 会启动后台协程并读设置缓存，在单测二进制里不该被真跑。
func TestInitWiresAutoAuditBehindRuntimeSwitches(t *testing.T) {
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

	followUpWired := false
	quickAuditRegistered := false
	quickAuditRunOnStart := true // 只有真的看到 false 实参才放过
	ast.Inspect(initFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "RegisterAuditFollowUp" {
				followUpWired = true
			}
			if fn.Name == "Register" && len(call.Args) > 0 {
				if first, ok := call.Args[0].(*ast.Ident); ok && first.Name == "TaskModelAuditQuick" {
					quickAuditRegistered = true
					if len(call.Args) < 4 {
						t.Errorf("Register(TaskModelAuditQuick, ...) should pass name, interval, runOnStart and fn; got %d args", len(call.Args))
						return true
					}
					if ident, ok := call.Args[2].(*ast.Ident); ok && ident.Name == "false" {
						quickAuditRunOnStart = false
					}
				}
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "RegisterAuditFollowUp" {
				followUpWired = true
			}
		}
		return true
	})

	if !quickAuditRegistered {
		t.Error("Init must register TaskModelAuditQuick (runtime switch model_audit_enabled keeps it from spending anything when off)")
	}
	if quickAuditRegistered && quickAuditRunOnStart {
		t.Error("TaskModelAuditQuick must be registered with runOnStart=false: the moment the process boots is not a moment to bill upstream")
	}
	if !followUpWired {
		t.Error("Init must call RegisterAuditFollowUp (runtime switches keep it from spending anything when off)")
	}
}
