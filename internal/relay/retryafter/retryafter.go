// Package retryafter 解析上游错误里的「等多久再试」，输出统一的冷却时长。
//
// 出处（照抄改造，注释就地标注）：
//   - HTTP 头三族（Retry-After 秒/HTTP-date、x-ratelimit-reset*、anthropic-ratelimit-*-reset）
//     与 1 小时上限：gpt-load internal/health/ratelimit.go（ParseExplicitRetryAfter:11-31、
//     ParseRateLimitReset:33-58、parseResetValue:93-124、maxRateLimitResetDelay:11）。
//     ⚠ 取值逻辑差异：gpt-load 对超上限的值**丢弃当没有**；本包按用户拍板**截断到 1 小时**
//     （「按那个时间冷却，上限 1 小时」），超上限截断有测试钉死。
//   - Gemini 错误体三级解析（ErrorInfo.quotaResetTimeStamp → quotaResetDelay → RetryInfo.retryDelay，
//     复合单位 s/m/h/d）：gcli2api-learning src/api/utils.py:155-216
//     （parse_quota_reset_timestamp）。
//   - OpenAI x-ratelimit-reset-requests/tokens 的 "1s"/"6m0s" 时长格式直接喂 time.ParseDuration，
//     与 gpt-load parseResetValue 的 duration 分支同义。
package retryafter

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxWait 上限 1 小时，照 gpt-load maxRateLimitResetDelay（ratelimit.go:11）。
const MaxWait = time.Hour

// Result 是一次解析的结论。Wait=0 且 Source="" 表示「无」（没解析到/负数/乱码）。
type Result struct {
	Wait   time.Duration
	Source string // 人类可读的来源说明，供日志/调试
}

func none() Result { return Result{} }

func ok(wait time.Duration, source string) Result {
	if wait <= 0 {
		return none()
	}
	// 上限 1 小时：截断（用户拍板），不同于 gpt-load 的丢弃。
	if wait > MaxWait {
		wait = MaxWait
		source += " (clamped to 1h)"
	}
	return Result{Wait: wait, Source: source}
}

// Input 是一次上游错误的全部证据。Header 可为 nil；Body 是错误体原文
// （JSON 或纯文本，截断与否都行，解析按 best-effort）；ChannelType 目前只用于
// 日志来源标注，解析逻辑对全部渠道类型一致（各家上游头族都可能出现在任何渠道后面）。
type Input struct {
	StatusCode  int
	Header      http.Header
	Body        []byte
	ChannelType int
	Now         time.Time // 零值 = time.Now()，测试注入用
}

// Parse 提取冷却时长。优先级：
//  1. Retry-After 头（本次拒绝的明确指示，照 gpt-load 把它放第一族）
//  2. x-ratelimit-reset* / anthropic-ratelimit-*-reset 头族
//  3. Gemini 错误体三级（ErrorInfo.quotaResetTimeStamp → quotaResetDelay → RetryInfo.retryDelay）
//  4. 错误文本里的 "try again in X"/"retry after N" 之类
//
// 全部解析失败/负数/乱码 → 返回零值 Result（Wait=0, Source=""）。
func Parse(in Input) Result {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	if in.Header != nil {
		if r := parseRetryAfterHeader(in.Header, now); r.Wait > 0 {
			return r
		}
		if r := parseResetHeaders(in.Header, now); r.Wait > 0 {
			return r
		}
	}
	if len(in.Body) > 0 {
		if r := parseGeminiBody(in.Body, now); r.Wait > 0 {
			return r
		}
		if r := parseTextHint(in.Body, now); r.Wait > 0 {
			return r
		}
	}
	return none()
}

// —— Retry-After 头：秒数或 HTTP-date。照 gpt-load parseRetryAfter（ratelimit.go:75-90）——
func parseRetryAfterHeader(header http.Header, now time.Time) Result {
	var best time.Duration
	for _, value := range headerValues(header, func(name string) bool { return name == "retry-after" }) {
		trimmed := strings.TrimSpace(value)
		if seconds, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			if seconds <= 0 {
				continue
			}
			if d := time.Duration(seconds) * time.Second; d > best {
				best = d
			}
			continue
		}
		if t, err := http.ParseTime(trimmed); err == nil && t.After(now) {
			if d := t.Sub(now); d > best {
				best = d
			}
		}
	}
	if best <= 0 {
		return none()
	}
	return ok(best, "retry-after header")
}

// —— x-ratelimit-reset*（OpenAI："1s"/"6m0s"/unix 秒/毫秒/RFC3339）与
// anthropic-ratelimit-*-reset（RFC3339）。照 gpt-load parseResetValue（ratelimit.go:93-124）——
func parseResetHeaders(header http.Header, now time.Time) Result {
	var best time.Duration
	var src string
	collect := func(match func(string) bool, label string) {
		for _, value := range headerValues(header, match) {
			if d, yes := parseResetValue(strings.TrimSpace(value), now); yes && d > best {
				best = d
				src = label
			}
		}
	}
	collect(func(name string) bool {
		return name == "x-ratelimit-reset" || strings.HasPrefix(name, "x-ratelimit-reset-")
	}, "x-ratelimit-reset header")
	collect(func(name string) bool {
		return strings.HasPrefix(name, "anthropic-ratelimit-") && strings.HasSuffix(name, "-reset")
	}, "anthropic-ratelimit-reset header")
	if best <= 0 {
		return none()
	}
	return ok(best, src)
}

