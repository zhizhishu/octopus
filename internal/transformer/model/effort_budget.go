package model

import "strings"

// 思考强度（effort）↔ 思考预算（thinking budget）的唯一真源。
//
// 为什么需要这个文件：改动前同一件事在四个地方各有一套互不相同的数字
//
//	入站解码   thinkingBudgetToReasoningEffort   5000 / 15000 / else→high（无 max）
//	死映射表   defaultReasoningEffortMapping     5000 / 15000 / 30000（无 max，零调用）
//	anthropic 出站 getThinkingBudget             1024 / 8192 / 32768 / 64000
//	gemini 出站 reasoningToThinkingBudget        1024 / 4096 / 24576（max→-1 丢失）
//
// 四列里没有任何两列相等，而且入站那套连 max 都产不出来。
// 后果是「同一条客户端请求换个通道，强度就变了」，日志里却看不出异常。
//
// 现在把「档位 → 预算」这一层收敛到这里。各出站转换器仍然可以有自己的
// 表达方式（gemini 的 thinkingLevel 枚举、volcengine 的开关），但**数字只有一个来源**。

// EffortBudget 是各档位对应的思考预算（token）。
//
// 数值取自 anthropic 侧既有的 getThinkingBudget —— 那是全仓唯一经过测试的
// 一张表，也是唯一正确处理了 max 的地方。其余三方向它对齐，而不是反过来。
//
// 注意 xhigh：它从 OpenAI Responses / codex 侧合法流入（codexEffortAllowSet
// 明确包含），必须夹在 high 与 max 之间，否则会出现「第二强的档位反而最省」。
var EffortBudget = map[string]int64{
	"low":    1024,
	"medium": 8192,
	"high":   32768,
	"xhigh":  48000,
	"max":    64000,
}

// EffortBudgetDefault 是遇到未知档位时的预算。
//
// 刻意选 high 而不是 medium：调用方明确表达了「要思考」，未知的具体档位
// 不该变成惩罚。宁可多给（可恢复），不要少给（静默降智，无法察觉）。
const EffortBudgetDefault = int64(32768)

// EffortBudgetFor 返回档位对应的预算；未知档位回落到 EffortBudgetDefault。
//
// 🔴 这是取预算的唯一入口。直接写 EffortBudget[...] 会绕过大小写与空白归一，
// 客户端发 "MAX" 就会落进未知分支、拿到默认值——比 max 低一半，
// 而且不报错、不告警，看上去完全正常。出站转换器一律走这里，别自己 switch。
func EffortBudgetFor(effort string) int64 {
	if b, ok := EffortBudget[normalizeEffortKey(effort)]; ok {
		return b
	}
	return EffortBudgetDefault
}

// EffortFromBudget 把思考预算反解成档位，用于入站把 budget_tokens 归一成 effort。
//
// 分档点直接取 EffortBudget 的值，保证「预算 → 档位 → 预算」往返不漂移：
// 出站 max 给 64000，回读 64000 仍落在 max 区间。
//
// 判定用「不超过该档预算」的升序扫描，所以任何预算都会落到最接近的档位。
func EffortFromBudget(budgetTokens int64) string {
	if budgetTokens <= 0 {
		return ""
	}
	if budgetTokens <= EffortBudget["low"] {
		return "low"
	}
	if budgetTokens <= EffortBudget["medium"] {
		return "medium"
	}
	if budgetTokens <= EffortBudget["high"] {
		return "high"
	}
	if budgetTokens <= EffortBudget["xhigh"] {
		return "xhigh"
	}
	return "max"
}

// NormalizeEffortKey 把各种写法收敛成映射表里的规范键。
//
// 统一处理三类差异，缺一类就会静默掉进默认值：
//
//	大小写   "MAX"     → "max"
//	首尾空白 " max "   → "max"
//	分隔符   "x-high"  → "xhigh"   "x_high" → "xhigh"
//
// 分隔符这一条是实测出来的真实缺口：改动前 x-high / x_high / X-HIGH /
// extra-high / extrahigh 五种写法全部落到默认值 32768，而正确答案是 48000
// （xhigh）。也就是说「第二强的档位」在带分隔符时反而比 high 更省——
// 客户端以为自己在加强推理，实际被降了一档，且全程无报错。
//
// 剥分隔符是安全的：映射表里没有任何一个键自身含 - 或 _，所以不会把两个
// 不同的合法档位压成同一个键。
func NormalizeEffortKey(effort string) string {
	key := strings.TrimSpace(strings.ToLower(effort))
	if key == "" {
		return ""
	}
	// 只剥真正可能出现的三种分隔符。用 Replacer 一次扫完，比链式
	// ReplaceAll 少建中间字符串。
	return effortKeySeparators.Replace(key)
}

var effortKeySeparators = strings.NewReplacer("-", "", "_", "", " ", "")

// normalizeEffortKey 是包内旧名，保留给同文件的历史调用点。
func normalizeEffortKey(effort string) string { return NormalizeEffortKey(effort) }

// IsKnownEffort 报告该写法是否能解析成映射表里的合法档位。
//
// 🔴 这是「这个档位我认识吗」的唯一入口。各处自己写 case 列表会漏掉大小写、
// 空白、分隔符三种变体中的至少一种，而且共享表将来新增档位时不会自动跟上。
func IsKnownEffort(effort string) bool {
	_, ok := EffortBudget[NormalizeEffortKey(effort)]
	return ok
}
