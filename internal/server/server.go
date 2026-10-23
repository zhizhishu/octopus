package server

import (
	"fmt"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
	_ "github.com/zhizhishu/octopus/internal/server/handlers"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
	"github.com/zhizhishu/octopus/static"
)

var httpSrv http.Server

func Start() error {
	if conf.IsDebug() {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// 启动时清理 Images 请求体临时文件（失败仅告警，不阻断启动）
	tmpDir := bodycache.TmpDirFromEnv()
	olderThan := bodycache.TmpCleanupOlderThanFromEnv()
	if err := bodycache.CleanupOldTmpFiles(tmpDir, bodycache.TmpFilePrefix, olderThan); err != nil {
		log.Warnf("cleanup images tmp files failed: dir=%s prefix=%s olderThan=%s err=%v", tmpDir, bodycache.TmpFilePrefix, olderThan, err)
	}

	r := gin.New()
	// Baseline for gin's own c.ClientIP(): trust Cloudflare edge IPs + any deploy-time
	// OCTOPUS_TRUSTED_PROXIES entries, and read the real client IP from
	// CF-Connecting-IP (then X-Forwarded-For). gin's default trusts ALL proxies, which
	// makes X-Forwarded-For client-forgeable; this replaces that. The app resolves the
	// client IP through middleware.ResolveClientIP, which additionally layers the
	// runtime trusted_proxies setting on top of this baseline without a restart.
	if err := r.SetTrustedProxies(middleware.StaticTrustedProxies()); err != nil {
		log.Warnf("failed to set trusted proxies: %v", err)
	}
	r.RemoteIPHeaders = []string{"CF-Connecting-IP", "X-Forwarded-For"}
	r.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		resp.Error(c, http.StatusInternalServerError, resp.ErrInternalServer)
		c.Abort()
	}))

	if conf.IsDebug() {
		r.Use(middleware.Logger())
	}
	r.Use(middleware.Cors())
	r.Use(middleware.StaticEmbed("/", static.StaticFS))

	router.RegisterAll(r)

	httpSrv.Addr = fmt.Sprintf("%s:%d", conf.AppConfig.Server.Host, conf.AppConfig.Server.Port)
	httpSrv.Handler = r

	// 🔴 先**同步**拿 listener，失败就把错误交回调用方。
	//
	// 旧实现直接 go httpSrv.ListenAndServe() 并恒返回 nil：端口被占、host 配错、
	// 前置代理占着端口时，ListenAndServe 立刻返回错误，但那只是 goroutine 里的
	// 一行 Error 日志。于是 Start() 返回 nil → 调用方以为启动成功 → 定时任务、
	// 轮询器满负荷跑，而**一个 HTTP 请求都收不到**：用户侧全站 502，日志里只有
	// 一行 Error 之后永不再出现，排查方向全错。
	//
	// net.Listen 是同步的，绑不上就立刻报错，让启动流程 fail fast
	// （cmd/start.go 里现在是 os.Exit(1)）。
	ln, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", httpSrv.Addr, err)
	}
	// 绑定成功后再起服务：这里 Serve 的错误只可能是运行期问题（如 listener
	// 被外部关闭），值得记 error 但已经不影响「服务是否可用」的判断。
	safe.SafeGo("http-server-serve", func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Errorf("http server serve error: %v", err)
		}
	})
	return nil
}

func Close() error {
	return httpSrv.Close()
}
