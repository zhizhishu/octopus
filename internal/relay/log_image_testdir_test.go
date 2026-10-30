package relay

import (
	"os"

	"github.com/zhizhishu/octopus/internal/logimage"
)

// 本包的测试绝不能把图片写进源码目录下的 data/log_images：统一指到临时目录。
func init() {
	dir, err := os.MkdirTemp("", "octopus-relay-logimg-")
	if err != nil {
		panic(err)
	}
	logimage.SetDir(dir)
}
