package update

import "fmt"

// UpdateCore 是"就地替换二进制"的更新入口。
//
// 本项目**永久禁用**这个功能，不是暂时关闭。
//
// 原因不是懒得支持：它要做的第一件事就是去下载某个外部的发行包并解压覆盖自己，
// 而我们的版本只由自己的构建流水线产出。保留这条路径意味着代码里永远存在一个
// "从别处拉一个二进制来跑"的出口——既是供应链风险，也让"版本由谁决定"这件事
// 说不清楚。容器部署下正确做法本来就是拉新镜像重启，不需要自我替换。
//
// 所以这里不但拒绝执行，而且**连下载地址、解压函数都不再存在**（相关常量与
// 辅助函数已删除），让"误走这条路"在编译期就不可能，而不是靠运行期判断兜住。
func UpdateCore() error {
	return fmt.Errorf("in-app binary update is disabled; pull the new image instead: docker compose pull && docker compose up -d")
}
