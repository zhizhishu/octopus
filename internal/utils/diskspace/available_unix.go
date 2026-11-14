//go:build !windows

package diskspace

// 磁盘剩余空间的读取，用于决定"现在做 VACUUM 安不安全"。
//
// 为什么这件事不能省：SQLite 的 VACUUM 会重建整个数据库文件，WAL 模式下峰值
// 临时占用约等于**当前库的大小**。生产库有 59GB 时，盲目 VACUUM 可能把磁盘写满，
// 进而让数据库写入失败——比"空间没还回来"严重得多。
//
// 这里只在 Linux/Unix 上用 syscall.Statfs 拿真实可用空间；Windows 侧没有对应
// 实现，返回 ok=false，调用方据此保守跳过 VACUUM(见 log_storage.go)。

import (
	"os"
	"path/filepath"
	"syscall"
)

func Available(path string) (int64, bool) {
	var st syscall.Statfs_t
	// An image directory may not exist yet. Probe its existing ancestor on the
	// same filesystem without creating anything during a pressure check.
	for {
		err := syscall.Statfs(path, &st)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return 0, false
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, false
		}
		path = parent
	}
	// Bavail 是非特权用户可用的块数；用 Bsize 换算成字节。
	// 两个字段都是平台相关的无符号类型，统一转 int64 避免溢出。
	free := int64(st.Bavail) * int64(st.Bsize)
	if free < 0 {
		return 0, false
	}
	return free, true
}
