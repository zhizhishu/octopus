package behavior

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 前端能勾选的探针列表必须与后端注册表逐字一致。
//
// 两边各自自洽时的两种破法，只跑各自那侧的测试都发现不了：
//   - 后端加了新探针但前端没加 → 用户永远选不到它，功能等于没做；
//   - 前端留了已删探针的选项 → 一勾就收到 "unknown probe"，白跑一趟。
//
// 这里直接读前端源文件比对，不做"维护两份常量"的约定——约定会漂，比对不会。
func TestFrontendAuditProbeIDsMatchRegistry(t *testing.T) {
	const fePath = "../../../web/src/api/endpoints/model-audit.ts"
	raw, err := os.ReadFile(fePath)
	if err != nil {
		t.Fatalf("read frontend probe list: %v", err)
	}

	// 抓 AUDIT_PROBE_IDS 数组里的字符串字面量。
	block := regexp.MustCompile(`(?s)AUDIT_PROBE_IDS\s*=\s*\[(.*?)\]`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("AUDIT_PROBE_IDS not found in %s", fePath)
	}
	fe := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		fe[string(m[1])] = struct{}{}
	}
	if len(fe) == 0 {
		t.Fatalf("parsed zero probe ids from the frontend list")
	}

	// 后端：注册表的键就是能被执行的探针。
	be := map[string]struct{}{}
	for id := range probeRegistry {
		be[string(id)] = struct{}{}
	}

	var onlyFE, onlyBE []string
	for id := range fe {
		if _, ok := be[id]; !ok {
			onlyFE = append(onlyFE, id)
		}
	}
	for id := range be {
		if _, ok := fe[id]; !ok {
			onlyBE = append(onlyBE, id)
		}
	}
	sort.Strings(onlyFE)
	sort.Strings(onlyBE)
	if len(onlyFE) > 0 {
		t.Errorf("前端列了后端没有的探针（勾了会收到 unknown probe）: %s", strings.Join(onlyFE, ", "))
	}
	if len(onlyBE) > 0 {
		t.Errorf("后端有前端选不到的探针（用户永远用不上）: %s", strings.Join(onlyBE, ", "))
	}
}
