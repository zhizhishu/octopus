package fingerprint

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, ", ")
}

func goldenTexts(t *testing.T) ([]string, []int) {
	t.Helper()
	var golden []struct {
		Texts         []string `json:"texts"`
		ExpectedCount []int    `json:"expected_count"`
	}
	loadJSON(t, "g_full.json", &golden)
	return golden[0].Texts, golden[0].ExpectedCount
}

// 真模型回答必须过关：误伤真模型比放过脚本更糟。
func TestSequenceGatePassesRealModelReplies(t *testing.T) {
	texts, expected := goldenTexts(t)
	outputs := make([]Output, len(texts))
	for i := range texts {
		outputs[i] = Output{Text: texts[i], ExpectedCount: expected[i]}
	}
	a, err := Analyze(outputs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Decision != DecisionScorable || a.Sequence == nil || !a.Sequence.ModelLike {
		t.Fatalf("真模型回答被挡: decision=%s seq=%+v", a.Decision, a.Sequence)
	}
	if len(a.AnomalousSamples) != 0 {
		t.Fatalf("真模型回答不该被标异常: %v", a.AnomalousSamples)
	}
	if a.ModelCount != 64 {
		t.Fatalf("参考库应有 64 个模型，得到 %d", a.ModelCount)
	}
}

// 排序过的数字、伪随机脚本的数字都必须判"不像模型手写"，且不给排名。
func TestSequenceGateRejectsScriptAndSorted(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	scripted := func() []int {
		xs := make([]int, 300)
		for i := range xs {
			xs[i] = 1 + rng.Intn(355)
		}
		return xs
	}
	sortedOf := func() []int { xs := scripted(); sort.Ints(xs); return xs }

	for name, gen := range map[string]func() []int{"sorted": sortedOf} {
		outputs := []Output{{Text: joinInts(gen()), ExpectedCount: 300}, {Text: joinInts(gen()), ExpectedCount: 300}, {Text: joinInts(gen()), ExpectedCount: 300}}
		a, err := Analyze(outputs)
		if err != nil {
			t.Fatal(err)
		}
		if a.Decision != DecisionNotModel {
			t.Fatalf("%s: 应判 not_model，得到 %s (%+v)", name, a.Decision, a.Sequence)
		}
		if len(a.Results) != 0 || a.Probability != nil {
			t.Fatalf("%s: not_model 不该给排名或置信度", name)
		}
	}

	// 均匀伪随机：自相关约 0（不够负），小步占比约 11%（> 5%），两项同时越界。
	shape := sequenceShape([][]int{scripted(), scripted(), scripted()})
	if shape.ModelLike {
		t.Fatalf("脚本均匀随机数不该像模型手写: %+v", shape)
	}
}

func TestAnomalousSampleFlagged(t *testing.T) {
	texts, expected := goldenTexts(t)
	high := make([]int, 300)
	for i := range high {
		// 来回大跳但全部 ≥ 200：形态像模型，取值却挤在上段。
		if i%2 == 0 {
			high[i] = 200 + (i*37)%40
		} else {
			high[i] = 315 + (i*11)%40
		}
	}
	outputs := []Output{
		{Text: texts[0], ExpectedCount: expected[0]},
		{Text: joinInts(high), ExpectedCount: 300},
		{Text: texts[2], ExpectedCount: expected[2]},
	}
	a, err := Analyze(outputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.AnomalousSamples) != 1 || a.AnomalousSamples[0] != 1 {
		t.Fatalf("应标出第 2 条异常，得到 %v", a.AnomalousSamples)
	}
	if !a.Diagnostics[1].Anomalous || a.Diagnostics[1].MinimumValue < 200 {
		t.Fatalf("诊断未标异常: %+v", a.Diagnostics[1])
	}
}

func TestGeneratedChallengesAreRandomAndWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for round := 0; round < 40; round++ {
		list := GenerateChallenges(3)
		if len(list) != 3 {
			t.Fatalf("应生成 3 道，得到 %d", len(list))
		}
		counts := map[int]bool{}
		for _, c := range list {
			if c.ExpectedCount < generatedMinCount || c.ExpectedCount > generatedMaxCount {
				t.Fatalf("数量越界: %d", c.ExpectedCount)
			}
			if counts[c.ExpectedCount] {
				t.Fatalf("同一轮数量重复: %d", c.ExpectedCount)
			}
			counts[c.ExpectedCount] = true
			if c.Condition != ConditionRandom || LanguageNames[c.Language] == "" {
				t.Fatalf("题目元数据不全: %+v", c)
			}
			if !strings.Contains(c.Prompt, fmt.Sprint(c.ExpectedCount)) || !strings.Contains(c.Prompt, "355") {
				t.Fatalf("题面缺少数量或范围: %q", c.Prompt)
			}
			if c.PromptWithPrefix() != c.Prompt {
				t.Fatal("随机题不应带前缀")
			}
			seen[c.Prompt] = true
		}
	}
	if len(seen) < 100 {
		t.Fatalf("120 道题只有 %d 种不同措辞，随机性不够", len(seen))
	}
}

// 每种语言的每个变体都要能渲染出完整题面（固定随机源走遍所有分支）。
func TestEveryPromptVariantRenders(t *testing.T) {
	for _, b := range promptBundles {
		for i := 0; i < 64; i++ {
			k := i
			intn := func(n int) int { k = (k*31 + 7) % 1000; return k % n }
			p := b.render(300, intn)
			if strings.Count(p, "300") != 1 {
				t.Fatalf("%s 题面应恰好含一次数量: %q", b.code, p)
			}
		}
	}
}
