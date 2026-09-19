package behavior

import (
	"sort"
	"strings"
)

// 模型家族别名表。
//
// 移植自 AI-Infra-Guard（github.com/Tencent/AI-Infra-Guard，Apache-2.0）
// services/api_checker/algorithms/relay_audit.py。
//
// 用途只有一处：identity 探针把「模型自称是谁」和「请求的模型属于哪个家族」
// 做一次粗粒度比对。它刻意不判定具体型号——那属于统计指纹层，靠分布而不是靠
// 模型自己的口供。上游同样把它定为弱信号：只有请求家族与被测文本家族**都识别
// 出来**、且**交集为空**时才记录，且要求人工佐证。
//
// 匹配方式是裸子串（上游原样如此）。因此 "o1" 会命中 "option1"、"palm" 会命中
// "palmy" 这类自然语言词。这个噪声水平对弱信号可以接受；真要收紧应当引入词边界，
// 但那会让 "gpt-5.5-turbo" 这类连字符型号漏掉，所以先保持与上游一致，把收紧留到
// 有真实误报样本之后再定。
//
// 相对上游的**唯一增补**：中文品牌名（通义/千问、文心、豆包、智谱、月之暗面等）。
// 上游只收英文与拼音别名，而国内渠道的模型自称常常是纯中文（"我是通义千问"），
// 只认拼音会让 identity 探针在这些渠道上永远识别不出家族、永远保持沉默——
// 沉默在这里等于白跑一趟。新增项都选了无歧义的品牌词，不引入通用词。
var familyAliases = map[string][]string{
	"openai":    {"openai", "gpt", "o1", "o3", "o4", "chatgpt"},
	"anthropic": {"anthropic", "claude", "sonnet", "opus", "haiku"},
	"google":    {"google", "gemini", "palm", "bard"},
	"mimo":      {"mimo"},
	"minimax":   {"minimax", "海螺"},
	"qwen":      {"qwen", "tongyi", "通义", "千问"},
	"deepseek":  {"deepseek", "深度求索"},
	"zhipu":     {"glm", "zhipu", "chatglm", "智谱"},
	"moonshot":  {"kimi", "moonshot", "月之暗面"},
	"bytedance": {"doubao", "豆包"},
	"baidu":     {"ernie", "wenxin", "文心"},
	"meta":      {"llama"},
	"mistral":   {"mistral", "mixtral"},
}

// familyNames 按字典序返回家族名。
//
// 存在的意义是让 InferFamilies 的输出**稳定**：Go 的 map 遍历序是随机的，
// 直接遍历会让同一段文本在不同次运行里产出不同顺序的家族列表，进而让结果
// 无法逐字节对比、也让测试变成偶发失败。
func familyNames() []string {
	names := make([]string, 0, len(familyAliases))
	for name := range familyAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// InferFamilies 从一段文本中推断模型家族，按字典序返回命中的家族名。
//
// 返回空切片表示「看不出来」——这与「看出来但不匹配」是两回事，调用方必须
// 区分：前者应当保持沉默，后者才是信号。
func InferFamilies(text string) []string {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return nil
	}
	var out []string
	for _, name := range familyNames() {
		for _, alias := range familyAliases[name] {
			if strings.Contains(lower, alias) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// FamiliesIntersect 判断两组家族是否有交集。
//
// 任一为空时返回 false：缺一边就无从谈起「不匹配」，这正是上游那条
// 「双方家族均已识别」前置条件的实现。
func FamiliesIntersect(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, name := range a {
		set[name] = struct{}{}
	}
	for _, name := range b {
		if _, ok := set[name]; ok {
			return true
		}
	}
	return false
}
