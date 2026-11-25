package mcpserver_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/mcpserver"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/server/auth"
	_ "github.com/zhizhishu/octopus/internal/server/handlers"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"testing/fstest"
)

const testModel = "gpt-image-mcp-test"
const geminiModel = "gemini-3.1-flash-image"
const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aOq0AAAAASUVORK5CYII="

type environment struct {
	ctx        context.Context
	engine     *gin.Engine
	server     *httptest.Server
	users      [2]model.User
	keys       [2]model.APIKey
	mu         sync.Mutex
	geminiBody map[string]any
}

func setup(t *testing.T) *environment {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "mcp.db"), false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatal(err)
	}
	e := &environment{ctx: context.Background()}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, ":generateContent") {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			e.mu.Lock()
			e.geminiBody = body
			e.mu.Unlock()
			fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]},"finishReason":"STOP"}]}`, pngBase64)
			return
		}
		if r.URL.Path != "/v1/images/generations" && r.URL.Path != "/v1/images/edits" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"created":1700000000,"data":[{"b64_json":%q}]}`, pngBase64)
	}))
	t.Cleanup(upstream.Close)
	for i, name := range []string{testModel, geminiModel} {
		typ := outbound.OutboundTypeOpenAIChat
		if i == 1 {
			typ = outbound.OutboundTypeGemini
		}
		channel := model.Channel{Name: name, Enabled: true, Type: typ, Model: name, Priority: 1, BaseUrls: []model.BaseUrl{{URL: upstream.URL}}, Keys: []model.ChannelKey{{Enabled: true, ChannelKey: auth.GenerateAPIKey()}}}
		if err := op.ChannelCreate(&channel, e.ctx); err != nil {
			t.Fatal(err)
		}
		price.SetLLMPrice(name, model.LLMPrice{PerCall: 0.25})
		t.Cleanup(func() { price.SetLLMPrice(name, model.LLMPrice{}) })
	}
	for i := range e.users {
		user, err := op.UserCreate(model.UserCreateRequest{Username: fmt.Sprintf("mcp-user-%d", i), Password: "fake-password-for-tests", Role: model.UserRoleUser, Status: model.UserStatusActive, Balance: 100}, e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		e.users[i] = user
		key := model.APIKey{UserID: user.ID, Name: fmt.Sprintf("demo-key-%d", i), APIKey: auth.GenerateAPIKey(), Enabled: true, MaxCost: 50, SupportedModels: testModel + "," + geminiModel}
		if err := op.APIKeyCreate(&key, e.ctx); err != nil {
			t.Fatal(err)
		}
		e.keys[i] = key
	}
	for _, setting := range []model.Setting{
		{Key: model.SettingKeyMCPEnabled, Value: "true"}, {Key: model.SettingKeyMCPGenerateEnabled, Value: "true"}, {Key: model.SettingKeyMCPEditEnabled, Value: "true"}, {Key: model.SettingKeyMCPDefaultModel, Value: testModel},
		{Key: model.SettingKeyRelayLogKeepEnabled, Value: "true"}, {Key: model.SettingKeyCORSAllowOrigins, Value: "https://client.example"},
	} {
		set(t, setting.Key, setting.Value)
	}
	e.engine = gin.New()
	e.engine.Use(middleware.Cors(), middleware.StaticEmbed("/", fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("fake static homepage")}, "mcp": &fstest.MapFile{Data: []byte("must not serve this file")}}))
	if err := router.RegisterAll(e.engine); err != nil {
		t.Fatal(err)
	}
	handler := gin.WrapH(mcpserver.NewHandler(e.engine))
	e.engine.Any("/mcp", handler)
	e.engine.Any("/mcp/sse", handler)
	e.server = httptest.NewServer(e.engine)
	t.Cleanup(e.server.Close)
	return e
}

