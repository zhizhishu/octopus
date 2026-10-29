package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
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
// 同时钉住 URL：决策渠道（type 7）只填根地址。用户也可能把 base 抄成 …/v1、
// …/v1/systemone 或 …/systemone。四种填法都必须收敛到 /v1/systemone。
// 自定义渠道（type 6）不再走这条拼接，它按填入的地址原样发送。
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
				Type:         outbound.OutboundTypeSystemOne,
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

// TestSystemoneUsageIsBilledFromDecisionResponse 钉住「决策端点的用量真的进了账单」。
//
// 决策类上游的响应体是 {id, model, answers, usage, provider}，没有 choices、
// 没有 chat 那套结构。会不会被计费完全取决于 usageScanner 能不能在原始响应流里
// 找到 "usage":{...} 并认出 input_tokens / output_tokens 这两个键。
//
// 【为什么要专门钉】buildBillingSnapshot 在 usage == nil 时直接返回、不计费
// （internal/relay/raw_protocol.go 里 `if usage == nil { return snapshot }`）。
// 「上游字段名对不上 → 一分钱不算」仍然可能发生。缺价是另一条路：请求照样成功、
// token 照记、金额是 0，但会打一条按模型去重的警告（见
// TestSystemoneUnpricedModelWarnsOnceAndStillSucceeds）。
// 这条用例把 input=281 / output=20 变成硬断言：将来谁把 usageScanner 改坏、
// 或者上游把用量换个地方放，测试立刻红，而不是账目悄悄归零。
func TestSystemoneUsageIsBilledFromDecisionResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 形态照抄真实决策上游：usage 里是 input_tokens / output_tokens，
		// 还带一个 cost 字段（oct 不采信上游自报的钱，自己按价目表算）。
		_, _ = w.Write([]byte(`{
			"id":"decision_billing","model":"upstream-billed",
			"answers":{"urgent":{"type":"noul","noul":0.97}},
			"usage":{"input_tokens":281,"output_tokens":20,"cost":5.901e-06},
			"provider":"typesafe"
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "systemone-billing",
		Type:     outbound.OutboundTypeSystemOne,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "xpl-billing-key"}},
		Model:    "upstream-billed",
		Priority: 1,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// 🔴 必须显式配价。
	//
	// 调用顺序（别弄反，容易说错）：SetUsage 先照抄 token —— InputToken /
	// OutputToken 在 raw_protocol.go 的 SetUsage 开头就写进 Stats 了 —— 然后才
	// 调 buildBillingSnapshot 算钱。而 buildBillingSnapshot 里
	// `if modelPrice == nil { return snapshot }` 在**算钱之前**就返回。
	// 所以缺价的后果是：**token 记对了，金额是 0**，不是两者一起清零。
	// 请求不因此失败；缺价且 usage>0 时另有一条按模型去重的警告
	// （TestSystemoneUnpricedModelWarnsOnceAndStillSucceeds）。
	// 只断言 token 的测试对"钱算没算"完全无感，绿得毫无意义。
	// 本用例显式配了价，所以金额必须非 0。
	//
	// 配一个能整除的价：input 每百万 3.0、output 每百万 15.0，
	// 281 输入 → 281*3*1e-6，20 输出 → 20*15*1e-6。
	const (
		inputPricePerM  = 3.0
		outputPricePerM = 15.0
		wantInputCost   = 281 * inputPricePerM * 1e-6
		wantOutputCost  = 20 * outputPricePerM * 1e-6
	)
	price.SetLLMPrice("upstream-billed", dbmodel.LLMPrice{
		Input:  inputPricePerM,
		Output: outputPricePerM,
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{
		"model":"upstream-billed",
		"state":"billing probe",
		"questions":{"urgent":{"type":"noul","instructions":"urgent?"}}
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/systemone", Name: "systemone"}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected success, got %d body %s", rec.Code, rec.Body.String())
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected a relay log row for the decision call")
	}
	log := logs[0]
	if log.InputTokens != 281 || log.OutputTokens != 20 {
		t.Fatalf("decision usage was not captured: input=%d output=%d, want 281/20 "+
			"(if this is 0/0 the response usage keys no longer match usageScanner and billing silently drops to zero)",
			log.InputTokens, log.OutputTokens)
	}
	if log.RequestEndpoint != "systemone" || log.RequestPath != "/v1/systemone" {
		t.Fatalf("unexpected log endpoint: endpoint=%q path=%q", log.RequestEndpoint, log.RequestPath)
	}

	// 钱必须真的算出来。这一条是 pi sol 审查指出的缺口：只断言 token 不够，
	// 因为「价格没配」同样能让整个计费静默归零。
	if log.BillingModel != "upstream-billed" {
		t.Fatalf("billing model = %q, want the request model %q", log.BillingModel, "upstream-billed")
	}
	if math.Abs(log.FinalInputCost-wantInputCost) > 1e-12 {
		t.Fatalf("input cost = %v, want %v (input_tokens from the decision response must be charged "+
			"at the configured price; 0 means either the usage keys or the price lookup broke)",
			log.FinalInputCost, wantInputCost)
	}
	if math.Abs(log.FinalOutputCost-wantOutputCost) > 1e-12 {
		t.Fatalf("output cost = %v, want %v", log.FinalOutputCost, wantOutputCost)
	}
}

// TestSystemoneUnpricedModelWarnsOnceAndStillSucceeds 钉住缺价不失败：
// 请求成功、token 照记、金额是 0，并且同一模型在本进程里只警告一次。
func TestSystemoneUnpricedModelWarnsOnceAndStillSucceeds(t *testing.T) {
	const modelName = "octopus-systemone-unpriced-fixture"

	var logged atomic.Int32
	previous := missingRawProtocolPriceLogf
	missingRawProtocolPriceLogf = func(string, ...any) { logged.Add(1) }
	t.Cleanup(func() { missingRawProtocolPriceLogf = previous })

	metrics := &rawProtocolRelayMetrics{Protocol: "systemone", RequestModel: modelName}
	usage := &imagesUsage{InputTokens: 11, OutputTokens: 4}
	first := metrics.buildBillingSnapshot(modelName, usage)
	if first.FinalInputCost != 0 || first.FinalOutputCost != 0 {
		t.Fatalf("missing price must leave cost at 0, got input %v output %v", first.FinalInputCost, first.FinalOutputCost)
	}
	if logged.Load() != 1 || rawProtocolUnpricedWarningCount(modelName) != 1 {
		t.Fatalf("first positive usage must warn once, logs=%d count=%d", logged.Load(), rawProtocolUnpricedWarningCount(modelName))
	}
	_ = metrics.buildBillingSnapshot(modelName, usage)
	if logged.Load() != 1 {
		t.Fatalf("second call for the same model must not warn again, logs=%d", logged.Load())
	}
	_ = metrics.buildBillingSnapshot("octopus-systemone-unpriced-zero-usage", &imagesUsage{})
	if logged.Load() != 1 {
		t.Fatalf("zero usage must not warn, logs=%d", logged.Load())
	}

	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"decision_unpriced","model":"` + modelName + `",
			"answers":{"urgent":{"type":"noul","noul":0.5}},
			"usage":{"input_tokens":9,"output_tokens":3},
			"provider":"typesafe"
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "systemone-unpriced",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "xpl-unpriced"}},
		Model:    modelName,
		Priority: 1,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{
			"model":"`+modelName+`",
			"state":"unpriced",
			"questions":{"urgent":{"type":"noul","instructions":"urgent?"}}
		}`))
		req.Header.Set("Content-Type", "application/json")
		c.Request = req
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")
		RawProtocolHandler(RawProtocolOptions{Endpoint: "/systemone", Name: "systemone"}, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("unpriced systemone request %d must succeed, got %d body %s", i, rec.Code, rec.Body.String())
		}
	}
	if logged.Load() != 1 {
		t.Fatalf("live requests must not emit a second warning, logs=%d", logged.Load())
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected a relay log for the unpriced decision call")
	}
	latest := logs[0]
	if latest.InputTokens != 9 || latest.OutputTokens != 3 {
		t.Fatalf("unpriced usage = %d/%d, want 9/3", latest.InputTokens, latest.OutputTokens)
	}
	if latest.FinalInputCost != 0 || latest.FinalOutputCost != 0 {
		t.Fatalf("unpriced cost = %v/%v, want 0/0", latest.FinalInputCost, latest.FinalOutputCost)
	}
}
