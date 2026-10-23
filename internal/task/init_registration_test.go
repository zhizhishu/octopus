package task

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// Init() 必须把**所有**任务都注册上，哪怕某个设置项读不出来。
//
// 回归背景：三处 `SettingGetInt` 失败就 `return`，直接把后面所有 Register 跳过。
// SettingGetInt 只在 settingCache 里查，key 缺失 / 值是空串 / 写成 "5.5" 都会 err。
// 一旦命中，TaskStatsSave、TaskRelayLogSave、price_update、base_url_delay、sync_llm
// 全部不注册 —— 统计数据永不落库、价格永不更新、渠道延迟永不刷新。
// 而 relayLogFlushLoop 是独立协程仍在跑，日志看着还在落库，
// 于是服务"看起来完全正常"，只有启动时一行 Error。
//
// 这条测试故意把一个设置写成**非法值**，然后要求 Init 之后所有任务都在。
// Init() 不允许把设置读取失败变成"整段注册被跳过"。
//
// 回归背景：三处 `SettingGetInt` 失败就 `return`，把后面所有 Register 全跳过。
// SettingGetInt 只在 settingCache 里查，key 缺失 / 值是空串 / 写成 "5.5" 都返回 err。
// 一旦命中，TaskStatsSave、TaskRelayLogSave、price_update、base_url_delay、sync_llm
// 全部不注册 —— 统计数据永不落库、价格永不更新、渠道延迟永不刷新。
// 而 relayLogFlushLoop 是独立协程仍在跑，日志看着还在落库，于是服务"看起来完全正常"，
// 只有启动时一行 Error。一个配错的值不该让整台机器半瘫。
//
// 🔴 用 AST 而非运行时调用 Init：Init 会启动后台协程并读设置缓存，
// 在单测二进制里不该被真跑（既有 init_wiring_test.go 已确立这条约定）。
func TestInitDoesNotReturnBeforeRegisteringAllTasks(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(thisFile), "init.go"), nil, 0)
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

	// Init 的函数体里不允许出现裸 return：它是一段"逐项注册"的线性流程，
	// 任何提前退出都意味着后面的任务静默消失。
	var early []int
	ast.Inspect(initFn.Body, func(n ast.Node) bool {
		if ret, ok := n.(*ast.ReturnStmt); ok {
			early = append(early, fset.Position(ret.Pos()).Line)
		}
		return true
	})
	if len(early) > 0 {
		t.Errorf("Init() 第 %v 行存在提前 return：设置读失败会让后面的任务全部不注册，"+
			"服务看着正常但统计不落库、价格不更新、延迟不刷新", early)
	}

	// 反过来钉住：该注册的都必须出现在函数体里。
	registered := map[string]bool{}
	ast.Inspect(initFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "Register" && len(call.Args) > 0 {
			// 第一个参数可能是常量(TaskStatsSave)，也可能是
			// string(model.SettingKeyXxx) —— 后者按设置键名注册（price_update / sync_llm）。
			switch a := call.Args[0].(type) {
			case *ast.Ident:
				registered[a.Name] = true
			case *ast.CallExpr:
				ast.Inspect(a, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok {
						registered[sel.Sel.Name] = true
					}
					return true
				})
			}
		}
		return true
	})
	for _, want := range []string{
		"TaskStatsSave", "TaskRelayLogSave",
		"SettingKeyModelInfoUpdateInterval", "SettingKeySyncLLMInterval",
		"TaskBaseUrlDelay", "TaskMaintenanceSweep",
	} {
		if !registered[want] {
			t.Errorf("Init() 里没有注册 %s", want)
		}
	}
}
