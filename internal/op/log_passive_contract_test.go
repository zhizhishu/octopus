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
		BedrockScore:    1,
		VertexScore:     2,
		CounterEvidence: []string{"missing service_tier"},
		Signals:         []string{"service_tier"},
	}
	b, _ := json.Marshal(src)
	js := string(b)
	for _, k := range []string{"anthropic_score", "bedrock_score", "vertex_score", "counter_evidence", "signals"} {
		if !strings.Contains(js, `"`+k+`"`) {
			t.Fatalf("passive_source 缺少键 %q（前端类型里声明的是这个）: %s", k, js)
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
