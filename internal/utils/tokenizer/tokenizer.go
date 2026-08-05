package tokenizer

import (
	"sync"

	"github.com/tiktoken-go/tokenizer/codec"
)

// o200kBase 进程级复用的编解码器单例，避免每次计数都重编译分词正则。
//
// 为什么要单例：codec.NewO200kBase() 里词表有 sync.Once 缓存，但那条巨型分词正则
// 每次调用都会重新 regexp2.MustCompile
// （tokenizer@v0.7.0 codec/o200k_base.go:6 是 Once，codec/o200k_base.go:8-10 是每次都编）。
// 上层对每个内容块调一次 CountTokens，长对话一个请求就要重编译几百次。
//
// 为什么单例够用、无需 sync.Pool（依据 regexp2@v1.11.5 与 tokenizer@v0.7.0 源码）：
//   - regexp2.Regexp 的匹配是并发安全的：runner 缓存由 muRun *sync.Mutex 保护
//     （regexp2 regexp.go:50-51），getRunner/putRunner 进出都加锁
//     （regexp2 runner.go:1585-1599、1605-1613），每次匹配借一个 runner、defer 归还
//     （regexp2 runner.go:76-91），所有可变的匹配状态都在借出的 runner 里；
//     code/caps/capsize 编译后只读（regexp2 regexp.go:38 注释 "read-only after Compile"）。
//     tidyMatch 返回前把 r.runmatch 置 nil（regexp2 runner.go:1393-1400），
//     返回的 *Match 已与 runner 脱钩，不会被后续匹配覆盖。
//   - Codec.Count 这条路径不写任何共享状态：Count → tokenize 只读 c.splitRegexp
//     与 c.vocabulary（tokenizer codec/codec.go:23-31、47-71），
//     mergePairs 只分配局部切片（同文件 97-146）。
//
// ⚠️ 约束：Codec.Decode 会惰性写 c.reverseVocabulary 且没有任何同步
// （tokenizer codec/codec.go:73-79）。本单例只许走 Count/Encode 这类只读路径；
// 将来若要 Decode，必须另行加锁或改回每次新建 / sync.Pool。
var (
	o200kBaseOnce sync.Once
	o200kBase     *codec.Codec
)

// getO200kBase 惰性初始化并返回共享的 o200k_base 编解码器。
func getO200kBase() *codec.Codec {
	o200kBaseOnce.Do(func() {
		o200kBase = codec.NewO200kBase()
	})
	return o200kBase
}

func CountTokens(content, model string) int {
	// TODO 更多模型
	tc, err := getO200kBase().Count(content)
	if err != nil {
		return 0
	}
	return tc
}
