package cmd

import (
	"os"
	"strings"
	"testing"
)

// 启动失败必须**退出进程**，不能只 return。
//
// 曾经是 `defer shutdown.Listen()` + 四处 `return`：Listen() 阻塞在
// `sig := <-quit`，只在收到信号时才 os.Exit(0)。于是任何一步初始化失败，
// 进程既不服务也不退出——PID 常驻、编排层看着"活着"，Restart=on-failure
// 永远不会触发（进程没退出）。值班人看到"起来了但全站 502"，重启无效。
//
// 这是一条**结构性断言**：它读源码而不是跑进程，因为要真的触发启动失败
// 需要构造坏 DB / 被占端口，测试会又慢又脆。但它守的正是那个回归：
// 谁把 fatal() 改回 return，这条就红。
func TestStartupFailureExitsInsteadOfFallingThrough(t *testing.T) {
	raw, err := os.ReadFile("start.go")
	if err != nil {
		t.Fatalf("读 cmd/start.go 失败: %v", err)
	}
	src := string(raw)

	// 1) 不能再用 defer 挂 Listen()：那会无条件在 Run 退出时阻塞等信号。
	if strings.Contains(src, "defer shutdown.Listen()") {
		t.Error("cmd/start.go 又出现了 `defer shutdown.Listen()`：任何初始化失败后它会把进程" +
			"阻塞在等信号上，既不服务也不退出。必须改成「初始化全部成功后才 Listen」")
	}

	// 2) 四个致命步骤必须走 fatal()（内部 os.Exit(1)），不能裸 return。
	fatalCalls := strings.Count(src, "fatal(")
	if fatalCalls < 4 {
		t.Errorf("cmd/start.go 里 fatal() 只出现 %d 次，期望 >=4（db.InitDB / op.UserInit / "+
			"op.InitCache / server.Start 四处）；少一处就意味着那条失败路径会静默继续", fatalCalls)
	}
	if !strings.Contains(src, "os.Exit(1)") {
		t.Error("cmd/start.go 里没有 os.Exit(1)：启动失败必须给编排层一个非零退出码，否则" +
			"容器/systemd 无法得知失败、不会重启")
	}

	// 3) Listen() 必须在所有初始化之后才被调用（成功路径），且只出现一次。
	if n := strings.Count(src, "shutdown.Listen()"); n != 1 {
		t.Errorf("shutdown.Listen() 出现 %d 次，期望恰好 1 次（初始化成功后的阻塞点）", n)
	}
}

// 端口被占/监听失败必须让 Start() 返回 err，而不是在 goroutine 里只打日志。
//
// 旧实现恒返回 nil：进程满负荷跑定时任务和轮询器，但一个 HTTP 请求都收不到，
// 用户侧全站 502，日志里只有一行 Error 之后永不再出现。
func TestServerStartReportsListenFailure(t *testing.T) {
	raw, err := os.ReadFile("../internal/server/server.go")
	if err != nil {
		t.Skipf("跳过：读不到 server.go (%v)", err)
	}
	src := string(raw)

	// 同步 Listen 才能把错误交回去；Serve 起在 goroutine 里没问题。
	if !strings.Contains(src, "net.Listen(") {
		t.Error("internal/server/server.go 没有同步的 net.Listen：监听失败会被吞进 goroutine，" +
			"Start() 仍返回 nil，进程活着但 HTTP 死")
	}
	if strings.Contains(src, "httpSrv.ListenAndServe()") && !strings.Contains(src, "net.Listen(") {
		t.Error("仍在使用 httpSrv.ListenAndServe()：它的错误只能在 goroutine 里打日志，" +
			"无法让 Start() 返回 err")
	}
}
