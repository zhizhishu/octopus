package op

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 本文件就是「两入口同一结果」这条要求的机器判据。
//
// 背景：检测端点映射此前写了两份 —— handlers 的 auditEndpointFor（手动检测）与
// task 的 scheduledAuditEndpoint（定时检测），函数体逐字相同。两份判据的后果是
// 「改一份忘另一份」：同一个渠道手动检测按 A 协议去探、定时检测按 B 协议去探，
// 一份绿一份红，或者都绿但探的压根不是同一条路。
//
// 为什么用**静态**判据而不是"对一批渠道类型分别跑两个入口、断言返回值相同"：
// 两个入口都是包内未导出函数，它们把端点名直接喂给 modeltest.NewProbeSender，
// 端点名本身不出现在任何可观测产物里（定时检测的结果结构里没有这个字段）。
// 想在运行时对比，就必然要给两个包各加一个导出接缝 —— 那等于**又造出两个名字**，
// 下一个改判据的人照样只会改其中一个，正是这次要根除的病。
// 静态判据更强也更省：它证明的是「两个入口的端点参数唯一来源就是
// op.AuditEndpointForChannel」，于是对**任意**渠道类型（不是抽样）两边结果必然相同。
// 单源函数本身的逐格取值由 audit_endpoint_test.go 钉死。
const auditEndpointSharedSelector = "AuditEndpointForChannel"

// 端点的四个取值只许出现在 op/audit_endpoint.go 里（测试文件除外）。
var auditEndpointLiterals = []string{
	"openai_chat",
	"openai_responses",
	"anthropic_messages",
	"gemini_generate_content",
}

func auditRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

func TestAuditEndpointBothEntriesShareOneSource(t *testing.T) {
	root := auditRepoRoot(t)

	// 两个入口所在的文件：手动检测（handler + 队列 worker）与定时检测。
	entryCalls := 0
	for _, rel := range []string{
		filepath.Join("internal", "server", "handlers", "model_audit.go"),
		filepath.Join("internal", "server", "handlers", "model_audit_worker.go"),
		filepath.Join("internal", "task", "audit.go"),
	} {
		entryCalls += assertFilePathUsesSharedEndpoint(t, filepath.Join(root, rel))
	}
	if entryCalls < 3 {
		t.Fatalf("审计入口里调用 modeltest.NewProbeSender 的次数 = %d, want >= 3（手动 handler / 手动队列 worker / 定时检测各一处）", entryCalls)
	}

	// 两个包的非测试代码里都不许再出现端点字面量：出现就说明又写了一份映射。
	for _, dir := range []string{
		filepath.Join(root, "internal", "server", "handlers"),
		filepath.Join(root, "internal", "task"),
	} {
		assertNoEndpointLiteralsInPackage(t, dir)
	}
}

// assertFilePathUsesSharedEndpoint 断言一个入口文件：
//  1. 没有留下任何一个端点映射函数（旧名字或新的同义 switch）；
//  2. 每一处 modeltest.NewProbeSender 的端点实参，最终都只可能来自
//     op.AuditEndpointForChannel。
//
// 返回该文件里 NewProbeSender 的调用次数。
func assertFilePathUsesSharedEndpoint(t *testing.T, path string) int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// 1) 旧函数名必须绝迹：留着任何一个都等于端点判据还有第二个名字。
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}
		switch fn.Name.Name {
		case "auditEndpointFor", "scheduledAuditEndpoint":
			t.Errorf("%s:%d 仍在声明 %s：端点判据只许有 op.%s 一个来源（不许留委派包装）",
				fset.Position(fn.Pos()).String(), fset.Position(fn.Pos()).Line, fn.Name.Name, auditEndpointSharedSelector)
		}
	}

	// 2) 每处 NewProbeSender 的端点实参唯一来源是共享函数。
	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "NewProbeSender" {
			return true
		}
		if len(call.Args) < 3 {
			t.Errorf("%s: NewProbeSender 实参不足 3 个（端点名是第 3 个）", fset.Position(call.Pos()).String())
			return true
		}
		calls++
		arg := call.Args[2]
		pos := fset.Position(arg.Pos()).String()
		if isSharedAuditEndpointCall(arg) {
			return true
		}
		ident, ok := arg.(*ast.Ident)
		if !ok {
			t.Errorf("%s: NewProbeSender 的端点实参既不是 op.%s(...) 也不是由它赋值的变量，而是 %T"+
				"——端点判据必须只有一个来源", pos, auditEndpointSharedSelector, arg)
			return true
		}
		sources := identAssignmentSources(file, ident.Name)
		if len(sources) == 0 {
			t.Errorf("%s: 端点变量 %s 在本文件里找不到赋值来源——无法确认它来自 op.%s",
				pos, ident.Name, auditEndpointSharedSelector)
			return true
		}
		for _, src := range sources {
			if !isSharedAuditEndpointCall(src) {
				t.Errorf("%s: 端点变量 %s 被赋成了非 op.%s(...) 的表达式（%s），"+
					"这就是第二个判据", pos, ident.Name, auditEndpointSharedSelector,
					fset.Position(src.Pos()).String())
			}
		}
		return true
	})
	return calls
}

// isSharedAuditEndpointCall 判断表达式是不是 op.AuditEndpointForChannel(...)。
func isSharedAuditEndpointCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != auditEndpointSharedSelector {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "op"
}

// identAssignmentSources 收集文件里对某个标识符的全部赋值右侧表达式。
func identAssignmentSources(file *ast.File, name string) []ast.Expr {
	var out []ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name != name {
					continue
				}
				if i < len(node.Rhs) {
					out = append(out, node.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, ident := range node.Names {
				if ident.Name != name {
					continue
				}
				if i < len(node.Values) {
					out = append(out, node.Values[i])
				}
			}
		}
		return true
	})
	return out
}

// assertNoEndpointLiteralsInPackage 扫一个包的非测试文件，端点字面量一处都不许有。
//
// 为什么按包扫而不是只扫入口文件：当初那两份重复判据就是分处两个包的。只要
// 这两个包里有人再写一次 switch，不管写在哪个新文件里，都得拦住。
func assertNoEndpointLiteralsInPackage(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(raw)
		for _, literal := range auditEndpointLiterals {
			if strings.Contains(text, `"`+literal+`"`) {
				t.Errorf("%s 出现端点字面量 %q：检测端点映射只许留在 internal/op/audit_endpoint.go，"+
					"这里再写一份就是第二处判据", path, literal)
			}
		}
	}
}
