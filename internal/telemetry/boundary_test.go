package telemetry

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 边界：telemetry 只用标准库（碰不到出站 client / utls / fhttp），而出站那几个包
// （client / relay / transformer）的正式代码也不许引用 telemetry。出站的 TLS 指纹、
// h2 序、头顺序都在 internal/client，这条把「入站观测不会长到出站上去」钉死在 import 图上。
func TestTelemetryStaysInboundOnly(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
				t.Fatalf("%s imports non-stdlib %s", f, p)
			}
		}
	}
	for _, dir := range []string{"../client", "../relay", "../transformer"} {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			af, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				return nil
			}
			for _, imp := range af.Imports {
				if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/internal/telemetry") {
					t.Errorf("outbound package file %s imports internal/telemetry", path)
				}
			}
			return nil
		})
	}
}