func set(t *testing.T, key model.SettingKey, value string) {
	t.Helper()
	if err := op.SettingSetString(key, value); err != nil {
		t.Fatal(err)
	}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	r.Header.Set("User-Agent", "mcp-test-client")
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, e *environment, key model.APIKey, sse bool) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-image-client", Version: "1"}, nil)
	httpClient := &http.Client{Transport: bearerTransport{token: key.APIKey}}
	var transport mcp.Transport = &mcp.StreamableClientTransport{Endpoint: e.server.URL + "/mcp", HTTPClient: httpClient}
	if sse {
		transport = &mcp.SSEClientTransport{Endpoint: e.server.URL + "/mcp/sse", HTTPClient: httpClient}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(session *mcp.ClientSession, args map[string]any, name string) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

func mustGenerate(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	result, err := call(session, map[string]any{"prompt": "a fake octopus", "n": 1}, "generate_image")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 2 {
		t.Fatalf("generate result = %+v", result)
	}
	img, ok := result.Content[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/png" || base64.StdEncoding.EncodeToString(img.Data) != pngBase64 {
		t.Fatalf("image = %+v", result.Content[0])
	}
}

func logs(t *testing.T) []model.RelayLog {
	t.Helper()
	if err := op.RelayLogSaveDBTask(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rows []model.RelayLog
	if err := db.GetDB().Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func request(e *environment, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("User-Agent", "mcp-test-client")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, r)
	return w
}

func TestMCPIntegration(t *testing.T) {
	e := setup(t)
	t.Run("empty-and-canceled-stats", func(t *testing.T) {
		stats, err := op.MCPStatsGet(e.ctx, e.users[0], 7)
		if err != nil || stats.Totals != (op.MCPUsage{}) || len(stats.TopKeys) != 0 {
			t.Fatalf("empty stats = %+v, %v", stats, err)
		}
		ctx, cancel := context.WithCancel(e.ctx)
		cancel()
		if _, err := op.MCPStatsGet(ctx, e.users[0], 7); err == nil {
			t.Fatal("canceled stats query succeeded")
		}
	})
	a := connect(t, e, e.keys[0], false)
	b := connect(t, e, e.keys[1], false)
	t.Run("initialize-list-and-isolation", func(t *testing.T) {
		list, err := a.ListTools(e.ctx, nil)
		if err != nil || len(list.Tools) != 2 {
			t.Fatalf("list = %+v, %v", list, err)
		}
		mustGenerate(t, a)
		if got, _ := op.UserGet(e.users[1].ID); got.Balance != 100 || op.StatsAPIKeyGet(e.keys[1].ID).OutputCost != 0 {
			t.Fatal("key A changed user B")
		}
		mustGenerate(t, b)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			for _, session := range []*mcp.ClientSession{a, b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := call(session, map[string]any{"prompt": "concurrent fake octopus"}, "generate_image")
					if err != nil || result.IsError {
						t.Errorf("concurrent call = %+v %v", result, err)
					}
				}()
			}
		}
		wg.Wait()
		rows := logs(t)
		if len(rows) != 42 {
			t.Fatalf("log count = %d, want 42", len(rows))
		}
		counts := map[int]int{}
		for _, row := range rows {
			counts[row.APIKeyID]++
			owner := 0
			if row.APIKeyID == e.keys[1].ID {
				owner = 1
			}
			if row.APIKeyID != e.keys[owner].ID || row.UserID != e.users[owner].ID || row.RequestSource == nil || *row.RequestSource != "mcp" || row.FinalOutputCost != 0.25 || row.ImageCount != 1 {
				t.Fatalf("wrong isolated log: %+v", row)
			}
		}
		for i := range e.users {
			user, err := op.UserGet(e.users[i].ID)
			stats := op.StatsAPIKeyGet(e.keys[i].ID)
			if err != nil || counts[e.keys[i].ID] != 21 || user.Balance != 94.75 || stats.OutputCost+stats.InputCost != 5.25 || stats.RequestSuccess != 21 {
				t.Fatalf("user %d balance=%v stats=%+v count=%d err=%v", i, user.Balance, stats, counts[e.keys[i].ID], err)
			}
			usage, err := op.MCPStatsGet(e.ctx, user, 7)
			if err != nil || usage.Totals.Cost != 100-user.Balance || usage.Totals.Cost != stats.InputCost+stats.OutputCost {
				t.Fatalf("stats/balance/key spending differ: usage=%+v balance=%v spent=%+v error=%v", usage, user.Balance, stats, err)
			}
			t.Logf("ISOLATION U%d user_id=%d api_key_id=%d relay_logs=%d request_source=mcp calls=1+20 balance=100->%.2f balance_delta=%.2f key_spent_delta=%.2f stats_cost=%.2f", i+1, user.ID, e.keys[i].ID, counts[e.keys[i].ID], user.Balance, 100-user.Balance, stats.InputCost+stats.OutputCost, usage.Totals.Cost)
		}
		admin := model.User{ID: e.users[0].ID, Role: model.UserRoleAdmin}
		all, err := op.MCPStatsGet(e.ctx, admin, 7)
		if err != nil || all.Totals.Calls != 42 || len(all.TopUsers) != 2 || len(all.TopChannels) == 0 {
			t.Fatalf("admin breakdown = %+v, %v", all, err)
		}
		var channelCalls int64
		for _, row := range all.TopChannels {
			channelCalls += row.Calls
			if row.ChannelID == 0 || row.Name == "" {
				t.Fatalf("channel row missing identity: %+v", row)
			}
		}
		for _, row := range all.TopUsers {
			if row.Calls != 21 || row.Cost != 5.25 || row.Name == "" {
				t.Fatalf("user row = %+v", row)
			}
		}
		if channelCalls != 42 {
			t.Fatalf("channel calls = %d, want 42", channelCalls)
		}
		own, err := op.MCPStatsGet(e.ctx, e.users[0], 7)
		if err != nil || len(own.TopUsers) != 0 || len(own.TopChannels) != 0 {
			t.Fatalf("non-admin saw breakdown: %+v %v", own, err)
		}
		t.Logf("BREAKDOWN admin users=%d channels=%d channel_calls=%d non_admin_breakdown=0", len(all.TopUsers), len(all.TopChannels), channelCalls)
	})
	t.Run("session-hijack", func(t *testing.T) {
		if a.ID() == "" {
			t.Fatal("missing stateful session ID")
		}
		for _, method := range []string{"GET", "POST", "DELETE"} {
			r := httptest.NewRequest(method, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`))
			r.Header.Set("Authorization", "Bearer "+e.keys[1].APIKey)
			r.Header.Set("Mcp-Session-Id", a.ID())
			r.Header.Set("MCP-Protocol-Version", "2025-11-25")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			w := httptest.NewRecorder()
			e.engine.ServeHTTP(w, r)
			if w.Code != 403 && w.Code != 404 {
				t.Fatalf("%s hijack = %d %s", method, w.Code, w.Body.String())
			}
			t.Logf("SESSION key_B_using_key_A_session method=%s status=%d rejected=true", method, w.Code)
		}
		if _, err := a.ListTools(e.ctx, nil); err != nil {
			t.Fatalf("hijack damaged original session: %v", err)
		}
	})
	t.Run("dynamic-tool-settings", func(t *testing.T) {
		set(t, model.SettingKeyMCPGenerateEnabled, "false")
		list, err := a.ListTools(e.ctx, nil)
		if err != nil || len(list.Tools) != 1 || list.Tools[0].Name != "edit_image" {
			t.Fatalf("filtered list = %+v %v", list, err)
		}
		result, err := call(a, map[string]any{"prompt": "fake"}, "generate_image")
		if err != nil || !result.IsError {
			t.Fatalf("disabled call = %+v %v", result, err)
		}
		set(t, model.SettingKeyMCPEditEnabled, "false")
		list, err = a.ListTools(e.ctx, nil)
		if err != nil || len(list.Tools) != 0 {
			t.Fatalf("empty list = %+v %v", list, err)
		}
		set(t, model.SettingKeyMCPGenerateEnabled, "true")
		set(t, model.SettingKeyMCPEditEnabled, "true")
		set(t, model.SettingKeyMCPEnabled, "false")
		if got := request(e, "POST", "/mcp", e.keys[0].APIKey, ""); got.Code != 404 {
			t.Fatalf("disabled = %d", got.Code)
		}
		set(t, model.SettingKeyMCPEnabled, "true")
	})
	t.Run("SSE", func(t *testing.T) {
		session := connect(t, e, e.keys[0], true)
		list, err := session.ListTools(e.ctx, nil)
		if err != nil || len(list.Tools) != 2 {
			t.Fatalf("SSE list = %+v %v", list, err)
		}
		mustGenerate(t, session)
	})
	t.Run("authentication", func(t *testing.T) {
		if err := op.StatsSaveDB(e.ctx); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/mcp", "/mcp/sse"} {
			for _, token := range []string{"", auth.GenerateAPIKey(), "invalid"} {
				if got := request(e, "POST", path, token, ""); got.Code != 401 {
					t.Fatalf("missing/invalid auth %s = %d", path, got.Code)
				}
			}
			if got := request(e, "POST", path+"?key="+e.keys[0].APIKey, e.keys[0].APIKey, ""); got.Code != 401 {
				t.Fatalf("query key = %d", got.Code)
			}
		}
		for _, column := range []string{"revoked_at", "expire_at", "enabled"} {
			key := model.APIKey{UserID: e.users[0].ID, Name: "fake-invalid-" + column, APIKey: auth.GenerateAPIKey(), Enabled: true}
			if err := op.APIKeyCreate(&key, e.ctx); err != nil {
				t.Fatal(err)
			}
			var value any = time.Now().Add(-time.Hour).Unix()
			if column == "enabled" {
				value = false
			}
			if err := db.GetDB().Model(&key).Update(column, value).Error; err != nil {
				t.Fatal(err)
			}
			if err := op.InitCache(); err != nil {
				t.Fatal(err)
			}
			if got := request(e, "POST", "/mcp", key.APIKey, ""); got.Code != 401 {
				t.Fatalf("%s key = %d", column, got.Code)
			}
		}
	})
	t.Run("model-whitelist", func(t *testing.T) {
		result, err := call(a, map[string]any{"prompt": "fake", "model": "not-in-key-whitelist"}, "generate_image")
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "model not supported") {
			t.Fatalf("model rejection = %+v %v", result, err)
		}
	})
	t.Run("billing-equality", func(t *testing.T) {
		before := len(logs(t))
		userBefore, _ := op.UserGet(e.users[0].ID)
		spentBefore := op.StatsAPIKeyGet(e.keys[0].ID)
		direct := request(e, "POST", "/v1/images/generations", e.keys[0].APIKey, `{"model":"`+testModel+`","prompt":"a fake octopus","n":1}`)
		if direct.Code != 200 {
			t.Fatalf("direct = %d %s", direct.Code, direct.Body.String())
		}
		userDirect, _ := op.UserGet(e.users[0].ID)
		spentDirect := op.StatsAPIKeyGet(e.keys[0].ID)
		mustGenerate(t, a)
		userMCP, _ := op.UserGet(e.users[0].ID)
		spentMCP := op.StatsAPIKeyGet(e.keys[0].ID)
		directCost := userBefore.Balance - userDirect.Balance
		mcpCost := userDirect.Balance - userMCP.Balance
		if directCost != 0.25 || mcpCost != directCost || spentDirect.InputCost+spentDirect.OutputCost-spentBefore.InputCost-spentBefore.OutputCost != directCost || spentMCP.InputCost+spentMCP.OutputCost-spentDirect.InputCost-spentDirect.OutputCost != mcpCost {
			t.Fatalf("billing deltas differ: direct=%v mcp=%v before=%+v direct=%+v mcp=%+v", directCost, mcpCost, spentBefore, spentDirect, spentMCP)
		}
		rows := logs(t)[before:]
		if len(rows) != 2 {
			t.Fatalf("equality logs = %d", len(rows))
		}
		x, y := rows[0], rows[1]
		if x.Cost != y.Cost || x.RequestSource != nil || y.RequestSource == nil {
			t.Fatalf("cost/source direct=%+v mcp=%+v", x, y)
		}
		if x.UserID != y.UserID || x.APIKeyID != y.APIKeyID || x.RequestEndpoint != y.RequestEndpoint || x.RequestPath != y.RequestPath || x.RequestModelName != y.RequestModelName || x.ActualModelName != y.ActualModelName || x.ChannelId != y.ChannelId || x.InputTokens != y.InputTokens || x.OutputTokens != y.OutputTokens || x.ImageCount != y.ImageCount || x.BillingModel != y.BillingModel || x.FinalMultiplier != y.FinalMultiplier || x.FinalOutputCost != y.FinalOutputCost {
			t.Fatalf("direct/MCP log mismatch: %+v vs %+v", x, y)
		}
		if !strings.HasPrefix(y.RequestUserAgent, "octopus-mcp/") {
			t.Fatal("missing MCP user agent prefix")
		}
		t.Logf("BILLING direct_balance_delta=%.2f mcp_balance_delta=%.2f direct_key_spent_delta=%.2f mcp_key_spent_delta=%.2f log_billing_fields_equal=true", directCost, mcpCost, spentDirect.InputCost+spentDirect.OutputCost-spentBefore.InputCost-spentBefore.OutputCost, spentMCP.InputCost+spentMCP.OutputCost-spentDirect.InputCost-spentDirect.OutputCost)
	})
	t.Run("Gemini-generation-and-mask", func(t *testing.T) {
		result, err := call(a, map[string]any{"model": geminiModel, "prompt": "fake Gemini", "size": "2048x1152"}, "generate_image")
		if err != nil || result.IsError {
			t.Fatalf("Gemini = %+v %v", result, err)
		}
		e.mu.Lock()
		body, _ := json.Marshal(e.geminiBody)
		e.mu.Unlock()
		if !strings.Contains(string(body), `"aspectRatio":"16:9"`) || !strings.Contains(string(body), `"imageSize":"2K"`) {
			t.Fatalf("Gemini body = %s", body)
		}
		before, _ := op.UserGet(e.users[0].ID)
		spent := op.StatsAPIKeyGet(e.keys[0].ID)
		result, err = call(a, map[string]any{"model": geminiModel, "prompt": "fake edit", "images": []string{"data:image/png;base64," + pngBase64}, "mask": pngBase64}, "edit_image")
		if err != nil || !result.IsError {
			t.Fatalf("Gemini mask = %+v %v", result, err)
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if text != "502: service temporarily unavailable, please retry" {
			t.Fatalf("mask text = %s", text)
		}
		after, _ := op.UserGet(e.users[0].ID)
		if before.Balance != after.Balance || op.StatsAPIKeyGet(e.keys[0].ID).OutputCost != spent.OutputCost {
			t.Fatal("failed Gemini mask was charged")
		}
	})
	t.Run("stats-scope-and-inputs", func(t *testing.T) {
		rows := logs(t)
		admin, err := op.UserCreate(model.UserCreateRequest{Username: "fake-mcp-admin", Password: "fake-password", Role: model.UserRoleAdmin, Status: model.UserStatusActive}, e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, user := range []model.User{admin, e.users[0], e.users[1]} {
			token, _, err := auth.GenerateJWTToken(user, 10)
			if err != nil {
				t.Fatal(err)
			}
			got := request(e, "GET", "/api/v1/mcp/stats?days=7", token, "")
			var payload struct {
				Data op.MCPStats `json:"data"`
			}
			if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &payload) != nil {
				t.Fatalf("stats = %d %s", got.Code, got.Body.String())
			}
			want := op.MCPUsage{}
			for _, row := range rows {
				if row.RequestSource != nil && *row.RequestSource == "mcp" && (user.IsAdmin() || row.UserID == user.ID) {
					want.Calls++
					want.Images += int64(row.ImageCount)
					if row.Error == "" {
						want.Cost += row.FinalInputCost + row.FinalOutputCost + row.FinalCacheReadCost + row.FinalCacheWriteCost
					}
					if row.Error != "" {
						want.Failed++
					}
				}
			}
			if payload.Data.Totals.Calls != want.Calls || payload.Data.Totals.Images != want.Images || payload.Data.Totals.Failed != want.Failed || math.Abs(payload.Data.Totals.Cost-want.Cost) > 1e-9 {
				t.Fatalf("stats=%+v want=%+v", payload.Data, want)
			}
			if !user.IsAdmin() {
				for _, key := range payload.Data.TopKeys {
					if key.APIKeyID != e.keys[user.ID-e.users[0].ID].ID {
						t.Fatalf("cross-user top key: %+v", key)
					}
				}
			}
			for _, days := range []string{"0", "91", "bad"} {
				if bad := request(e, "GET", "/api/v1/mcp/stats?days="+days, token, ""); bad.Code != 400 {
					t.Fatalf("invalid days = %d", bad.Code)
				}
			}
		}
		if got := request(e, "GET", "/api/v1/mcp/stats", "", ""); got.Code != 400 {
			t.Fatalf("stats without JWT=%d", got.Code)
		}
	})
}
