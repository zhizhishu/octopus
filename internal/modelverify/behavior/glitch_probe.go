package behavior

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/modelverify"
)

// glitchPrompt 生成 glitch 探针的题面。
//
// 措辞照搬上游：明确禁止解释、翻译、改写，并要求保留编号逐行输出。
// 这三条限制都是为了让「记错的编号」成为唯一可观测差异——一旦允许自由发挥，
// 模型会用自己的话重述，正则就再也对不上原 token。
func glitchPrompt() string {
	var b strings.Builder
	b.WriteString("请逐项复述下面的内容。不要解释、翻译或改写；保留编号，每项单独一行：\n")
	for i := 1; i <= GlitchTokenCount(); i++ {
		fmt.Fprintf(&b, "%d. %s\n", i, GlitchToken(i))
	}
	return b.String()
}

// glitchMaxTokens 是本题需要的输出预算。
//
// 15 条 token 里有好几条是二三十字符的长串，逐行复述连同编号在 UTF-8 下容易
// 逼近 500 token，256 会在中途截断并把「复述不全」误判成「记错编号」。
// 上游取 1536，这里沿用。注意：Anthropic 渠道的探针预算由出站 shape 固定为
// 64000（真 claude-cli 就是这个数），本值不参与，也不会去改它。
const glitchMaxTokens = 1536

// glitchNumberedLine 匹配行首编号。分隔符接受 . ) 、 ． 四种，
// 因为模型会按自己的习惯选用半角或全角标点。
var glitchNumberedLine = regexp.MustCompile(`(?m)^\s*(\d{1,2})\s*[.)、．]\s*`)

// glitchSection 是解析出的一段回答。
type glitchSection struct {
	Index int
	Text  string
}

// parseGlitchSections 按行首编号把回答切成若干段。
//
// 只接受落在 1..15 的编号：模型偶尔会在开头写一句"好的，以下是复述："，
// 或者把某条 token 自己拆行多写了几个数字，越界值必须丢掉而不是当成新条目。
func parseGlitchSections(text string) []glitchSection {
	locs := glitchNumberedLine.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return nil
	}
	sections := make([]glitchSection, 0, len(locs))
	for i, loc := range locs {
		n, err := strconv.Atoi(text[loc[2]:loc[3]])
		if err != nil || n < 1 || n > GlitchTokenCount() {
			continue
		}
		bodyStart := loc[1]
		bodyEnd := len(text)
		if i+1 < len(locs) {
			bodyEnd = locs[i+1][0]
		}
		sections = append(sections, glitchSection{
			Index: n,
			Text:  strings.TrimSpace(text[bodyStart:bodyEnd]),
		})
	}
	return sections
}

// glitchObservation 是一次 glitch 解析的完整结果。
type glitchObservation struct {
	Sections     []glitchSection
	Indices      []int
	MatchedIdx   []int // 复述正确的编号
	FailedIdx    []int // 复述错误的编号
	Complete     bool  // 15 条全部给出且未因长度截断
	EarlyStopIdx int   // 早停编号（0 表示不适用）
	Analyzable   bool  // 是否可用于家族匹配
	FinishReason string
	Raw          string
}

