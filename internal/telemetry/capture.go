package telemetry

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Snapshot 是一次入站请求能在服务端拿到的客户端特征。只读请求，不改请求。
//
// 不存任何秘密：Authorization / x-api-key / Cookie 等只记「带了这个头」（头名），
// 值一律不碰；只有白名单里的非秘密头才留值（见 notableHeader）。
type Snapshot struct {
	UserAgent     string
	ClientName    string
	ClientVersion string
	// HeaderNames：原始顺序 + 原始大小写（OrderKnown=true）；拿不到原始顺序时是
	// Go 规范化后的头名按字母排（OrderKnown=false）。
	HeaderNames []string
	OrderKnown  bool
	Notable     map[string]string
	HTTPVersion string
	JA3         string
	JA4         string
	JA4H        string
	Country     string
	// ViaProxy：直接连进来的是可信反代（此时头顺序 / HTTP 版本是反代那一段的，JA3/JA4 只能靠反代转交）。
	ViaProxy bool
	Path     string
	// Hash：稳定特征算出来的指纹，同一个客户端同一种配置每次一样（会话 id、重试次数这类每次变的不算进去）。
	Hash string
}

const maxValueRunes = 200

// 这些头的值绝不保存（连哈希都不进）。
var secretHeaderNames = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"x-api-key": true, "api-key": true, "x-goog-api-key": true, "mj-api-secret": true,
	"sec-websocket-protocol": true, "sec-websocket-key": true, "x-auth-token": true,
}

var secretNameHints = []string{"auth", "token", "secret", "key", "password", "passwd", "signature", "checksum", "credential", "cookie", "session-token"}

