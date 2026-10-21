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

// normalizeEffortKey 统一档位大小写与空白，避免 "MAX" 之类绕过映射表。
func normalizeEffortKey(effort string) string {
	return strings.TrimSpace(strings.ToLower(effort))
}
