package helper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// setupFetchTestDB initialises the sqlite cache the HTTP client path reads
// (upstream uTLS switch, proxy settings). Without it ChannelHttpClient cannot
// answer and every fetch would fail before reaching the httptest upstream.
func setupFetchTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	return context.Background()
}

func customFetchChannel(baseURL string) model.Channel {
	return model.Channel{
		Name:         "custom-fetch",
		Type:         outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls:     []model.BaseUrl{{URL: baseURL}},
		Keys:         []model.ChannelKey{{Enabled: true, ChannelKey: "sk-fetch-test"}},
		CustomHeader: []model.CustomHeader{{HeaderKey: "X-Org", HeaderValue: "octopus"}},
	}
}

type fetchHit struct {
	path          string
	query         string
	authorization string
	org           string
}

// recordingModelsUpstream answers every request with reply(path) and records what
// arrived. reply returns (statusCode, body).
func recordingModelsUpstream(t *testing.T, reply func(path string) (int, string)) (*httptest.Server, func() []fetchHit) {
	t.Helper()
	var (
		mu   sync.Mutex
		hits []fetchHit
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, fetchHit{
			path:          r.URL.Path,
			query:         r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			org:           r.Header.Get("X-Org"),
		})
		mu.Unlock()
		status, body := reply(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, func() []fetchHit {
		mu.Lock()
		defer mu.Unlock()
		return append([]fetchHit(nil), hits...)
	}
}

const modelsListJSON = `{"object":"list","data":[{"id":"model-a"},{"id":"model-b"}]}`

// 自定义渠道的模型同步：先按版本前缀派生兄弟 /models，成功就直接返回。
func TestFetchModelsCustomChannelDerivesSiblingModelsURL(t *testing.T) {
	ctx := setupFetchTestDB(t)
	server, hits := recordingModelsUpstream(t, func(string) (int, string) {
		return http.StatusOK, modelsListJSON
	})

	channel := customFetchChannel(server.URL + "/api/paas/v4/chat/completions?api-version=2024-10-21")
	models, err := FetchModels(ctx, channel)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Fatalf("models = %v", models)
	}

	got := hits()
	if len(got) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(got))
	}
	if got[0].path != "/api/paas/v4/models" {
		t.Fatalf("upstream path = %q, want the version-prefixed sibling", got[0].path)
	}
	if got[0].query != "api-version=2024-10-21" {
		t.Fatalf("upstream query = %q, want the channel query preserved", got[0].query)
	}
	if got[0].authorization != "Bearer sk-fetch-test" {
		t.Fatalf("authorization = %q, want the channel key", got[0].authorization)
	}
	if got[0].org != "octopus" {
		t.Fatalf("custom header lost: %#v", got[0])
	}
}

// 标准 /v1/chat/completions 的兄弟就是 /v1/models：候选去重后只发一次。
func TestFetchModelsCustomChannelDedupesCandidates(t *testing.T) {
	ctx := setupFetchTestDB(t)
	server, hits := recordingModelsUpstream(t, func(string) (int, string) {
		return http.StatusOK, modelsListJSON
	})

	channel := customFetchChannel(server.URL + "/v1/chat/completions")
	if _, err := FetchModels(ctx, channel); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	got := hits()
	if len(got) != 1 || got[0].path != "/v1/models" {
		t.Fatalf("requests = %#v, want exactly one /v1/models request", got)
	}
}

// 兄弟 /models 失败或返回空列表时，退到同源 /v1/models；两次为界，不无限重试。
func TestFetchModelsCustomChannelFallsBackWhenSiblingIsMissingOrEmpty(t *testing.T) {
	cases := []struct {
		name  string
		reply func(path string) (int, string)
	}{
		{
			name: "sibling 404",
			reply: func(path string) (int, string) {
				if path == "/api/paas/v4/models" {
					return http.StatusNotFound, `{"error":"not found"}`
				}
				return http.StatusOK, modelsListJSON
			},
		},
		{
			name: "sibling empty list",
			reply: func(path string) (int, string) {
				if path == "/api/paas/v4/models" {
					return http.StatusOK, `{"object":"list","data":[]}`
				}
				return http.StatusOK, modelsListJSON
			},
		},
		{
			name: "sibling not json",
			reply: func(path string) (int, string) {
				if path == "/api/paas/v4/models" {
					return http.StatusOK, `<html>gateway</html>`
				}
				return http.StatusOK, modelsListJSON
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupFetchTestDB(t)
			server, hits := recordingModelsUpstream(t, tc.reply)
			channel := customFetchChannel(server.URL + "/api/paas/v4/chat/completions")

			models, err := FetchModels(ctx, channel)
			if err != nil {
				t.Fatalf("FetchModels: %v", err)
			}
			if len(models) != 2 {
				t.Fatalf("models = %v, want the fallback list", models)
			}
			got := hits()
			if len(got) != 2 || got[0].path != "/api/paas/v4/models" || got[1].path != "/v1/models" {
				t.Fatalf("requests = %#v, want sibling then same-origin fallback", got)
			}
		})
	}
}

