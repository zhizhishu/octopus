package behavior

import "context"

// ProgressObserver 只读地看一次 Run 走到了哪一项、发了几次请求。
//
// 为什么放在 context 里而不是给 Run 加参数：Run 的调用点有手动检测、队列 worker、
// 定时任务和测试四处，只有队列 worker 需要「任务页上每一项的小方块」。走 context
// 不改任何调用点的签名，也不改任何出站字节、结论或评分——观察者拿到的只是事件。
//
// 实现方必须是并发安全且不阻塞的：它在探针的发送路径上被同步调用。
type ProgressObserver interface {
	// ProbeStarted 某一项开始跑（不适用的项也会开始，然后立刻结束、零请求）。
	ProbeStarted(id ProbeID)
	// RequestSent 当前这一项又发出了一次上游请求（成功失败都算，花钱的是发出去这件事）。
	RequestSent()
	// ProbeFinished 某一项跑完。ok=true 表示拿到了可用结果（与 worker 的「可用」同口径）。
	ProbeFinished(id ProbeID, ok bool)
}

type progressKey struct{}

// WithProgress 把观察者挂到 ctx 上。obs 为 nil 时原样返回 ctx。
func WithProgress(ctx context.Context, obs ProgressObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, obs)
}

// progressFrom 取出观察者；没有就返回 nil（绝大多数调用方都没有）。
func progressFrom(ctx context.Context) ProgressObserver {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(progressKey{}).(ProgressObserver)
	return obs
}
