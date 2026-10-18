//go:build windows

package op

// Windows 侧不实现磁盘剩余空间探测：调用方拿到 ok=false 会保守跳过 VACUUM。
// 跳过只是"空间暂未回收"，不会出错；而误判成"空间够"去做一次重建整个库的
// VACUUM，风险要大得多。

func diskFreeBytes(path string) (int64, bool) {
	return 0, false
}