// observeGlitch 解析回答并判定它是否「可分析」。
//
// 这是整个探针最容易误报的地方，上游在此踩过坑并留下了两条豁免，这里照搬：
//
//  1. 正常结束但内容不全，只要「编号从 1 连续、前 n-1 条全部复述正确、
//     第 n 条复述错误、且 n 恰好是某家族的签名编号」，就当作**可分析的早停**。
//     上游注释举的例子是 GPT-4.1-mini 会在第 8 项输出空的「8.」后以 stop 正常
//     结束——如果把它当截断丢弃，就会永久丢掉这个模型最特征化的那一条证据。
//  2. 反过来，n 不在任何家族签名里时**不**采信。否则任何一次随机的提前收尾
//     都会被包装成"指纹"。
func observeGlitch(text, finishReason string) glitchObservation {
	sections := parseGlitchSections(text)
	obs := glitchObservation{
		Sections:     sections,
		FinishReason: finishReason,
		Raw:          text,
	}
	for _, sec := range sections {
		obs.Indices = append(obs.Indices, sec.Index)
	}

	// 逐条比对：第 i 条是否复述正确。
	present := make(map[int]string, len(sections))
	for _, sec := range sections {
		present[sec.Index] = sec.Text
	}
	for i := 1; i <= GlitchTokenCount(); i++ {
		body, ok := present[i]
		if !ok {
			continue
		}
		if strings.Contains(normalizeGlitchText(body), normalizeGlitchText(GlitchToken(i))) {
			obs.MatchedIdx = append(obs.MatchedIdx, i)
		} else {
			obs.FailedIdx = append(obs.FailedIdx, i)
		}
	}

	// complete：15 条齐全、编号从 1 连续、且不是被长度上限砍断的。
	if len(present) == GlitchTokenCount() && isContinuousFromOne(obs.Indices) && !isTruncated(finishReason) {
		obs.Complete = true
		obs.Analyzable = true
		return obs
	}

	// 早停豁免：编号连续到 n，前 n-1 条全对，第 n 条错，且 n 是某家族签名编号。
	if strings.EqualFold(strings.TrimSpace(finishReason), "stop") && isContinuousFromOne(obs.Indices) {
		n := len(obs.Indices)
		if n >= 2 && n < GlitchTokenCount() {
			priorAllMatched := true
			for i := 1; i < n; i++ {
				if !containsInt(obs.MatchedIdx, i) {
					priorAllMatched = false
					break
				}
			}
			if priorAllMatched && containsInt(obs.FailedIdx, n) && isSignatureIndex(n) {
				obs.EarlyStopIdx = n
				obs.Analyzable = true
				obs.FailedIdx = []int{n}
				return obs
			}
		}
	}
	return obs
}

// runGlitch 执行 glitch 弱指纹探针。
//
// 探针自身的 OK 只表示"拿到了一份可分析的样本"。家族是否与请求一致、
// 是否构成风险，一律交给评分层。
func runGlitch(ctx context.Context, s modelverify.Sender) Result {
	resp, err := s.Ask(ctx, modelverify.Ask{Prompt: glitchPrompt(), MaxTokens: intPtr(glitchMaxTokens)})
	if err != nil {
		return Result{Err: fmt.Errorf("请求失败: %w", err)}
	}
	text := strings.TrimSpace(resp.Content)
	obs := observeGlitch(text, resp.FinishReason)

	candidates := MatchGlitchFamilies(obs.FailedIdx)
	bestFamily := ""
	if len(candidates) > 0 {
		bestFamily = candidates[0].Family
	}

	data := map[string]any{
		"complete":         obs.Complete,
		"analyzable":       obs.Analyzable,
		"early_stop_index": obs.EarlyStopIdx,
		"finish_reason":    resp.FinishReason,
		"numbered_indices": obs.Indices,
		"matched_indices":  obs.MatchedIdx,
		"failed_indices":   obs.FailedIdx,
		"candidates":       candidates,
		"best_family":      bestFamily,
		"actual":           clip(text, 2000),
	}
	return Result{OK: obs.Analyzable, Data: data}
}

// ---------------------------------------------------------------- 判定工具

// isContinuousFromOne 判断编号序列是否恰为 1..n。
//
// 必须严格连续。模型可能漏掉中间某条却继续编号，那种情况说明它没按题目做，
// 失败集合就不再反映"记错哪几条"，据此匹配家族等于凭空造指纹。
func isContinuousFromOne(indices []int) bool {
	if len(indices) == 0 {
		return false
	}
	sorted := append([]int(nil), indices...)
	for i := range sorted {
		if sorted[i] != i+1 {
			return false
		}
	}
	return true
}

func containsInt(list []int, want int) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// isSignatureIndex 判断某个编号是否出现在任一家族签名中。
//
// ⚠️ 在当前词表下这个函数**恒为真**：八个家族的签名并集恰好是 1..15 全覆盖。
// 保留它有两个理由：一是与上游的判定流程逐字对齐，便于两边对照读；二是词表
// 一旦演进（上游注释明说 glitch 信号依赖模型版本，这张表迟早要改），新编号
// 未必落在任何签名里，届时这道闸门就会开始真正起作用。
//
// 它**不**是早停豁免的主要防线。真正挡住误报的是「编号从 1 连续」与「前 n-1 条
// 全部复述正确」——随手提前收尾的回答几乎不可能同时满足这两条。
func isSignatureIndex(n int) bool {
	for _, signature := range glitchFamilySignatures {
		if containsInt(signature, n) {
			return true
		}
	}
	return false
}
