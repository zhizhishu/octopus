package task

import (
	"github.com/zhizhishu/octopus/internal/batchimage"
)

// 图片批处理的推进器接线。
//
// 【为什么单独一个文件】它和 image_task_poll.go 一样有个"绝不能自己起来"的约束，
// 而那个约束必须写在离调用点最近的地方，否则下一个人会在包级 init() 里加一行
// "顺手启动"，然后每个 go test 用例都会撞上一个正在遍历临时库、还会真扣钱的写者。

// batchImageWorker 是进程内单例。
var batchImageWorker *batchimage.Worker

// BatchImageWorkerStart 启动批处理推进器（重复调用无副作用）。
//
// 【绝不许改成 init() 自注册】理由与 ImageTaskPollStart / VideoTaskPollStart 逐字相同：
// 本推进器每一轮都读库、写库、调上游，并且会真花用户的钱（结算/退款）。
// 靠 init() 自己起来等于让每个 go test 用例都可能撞上一个正在遍历临时库的写者。
//
// 所以只在生产启动路径上由 internal/task/init.go 显式调用。
func BatchImageWorkerStart() {
	if batchImageWorker != nil {
		return
	}
	service := &batchimage.Service{
		Settings: batchimage.SettingsFromStore,
		Resolver: batchimage.DefaultResolver(),
	}
	worker := batchimage.NewWorker(service)
	worker.Settings = batchimage.SettingsFromStore
	batchImageWorker = worker
	worker.Start()
}