func isSecretHeader(lower string) bool {
	if secretHeaderNames[lower] {
		return true
	}
	for _, h := range secretNameHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// 留值的非秘密头。stable=true 的进指纹哈希；false 的每次可能变（会话 id、重试次数），只留最近一次的值。
var notableHeaders = map[string]bool{
	"user-agent":        true,
	"originator":        true,
	"version":           true,
	"x-app":             true,
	"anthropic-version": true,
	"anthropic-beta":    false,
	"anthropic-dangerous-direct-browser-access": true,
	"openai-beta":               true,
	"x-title":                   true,
	"http-referer":              true,
	"referer":                   true,
	"origin":                    true,
	"accept":                    true,
	"accept-encoding":           true,
	"accept-language":           true,
	"content-type":              true,
	"connection":                true,
	"te":                        true,
	"priority":                  true,
	"x-goog-api-client":         true,
	"x-client-name":             true,
	"x-client-version":          true,
	"session_id":                false,
	"session-id":                false,
	"x-session-id":              false,
	"conversation_id":           false,
	"conversation-id":           false,
	"x-claude-code-session-id":  false,
	"x-client-request-id":       false,
	"x-request-id":              false,
	"ah-thread-id":              false,
	"ah-trace-id":               false,
	"x-stainless-retry-count":   false,
	"x-stainless-timeout":       false,
	"x-stainless-read-timeout":  false,
	"x-stainless-helper-method": false,
	"x-octopus-plan":            true,
	"x-octopus-group":           true,
	"cf-ipcountry":              true,
	"x-forwarded-proto":         true,
	"via":                       true,
}

var notablePrefixes = []string{"x-stainless-", "sec-ch-ua", "sec-fetch-", "x-codex-", "x-cursor-"}

// notableHeader：这个头要不要留值，以及值是否稳定（进哈希）。
func notableHeader(lower string) (keep, stable bool) {
	if isSecretHeader(lower) {
		return false, false
	}
	if s, ok := notableHeaders[lower]; ok {
		return true, s
	}
	for _, p := range notablePrefixes {
		if strings.HasPrefix(lower, p) {
			return true, true
		}
	}
	return false, false
}

// 反代转交的 TLS / 协议信息。只有直接连进来的是可信反代时才认，否则谁都能伪造。
var (
	ja3Headers     = []string{"X-JA3-Hash", "X-JA3", "CF-JA3-Hash", "X-TLS-JA3"}
	ja4Headers     = []string{"X-JA4", "CF-JA4", "X-TLS-JA4"}
	httpVerHeaders = []string{"X-HTTP-Version", "X-Client-HTTP-Version"}
	ja3HashRe      = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	ja3FullRe      = regexp.MustCompile(`^[0-9]+,[0-9-]*,[0-9-]*,[0-9-]*,[0-9-]*$`)
	ja4Re          = regexp.MustCompile(`^[A-Za-z0-9_]{10,80}$`)
	httpVerRe      = regexp.MustCompile(`^(?i:(HTTP/)?(1\.0|1\.1|2|2\.0|3|3\.0|h2|h3))$`)
	countryRe      = regexp.MustCompile(`^[A-Z]{2}$`)
)

// ReverseProxyHeaders 是反代（Caddy / nginx / Cloudflare 等）在入站请求上加的、只给本服务看的头：
// 转交的 JA3 / JA4 / HTTP 版本 / 国家，以及 Via。真 Claude / Codex CLI 自己从来不发这些，
// 所以出站绝不能把它们转给上游（relay 的 shouldForwardClientHeader 剥掉同一批）。
// relay 不许 import 本包，所以那边另列一份；relay 的测试拿这份对照，指纹这里多读一个头而那边没剥就会失败。
func ReverseProxyHeaders() []string {
	out := make([]string, 0, len(ja3Headers)+len(ja4Headers)+len(httpVerHeaders)+2)
	out = append(out, ja3Headers...)
	out = append(out, ja4Headers...)
	out = append(out, httpVerHeaders...)
	out = append(out, "CF-IPCountry", "Via")
	return out
}

// Capture 读一个入站请求的特征。viaTrustedProxy：直接连进来的是不是可信反代
// （与 middleware.ResolveClientIP 同一套名单）。只读 req，不改 req.Header、不读 body。
func Capture(req *http.Request, viaTrustedProxy bool) Snapshot {
	s := Snapshot{ViaProxy: viaTrustedProxy, Notable: map[string]string{}}
	if req == nil {
		return s
	}
	s.Path = cut(req.URL.Path, 128)
	s.UserAgent = cleanValue(req.Header.Get("User-Agent"), 512)
	s.HTTPVersion = req.Proto

	if names, ok := RawHeaderNames(req); ok {
		s.HeaderNames = append([]string(nil), names...)
		s.OrderKnown = true
	} else {
		names := make([]string, 0, len(req.Header)+1)
		if req.Host != "" {
			names = append(names, "Host")
		}
		for k := range req.Header {
			names = append(names, k)
		}
		sort.Strings(names[min(1, len(names)):])
		s.HeaderNames = names
	}

	for k, vs := range req.Header {
		lower := strings.ToLower(k)
		keep, _ := notableHeader(lower)
		if !keep || len(vs) == 0 || lower == "user-agent" {
			continue
		}
		joined := strings.Join(vs, ", ")
		if urlValuedHeaders[lower] {
			joined = stripURLQuery(joined)
		}
		if v := cleanValue(joined, maxValueRunes); v != "" {
			s.Notable[lower] = v
		}
	}

	if viaTrustedProxy {
		for _, h := range ja3Headers {
			v := strings.TrimSpace(req.Header.Get(h))
			if ja3HashRe.MatchString(v) {
				s.JA3 = strings.ToLower(v)
				break
			}
			if ja3FullRe.MatchString(v) {
				sum := md5.Sum([]byte(v))
				s.JA3 = hex.EncodeToString(sum[:])
				break
			}
		}
		for _, h := range ja4Headers {
			if v := strings.TrimSpace(req.Header.Get(h)); ja4Re.MatchString(v) {
				s.JA4 = v
				break
			}
		}
		for _, h := range httpVerHeaders {
			if v := strings.TrimSpace(req.Header.Get(h)); httpVerRe.MatchString(v) {
				s.HTTPVersion = normalizeHTTPVersion(v)
				break
			}
		}
		if v := strings.ToUpper(strings.TrimSpace(req.Header.Get("CF-IPCountry"))); countryRe.MatchString(v) && v != "XX" {
			s.Country = v
		}
	}
	if s.OrderKnown {
		s.JA4H = ja4h(req, s.HeaderNames)
	}
	s.ClientName, s.ClientVersion = IdentifyClient(s.UserAgent, req.Header)
	s.Hash = s.fingerprintHash()
	return s
}

func normalizeHTTPVersion(v string) string {
	v = strings.ToUpper(strings.TrimPrefix(strings.ToUpper(v), "HTTP/"))
	switch v {
	case "H2", "2", "2.0":
		return "HTTP/2.0"
	case "H3", "3", "3.0":
		return "HTTP/3.0"
	default:
		return "HTTP/" + v
	}
}

func (s Snapshot) fingerprintHash() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	if s.OrderKnown {
		write("order", strings.Join(s.HeaderNames, ","))
	} else {
		write("names", strings.Join(s.HeaderNames, ","))
	}
	write(s.UserAgent, s.HTTPVersion, s.JA3, s.JA4)
	if s.ViaProxy {
		write("proxy")
	}
	keys := make([]string, 0, len(s.Notable))
	for k := range s.Notable {
		if _, stable := notableHeader(k); stable {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, s.Notable[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ja4h 按 FoxIO JA4H 规范算 HTTP 客户端指纹（a_b_c_d）：
//   - a：方法前两位 + 版本 + 有无 Cookie + 有无 Referer + 头个数（不含 Cookie/Referer）+ Accept-Language 前 4 位；
//   - b：头名（原始顺序，不含 Cookie/Referer）的 sha256 前 12 位；
//   - c：Cookie 名排序后的 sha256 前 12 位；
//   - d：规范里是 Cookie「名=值」的哈希。这里不算值（不碰 Cookie 值），固定填 0。
func ja4h(req *http.Request, names []string) string {
	method := strings.ToLower(req.Method)
	if len(method) > 2 {
		method = method[:2]
	}
	ver := "11"
	switch req.ProtoMajor*10 + req.ProtoMinor {
	case 10:
		ver = "10"
	case 20:
		ver = "20"
	case 30:
		ver = "30"
	}
	cookie, referer := "n", "n"
	kept := make([]string, 0, len(names))
	for _, n := range names {
		switch strings.ToLower(n) {
		case "cookie":
			cookie = "c"
			continue
		case "referer":
			referer = "r"
			continue
		}
		kept = append(kept, n)
	}
	count := len(kept)
	if count > 99 {
		count = 99
	}
	lang := strings.ToLower(strings.ReplaceAll(req.Header.Get("Accept-Language"), "-", ""))
	if i := strings.IndexAny(lang, ",;"); i >= 0 {
		lang = lang[:i]
	}
	lang = (lang + "0000")[:4]
	a := method + ver + cookie + referer + twoDigits(count) + lang
	b := sha12(strings.Join(kept, ","))
	c := "000000000000"
	if cookie == "c" {
		cnames := make([]string, 0)
		for _, ck := range req.Cookies() {
			cnames = append(cnames, ck.Name)
		}
		sort.Strings(cnames)
		if len(cnames) > 0 {
			c = sha12(strings.Join(cnames, ","))
		}
	}
	return a + "_" + b + "_" + c + "_000000000000"
}

func twoDigits(n int) string {
	return string([]byte{byte('0' + n/10), byte('0' + n%10)})
}

func sha12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// cleanValue 去掉控制字符并截断。
func cleanValue(v string, maxRunes int) string {
	v = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v))
	if r := []rune(v); len(r) > maxRunes {
		v = string(r[:maxRunes])
	}
	return v
}

// urlValuedHeaders 是值为 URL 的白名单头。只留「从哪个页面来」（scheme + host + path），
// 查询串和 # 片段一律去掉：那里常带 token、code、邮箱、搜索词之类，不该进指纹记录。
var urlValuedHeaders = map[string]bool{
	"referer":      true,
	"http-referer": true,
	"origin":       true,
}

// stripURLQuery 去掉 URL 里的查询串、片段和 user:pass@。解析不了的值按第一个 ? / # 截断。
func stripURLQuery(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		u.RawQuery, u.ForceQuery = "", false
		u.Fragment, u.RawFragment = "", ""
		u.User = nil
		return u.String()
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}
