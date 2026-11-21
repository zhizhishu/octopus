package balancer

import (
	"sync"
	"sync/atomic"
)

// ── 共享状态的身份与提交边界 ─────────────────────────────────────────────
//
// 熔断表按作用域拆成多条（同一个 channel:key:model 可以有多个 endpoint/capability
// 变体），每条有自己的 mutex；hold 是另一套结构。熔断探测要在"准入时"记住一条共享
// 状态、在"结算时"确认它没被换过、然后对**整个 tuple** 做一次提交。这需要两样东西：
//
//  1. **全进程唯一的版本号**（circuitRevision）：每次写共享状态都取一个新值，单调递增、
//     永不重复。条目内部计数器从 0 开始、重建后重新计数，做不到这点——旧 claim 拿着
//     "第 3 版"很可能正好撞上重建后的"第 3 版"，于是把新一代当成自己那一条给清了。
//     改用全局号之后，"某条条目的某个版本"在进程生命周期内就是一个唯一身份。
//
//  2. **tuple 级提交锁**（circuitCommitLocks）：同一 channel:key:model 的写入互相串行。
//     没有它，切片里的"复核 → 删除 → 清理"三步之间可以漏进真实流量新建的条目
//     （RecordFailureWithStatusScoped 会 getOrCreateEntry），于是重置删掉别人的新一代。
//
// 锁序固定为：tuple 锁 → entry.mu / hold 表 / 运行时冷却。反向一次都没有
// （读路径只拿 entry.mu，不拿 tuple 锁）。
//
// 表的大小与熔断表同阶（每个被写过的 channel:key:model 一把），不会随请求数增长。

// circuitRevision 是共享状态的全局版本号。
var circuitRevision atomic.Uint64

// nextCircuitRevision 取一个新的全局版本号（永不为 0，永不重复）。
func nextCircuitRevision() uint64 {
	return circuitRevision.Add(1)
}

// circuitCommitLocks 是 tuple 级提交锁表（按需创建）。
var circuitCommitLocks sync.Map // tuple key -> *sync.Mutex

// lockCircuitTuple 锁住一个 tuple，返回解锁函数。
//
// 必须拿它的写者：真实失败计数（唯一会"新建条目"的路径）、hold 延长、探测结算
// （重置/升级）。只读路径与"不新建条目的写"（真实成功）不需要它。
func lockCircuitTuple(channelID, keyID int, modelName string) func() {
	key := circuitKey(channelID, keyID, modelName)
	if existing, ok := circuitCommitLocks.Load(key); ok {
		mutex := existing.(*sync.Mutex)
		mutex.Lock()
		return mutex.Unlock
	}
	created := &sync.Mutex{}
	actual, _ := circuitCommitLocks.LoadOrStore(key, created)
	mutex := actual.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}
