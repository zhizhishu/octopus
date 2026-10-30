package tokenbank

import (
	"fmt"
	"regexp"
	"strings"
)

// 本地可信基线：给"厂商没公开分词器"的型号（库里 class=null，如 Claude / Grok）补一个类。
//
// 上游库的每个类都是离线拿开放分词器算出来的差分计数；闭源型号没有可下载的分词器，
// 参考实现对它们只能判"库外 = 相容"。这里提供同格式的补充：管理员在**自己确认可信**
// 的渠道上把 72 条探测文本全量发一遍（Sweep），得到与库同口径的差分计数
// count(P t S) − count(P S)，存成一个本地类。之后检测声明该型号的渠道时，
// 这个本地类按厂商映射（FirstParty，只认精确吻合）参与核对。
//
// 这是**机制**，不附带任何数据：仓库里没有、也不会编造闭源型号的计数。
// 没录制过的型号行为与参考实现完全一致。

// LocalBaseline 一条本地录制的分词器基线。
type LocalBaseline struct {
	// Model 该基线代表的型号（按 NormalizeModel 归一后精确匹配）。
	Model string
	// Label 展示名。
	Label string
	// Counts 探测文本 id → 差分计数，必须覆盖库里全部探测文本。
	Counts map[string]int
	// BankGeneratedAt 录制时所用库的 generated_at；与当前库不一致即拒用（题面可能已变）。
	BankGeneratedAt string
}

// LocalLab 本地类的厂商标识。
const LocalLab = "local"

// LocalClassID 本地类的 id。
func LocalClassID(model string) string {
	return "local:" + NormalizeModel(model)
}

// DifferentialCounts 把一次全量 Sweep 的结果转成差分计数。
// 必须：无错误、首尾基线一致（无漂移）、每条探测文本恰好答复一次。
func (b *Bank) DifferentialCounts(run RunResult) (map[string]int, error) {
	if run.Err != nil {
		return nil, fmt.Errorf("录制未完成: %w", run.Err)
	}
	if run.BaselineDrift {
		return nil, fmt.Errorf("两次基线计数不同，上游附加的隐藏输入长度在变，不能作为基线")
	}
	baseline, haveBase := 0, false
	counts := map[string]int{}
	for _, o := range run.Observations {
		if o.Baseline {
			baseline, haveBase = o.Tokens, true
			continue
		}
		if _, dup := counts[o.Probe]; dup {
			return nil, fmt.Errorf("探测文本 %s 答复了不止一次", o.Probe)
		}
		counts[o.Probe] = o.Tokens
	}
	if !haveBase {
		return nil, fmt.Errorf("缺少基线请求")
	}
	for _, p := range b.Probes {
		v, ok := counts[p.ID]
		if !ok {
			return nil, fmt.Errorf("探测文本 %s 没有答复，基线不完整", p.ID)
		}
		counts[p.ID] = v - baseline
	}
	if len(counts) != len(b.Probes) {
		return nil, fmt.Errorf("录制结果含库外探测文本")
	}
	return counts, nil
}

// WithLocal 返回合入本地基线后的新库（原库不变）。
//
// 本地类追加到类表末尾；对应的型号映射插到 api_models 最前面（精确型号，优先于
// `^claude` → null 这类厂商通配），FirstParty 语义：只有精确吻合才算一致。
// 与当前库版本不符、计数不完整、型号为空的基线一律拒绝（返回错误，不静默跳过）。
func (b *Bank) WithLocal(locals []LocalBaseline) (*Bank, error) {
	if len(locals) == 0 {
		return b, nil
	}
	out := &Bank{
		Schema:      b.Schema,
		GeneratedAt: b.GeneratedAt,
		Source:      b.Source,
		Wrapper:     b.Wrapper,
		Probes:      b.Probes,
		Classes:     append([]Class(nil), b.Classes...),
	}
	var models []APIModel
	seen := map[string]bool{}
	for _, l := range locals {
		id := NormalizeModel(l.Model)
		if id == "" {
			return nil, fmt.Errorf("本地基线缺少型号")
		}
		if seen[id] {
			return nil, fmt.Errorf("型号 %s 有不止一条本地基线", id)
		}
		seen[id] = true
		if l.BankGeneratedAt != b.GeneratedAt {
			return nil, fmt.Errorf("型号 %s 的本地基线录制于分词器库 %s，与当前库 %s 不符，需重新录制",
				id, l.BankGeneratedAt, b.GeneratedAt)
		}
		counts := make([]int, len(b.Probes))
		for j, p := range b.Probes {
			v, ok := l.Counts[p.ID]
			if !ok {
				return nil, fmt.Errorf("型号 %s 的本地基线缺少探测文本 %s", id, p.ID)
			}
			counts[j] = v
		}
		classID := LocalClassID(id)
		label := strings.TrimSpace(l.Label)
		if label == "" {
			label = id
		}
		out.Classes = append(out.Classes, Class{
			ID: classID, Lab: LocalLab, LabName: "本地可信基线",
			Series: label, SeriesZh: label,
			Members: []string{id}, Aliases: []string{}, Counts: counts, Local: true,
		})
		cls := classID
		models = append(models, APIModel{
			Pattern: "^" + regexp.QuoteMeta(id) + "$",
			Vendor:  "本地可信基线",
			Class:   &cls,
		})
	}
	out.APIModels = append(models, b.APIModels...)
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}
