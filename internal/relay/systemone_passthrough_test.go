package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// TestRawProtocolHandlerSystemonePassesDecisionPayloadThrough 钉住决策类上游
// （systemone 协议）的透传契约：
//
//   - 请求体里的 state / questions 必须原样到达上游，一个字节都不许被改写。
//     这是整条链路存在的理由：这类模型不接受 messages 数组，任何协议转换都会把
//     它改坏（上游会回 400 "supports native decisions, not chat"）。
//   - 只有 model 字段允许被 model_mapping 重写。
//   - 上游按 id 返回的 answers 必须原样回到客户端。
//
// 同时钉住 URL：用户在渠道面板里可能把 base 抄成根域名、抄成 …/v1、也可能连着
// 端点一起抄成 …/v1/systemone 甚至 …/systemone。四种填法都必须收敛到
// /v1/systemone。后两种在旧实现里会叠成 …/systemone/v1/systemone（上游
// 404 Unsupported serving path），是这条用例的靶心。
//
// 🔴 每个子用例用**独立的模型名**建独立渠道。否则四个渠道会一起参与同一模型的
// 路由，负载均衡可能把请求发给另一个（base 填法不同的）渠道，用例就算失败也
// 指不出是哪种填法坏的 —— 曾经因此让这条用例在未修复的代码上假绿。
func TestRawProtocolHandlerSystemonePassesDecisionPayloadThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	baseForms := []struct {
		name   string
		suffix string
	}{
		{"root_base", ""},
		{"base_with_v1", "/v1"},
		{"base_with_full_endpoint", "/v1/systemone"},
		{"base_with_bare_endpoint", "/systemone"},
	}

	for _, bf := range baseForms {
		t.Run(bf.name, func(t *testing.T) {
			// 每个子用例一套干净的库 + 渠道 + 上游，做到完全隔离。
			ctx := setupRelayErrorDB(t)

			requestModel := "decision-alias-" + bf.name
			upstreamModel := "upstream-decision-" + bf.name

			var (
				mu      sync.Mutex
				gotPath string
				gotBody map[string]any
				gotRaw  string
				gotAuth string
				gotCT   string
				called  bool
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body map[string]any
				_ = json.Unmarshal(raw, &body)

				mu.Lock()
				gotPath = r.URL.Path
				gotBody = body
				gotRaw = string(raw)
				gotAuth = r.Header.Get("Authorization")
				gotCT = r.Header.Get("Content-Type")
				called = true
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id":"decision_test","model":"` + upstreamModel + `",
					"answers":{"urgent":{"type":"noul","noul":0.97}},
					"usage":{"input_tokens":281,"output_tokens":20,"cost":5.901e-06},
					"provider":"typesafe"
				}`))
			}))
			t.Cleanup(upstream.Close)

			channel := dbmodel.Channel{
				Name:         "systemone-" + bf.name,
				Type:         outbound.OutboundTypeCustomOpenAIChat,
				Enabled:      true,
				BaseUrls:     []dbmodel.BaseUrl{{URL: upstream.URL + bf.suffix}},
				Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "xpl-test-key"}},
				Model:        upstreamModel,
				ModelMapping: map[string]string{requestModel: upstreamModel},
				Priority:     1,
			}
			if err := op.ChannelCreate(&channel, ctx); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			clientBody := fmt.Sprintf(`{
				"model":%q,
				"state":"The checkout is broken and users cannot pay.",
				"questions":{
					"urgent":{"type":"noul","instructions":"Does this need immediate attention?"},
					"route":{"type":"choice","instructions":"Which team?","criteria":{"eng":"Engineering","biz":"Business"}}
				}
			}`, requestModel)

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(clientBody))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			RawProtocolHandler(RawProtocolOptions{Endpoint: "/systemone", Name: "systemone"}, c)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected systemone passthrough to succeed, got %d body %s", rec.Code, rec.Body.String())
			}

			mu.Lock()
			path, body, rawBody, auth, ct, wasCalled := gotPath, gotBody, gotRaw, gotAuth, gotCT, called
			mu.Unlock()

			if !wasCalled {
				t.Fatal("upstream was never called")
			}
			if path != "/v1/systemone" {
				t.Fatalf("base suffix %q: upstream path = %q, want %q (must not double the systemone suffix)", bf.suffix, path, "/v1/systemone")
			}
			if auth != "Bearer xpl-test-key" {
				t.Fatalf("upstream auth = %q, want %q", auth, "Bearer xpl-test-key")
			}
			if ct != "application/json" {
				t.Fatalf("upstream content-type = %q, want application/json", ct)
			}
			if got := body["model"]; got != upstreamModel {
				t.Fatalf("upstream model = %v, want %q (model_mapping must rewrite the model)", got, upstreamModel)
			}
			if got := body["state"]; got != "The checkout is broken and users cannot pay." {
				t.Fatalf("upstream state = %v, want the original decision state untouched", got)
			}

			questions, ok := body["questions"].(map[string]any)
			if !ok {
				t.Fatalf("upstream questions missing or wrong shape: %v (raw body: %s)", body["questions"], rawBody)
			}
			if len(questions) != 2 {
				t.Fatalf("upstream questions has %d ids, want 2: %v", len(questions), questions)
			}
			urgent, ok := questions["urgent"].(map[string]any)
			if !ok {
				t.Fatalf("urgent question wrong shape: %v", questions["urgent"])
			}
			if urgent["type"] != "noul" || urgent["instructions"] != "Does this need immediate attention?" {
				t.Fatalf("urgent question mutated: %v", urgent)
			}
			route, ok := questions["route"].(map[string]any)
			if !ok {
				t.Fatalf("route question wrong shape: %v", questions["route"])
			}
			criteria, ok := route["criteria"].(map[string]any)
			if !ok || criteria["eng"] != "Engineering" || criteria["biz"] != "Business" {
				t.Fatalf("route criteria mutated: %v", route["criteria"])
			}

			// 决策协议不该被塞进聊天参数。
			for _, forbidden := range []string{"messages", "stream", "max_tokens"} {
				if _, present := body[forbidden]; present {
					t.Fatalf("upstream body must not gain a %q field (it is not a chat request): %s", forbidden, rawBody)
				}
			}

			// 响应原样回给客户端。
			if !strings.Contains(rec.Body.String(), `"noul":0.97`) {
				t.Fatalf("client response lost the decision answers: %s", rec.Body.String())
			}
		})
	}
}
