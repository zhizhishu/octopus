package tokenbank

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// 对拍测试：testdata/reference_golden.json 由参考实现本身生成——
// testdata/gen_golden.mjs 把 lm-detector@175355e 的 shared/tokenizer-probe.ts
// （probeTokenizer，concurrency=1）用 esbuild 打包后，对 93 个模拟上游逐条跑出
// 请求序列、后验与判定。Go 移植必须逐条复现：同样的选题顺序、同样的判定种类、
// 同样的头名与置信度（浮点容差 1e-9）。
//
// 重新生成：在含 refs/lm-detector 的目录旁执行
//   npx esbuild entry.ts --bundle --format=esm --platform=node --outfile=ref.mjs
//   node gen_golden.mjs
// （entry.ts 只导出 probeTokenizer / tokenizerPosterior，见 gen_golden.mjs 顶部 import）。

type goldenCase struct {
	Name     string         `json:"name"`
	Model    string         `json:"model"`
	Baseline []int          `json:"baseline"`
	Table    map[string]int `json:"table"`
	Expect   struct {
		Steps   []*string `json:"steps"`
		Drift   bool      `json:"drift"`
		Verdict struct {
			Kind       string     `json:"kind"`
			Top        ClassScore `json:"top"`
			Confidence float64    `json:"confidence"`
			Claim      *struct {
				Expected    []string `json:"expected"`
				Vendor      string   `json:"vendor"`
				FirstParty  bool     `json:"firstParty"`
				Probability float64  `json:"probability"`
				Status      string   `json:"status"`
			} `json:"claim"`
		} `json:"verdict"`
		Unknown float64 `json:"unknown"`
		Alien   float64 `json:"alien"`
		Classes []struct {
			ID       string  `json:"id"`
			Exact    float64 `json:"exact"`
			Related  float64 `json:"related"`
			Matched  int     `json:"matched"`
			Compared int     `json:"compared"`
			Overhead int     `json:"overhead"`
		} `json:"classes"`
	} `json:"expect"`
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/reference_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []goldenCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) < 90 {
		t.Fatalf("golden cases = %d, want >= 90", len(file.Cases))
	}
	return file.Cases
}

func near(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9+1e-9*math.Abs(b)
}

// simulate 按 golden 表模拟上游：基线按调用次序取 Baseline，其余按题面反查探测 id。
func simulate(b *Bank, c goldenCase) AskFunc {
	byPrompt := map[string]string{}
	for _, p := range b.Probes {
		byPrompt[b.Wrap(p.Text)] = p.ID
	}
	calls := 0
	return func(_ context.Context, prompt string) (int, string, error) {
		if prompt == b.Wrap("") {
			i := calls
			if i >= len(c.Baseline) {
				i = len(c.Baseline) - 1
			}
			calls++
			return c.Baseline[i], "x", nil
		}
		return c.Table[byPrompt[prompt]], "x", nil
	}
}

func TestProbeMatchesReferenceImplementation(t *testing.T) {
	b, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range loadGolden(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			run := b.Probe(context.Background(), simulate(b, c), c.Model, 0)
			if run.Err != nil {
				t.Fatalf("run error: %v", run.Err)
			}
			got := make([]string, len(run.Steps))
			for i, s := range run.Steps {
				if s.Baseline {
					got[i] = "<baseline>"
				} else {
					got[i] = s.Probe
				}
			}
			want := make([]string, len(c.Expect.Steps))
			for i, s := range c.Expect.Steps {
				if s == nil {
					want[i] = "<baseline>"
				} else {
					want[i] = *s
				}
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("steps\n got %v\nwant %v", got, want)
			}
			if run.BaselineDrift != c.Expect.Drift {
				t.Fatalf("drift = %v, want %v", run.BaselineDrift, c.Expect.Drift)
			}
			p := run.Posterior
			if !near(p.Unknown, c.Expect.Unknown) || !near(p.Alien, c.Expect.Alien) {
				t.Fatalf("unknown/alien = %v/%v, want %v/%v", p.Unknown, p.Alien, c.Expect.Unknown, c.Expect.Alien)
			}
			for i, wc := range c.Expect.Classes {
				gc := p.Classes[i]
				if gc.ID != wc.ID || !near(gc.Exact, wc.Exact) || !near(gc.Related, wc.Related) ||
					gc.Matched != wc.Matched || gc.Compared != wc.Compared || gc.Overhead != wc.Overhead {
					t.Fatalf("class[%d] = %+v, want %+v", i, gc, wc)
				}
			}
			v := run.Verdict
			wv := c.Expect.Verdict
			if v.Kind != wv.Kind || v.Top.ID != wv.Top.ID || !near(v.Confidence, wv.Confidence) {
				t.Fatalf("verdict = %s/%s/%v, want %s/%s/%v", v.Kind, v.Top.ID, v.Confidence, wv.Kind, wv.Top.ID, wv.Confidence)
			}
			if (v.Claim == nil) != (wv.Claim == nil) {
				t.Fatalf("claim presence = %v, want %v", v.Claim != nil, wv.Claim != nil)
			}
			if v.Claim != nil {
				if v.Claim.Status != wv.Claim.Status || v.Claim.FirstParty != wv.Claim.FirstParty ||
					v.Claim.Vendor != wv.Claim.Vendor || !near(v.Claim.Probability, wv.Claim.Probability) ||
					strings.Join(v.Claim.Expected, ",") != strings.Join(wv.Claim.Expected, ",") {
					t.Fatalf("claim = %+v, want %+v", *v.Claim, *wv.Claim)
				}
			}
		})
	}
}
