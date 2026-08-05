package tokenizer

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/tiktoken-go/tokenizer/codec"
)

// goldenCases 固定输入 → 固定 token 数，锁死"改成单例后计数不变"。
// 期望值由改动前的实现（每次 codec.NewO200kBase()）实测得出。
var goldenCases = []struct {
	name  string
	input string
	want  int
}{
	{"empty", "", 0},
	{"ascii_short", "hello world", 2},
	{"ascii_sentence", "Hello, world! This is a plain ASCII sentence.", 11},
	{"chinese", "你好，世界！这是一段中文测试文本。", 11},
	{"json", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, 21},
	{"newlines_tabs", "line one\nline two\r\nline three\t tabbed", 11},
	{"emoji_mixed", "emoji 🐙🚀 mixed with 中文 and ASCII", 11},
	{"url", "https://upstream.example/v1/messages", 8},
	{"repeated_char", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 5},
	{"long_ascii", "The quick brown fox jumps over the lazy dog. " +
		"The quick brown fox jumps over the lazy dog. " +
		"The quick brown fox jumps over the lazy dog.", 30},
}

func TestCountTokensGolden(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountTokens(tc.input, "gpt-4o"); got != tc.want {
				t.Fatalf("CountTokens(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestCountTokensMatchesFreshCodec 直接对照改动前的行为：
// 每次新建 codec 的计数结果，必须与复用单例的结果逐条相等。
func TestCountTokensMatchesFreshCodec(t *testing.T) {
	inputs := make([]string, 0, len(goldenCases)+64)
	for _, tc := range goldenCases {
		inputs = append(inputs, tc.input)
	}
	inputs = append(inputs, randomInputs(64, 1)...)

	for i, in := range inputs {
		// 改动前的写法：每次调用都重新构造 codec（含重编译分词正则）。
		fresh, err := codec.NewO200kBase().Count(in)
		if err != nil {
			t.Fatalf("fresh codec Count failed on input %d: %v", i, err)
		}
		if got := CountTokens(in, "gpt-4o"); got != fresh {
			t.Fatalf("input %d (%q): shared codec = %d, fresh codec = %d", i, truncate(in), got, fresh)
		}
	}
}

// TestGetO200kBaseSingleton 证明确实只构造一次（正则也就只编译一次）。
func TestGetO200kBaseSingleton(t *testing.T) {
	a := getO200kBase()
	b := getO200kBase()
	if a == nil {
		t.Fatal("getO200kBase() returned nil")
	}
	if a != b {
		t.Fatalf("getO200kBase() returned different instances: %p vs %p", a, b)
	}
}

// TestCountTokensConcurrent 并发跑同一个共享 codec，配合 -race 验证无数据竞争，
// 且每个 goroutine 拿到的计数与串行结果一致。
func TestCountTokensConcurrent(t *testing.T) {
	const (
		goroutines = 32
		iterations = 40
	)

	extra := randomInputs(16, 7)

	var wg sync.WaitGroup
	errCh := make(chan string, goroutines*iterations)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				tc := goldenCases[(g+i)%len(goldenCases)]
				if got := CountTokens(tc.input, "gpt-4o"); got != tc.want {
					errCh <- fmt.Sprintf("goroutine %d iter %d: CountTokens(%q) = %d, want %d",
						g, i, truncate(tc.input), got, tc.want)
				}

				// 同时压一批不在 golden 表里的输入，让并发匹配路径走得更杂。
				in := extra[(g*iterations+i)%len(extra)]
				if got := CountTokens(in, "gpt-4o"); got <= 0 {
					errCh <- fmt.Sprintf("goroutine %d iter %d: CountTokens(random) = %d, want > 0", g, i, got)
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	for msg := range errCh {
		t.Error(msg)
	}
}

// TestGetO200kBaseConcurrentSingleton 并发抢单例：所有 goroutine 必须拿到同一个实例、
// 计数结果一致。（注意：本用例跑到时 once 可能已被前面的用例触发过，
// 真正的初始化期竞争由 -race 覆盖整包运行来兜。）
func TestGetO200kBaseConcurrentSingleton(t *testing.T) {
	const goroutines = 64

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]int, 0, goroutines)
		insts   = make(map[*codec.Codec]struct{})
	)

	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			inst := getO200kBase()
			n := CountTokens("hello world", "gpt-4o")
			mu.Lock()
			results = append(results, n)
			insts[inst] = struct{}{}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if len(insts) != 1 {
		t.Fatalf("expected exactly 1 codec instance, got %d", len(insts))
	}
	for i, n := range results {
		if n != 2 {
			t.Fatalf("result %d = %d, want 2", i, n)
		}
	}
}

// randomInputs 生成确定性的伪随机文本（固定种子，跑多少次都一样）。
func randomInputs(n int, seed int64) []string {
	r := rand.New(rand.NewSource(seed))
	alphabet := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 \n\t,.!?{}[]\"'中文测试内容标点符号🐙")

	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		length := 1 + r.Intn(400)
		for j := 0; j < length; j++ {
			b.WriteRune(alphabet[r.Intn(len(alphabet))])
		}
		out = append(out, b.String())
	}
	return out
}

func truncate(s string) string {
	const max = 60
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "..."
}
