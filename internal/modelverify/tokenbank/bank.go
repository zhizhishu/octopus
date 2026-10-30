// Package tokenbank 是分词器差分计数探针的判定层：内置分词器库、开放集贝叶斯后验、
// 自适应选题与声明核对。
//
// 移植自 lm-detector（https://github.com/Ikaleio/lm-detector，MIT，
// main @ 175355e）的 shared/tokenizer-bank.ts、shared/tokenizer-posterior.ts、
// shared/tokenizer-probe.ts；数据 data/tokenizer_bank.json 原样拷贝（来源与许可见
// data/README.md）。算法常量、判定阈值与停止条件逐项照抄，不做本地调参。
//
// 本包只做统计，不发请求：发题走 behavior 包的既有 Sender（oct 出站链路）。
package tokenbank

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/dlclark/regexp2"
)

// Schema 是唯一支持的分词器库格式。
const Schema = "tokenizer-bank-v1"

//go:embed data/tokenizer_bank.json
var bankBlob []byte

// Probe 一条探测文本。请求只发一条 user 消息：Wrapper.Prefix + Text + Wrapper.Suffix。
type Probe struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Text     string `json:"text"`
}

// Class 一个分词器类：同一谱系、在全部探测文本上计数一致的开放分词器。
// Counts[j] = count(prefix + probes[j].text + suffix) − count(prefix + suffix)，
// Alternatives[j] 列出成员之间不一致时的全部取值（任一取值都算吻合）。
type Class struct {
	ID           string           `json:"id"`
	Lab          string           `json:"lab"`
	LabName      string           `json:"lab_name"`
	Series       string           `json:"series"`
	SeriesZh     string           `json:"series_zh"`
	Members      []string         `json:"members"`
	Aliases      []string         `json:"aliases"`
	Counts       []int            `json:"counts"`
	Alternatives map[string][]int `json:"alternatives,omitempty"`
	Note         string           `json:"note,omitempty"`
	// Local 标记本类来自本地可信渠道录制的基线（非上游库）。只用于展示。
	Local bool `json:"local,omitempty"`
}

// APIModel 厂商模型名到分词器类的映射。Class 为 nil 表示厂商未公开分词器。
type APIModel struct {
	Pattern string  `json:"pattern"`
	Vendor  string  `json:"vendor"`
	Class   *string `json:"class"`
}

// Source 库的生成来源。
type Source struct {
	Tool                  string `json:"tool"`
	ArchiveManifestSHA256 string `json:"archive_manifest_sha256"`
	Tokenizers            int    `json:"tokenizers"`
	Candidates            int    `json:"candidates"`
}

// Wrapper 每条请求固定的前后缀。
type Wrapper struct {
	Prefix string `json:"prefix"`
	Suffix string `json:"suffix"`
}

// Bank 分词器库。构造后只读；Prepare 的缓存挂在实例上。
type Bank struct {
	Schema      string     `json:"schema"`
	GeneratedAt string     `json:"generated_at"`
	Source      Source     `json:"source"`
	Wrapper     Wrapper    `json:"wrapper"`
	Probes      []Probe    `json:"probes"`
	Classes     []Class    `json:"classes"`
	APIModels   []APIModel `json:"api_models"`

	once     sync.Once
	prepared *prepared
	patterns map[string]*regexp2.Regexp
}

var (
	builtinOnce sync.Once
	builtin     *Bank
	builtinErr  error
)

// Builtin 返回内置分词器库（首次调用时解析并校验）。
func Builtin() (*Bank, error) {
	builtinOnce.Do(func() {
		builtin, builtinErr = Parse(bankBlob)
	})
	return builtin, builtinErr
}

// Parse 解析并校验一份分词器库。
func Parse(raw []byte) (*Bank, error) {
	var b Bank
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("分词器库不是合法 JSON: %w", err)
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

// validate 对应 assertTokenizerBank：形状不对就在发任何请求之前失败。
func (b *Bank) validate() error {
	if b.Schema != Schema {
		return fmt.Errorf("分词器库 schema 必须是 %s", Schema)
	}
	if len(b.Probes) == 0 || len(b.Classes) < 2 {
		return fmt.Errorf("分词器库需要非空的 probes 与至少两个 classes")
	}
	seenProbe := map[string]bool{}
	for _, p := range b.Probes {
		if seenProbe[p.ID] {
			return fmt.Errorf("探测文本 %s 重复", p.ID)
		}
		seenProbe[p.ID] = true
	}
	ids := map[string]bool{}
	for _, c := range b.Classes {
		if ids[c.ID] {
			return fmt.Errorf("分词器类 %s 在库中出现不止一次", c.ID)
		}
		ids[c.ID] = true
		if len(c.Counts) != len(b.Probes) {
			return fmt.Errorf("分词器类 %s 需要为每条探测文本给出一个整数计数", c.ID)
		}
		for key := range c.Alternatives {
			j, err := strconv.Atoi(key)
			if err != nil || j < 0 || j >= len(b.Probes) {
				return fmt.Errorf("分词器类 %s 的 alternatives 键 %q 不是合法的探测下标", c.ID, key)
			}
		}
		if c.Aliases == nil {
			return fmt.Errorf("分词器类 %s 需要 aliases 数组", c.ID)
		}
	}
	if b.APIModels == nil {
		return fmt.Errorf("分词器库需要 api_models 数组")
	}
	for _, m := range b.APIModels {
		if m.Class != nil && !ids[*m.Class] {
			return fmt.Errorf("API 模型 %s 指向库中不存在的分词器类 %s", m.Pattern, *m.Class)
		}
	}
	// 预编译全部正则：坏库在计费请求发出前就失败。
	b.patterns = map[string]*regexp2.Regexp{}
	compile := func(p string) error {
		if _, ok := b.patterns[p]; ok {
			return nil
		}
		re, err := regexp2.Compile(p, regexp2.ECMAScript|regexp2.IgnoreCase)
		if err != nil {
			return fmt.Errorf("分词器库中的正则 %s 不合法: %w", p, err)
		}
		b.patterns[p] = re
		return nil
	}
	for _, m := range b.APIModels {
		if err := compile(m.Pattern); err != nil {
			return err
		}
	}
	for _, c := range b.Classes {
		for _, a := range c.Aliases {
			if err := compile(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bank) match(pattern, s string) bool {
	re := b.patterns[pattern]
	if re == nil {
		return false
	}
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// Wrap 返回某条探测文本的完整题面；text 为空即基线请求（只发前后缀）。
func (b *Bank) Wrap(text string) string {
	return b.Wrapper.Prefix + text + b.Wrapper.Suffix
}

// ProbeText 按 id 取探测文本。
func (b *Bank) ProbeText(id string) (string, bool) {
	for _, p := range b.Probes {
		if p.ID == id {
			return p.Text, true
		}
	}
	return "", false
}

// Class 按 id 取分词器类。
func (b *Bank) Class(id string) (Class, bool) {
	for _, c := range b.Classes {
		if c.ID == id {
			return c, true
		}
	}
	return Class{}, false
}