// 显式绝对 models 地址优先，别的候选一个都不试。
func TestFetchModelsCustomChannelExplicitOverrideWins(t *testing.T) {
	ctx := setupFetchTestDB(t)
	server, hits := recordingModelsUpstream(t, func(string) (int, string) {
		return http.StatusOK, modelsListJSON
	})

	channel := customFetchChannel(server.URL + "/api/paas/v4/chat/completions")
	channel.OpenAIModelsPath = server.URL + "/explicit/models?team=octopus"
	if _, err := FetchModels(ctx, channel); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	got := hits()
	if len(got) != 1 || got[0].path != "/explicit/models" || got[0].query != "team=octopus" {
		t.Fatalf("requests = %#v, want only the explicit models URL", got)
	}
}

// 全失败时必须给出有信息量的错误，且错误里不许出现原始 URL 或 key。
func TestFetchModelsCustomChannelAllCandidatesFailKeepsErrorSafe(t *testing.T) {
	ctx := setupFetchTestDB(t)
	server, hits := recordingModelsUpstream(t, func(string) (int, string) {
		return http.StatusNotFound, `{"error":"no such route"}`
	})

	channel := customFetchChannel(server.URL + "/api/paas/v4/chat/completions")
	models, err := FetchModels(ctx, channel)
	if err == nil {
		t.Fatalf("expected an error, got models %v", models)
	}
	if models != nil {
		t.Fatalf("a failed fetch must not return a list: %v", models)
	}
	msg := err.Error()
	for _, want := range []string{"HTTP 404", "derived /models endpoint", "origin /v1/models fallback"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q is missing %q", msg, want)
		}
	}
	// No URL, no key, and no upstream body: a gateway error body can echo either.
	for _, forbidden := range []string{server.URL, "127.0.0.1", "sk-fetch-test", "no such route"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("error %q leaks %q", msg, forbidden)
		}
	}
	if got := hits(); len(got) != 2 {
		t.Fatalf("upstream requests = %d, want the bounded 2 candidates", len(got))
	}
}

