package server

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/relay/bodycache"
	_ "github.com/bestruirui/octopus/internal/server/handlers"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/static"
	"github.com/gin-gonic/gin"
)

var httpSrv http.Server

// cloudflareTrustedProxies is Cloudflare's published edge IP ranges
// (https://www.cloudflare.com/ips/). When the app sits behind Cloudflare, only
// forwarding headers arriving from these ranges are trusted, so c.ClientIP()
// resolves the real client IP (used for rate limiting and IP auditing) and
// cannot be spoofed by a forged X-Forwarded-For / CF-Connecting-IP from a
// non-Cloudflare source. Safe for non-Cloudflare deployments too: if the
// immediate peer is not one of these trusted proxies, gin falls back to the
// direct remote address.
var cloudflareTrustedProxies = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// trustedProxiesEnv is the env var (OCTOPUS_TRUSTED_PROXIES) for additional
// trusted proxy IPs/CIDRs, comma-separated. It exists because the built-in list
// only covers Cloudflare: a deployment sitting behind a LOCAL reverse proxy
// (nginx/caddy in Docker, or the Docker port-forward gateway) would otherwise
// have that proxy's address untrusted, so gin ignores its X-Forwarded-For and
// c.ClientIP() collapses to the proxy/gateway IP (e.g. 172.24.0.1) — losing the
// real client IP for both audit logs and per-IP rate limiting. Set this to the
// reverse proxy's address or subnet (e.g. "172.16.0.0/12,10.0.0.0/8" for Docker
// bridge networks) so the real client IP is honored. Left unset, behavior is
// unchanged (Cloudflare-only), so it is safe by default and never widens trust
// implicitly.
const trustedProxiesEnv = "OCTOPUS_TRUSTED_PROXIES"

// buildTrustedProxies returns the Cloudflare ranges plus any valid extra entries
// from OCTOPUS_TRUSTED_PROXIES. Each extra entry must be a bare IP or a CIDR;
// invalid ones are skipped with a warning rather than failing startup.
func buildTrustedProxies() []string {
	proxies := append([]string(nil), cloudflareTrustedProxies...)
	raw := strings.TrimSpace(os.Getenv(trustedProxiesEnv))
	if raw == "" {
		return proxies
	}
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(entry); err == nil {
			proxies = append(proxies, entry)
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			proxies = append(proxies, entry)
			continue
		}
		log.Warnf("ignoring invalid %s entry %q (want an IP or CIDR)", trustedProxiesEnv, entry)
	}
	return proxies
}

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
	// Trust only Cloudflare edge IPs as proxies and read the real client IP from
	// CF-Connecting-IP (then X-Forwarded-For), so c.ClientIP() is accurate and
	// non-spoofable behind Cloudflare. gin's default trusts ALL proxies, which
	// makes X-Forwarded-For client-forgeable; this replaces that.
	if err := r.SetTrustedProxies(buildTrustedProxies()); err != nil {
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
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorf("http server listen and serve error: %v", err)
		}
	}()
	return nil
}

func Close() error {
	return httpSrv.Close()
}
