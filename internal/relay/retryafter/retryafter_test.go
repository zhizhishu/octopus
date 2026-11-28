package retryafter

import (
	"net/http"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

func parse(headers map[string]string, body string) Result {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return Parse(Input{StatusCode: 429, Header: h, Body: []byte(body), Now: testNow})
}

func TestRetryAfterSeconds(t *testing.T) {
	r := parse(map[string]string{"Retry-After": "30"}, "")
	if r.Wait != 30*time.Second || r.Source == "" {
		t.Fatalf("retry-after seconds: %+v", r)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	r := parse(map[string]string{"Retry-After": testNow.Add(45 * time.Second).UTC().Format(http.TimeFormat)}, "")
	if r.Wait < 44*time.Second || r.Wait > 45*time.Second {
		t.Fatalf("retry-after http date: %v", r.Wait)
	}
}

func TestOpenAIResetTokensDurationForm(t *testing.T) {
	// OpenAI 的 "6m0s"/"1s" 时长格式（x-ratelimit-reset-tokens/requests）。
	r := parse(map[string]string{"X-Ratelimit-Reset-Tokens": "6m0s"}, "")
	if r.Wait != 6*time.Minute {
		t.Fatalf("openai duration form: %v", r.Wait)
	}
	r = parse(map[string]string{"X-Ratelimit-Reset-Requests": "1s"}, "")
	if r.Wait != time.Second {
		t.Fatalf("openai 1s: %v", r.Wait)
	}
}

func TestOpenAIResetUnixSeconds(t *testing.T) {
	r := parse(map[string]string{"X-Ratelimit-Reset": "" + time.Unix(testNow.Unix()+90, 0).Format("")}, "")
	if r.Wait != 0 {
		t.Fatalf("garbage format must not parse: %+v", r)
	}
	r = parse(map[string]string{"X-Ratelimit-Reset": "90"}, "")
	if r.Wait != 90*time.Second {
		t.Fatalf("numeric relative seconds: %v", r.Wait)
	}
}

func TestAnthropicResetRFC3339(t *testing.T) {
	r := parse(map[string]string{"Anthropic-Ratelimit-Requests-Reset": testNow.Add(2 * time.Minute).Format(time.RFC3339)}, "")
	if r.Wait < 119*time.Second || r.Wait > 120*time.Second {
		t.Fatalf("anthropic rfc3339 reset: %v", r.Wait)
	}
}

func TestRetryAfterWinsOverResetHeaders(t *testing.T) {
	r := parse(map[string]string{"Retry-After": "5", "X-Ratelimit-Reset-Tokens": "6m0s"}, "")
	if r.Wait != 5*time.Second {
		t.Fatalf("retry-after must win (explicit per-rejection): %v", r.Wait)
	}
}

func TestGeminiRetryInfoRetryDelay(t *testing.T) {
	body := `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17.5s"}]}}`
	r := parse(nil, body)
	if r.Wait != 17500*time.Millisecond {
		t.Fatalf("gemini retryDelay: %v", r.Wait)
	}
}

func TestGeminiPriorityTimestampOverDelayOverRetryInfo(t *testing.T) {
	// 三级优先照 gcli2api：quotaResetTimeStamp > quotaResetDelay > retryDelay。
	body := `{"error":{"details":[
		{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetTimeStamp":"` + testNow.Add(10*time.Minute).Format(time.RFC3339) + `","quotaResetDelay":"5m0s"}},
		{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`
	r := parse(nil, body)
	if r.Wait < 9*time.Minute+59*time.Second || r.Wait > 10*time.Minute {
		t.Fatalf("timestamp must win: %v", r.Wait)
	}
	// 没有 timestamp 时 quotaResetDelay 赢
	body = `{"error":{"details":[
		{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetDelay":"154h21m14.468080073s"}},
		{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`
	r = parse(nil, body)
	if r.Wait != MaxWait {
		t.Fatalf("154h compound delay must clamp to 1h cap, got %v", r.Wait)
	}
}

func TestTextHints(t *testing.T) {
	for body, want := range map[string]time.Duration{
		`{"error":{"message":"Rate limited, please try again in 3.5s"}}`: 3500 * time.Millisecond,
		`Rate limit reached. Please retry after 2 minutes.`:              2 * time.Minute,
		`upstream says: retry after 42 seconds ...`:                      42 * time.Second,
		`please retry after 42s`:                                         42 * time.Second,
		`Try again in 500ms`:                                             500 * time.Millisecond,
		`retry in 2m`:                                                    2 * time.Minute,
	} {
		r := parse(nil, body)
		if r.Wait != want {
			t.Fatalf("body %q: wait=%v want %v", body, r.Wait, want)
		}
	}
}

func TestClampToOneHour(t *testing.T) {
	// 超上限截断（用户拍板）——gpt-load 是丢弃，这里是截断，差异已写进包注释。
	r := parse(map[string]string{"Retry-After": "7200"}, "")
	if r.Wait != MaxWait {
		t.Fatalf("2h retry-after must clamp to 1h, got %v", r.Wait)
	}
}

func TestNegativeAndGarbageReturnNone(t *testing.T) {
	for name, in := range map[string]Result{
		"negative retry-after": parse(map[string]string{"Retry-After": "-5"}, ""),
		"zero retry-after":     parse(map[string]string{"Retry-After": "0"}, ""),
		"garbage header":       parse(map[string]string{"Retry-After": "soon(tm)"}, ""),
		"empty everything":     parse(nil, ""),
		"garbage body":         parse(nil, `\xff\xfe not json at all`),
		"past http date":       parse(map[string]string{"Retry-After": testNow.Add(-time.Minute).UTC().Format(http.TimeFormat)}, ""),
		"past rfc3339 reset":   parse(map[string]string{"X-Ratelimit-Reset": testNow.Add(-time.Minute).Format(time.RFC3339)}, ""),
	} {
		if in.Wait != 0 || in.Source != "" {
			t.Fatalf("%s: must be none, got %+v", name, in)
		}
	}
}

func TestGeminiGarbageJSONFallsToText(t *testing.T) {
	// 非 Gemini 形状但带文本提示的纯文本错误。
	r := parse(nil, `Service unavailable, try again in 8s`)
	if r.Wait != 8*time.Second {
		t.Fatalf("text fallback: %v", r.Wait)
	}
}
