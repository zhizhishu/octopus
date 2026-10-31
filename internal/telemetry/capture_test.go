package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func claudeCodeReq(session string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	h := req.Header
	h.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
	h.Set("X-App", "cli")
	h.Set("Anthropic-Version", "2023-06-01")
	h.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Stainless-Package-Version", "0.60.0")
	h.Set("X-Stainless-Os", "MacOS")
	h.Set("X-Stainless-Retry-Count", "0")
	h.Set("X-Claude-Code-Session-Id", session)
	h.Set("Authorization", "Bearer sk-secret-value")
	h.Set("X-Api-Key", "sk-ant-secret")
	h.Set("Cookie", "sid=topsecret")
	h.Set("X-Custom-Token", "tok-secret")
	h.Set("Content-Type", "application/json")
	return req
}

// 秘密头只记头名，值一个字都不留（Notable、哈希输入、整个 Snapshot 都查）。
func TestCaptureNeverKeepsSecretValues(t *testing.T) {
	s := Capture(claudeCodeReq("s1"), false)
	dump := strings.Join(s.HeaderNames, ",") + "|" + s.UserAgent
	for k, v := range s.Notable {
		dump += "|" + k + "=" + v
	}
	for _, secret := range []string{"sk-secret-value", "sk-ant-secret", "topsecret", "tok-secret"} {
		if strings.Contains(dump, secret) {
			t.Fatalf("secret %q leaked: %s", secret, dump)
		}
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "X-Custom-Token"} {
		found := false
		for _, n := range s.HeaderNames {
			if n == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("header name %s should still be listed: %v", name, s.HeaderNames)
		}
	}
	if s.Notable["x-stainless-os"] != "MacOS" || s.Notable["anthropic-beta"] == "" || s.Notable["x-claude-code-session-id"] != "s1" {
		t.Fatalf("notable %#v", s.Notable)
	}
	if s.ClientName != "Claude Code" || s.ClientVersion != "2.0.14" {
		t.Fatalf("client %q %q", s.ClientName, s.ClientVersion)
	}
}

// 会话 id、重试次数每次变，不能让同一个客户端裂成一堆指纹；换 UA / 换 SDK 版本才算新指纹。
func TestFingerprintHashStability(t *testing.T) {
	a := Capture(claudeCodeReq("s1"), false)
	b := claudeCodeReq("s2")
	b.Header.Set("X-Stainless-Retry-Count", "2")
	b.Header.Set("Anthropic-Beta", "claude-code-20250219")
	if Capture(b, false).Hash != a.Hash {
		t.Fatal("volatile headers must not change the hash")
	}
	c := claudeCodeReq("s1")
	c.Header.Set("User-Agent", "claude-cli/2.0.15 (external, cli)")
	if Capture(c, false).Hash == a.Hash {
		t.Fatal("UA change must change the hash")
	}
	d := claudeCodeReq("s1")
	d.Header.Set("X-Stainless-Package-Version", "0.61.0")
	if Capture(d, false).Hash == a.Hash {
		t.Fatal("sdk version change must change the hash")
	}
	if len(a.Hash) != 16 {
		t.Fatalf("hash %q", a.Hash)
	}
}

// 反代转交的 JA3 / JA4 / 国家 / HTTP 版本只在直连方是可信反代时才认。
func TestProxyHeadersOnlyFromTrustedProxy(t *testing.T) {
	req := claudeCodeReq("s")
	req.Header.Set("X-JA3-Hash", "E7D705A3286E19EA42F587B344EE6865")
	req.Header.Set("X-JA4", "t13d1516h2_8daaf6152771_02713d6af862")
	req.Header.Set("CF-IPCountry", "jp")
	req.Header.Set("X-HTTP-Version", "h2")
	untrusted := Capture(req, false)
	if untrusted.JA3 != "" || untrusted.JA4 != "" || untrusted.Country != "" || untrusted.HTTPVersion != "HTTP/1.1" {
		t.Fatalf("untrusted peer must not set proxy facts: %#v", untrusted)
	}
	trusted := Capture(req, true)
	if trusted.JA3 != "e7d705a3286e19ea42f587b344ee6865" || trusted.JA4 != "t13d1516h2_8daaf6152771_02713d6af862" ||
		trusted.Country != "JP" || trusted.HTTPVersion != "HTTP/2.0" || !trusted.ViaProxy {
		t.Fatalf("trusted: %#v", trusted)
	}
	if trusted.Hash == untrusted.Hash {
		t.Fatal("tls fingerprint must be part of the hash")
	}
	// 完整 JA3 串算成 md5；乱写的值不认。
	req.Header.Set("X-JA3-Hash", "")
	req.Header.Set("X-JA3", "771,4865-4866,0-23-65281,29-23,0")
	if s := Capture(req, true); len(s.JA3) != 32 {
		t.Fatalf("full ja3 → md5, got %q", s.JA3)
	}
	req.Header.Set("X-JA3", "<script>")
	req.Header.Set("X-JA4", "bad value!")
	if s := Capture(req, true); s.JA3 != "" || s.JA4 != "" {
		t.Fatalf("garbage must be ignored: %q %q", s.JA3, s.JA4)
	}
}

