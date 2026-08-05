package tokenizer

import (
	"strings"
	"testing"

	"github.com/tiktoken-go/tokenizer/codec"
)

// benchInput 模拟一个内容块：上层对每个内容块调一次 CountTokens。
var benchInput = strings.Repeat("The quick brown fox jumps over the lazy dog. 你好，世界！", 20)

// countTokensPerCallCodec 是改动前的实现，只留在基准里当对照组：
// 每次调用都 codec.NewO200kBase()，其中那条巨型分词正则每次都重新编译。
func countTokensPerCallCodec(content string) int {
	enc := codec.NewO200kBase()
	tc, err := enc.Count(content)
	if err != nil {
		return 0
	}
	return tc
}

// BenchmarkCountTokensSharedCodec 改动后：复用进程级单例。
func BenchmarkCountTokensSharedCodec(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if CountTokens(benchInput, "gpt-4o") == 0 {
			b.Fatal("unexpected zero count")
		}
	}
}

// BenchmarkCountTokensPerCallCodec 改动前：每次调用重建 codec（重编译正则）。
func BenchmarkCountTokensPerCallCodec(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if countTokensPerCallCodec(benchInput) == 0 {
			b.Fatal("unexpected zero count")
		}
	}
}

// BenchmarkCountTokensSharedCodecParallel 多请求并发下的共享单例表现
// （runner 池按并发度增长，之后稳定复用）。
func BenchmarkCountTokensSharedCodecParallel(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if CountTokens(benchInput, "gpt-4o") == 0 {
				b.Fatal("unexpected zero count")
			}
		}
	})
}

// BenchmarkCountTokensPerCallCodecParallel 同并发条件下的改动前行为，作为对照。
func BenchmarkCountTokensPerCallCodecParallel(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if countTokensPerCallCodec(benchInput) == 0 {
				b.Fatal("unexpected zero count")
			}
		}
	})
}

// BenchmarkNewO200kBaseConstruct 单量构造成本。词表已被库里的 sync.Once 缓存，
// 所以这里量到的基本就是那条巨型分词正则的重编译开销——也就是本次改动省掉的东西。
func BenchmarkNewO200kBaseConstruct(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if codec.NewO200kBase() == nil {
			b.Fatal("nil codec")
		}
	}
}

// BenchmarkCountTokensEmptySharedCodec / BenchmarkCountTokensEmptyPerCallCodec
// 用空串把分词工作降到最低，凸显"每次调用的固定开销"这一项差多少。
func BenchmarkCountTokensEmptySharedCodec(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		CountTokens("", "gpt-4o")
	}
}

func BenchmarkCountTokensEmptyPerCallCodec(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		countTokensPerCallCodec("")
	}
}
