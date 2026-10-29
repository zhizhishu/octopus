package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// systemone 是决策协议，不能发给嵌入渠道（type 5）。共享的
// isOpenAIWireChannelType 仍然收 0/1/5/6——音频、completions、rerank 还靠它。
func TestSystemoneSkipsEmbeddingChannelAndKeepsChatTypes(t *testing.T) {
	if !isOpenAIWireChannelType(outbound.OutboundTypeOpenAIEmbedding) {
		t.Fatal("isOpenAIWireChannelType must still accept Embedding; only systemone narrows it")
	}
	if rawProtocolAllowsChannel(RawProtocolOptions{Name: "systemone", Endpoint: "/systemone"}, outbound.OutboundTypeOpenAIEmbedding) {
		t.Fatal("systemone must reject Embedding")
	}
	for _, typ := range []outbound.OutboundType{
		outbound.OutboundTypeOpenAIChat,
		outbound.OutboundTypeOpenAIResponse,
		outbound.OutboundTypeCustomOpenAIChat,
		outbound.OutboundTypeSystemOne,
	} {
		if !rawProtocolAllowsChannel(RawProtocolOptions{Name: "systemone", Endpoint: "/systemone"}, typ) {
			t.Fatalf("systemone must still allow channel type %d", typ)
		}
	}
	if !rawProtocolAllowsChannel(RawProtocolOptions{Name: "rerank", Endpoint: "/rerank"}, outbound.OutboundTypeOpenAIEmbedding) {
		t.Fatal("rerank must still allow Embedding; the shared wire table must not shrink")
	}
	if rawProtocolAllowsChannel(RawProtocolOptions{Name: "rerank", Endpoint: "/rerank"}, outbound.OutboundTypeSystemOne) {
		t.Fatal("type 7 must not serve rerank")
	}
	if rawProtocolAllowsChannel(RawProtocolOptions{Name: "chat", Endpoint: "/chat/completions"}, outbound.OutboundTypeSystemOne) {
		t.Fatal("type 7 must not serve chat raw endpoints")
	}

	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	t.Run("embedding skipped", func(t *testing.T) {
		var hits int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)
		const modelName = "systemone-embed-only"
		newSystemoneChannel(t, ctx, "systemone-embed", outbound.OutboundTypeOpenAIEmbedding, upstream.URL, modelName)

		rec := postSystemone(t, modelName)
		if rec.Code == http.StatusOK {
			t.Fatalf("embedding channel must not serve systemone, body %s", rec.Body.String())
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Fatalf("embedding channel was contacted %d times", got)
		}
		assertSystemoneSkip(t, ctx, "unsupported channel type for systemone")
	})

	allowed := []struct {
		name string
		typ  outbound.OutboundType
	}{
		{"openai_chat", outbound.OutboundTypeOpenAIChat},
		{"openai_response", outbound.OutboundTypeOpenAIResponse},
		{"custom_openai_chat", outbound.OutboundTypeCustomOpenAIChat},
		{"systemone", outbound.OutboundTypeSystemOne},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"ok","answers":{"q":{"type":"noul","noul":1}},"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			t.Cleanup(upstream.Close)
			modelName := "systemone-allow-" + tc.name
			newSystemoneChannel(t, ctx, "systemone-"+tc.name, tc.typ, upstream.URL, modelName)

			rec := postSystemone(t, modelName)
			if rec.Code != http.StatusOK {
				t.Fatalf("type %d must serve systemone, got %d body %s", tc.typ, rec.Code, rec.Body.String())
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Fatalf("upstream hits = %d, want 1", got)
			}
		})
	}
}

func newSystemoneChannel(t *testing.T, ctx context.Context, name string, typ outbound.OutboundType, upstreamURL, modelName string) {
	t.Helper()
	ch := dbmodel.Channel{
		Name:     name,
		Type:     typ,
		Enabled:  true,
		Model:    modelName,
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "systemone-type-key"}},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
}

func postSystemone(t *testing.T, modelName string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{"model":"`+modelName+`","state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/systemone", Name: "systemone"}, c)
	return rec
}

func TestCustomChannelSystemoneUsesVerbatimURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","answers":{"q":{"type":"noul","noul":1}}}`))
	}))
	t.Cleanup(upstream.Close)
	const modelName = "verbatim-systemone"
	newSystemoneChannel(t, ctx, "verbatim-custom", outbound.OutboundTypeCustomOpenAIChat, upstream.URL+"/custom/final", modelName)
	rec := postSystemone(t, modelName)
	if rec.Code != http.StatusOK {
		t.Fatalf("type 6 systemone got %d body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/custom/final" {
		t.Fatalf("type 6 path = %q, want the verbatim base path", gotPath)
	}
}

func TestSystemOneChannelIsNotUsedForChat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	const modelName = "decision-not-chat"
	newSystemoneChannel(t, ctx, "decision-only", outbound.OutboundTypeSystemOne, upstream.URL, modelName)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+modelName+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	Handler(inbound.InboundTypeOpenAIChat, c)
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("chat routing contacted a type 7 channel %d times", hits)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("chat request must not succeed on a systemone-only channel, body %s", rec.Body.String())
	}
}

func TestSystemOneChannelHasNoCLIIdentity(t *testing.T) {
	ch := &dbmodel.Channel{
		Type:  outbound.OutboundTypeSystemOne,
		Cloak: dbmodel.ChannelCloak{Mode: "always", Shape: "codex"},
	}
	if got := channelTLSFace(ch); got != "" {
		t.Fatalf("type 7 TLS face = %q, want empty", got)
	}
	if codexIdentityActive(ch) {
		t.Fatal("type 7 must not wear a codex identity")
	}
	req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/systemone", nil)
	if err != nil {
		t.Fatal(err)
	}
	ApplyChannelWireHeaders(req, ChannelWireHeaderOptions{Channel: ch})
	ua := strings.ToLower(req.Header.Get("User-Agent"))
	if ua == "" || strings.Contains(ua, "codex") || strings.Contains(ua, "claude") {
		t.Fatalf("type 7 user agent = %q, want a generic identity", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("Originator") != "" {
		t.Fatalf("type 7 synthesized originator %q", req.Header.Get("Originator"))
	}
}

func assertSystemoneSkip(t *testing.T, ctx context.Context, needle string) {
	t.Helper()
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 20, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	for _, row := range logs {
		for _, attempt := range row.Attempts {
			if strings.Contains(attempt.Msg, needle) {
				return
			}
		}
	}
	t.Fatalf("no attempt message contains %q in %+v", needle, logs)
}