// Capture 只读：请求头 map、body 都不动（出站从入站请求派生，这是「出站不变」的第一道保证）。
func TestCaptureDoesNotMutateRequest(t *testing.T) {
	req := claudeCodeReq("s")
	before := req.Header.Clone()
	_ = Capture(req, true)
	if !reflect.DeepEqual(before, req.Header) {
		t.Fatal("headers mutated")
	}
	b, _ := io.ReadAll(req.Body)
	if string(b) != `{"model":"m"}` {
		t.Fatalf("body consumed or changed: %q", b)
	}
}

// 拿到原始顺序时算 JA4H；Cookie 值不进（d 段固定 0）。
func TestJA4H(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cookie", "b=2; a=1")
	names := []string{"Host", "User-Agent", "Accept-Language", "Cookie", "Referer"}
	req = req.WithContext(context.WithValue(req.Context(), orderKey{}, headerOrder{names: names, known: true}))
	s := Capture(req, false)
	parts := strings.Split(s.JA4H, "_")
	if len(parts) != 4 || parts[0] != "ge11cr03enus" || parts[1] != sha12("Host,User-Agent,Accept-Language") ||
		parts[2] != sha12("a,b") || parts[3] != "000000000000" {
		t.Fatalf("ja4h %q", s.JA4H)
	}
	if !s.OrderKnown || !reflect.DeepEqual(s.HeaderNames, names) {
		t.Fatalf("order %v", s.HeaderNames)
	}
	// 没有原始顺序：不算 JA4H，头名按字母排。
	plain := Capture(claudeCodeReq("s"), false)
	if plain.JA4H != "" || plain.OrderKnown || plain.HeaderNames[0] != "Host" {
		t.Fatalf("plain %#v", plain)
	}
}

func TestIdentifyClient(t *testing.T) {
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	cases := []struct {
		ua      string
		h       http.Header
		name, v string
	}{
		{"claude-cli/2.0.14 (external, cli)", nil, "Claude Code", "2.0.14"},
		{"codex_cli_rs/0.50.0 (Mac OS 15.1.0; arm64) iTerm.app/3.5", h("Originator", "codex_cli_rs"), "Codex CLI", "0.50.0"},
		{"codex_vscode/0.44.0", h("Originator", "codex_vscode"), "Codex (VS Code)", "0.44.0"},
		{"codex_exec/0.50.0", h("Originator", "codex_exec"), "Codex exec", "0.50.0"},
		{"Anthropic/JS 0.65.0", h("X-Stainless-Lang", "js"), "Anthropic SDK (JS)", "0.65.0"},
		{"OpenAI/Python 1.109.1", nil, "OpenAI SDK (Python)", "1.109.1"},
		{"AsyncOpenAI/Python 1.2.3", nil, "OpenAI SDK (Python)", "1.2.3"},
		{"my-app", h("X-Stainless-Lang", "python", "X-Stainless-Package-Version", "1.50.0"), "Stainless SDK (python)", "1.50.0"},
		{"", h("X-Stainless-Lang", "python", "X-Stainless-Package-Version", "1.50.0"), "Stainless SDK (python)", "1.50.0"},
		{"GeminiCLI/0.9.0 (darwin; arm64)", nil, "Gemini CLI", "0.9.0"},
		{"", h("X-Goog-Api-Client", "google-genai-sdk/1.20.0 gl-node/v22"), "Google GenAI SDK (google-genai-sdk)", "1.20.0"},
		{"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/141.0 Safari/537.36 Edg/141.0", nil, "浏览器 Edge", "141"},
		{"Mozilla/5.0 (Macintosh) AppleWebKit/605 Version/18.1 Safari/605.1.15", nil, "浏览器 Safari", "18"},
		{"CherryStudio/1.5.2", nil, "Cherry Studio", "1.5.2"},
		{"curl/8.7.1", nil, "curl", "8.7.1"},
		{"python-httpx/0.28.1", nil, "Python httpx", "0.28.1"},
		{"Go-http-client/1.1", nil, "Go", "1.1"},
		{"deepseek-harness/0.1.5-rc.2 (+https://github.com/x/y)", nil, "deepseek-harness", "0.1.5-rc.2"},
		{"", nil, "", ""},
	}
	for _, c := range cases {
		name, v := IdentifyClient(c.ua, c.h)
		if name != c.name || v != c.v {
			t.Errorf("IdentifyClient(%q) = %q %q, want %q %q", c.ua, name, v, c.name, c.v)
		}
	}
}