func parseResetValue(value string, now time.Time) (time.Duration, bool) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		if !t.After(now) {
			return 0, false
		}
		return t.Sub(now), true
	}
	// OpenAI 风格时长："1s"、"6m0s"、"1ms"。
	if dur, err := time.ParseDuration(value); err == nil {
		if dur <= 0 {
			return 0, false
		}
		return dur, true
	}
	// 纯数字：unix 毫秒 / unix 秒 / 相对秒，照 gpt-load 的量级判断。
	numeric, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	switch {
	case numeric >= 1_000_000_000_000:
		if t := time.UnixMilli(numeric); t.After(now) {
			return t.Sub(now), true
		}
	case numeric >= 1_000_000_000:
		if t := time.Unix(numeric, 0); t.After(now) {
			return t.Sub(now), true
		}
	case numeric > 0:
		return time.Duration(numeric) * time.Second, true
	}
	return 0, false
}

func headerValues(header http.Header, match func(string) bool) []string {
	var values []string
	for name, entries := range header {
		if match(strings.ToLower(name)) {
			values = append(values, entries...)
		}
	}
	return values
}

// —— Gemini 错误体三级解析。照 gcli2api src/api/utils.py:155-216：
//  1. ErrorInfo.metadata.quotaResetTimeStamp（绝对时间戳 RFC3339，最精确）
//  2. ErrorInfo.metadata.quotaResetDelay（复合时长 "154h21m14.468080073s"）
//  3. RetryInfo.retryDelay（纯秒数带小数 "555674.468080073s"）
type geminiErrorBody struct {
	Error struct {
		Details []struct {
			Type       string            `json:"@type"`
			Metadata   map[string]string `json:"metadata"`
			RetryDelay string            `json:"retryDelay"`
		} `json:"details"`
	} `json:"error"`
}

var durationPart = regexp.MustCompile(`(\d+(?:\.\d+)?)([smhd])`)

func parseCompoundDuration(value string) (time.Duration, bool) {
	unitToSeconds := map[string]float64{"s": 1, "m": 60, "h": 3600, "d": 86400}
	parts := durationPart.FindAllStringSubmatch(value, -1)
	if len(parts) == 0 {
		return 0, false
	}
	var seconds float64
	for _, part := range parts {
		v, err := strconv.ParseFloat(part[1], 64)
		if err != nil {
			return 0, false
		}
		seconds += v * unitToSeconds[part[2]]
	}
	if seconds <= 0 {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

func parseGeminiBody(body []byte, now time.Time) Result {
	var parsed geminiErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return none()
	}
	var metadata map[string]string
	var retryDelay string
	for _, detail := range parsed.Error.Details {
		switch detail.Type {
		case "type.googleapis.com/google.rpc.ErrorInfo":
			metadata = detail.Metadata
		case "type.googleapis.com/google.rpc.RetryInfo":
			retryDelay = detail.RetryDelay
		}
	}
	// 1. quotaResetTimeStamp 绝对时间
	if metadata != nil {
		if ts := strings.TrimSpace(metadata["quotaResetTimeStamp"]); ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(now) {
				return ok(t.Sub(now), "gemini quotaResetTimeStamp")
			}
		}
	}
	// 2. quotaResetDelay 复合相对时长
	if metadata != nil {
		if delay := strings.TrimSpace(metadata["quotaResetDelay"]); delay != "" {
			if d, yes := parseCompoundDuration(delay); yes {
				return ok(d, "gemini quotaResetDelay")
			}
		}
	}
	// 3. RetryInfo.retryDelay
	if retryDelay != "" {
		if d, yes := parseCompoundDuration(retryDelay); yes {
			return ok(d, "gemini RetryInfo.retryDelay")
		}
	}
	return none()
}

// —— 错误文本里的口头提示："try again in 5s"、"retry after 30 seconds"、
// "please retry after 1.5 ms" 等。自实现（十家参考里没人做文本 fallback，gcli2api
// 也只解析结构化字段）——只匹配显式带单位的短句，单位覆盖 ms/s/m/h，避免误吞。——
// 单位全写/缩写都收（"42 seconds"、"3.5s"、"500ms"），长词在前避免 "m" 抢 "minutes" 的词首。
const hintUnit = `(milliseconds?|ms|seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h)\b`

var textHintPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)try again in\s+(\d+(?:\.\d+)?)\s*` + hintUnit),
	regexp.MustCompile(`(?i)retry after\s+(\d+(?:\.\d+)?)\s*` + hintUnit),
	regexp.MustCompile(`(?i)retry in\s+(\d+(?:\.\d+)?)\s*` + hintUnit),
	regexp.MustCompile(`(?i)please wait\s+(\d+(?:\.\d+)?)\s*` + hintUnit),
}

func hintUnitDuration(value float64, unit string) time.Duration {
	switch strings.ToLower(unit) {
	case "ms", "millisecond", "milliseconds":
		return time.Duration(value * float64(time.Millisecond))
	case "s", "sec", "secs", "second", "seconds":
		return time.Duration(value * float64(time.Second))
	case "m", "min", "mins", "minute", "minutes":
		return time.Duration(value * float64(time.Minute))
	case "h", "hr", "hrs", "hour", "hours":
		return time.Duration(value * float64(time.Hour))
	}
	return 0
}

func parseTextHint(body []byte, now time.Time) Result {
	// 只看前 4KB：错误细节里的提示句都在开头，扫全文只会增加误匹配面。
	text := string(body)
	if len(text) > 4096 {
		text = text[:4096]
	}
	for _, pattern := range textHintPatterns {
		m := pattern.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil || v <= 0 {
			continue
		}
		if d := hintUnitDuration(v, m[2]); d > 0 {
			return ok(d, "error text hint")
		}
	}
	return none()
}
