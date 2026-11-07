package numberdist

import "strings"

// Parse 从**可见正文**取第一个 ASCII 数字串，返回 1..355 范围内的整数。
//
// 与 hlwy 原 extractNumber（hlwy-ai-checker.html:2252-2259）逐字对齐：
//
//	text.match(/\d+/) → parseInt → 1<=n<=355 否则 null
//
// 刻意保留的边界：
//   - 只认 ASCII 数字（JS \d 不带 u 标志）；全角数字不算数字。
//   - 取第一个数字串，不跳过它去找后面的数字（`0，然后217` → 无效）。
//   - `-7` → 7、`1.5` → 1：都是宽松解析，StrictInteger 标为 false。
//   - 超长数字溢出或超出范围一律无效；不做截断或四舍五入。
//
// 不读 ReasoningContent／工具参数补答案——调用方只应把归一后的可见正文传进来。
func Parse(visible string) ParseResult {
	start := -1
	for i := 0; i < len(visible); i++ {
		if isASCIIDigit(visible[i]) {
			start = i
			break
		}
	}
	if start == -1 {
		reason := "no_ascii_integer"
		if strings.TrimSpace(visible) == "" {
			reason = "empty_visible"
		}
		return ParseResult{Valid: false, Reason: reason}
	}

	end := start
	for end < len(visible) && isASCIIDigit(visible[end]) {
		end++
	}
	digits := visible[start:end]

	num, overflow := parseDecimal(digits)
	if overflow || num < 1 || num > DistLength {
		return ParseResult{Valid: false, Reason: "out_of_range"}
	}

	// 严格单整数：去掉首尾空白后整段就是一个整数，没有其他可见字符。
	return ParseResult{
		Number:        num,
		Valid:         true,
		StrictInteger: strings.TrimSpace(visible) == digits,
	}
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// parseDecimal 累加十进制数字，超过 100000 立即判溢出，避免整数溢出。
// 前导零按 JS parseInt 语义自然吸收（`0000217` → 217）。
func parseDecimal(digits string) (value int, overflow bool) {
	v := 0
	for i := 0; i < len(digits); i++ {
		v = v*10 + int(digits[i]-'0')
		if v > 100000 {
			return 0, true
		}
	}
	return v, false
}
