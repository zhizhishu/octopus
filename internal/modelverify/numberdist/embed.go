package numberdist

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sync"
)

// builtinFS 内嵌 hlwy 基准原始字节（LGPL-2.1，见 data/hlwy/README.md 与 LICENSE）。
//
//go:embed data/hlwy/*.json
var builtinFS embed.FS

const builtinDir = "data/hlwy"

// builtinSHA256 是内嵌文件的固定摘要清单；加载时逐个校验，防止数据被静默改动。
var builtinSHA256 = map[string]string{
	"index.json":                    "f73ef6c3f5cfeaf7ba217c0a50aeaf49d90338d1b6a70c09fc4311db5e0c8afe",
	"claude-fable-5-1.json":         "000f238d644c4316a6f7126ff396f9bc8f3a90d700186aecab77eb6ad799be41",
	"claude-opus-5-5.json":          "15a405211dd3ea18b8090486f6929beee4f9443d87e8366ed27d45821b17bc42",
	"claude-opus-5.json":            "8d7bc644412b0f282869930300afe9243d73185901ea03ebd7779b868e8914e6",
	"deepseek-v4.1-flash-1017.json": "18713b9605ead32ee58eae43fd1408b4657a12b8ddf0f1f343f1cc4ad20bbd5d",
	"deepseek-v4.1-flash.json":      "75662fa602b5e85e20915116379d345bf225762a22b4a01404642a7709a78aa9",
	"gpt-5.6-sol.json":              "60fc9745bd141faff6a193ed062dd9d01ed5be579eb9e1530c5389eefb58ce3d",
	"gpt-6-astra.json":              "db64ce89111b3e15c7a004dfcd5cf274019e450f2f06d11b4b4c4474f56167ab",
}

// builtinSource 是内置数据的固定来源声明（固定 commit，不随构建变化）。
var builtinSource = Source{
	Kind:   SourceBuiltin,
	Repo:   "hlwy",
	Commit: "7bddead107da09ddec459522df4b74eb0c7f8ac6",
	Path:   builtinDir,
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var (
	builtinOnce sync.Once
	builtinData []Baseline
	builtinErr  error
)

// LoadBuiltins 加载 7 份内嵌基准，返回相互独立的深拷贝（调用方可安全改动）。
//
// 校验：index.json 列出的每份文件 sha256 必须与清单一致；解析后必须恰好 7 份、
// ID 互不相同、分布合法。任一失败返回错误，绝不返回半份数据。
func LoadBuiltins() ([]Baseline, error) {
	builtinOnce.Do(func() {
		builtinData, builtinErr = loadBuiltins()
	})
	if builtinErr != nil {
		return nil, builtinErr
	}
	return cloneAll(builtinData), nil
}

func loadBuiltins() ([]Baseline, error) {
	indexRaw, ok := builtinFile("index.json")
	if !ok {
		return nil, fmt.Errorf("缺少或损坏内置基准清单 index.json")
	}
	var names []string
	if err := decodeJSON(indexRaw, &names); err != nil {
		return nil, fmt.Errorf("解析内置清单失败: %w", err)
	}
	if len(names) != 7 {
		return nil, fmt.Errorf("内置基准应为 7 份，实测 %d", len(names))
	}

	out := make([]Baseline, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		raw, ok := builtinFile(name + ".json")
		if !ok {
			return nil, fmt.Errorf("缺少或损坏内置基准 %s.json", name)
		}
		src := builtinSource
		src.Path = builtinDir + "/" + name + ".json"
		b, err := buildBaseline(raw, src, DefaultMaxNumbers, true)
		if err != nil {
			return nil, fmt.Errorf("内置基准 %s 解析失败: %w", name, err)
		}
		if seen[b.ID] {
			return nil, fmt.Errorf("内置基准 ID 重复: %s", b.ID)
		}
		seen[b.ID] = true
		out = append(out, b)
	}
	if len(out) != 7 {
		return nil, fmt.Errorf("内置基准应为 7 份，实测 %d", len(out))
	}
	return out, nil
}

// builtinFile 读取内嵌文件并校验 sha256 清单；不符/缺失返回 ok=false。
func builtinFile(name string) ([]byte, bool) {
	want, ok := builtinSHA256[name]
	if !ok {
		return nil, false
	}
	raw, err := builtinFS.ReadFile(builtinDir + "/" + name)
	if err != nil {
		return nil, false
	}
	if hashHex(raw) != want {
		return nil, false
	}
	return raw, true
}

// clone 返回基准的深拷贝：切片、可空 Conditions 指针（temperature/top_p/stream/
// max_tokens/system_injected/tools_injected）与 CollectedAt 都复制，避免调用方改动污染
// 内嵌缓存（即使当前内置值为 nil，公开不可变性不能因将来加入已知值而破）。
func (b Baseline) clone() Baseline {
	c := b
	if b.RawNumbers != nil {
		c.RawNumbers = append([]int(nil), b.RawNumbers...)
	}
	if b.Conditions.UnknownFields != nil {
		c.Conditions.UnknownFields = append([]string(nil), b.Conditions.UnknownFields...)
	}
	c.Conditions.Temperature = cloneFloatPtr(b.Conditions.Temperature)
	c.Conditions.TopP = cloneFloatPtr(b.Conditions.TopP)
	c.Conditions.Stream = cloneBoolPtr(b.Conditions.Stream)
	c.Conditions.MaxTokens = cloneIntPtr(b.Conditions.MaxTokens)
	c.Conditions.SystemInjected = cloneBoolPtr(b.Conditions.SystemInjected)
	c.Conditions.ToolsInjected = cloneBoolPtr(b.Conditions.ToolsInjected)
	if b.CollectedAt != nil {
		t := *b.CollectedAt
		c.CollectedAt = &t
	}
	return c
}

func cloneFloatPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneAll(bs []Baseline) []Baseline {
	out := make([]Baseline, len(bs))
	for i, b := range bs {
		out[i] = b.clone()
	}
	return out
}
