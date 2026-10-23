package relay

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// errorBodyReadBudget 是「读上游错误响应体」的硬预算。
//
// 【为什么必须存在】非 2xx 分支要先把错误体读出来做归类（upstream_error.go 靠它判
// user_error / 限流 / 余额不足）。`io.LimitReader` 只限**字节数**、不限**时间**：
// 上游发完状态码和响应头就不再写正文时，裸 `io.ReadAll` 会一直阻塞。实测在
// TestEpisodeNormalSuccessRestores 里卡满数分钟才被包超时打断，goroutine 栈停在
// forward → io.ReadAll。后果是三层叠加的：
//   - relay 协程卡死，本次请求永不返回，用户侧一直转圈；
//   - 它卡在 attempt.forward 里，于是中间件里 `defer op.UserReleaseQuota` 永不执行
//     —— 该用户的在途预留额度被永久占住，余额足够也再也进不来
//     （在途预留见 op.UserCanRelay / UserReleaseQuota）；
//   - 上游健康统计不更新：这次尝试既没记成功也没记失败。
//
// 【为什么默认启用，而不像 upstream_header_timeout 那样默认 0】那个预算卡的是
// 「健康渠道慢一点」，太短会误伤正常的慢渠道，必须由运维显式开启。这里完全不同：
// 状态码**已经收到了**，这已经是一次失败尝试，正文只是用来归类原因的附加信息。
// 10s 足够正常上游把 16KB 错误体吐完；超过 10s 的沉默说明上游已经坏了，此时
// "拿不到错误体"远好过"请求永远挂住"。所以它是硬上限，不是开关。
//
// 用 var 而不是 const：测试要把它压到毫秒级才能快速验证"卡住"这个行为本身。
var errorBodyReadBudget = 10 * time.Second

// errErrorBodyReadTimeout 表示错误响应体没能在预算内读完。
var errErrorBodyReadTimeout = errors.New("upstream error body read timed out")

// readErrorBodyLimited 在预算内读取至多 limit 字节的错误响应体。
//
// 返回 error 非 nil 时调用方应当把 body 当作不完整/不可用：按状态码归类即可，
// 绝不能因此把这次尝试当成成功。
//
// 【为什么是"独立协程 + 到点放弃"，而不是"到点关 Body"】实测过两条都不行：
//   - `body.Close()`：Body 还没读干净时，Close 会先把剩余内容排空，于是它自己
//     阻塞在读上 —— 定时器关不掉这次读（实测仍然卡满包超时）。
//   - 取消 context：确实能立刻中断（实测 300ms 返回），但这个 Body 绑的是**父请求**
//     的 context，取消它等于把整次请求杀掉，代价不可接受。
//
// 所以唯一可靠的做法是把读放到独立协程，到点就**放弃等待**并返回。代价是那个
// 协程要等上游关闭/TCP 超时才退出，属于"泄漏一个协程"换取"请求一定按时返回"——
// 对一个已经坏了的上游，这个交换是值得的，而卡死整个请求不是。
func readErrorBodyLimited(body io.ReadCloser, limit int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = upstreamErrorBodyLimit
	}
	// 预算 <= 0 表示禁用等待上限，只保留字节上限（测试里可临时关掉）。
	if errorBodyReadBudget <= 0 {
		return io.ReadAll(io.LimitReader(body, limit))
	}

	type result struct {
		data []byte
		err  error
	}
	// 带缓冲：即使我们超时走了，迟到的结果也不会把协程永久堵在发送上。
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, limit))
		done <- result{data, err}
	}()

	timer := time.NewTimer(errorBodyReadBudget)
	defer timer.Stop()
	select {
	case got := <-done:
		return got.data, got.err
	case <-timer.C:
		// 放弃这次读取。异步关一次 Body，尽最大努力释放连接；它自己可能阻塞，
		// 所以必须放在独立协程里，绝不能在这里等它。
		go func() { _ = body.Close() }()
		return nil, errErrorBodyReadTimeout
	}
}

// readErrorBodyFromResponse 是 readErrorBodyLimited 对 *http.Response 的便捷包装。
func readErrorBodyFromResponse(resp *http.Response) ([]byte, error) {
	if resp == nil {
		return nil, nil
	}
	return readErrorBodyLimited(resp.Body, upstreamErrorBodyLimit)
}
