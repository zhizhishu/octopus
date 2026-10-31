package telemetry

import (
	"net/http"
	"regexp"
	"strings"
)

// IdentifyClient 从 User-Agent 和几个身份头认出客户端名和版本。认不出时用 UA 第一个产品名。
//
// 依据（各客户端实际发的头）：
//   - Claude Code：UA `claude-cli/<ver> (external, cli)`，另带 `x-app: cli`、x-stainless-*（Anthropic TS SDK）；
//   - Codex CLI：UA `codex_cli_rs/<ver> (...)`，`originator: codex_cli_rs`（VS Code 插件 codex_vscode，exec 模式 codex_exec）；
//   - 官方 SDK：UA `Anthropic/JS <ver>`、`OpenAI/Python <ver>`，另带 x-stainless-lang / x-stainless-package-version；
//   - Gemini CLI：UA `GeminiCLI/<ver>`；Google GenAI SDK：x-goog-api-client `genai-js/<ver>` 等。
func IdentifyClient(ua string, h http.Header) (name, version string) {
	lua := strings.ToLower(ua)
	get := func(k string) string {
		if h == nil {
			return ""
		}
		return strings.TrimSpace(h.Get(k))
	}
	originator := strings.ToLower(get("Originator"))
	stLang := strings.ToLower(get("X-Stainless-Lang"))
	stVer := get("X-Stainless-Package-Version")

	switch {
	case strings.HasPrefix(lua, "claude-cli/") || strings.HasPrefix(lua, "claude-code/"):
		return "Claude Code", uaVersion(ua)
	case strings.HasPrefix(originator, "codex") || strings.HasPrefix(lua, "codex"):
		v := uaVersion(ua)
		if v == "" {
			v = get("Version")
		}
		switch {
		case strings.Contains(originator, "vscode") || strings.Contains(lua, "vscode"):
			return "Codex (VS Code)", v
		case strings.Contains(originator, "exec") || strings.Contains(lua, "codex_exec"):
			return "Codex exec", v
		}
		return "Codex CLI", v
	}
	for _, c := range knownClients {
		if c.re.MatchString(ua) {
			v := ""
			if m := c.re.FindStringSubmatch(ua); len(m) > 1 {
				v = m[1]
			}
			return c.name, v
		}
	}
	if m := sdkUARe.FindStringSubmatch(ua); m != nil {
		return sdkName(m[1], m[2]), m[3]
	}
	if stLang != "" && stVer != "" {
		// 改过 UA 的 Stainless SDK（OpenAI / Anthropic 官方 SDK 都是 Stainless 生成的）。
		return "Stainless SDK (" + stLang + ")", stVer
	}
	if gac := get("X-Goog-Api-Client"); gac != "" {
		if m := googRe.FindStringSubmatch(gac); m != nil {
			return "Google GenAI SDK (" + m[1] + ")", m[2]
		}
	}
	if strings.HasPrefix(lua, "mozilla/") {
		for _, b := range browsers {
			if m := b.re.FindStringSubmatch(ua); m != nil {
				return "浏览器 " + b.name, m[1]
			}
		}
		return "浏览器", ""
	}
	if m := firstProductRe.FindStringSubmatch(ua); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

type knownClient struct {
	name string
	re   *regexp.Regexp
}

// 第一个捕获组是版本。顺序有讲究：更具体的放前面。
var knownClients = []knownClient{
	{"Gemini CLI", regexp.MustCompile(`(?i)^GeminiCLI/v?([\w.+-]+)`)},
	{"Cursor", regexp.MustCompile(`(?i)\bcursor(?:/v?([\w.+-]+))?`)},
	{"Cherry Studio", regexp.MustCompile(`(?i)CherryStudio/v?([\w.+-]+)`)},
	{"Kilo Code", regexp.MustCompile(`(?i)kilo-?code(?:/v?([\w.+-]+))?`)},
	{"Roo Code", regexp.MustCompile(`(?i)^roo-?(?:code|cline)(?:/v?([\w.+-]+))?`)},
	{"Cline", regexp.MustCompile(`(?i)^cline(?:/v?([\w.+-]+))?`)},
	{"Continue", regexp.MustCompile(`(?i)\bcontinue/v?([\w.+-]+)`)},
	{"Zed", regexp.MustCompile(`(?i)^Zed/v?([\w.+-]+)`)},
	{"opencode", regexp.MustCompile(`(?i)^opencode/v?([\w.+-]+)`)},
	{"Windsurf", regexp.MustCompile(`(?i)windsurf(?:/v?([\w.+-]+))?`)},
	{"LobeChat", regexp.MustCompile(`(?i)lobe-?chat(?:/v?([\w.+-]+))?`)},
	{"Vercel AI SDK", regexp.MustCompile(`(?i)\bai-sdk/[\w-]+/v?([\w.+-]+)`)},
	{"LiteLLM", regexp.MustCompile(`(?i)litellm(?:/v?([\w.+-]+))?`)},
	{"DSH", regexp.MustCompile(`(?i)^dsh/v?([\w.+-]+)`)},
	{"Pi", regexp.MustCompile(`(?i)^pi/v?([\w.+-]+)`)},
	{"Postman", regexp.MustCompile(`(?i)^PostmanRuntime/v?([\w.+-]+)`)},
	{"Apifox", regexp.MustCompile(`(?i)^Apifox/v?([\w.+-]+)`)},
	{"curl", regexp.MustCompile(`(?i)^curl/v?([\w.+-]+)`)},
	{"Python httpx", regexp.MustCompile(`(?i)^python-httpx/v?([\w.+-]+)`)},
	{"Python requests", regexp.MustCompile(`(?i)^python-requests/v?([\w.+-]+)`)},
	{"aiohttp", regexp.MustCompile(`(?i)aiohttp/v?([\w.+-]+)`)},
	{"Node fetch", regexp.MustCompile(`(?i)^(?:node-fetch|undici)(?:/v?([\w.+-]+))?`)},
	{"Node.js", regexp.MustCompile(`(?i)^node(?:/v?([\w.+-]+))?$`)},
	{"axios", regexp.MustCompile(`(?i)^axios/v?([\w.+-]+)`)},
	{"Go", regexp.MustCompile(`(?i)^Go-http-client/v?([\w.+-]+)`)},
	{"OkHttp", regexp.MustCompile(`(?i)^okhttp/v?([\w.+-]+)`)},
	{"reqwest", regexp.MustCompile(`(?i)^reqwest/v?([\w.+-]+)`)},
}

var (
	// `Anthropic/JS 0.65.0`、`OpenAI/Python 1.109.1`、`AsyncOpenAI/Python 1.x`
	sdkUARe        = regexp.MustCompile(`(?i)^(?:Async)?(OpenAI|Anthropic)/([A-Za-z]+) v?([\w.+-]+)`)
	googRe         = regexp.MustCompile(`(?i)\b(genai-js|google-genai-sdk|genai-py|gl-python|gl-node)/v?([\w.+-]+)`)
	firstProductRe = regexp.MustCompile(`^([A-Za-z0-9][\w.\-]{0,63})(?:/v?([\w.+-]{1,32}))?`)
	uaVerRe        = regexp.MustCompile(`^[^\s/]+/v?(\d[\w.+-]*)`)
)

// Edge / Opera 的 UA 里也有 Chrome 和 Safari，Chrome 的 UA 里也有 Safari，所以按这个顺序认。
var browsers = []knownClient{
	{"Edge", regexp.MustCompile(`Edg/(\d+)`)},
	{"Opera", regexp.MustCompile(`OPR/(\d+)`)},
	{"Firefox", regexp.MustCompile(`Firefox/(\d+)`)},
	{"Chrome", regexp.MustCompile(`Chrome/(\d+)`)},
	{"Safari", regexp.MustCompile(`Version/(\d+).*Safari/`)},
}

func sdkName(vendor, lang string) string {
	v := "OpenAI"
	if strings.EqualFold(vendor, "anthropic") {
		v = "Anthropic"
	}
	return v + " SDK (" + lang + ")"
}

func uaVersion(ua string) string {
	if m := uaVerRe.FindStringSubmatch(ua); m != nil {
		return m[1]
	}
	return ""
}