// 空的 / 非法模型清单要报错，不能静默变成"这个渠道没有模型"。
func TestFetchModelsCustomChannelRejectsEmptyAndMalformedCatalogs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty", body: `{"object":"list","data":[]}`, want: "empty model list"},
		{name: "malformed", body: `{"object":"list","data":`, want: "JSON model list"},
		{name: "wrong shape", body: `{"data":{"id":"nope"}}`, want: "JSON model list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupFetchTestDB(t)
			server, _ := recordingModelsUpstream(t, func(string) (int, string) {
				return http.StatusOK, tc.body
			})
			channel := customFetchChannel(server.URL + "/v1/chat/completions")

			models, err := FetchModels(ctx, channel)
			if err == nil {
				t.Fatalf("expected an error, got models %v", models)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// 存的那条 URL 带 userinfo 时，派生请求不带它，错误里也绝不回显它。
func TestFetchModelsCustomChannelNeverLeaksCredentials(t *testing.T) {
	ctx := setupFetchTestDB(t)
	server, hits := recordingModelsUpstream(t, func(string) (int, string) {
		return http.StatusNotFound, `{"error":"no such route"}`
	})

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword("gateway-user", "supersecret")
	// userinfo + query together: the derivation must parse them, keep the query on
	// every candidate, and never copy the credentials into a derived URL or message.
	channel := customFetchChannel(parsed.String() + "/api/paas/v4/chat/completions?api-version=v9")

	_, err = FetchModels(ctx, channel)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "supersecret") || strings.Contains(err.Error(), "gateway-user") {
		t.Fatalf("error leaks credentials: %v", err)
	}
	got := hits()
	if len(got) == 0 {
		t.Fatal("no upstream request was made")
	}
	if got[0].path != "/api/paas/v4/models" || got[0].query != "api-version=v9" {
		t.Fatalf("first request = %#v, want the derived sibling with the query preserved", got[0])
	}
	for _, hit := range got {
		if hit.authorization != "Bearer sk-fetch-test" {
			t.Fatalf("derived request must authenticate with the channel key, got %q", hit.authorization)
		}
	}
}

// 渠道配置本身只被读：字典里的 BaseUrls / models 覆盖 / 类型都不许被改动。
func TestFetchModelsCustomChannelDoesNotMutateConfiguration(t *testing.T) {
	ctx := setupFetchTestDB(t)
	server, _ := recordingModelsUpstream(t, func(string) (int, string) {
		return http.StatusOK, modelsListJSON
	})

	base := server.URL + "/api/paas/v4/chat/completions"
	channel := customFetchChannel(base)
	if _, err := FetchModels(ctx, channel); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if channel.Type != outbound.OutboundTypeCustomOpenAIChat {
		t.Fatalf("channel type mutated: %d", channel.Type)
	}
	if len(channel.BaseUrls) != 1 || channel.BaseUrls[0].URL != base {
		t.Fatalf("channel base urls mutated: %#v", channel.BaseUrls)
	}
	if channel.OpenAIModelsPath != "" {
		t.Fatalf("models path mutated: %q", channel.OpenAIModelsPath)
	}
}

// 非自定义渠道的行为一个字节没变：标准 /v1/models 拼接，空清单仍返回空列表。
func TestFetchModelsStandardChannelKeepsLegacyBehavior(t *testing.T) {
	ctx := setupFetchTestDB(t)
	server, hits := recordingModelsUpstream(t, func(string) (int, string) {
		return http.StatusOK, `{"object":"list","data":[]}`
	})

	channel := model.Channel{
		Name:     "plain-chat",
		Type:     outbound.OutboundTypeOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: server.URL + "/v1"}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "sk-plain"}},
	}
	models, err := FetchModels(ctx, channel)
	if err != nil {
		t.Fatalf("plain channel fetch must keep returning an empty list without an error: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %v, want empty", models)
	}
	if got := hits(); len(got) != 1 || got[0].path != "/v1/models" {
		t.Fatalf("requests = %#v, want one /v1/models", got)
	}
}

// 传输层失败只报固定成因（超时 / 取消 / 其他）。底层错误可能带着请求 URL（含
// userinfo）或解析后的主机名，这些文字会进 UI 和日志，所以一律不透出。
func TestFetchModelsCustomChannelTransportFailuresAreGeneric(t *testing.T) {
	assertSafe := func(t *testing.T, msg, serverURL string) {
		t.Helper()
		for _, forbidden := range []string{serverURL, "127.0.0.1", "sk-fetch-test", "connection refused"} {
			if forbidden != "" && strings.Contains(msg, forbidden) {
				t.Fatalf("error %q leaks %q", msg, forbidden)
			}
		}
	}

	t.Run("cancelled context", func(t *testing.T) {
		setupFetchTestDB(t)
		server, _ := recordingModelsUpstream(t, func(string) (int, string) {
			return http.StatusOK, modelsListJSON
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := FetchModels(ctx, customFetchChannel(server.URL+"/v1/chat/completions"))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("error %q should report a cancelled request", err.Error())
		}
		assertSafe(t, err.Error(), server.URL)
	})

	t.Run("unreachable upstream", func(t *testing.T) {
		setupFetchTestDB(t)
		server, _ := recordingModelsUpstream(t, func(string) (int, string) {
			return http.StatusOK, modelsListJSON
		})
		channel := customFetchChannel(server.URL + "/v1/chat/completions")
		server.Close()

		_, err := FetchModels(context.Background(), channel)
		if err == nil {
			t.Fatal("expected an error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "could not be completed") && !strings.Contains(msg, "timed out") {
			t.Fatalf("error %q should report a fixed transport cause", msg)
		}
		if !strings.Contains(msg, "derived /models endpoint") {
			t.Fatalf("error %q should keep the candidate label", msg)
		}
		assertSafe(t, msg, server.URL)
	})
}
