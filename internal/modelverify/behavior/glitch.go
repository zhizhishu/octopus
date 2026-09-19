package behavior

import (
	"regexp"
	"sort"
	"strings"
)

// glitchTokens 是 15 个已知会让特定模型"咬到舌头"的 token。
//
// 数据来源：AI-Infra-Guard（Apache-2.0）services/api_checker/algorithms/relay_audit.py，
// 原样摘录，未做增删。它们之所以有效，是因为这些串在多数分词器里会被切成一堆
// 无意义碎片，而不同模型的分词器与训练语料决定了它们**记错哪几条**。
//
// ⚠️ 这是弱指纹，且会随模型版本过期。上游注释原文：「该信号依赖模型版本，只能作为
// 弱指纹使用。例如 MiMo v2.5 已可能不再复现编号 1，因此匹配时同时保留 overlap 信息。」
// 所以匹配结果必须带 coverage/confidence，绝不允许把「没匹配上」读成「不是这个模型」。
var glitchTokens = []string{
	".DataGridViewColumnHeadersHeightSizeMode",
	"日以上更新していないブログに表示しています",
	"锅内倒入植物油烧热",
	"开通天眼生意通银牌及以上会员",
	"百度百科内容由网友共同编辑",
	"EDMFunc",
	"StarSrvGroupBody",
	"给主人留下些什么吧",
	"方も多いのではないでしょうか",
	"转载请附上原文出处链接和本声明",
	"不代表新浪看点观点或立场",
	"豫冠薰衣草疤痕精华素",
	"请问 everydaycalculation是什么意思",
	"百度百科企业词条极速创建通道",
	"intFragmentation",
}

// glitchFamilySignatures 家族 → 该家族观察到会记错的编号集合（1-based，对应 glitchTokens）。
//
// 同为上游原样摘录。注意 mimo / openai 只有一个编号，单点证据极弱；
// moonshot 有三个，是本表里相对最结实的。
var glitchFamilySignatures = map[string][]int{
	"mimo":     {1},
	"minimax":  {2, 9},
	"zhipu":    {3, 14},
	"qwen":     {4, 10},
	"moonshot": {5, 11, 12},
	"deepseek": {6, 13},
	"google":   {7, 15},
	"openai":   {8},
}

// GlitchTokenCount 返回词表长度（15）。探针用它生成题面编号。
func GlitchTokenCount() int { return len(glitchTokens) }

// GlitchToken 返回第 i 个 token（1-based）。越界返回空串。
func GlitchToken(i int) string {
	if i < 1 || i > len(glitchTokens) {
		return ""
	}
	return glitchTokens[i-1]
}

// glitchTextNormalizer 抹掉空白与中英文引号。
//
// 上游做法：re.sub(r'[\s"“”]+', "", text).lower()。模型复述时插入的换行、
// 缩进和它自己加的引号都与"是否记错"无关，不抹掉就会把格式差异误判成内容差异。
var glitchTextNormalizer = regexp.MustCompile(`[\s"“”]+`)

func normalizeGlitchText(s string) string {
	return strings.ToLower(glitchTextNormalizer.ReplaceAllString(s, ""))
}

// GlitchCandidate 是一个家族针对当前失败编号的匹配结果。
//
// 排序与判定语义：
//   - Exact：失败编号集合与该家族签名**完全相同**（最强，但样本少时也最脆）
//   - Consistent：失败集合非空且**是签名的子集**（无签名外编号，可信度够用）
//   - Confidence：失败编号里有多少落在签名内（overlap / len(failed)）
//   - Coverage：签名有多少被覆盖（overlap / len(signature)）
//
// 上游只在 consistent 为真时才把家族不匹配记成一条风险，理由很直白：
// 失败了签名之外的编号，说明模型的错法根本不符合这个家族的"特征错法"，
// 那更可能是题目没做对（截断、拒答、格式乱），而不是模型身份有问题。
type GlitchCandidate struct {
	Family           string
	MatchedIndices   []int
	SignatureIndices []int
	Exact            bool
	Consistent       bool
	Coverage         float64
	Confidence       float64
}

// MatchGlitchFamilies 把失败编号集合与各家族签名比对，按强度降序返回候选。
//
// failed 为空时返回 nil：没有失败项就没有指纹可言，这不是"匹配上了所有家族"。
func MatchGlitchFamilies(failed []int) []GlitchCandidate {
	if len(failed) == 0 {
		return nil
	}
	failedSet := make(map[int]struct{}, len(failed))
	for _, n := range failed {
		failedSet[n] = struct{}{}
	}

	candidates := make([]GlitchCandidate, 0, len(glitchFamilySignatures))
	for family, signature := range glitchFamilySignatures {
		sigSet := make(map[int]struct{}, len(signature))
		for _, n := range signature {
			sigSet[n] = struct{}{}
		}

		var overlap []int
		for _, n := range failed {
			if _, ok := sigSet[n]; ok {
				overlap = append(overlap, n)
			}
		}
		if len(overlap) == 0 {
			continue
		}
		sort.Ints(overlap)

		// consistent：失败的每一条都在签名内（签名外的失败会让它变假）。
		consistent := len(overlap) == len(failed)
		exact := consistent && len(failed) == len(signature)
		candidates = append(candidates, GlitchCandidate{
			Family:           family,
			MatchedIndices:   overlap,
			SignatureIndices: append([]int(nil), signature...),
			Exact:            exact,
			Consistent:       consistent,
			Coverage:         round3(float64(len(overlap)) / float64(len(signature))),
			Confidence:       round3(float64(len(overlap)) / float64(len(failed))),
		})
	}

	// 强度排序。家族名兜底，保证同强度候选在不同次运行里顺序一致——
	// 否则 buildFindings 取 candidates[0] 会时好时坏。
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Exact != b.Exact {
			return a.Exact
		}
		if a.Consistent != b.Consistent {
			return a.Consistent
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if a.Coverage != b.Coverage {
			return a.Coverage > b.Coverage
		}
		return a.Family < b.Family
	})
	return candidates
}

// round3 保留三位小数，与上游 coverage/confidence 的呈现精度一致。
func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}
