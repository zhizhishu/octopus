package op

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 后端产出的 JSON 键，必须和前端 types 里声明的键逐字一致。
// 只测 Go 侧 tag、或只跑 tsc，都发现不了"两边各自自洽但拼不上"。
func TestPassiveFieldJSONKeysMatchFrontend(t *testing.T) {
	// 1) passive_source 的键
	src := model.RelayLogPassiveSource{
		AnthropicScore:  6,
		CounterEvidence: []string{"missing service_tier"},
		Signals:         []string{"service_tier"},
	}
	b, _ := json.Marshal(src)
	js := string(b)
	for _, k := range []string{"anthropic_score", "counter_evidence", "signals"} {
		if !strings.Contains(js, `"`+k+`"`) {
			t.Fatalf("passive_source 缺少键 %q（前端类型里声明的是这个）: %s", k, js)
		}
	}

	// 🔴 反向检查：前端声明了、后端永远产不出的键，一个都不许有。
	//
	// 这里踩过：为了"对齐参考项目的三源打分"，结构体和前端都加了
	// bedrock_score / vertex_score，但没有任何代码给它们赋值（日志里压根没有
	// 响应头，分不出是哪家云）。于是界面永远显示 "Bedrock 0 / Vertex 0"——
	// 用户看到的是"检测过了，没证据"，实际是"这个检测根本不存在"。
	// 空字段比缺字段更危险：它把"没做"伪装成"做了但没发现"。
	for _, k := range []string{"bedrock_score", "vertex_score"} {
		if strings.Contains(js, `"`+k+`"`) {
			t.Fatalf("passive_source 不该有键 %q：没有任何代码给它赋值，前端显示它等于展示假信息", k)
		}
	}

	// 2) passive_summary.features 的两个新键
	f := model.RelayLogPassiveFeatures{
		Chars: 10, UpstreamFingerprintPresent: true, ServiceTier: "standard",
	}
	b2, _ := json.Marshal(f)
	js2 := string(b2)
	for _, k := range []string{"upstream_fingerprint_present", "service_tier"} {
		if !strings.Contains(js2, `"`+k+`"`) {
			t.Fatalf("passive_summary.features 缺少键 %q: %s", k, js2)
		}
	}

	// 3) 版本号必须与前端的联合类型一致（'v1' | 'v2'）
	if model.PassiveSummaryVersion != "v2" {
		t.Fatalf("后端版本号 %q，前端联合类型写的是 'v1' | 'v2'", model.PassiveSummaryVersion)
	}
}
