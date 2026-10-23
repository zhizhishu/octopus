package cmd

import (
	"github.com/spf13/cobra"
	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server"
	"github.com/zhizhishu/octopus/internal/server/handlers"
	"github.com/zhizhishu/octopus/internal/task"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/shutdown"
	"os"
)

var cfgFile string

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start " + conf.APP_NAME,
	PreRun: func(cmd *cobra.Command, args []string) {
		conf.PrintBanner()
		conf.Load(cfgFile)
		log.SetLevel(conf.AppConfig.Log.Level)
	},
	Run: func(cmd *cobra.Command, args []string) {
		shutdown.Init(log.Logger)

		// 🔴 启动失败必须**退出进程**，不能 return。
		//
		// 这里曾经是用 defer 挂 Listen() + 四处裸 return。Listen() 会阻塞在
		// `sig := <-quit` 等信号，只有在收到信号时才 os.Exit(0)——所以任何一步
		// 初始化失败，进程既不服务也不退出：PID 常驻、容器/ systemd 看着"活着"，
		// Restart=on-failure 也永远不会被触发（进程没退出）。值班人看到"起来了
		// 但全站 502"，重启无数次都无效，只能手动 kill。
		//
		// 致命错误的正确姿势是 fail fast：打日志 + 非零退出，让编排层接管。
		fatal := func(what string, err error) {
			log.Errorf("%s: %v", what, err)
			shutdown.Shutdown() // 跑已注册的清理函数（此时通常还没注册什么）
			os.Exit(1)
		}

		if err := db.InitDB(conf.AppConfig.Database.Type, conf.AppConfig.Database.Path, conf.IsDebug()); err != nil {
			fatal("database init error", err)
		}
		shutdown.Register(db.Close)

		if err := op.UserInit(); err != nil {
			fatal("user init error", err)
		}

		if err := op.InitCache(); err != nil {
			fatal("cache init error", err)
		}
		shutdown.Register(op.SaveCache)

		if err := server.Start(); err != nil {
			fatal("server start error", err)
		}
		shutdown.Register(server.Close)

		task.Init()
		go task.RUN()

		// 模型检测队列 worker：检测是花钱动作，改为入队后台串行跑。
		// 放在 server.Start 之后启动，确保任何入队请求到达时 worker 已在轮询。
		handlers.StartModelAuditQueueWorker()

		// 全部初始化成功，现在才进入阻塞等待退出信号。
		shutdown.Listen()
	},
}

func init() {
	startCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./data/config.json)")
	rootCmd.AddCommand(startCmd)
}
