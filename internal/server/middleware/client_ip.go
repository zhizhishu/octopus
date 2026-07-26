package middleware

import (
	"net"
	"os"
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
)

// cloudflareTrustedProxies is Cloudflare's published edge IP ranges
// (https://www.cloudflare.com/ips/). When octopus sits behind Cloudflare, only
// forwarding headers arriving from these ranges are trusted, so the resolved
// client IP cannot be spoofed by a forged X-Forwarded-For / CF-Connecting-IP from
// a non-Cloudflare source.
var cloudflareTrustedProxies = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// trustedProxiesEnv adds trusted proxy IPs/CIDRs at deploy time (comma-separated).
// The runtime `trusted_proxies` setting (SettingKeyTrustedProxies) layers on top of
// this without a restart. Both exist because the built-in list only covers
// Cloudflare: behind a LOCAL reverse proxy (nginx/caddy in Docker, or the Docker
// port-forward gateway) that proxy's address would otherwise be untrusted, so its
// X-Forwarded-For is ignored and the resolved client IP collapses to the
// proxy/gateway IP (e.g. 172.24.0.1) — losing the real client IP for both audit
// logs and per-IP rate limiting.
const trustedProxiesEnv = "OCTOPUS_TRUSTED_PROXIES"

// remoteIPHeaders mirrors the order set on the gin engine in server.Start.
var remoteIPHeaders = []string{"CF-Connecting-IP", "X-Forwarded-For"}

// staticProxy* is the deploy-time base (Cloudflare + env), parsed once.
var (
	staticProxyOnce sync.Once
	staticProxyStrs []string
	staticProxyNets []*net.IPNet
)

func initStaticProxies() {
	staticProxyOnce.Do(func() {
		strs := append([]string(nil), cloudflareTrustedProxies...)
		raw := strings.TrimSpace(os.Getenv(trustedProxiesEnv))
		if raw != "" {
			for _, entry := range splitTrimmed(raw) {
				if isIPOrCIDR(entry) {
					strs = append(strs, entry)
				} else {
					log.Warnf("ignoring invalid %s entry %q (want an IP or CIDR)", trustedProxiesEnv, entry)
				}
			}
		}
		staticProxyStrs = strs
		staticProxyNets = parseNets(strs)
	})
}

// StaticTrustedProxies returns the Cloudflare + env entries as strings, for gin's
// SetTrustedProxies at startup (baseline for gin's own c.ClientIP). The runtime
// setting is applied dynamically by ResolveClientIP, not here.
func StaticTrustedProxies() []string {
	initStaticProxies()
	return staticProxyStrs
}

// dynProxy* caches the effective trusted set (static + the trusted_proxies setting)
// and is rebuilt only when the setting string changes, so ResolveClientIP does not
// re-parse CIDRs on every request.
var (
	dynProxyMu   sync.RWMutex
	dynProxyKey  string
	dynProxyNets []*net.IPNet
	dynProxyInit bool
)

func trustedProxyNets() []*net.IPNet {
	initStaticProxies()

	settingStr := ""
	if v, err := op.SettingGetString(model.SettingKeyTrustedProxies); err == nil {
		settingStr = strings.TrimSpace(v)
	}

	dynProxyMu.RLock()
	if dynProxyInit && dynProxyKey == settingStr {
		nets := dynProxyNets
		dynProxyMu.RUnlock()
		return nets
	}
	dynProxyMu.RUnlock()

	dynProxyMu.Lock()
	defer dynProxyMu.Unlock()
	if dynProxyInit && dynProxyKey == settingStr {
		return dynProxyNets
	}
	nets := append([]*net.IPNet(nil), staticProxyNets...)
	if settingStr != "" {
		nets = append(nets, parseNets(splitTrimmed(settingStr))...)
	}
	dynProxyKey = settingStr
	dynProxyNets = nets
	dynProxyInit = true
	return nets
}

// ResolveClientIP returns the real downstream client IP. When the immediate peer is
// a trusted proxy (Cloudflare + OCTOPUS_TRUSTED_PROXIES + the trusted_proxies
// setting), the client is read from CF-Connecting-IP / X-Forwarded-For using the
// same right-to-left trusted-walk gin uses; otherwise the forwarding headers are
// client-forgeable and the direct peer is returned. The setting is read per call
// (cached), so changing it takes effect without a restart. Use this everywhere the
// app needs the client IP so audit logging and per-IP rate limiting agree.
func ResolveClientIP(c *gin.Context) string {
	return resolveClientIP(c.RemoteIP(), c.GetHeader, trustedProxyNets())
}

// resolveClientIP is the pure core of ResolveClientIP: given the direct peer, a
// header lookup, and the trusted-proxy set, it returns the real client IP. Split
// out from the gin context so the security-critical trusted-walk is unit-testable
// without a request or the settings store.
func resolveClientIP(peerStr string, header func(string) string, nets []*net.IPNet) string {
	peer := net.ParseIP(peerStr)
	if peer == nil {
		return peerStr
	}
	if ipInNets(peer, nets) {
		for _, h := range remoteIPHeaders {
			if ip, ok := validateForwardedHeader(header(h), nets); ok {
				return ip
			}
		}
	}
	return peerStr
}

// validateForwardedHeader walks a forwarding header right-to-left and returns the
// first client-side IP: the first entry that is either at index 0 or not itself a
// trusted proxy. Mirrors gin's engine.validateHeader (including the break on a
// malformed entry) so behavior matches gin's ClientIP for the same trusted set.
func validateForwardedHeader(header string, nets []*net.IPNet) (string, bool) {
	if header == "" {
		return "", false
	}
	items := strings.Split(header, ",")
	for i := len(items) - 1; i >= 0; i-- {
		ipStr := strings.TrimSpace(items[i])
		ip := net.ParseIP(ipStr)
		if ip == nil {
			break
		}
		if i == 0 || !ipInNets(ip, nets) {
			return ipStr, true
		}
	}
	return "", false
}

func parseNets(entries []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, cidr)
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return nets
}

func splitTrimmed(s string) []string {
	out := make([]string, 0)
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func isIPOrCIDR(entry string) bool {
	if _, _, err := net.ParseCIDR(entry); err == nil {
		return true
	}
	return net.ParseIP(entry) != nil
}

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
