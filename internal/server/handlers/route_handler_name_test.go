package handlers

import "strings"

// 这里放「比对 engine.Routes() 里 handler 函数名」这件事的唯一实现。
//
// 为什么单独成文件而不是留在某个用例里：2026-10-23 一天之内有**两条**互不相干的
// 测试各写了一遍朴素比较，然后**在云端同时红、本机同时绿**（videos 与 images async）。
// 同一语义写两遍判据＝埋两个同样的雷（硬规矩第 1 条）。所以收成一个来源。

// handlerNameMatches 判断 engine.Routes() 给出的函数名是否就是 want 那一层。
//
// 为什么不能只 TrimPrefix 包路径再比：gin 的 RouteInfo.Handler 在不同构建下会长成
//
//	github.com/zhizhishu/octopus/internal/server/handlers.createVideoWithMediaKind.func2
//	github.com/zhizhishu/octopus/internal/server/handlers.init.22.createVideoWithMediaKind.func2
//
// 第二段里的 "init.22" 是**包内 init 注册序号**，会随构建/文件顺序变化。
// 本地跑出来是前者、CI 上是后者 —— 这条测试因此在云端红而本地绿（2026-10-23 实测）。
// 断言一个会漂的序号等于给自己埋一条随机红。
//
// 所以只取「最后一个非 funcN 段」当函数名比：funcN 是闭包层，init.N 是注册序号，
// 两者都不是被测语义。
func handlerNameMatches(full, want string) bool {
	const pkgPrefix = "github.com/zhizhishu/octopus/internal/server/handlers."
	name := strings.TrimPrefix(full, pkgPrefix)
	parts := strings.Split(name, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]
		if part == "" || strings.HasPrefix(part, "func") {
			continue
		}
		if i > 0 && parts[i-1] == "init" {
			// "init.22" 里的 22 是序号，跳过它继续往前找真实函数名。
			continue
		}
		return part == want
	}
	return false
}
