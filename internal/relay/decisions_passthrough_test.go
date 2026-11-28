package relay

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// TestRawProtocolHandlerDecisionsPassthrough 钉住 OpenAI Decisions API 透传契约
// （照 systemone_passthrough_test.go 改建）：
//   - 挂 1 号 OpenAI 类型渠道（生产渠道 68 同型），上游拼 /v1/decisions；
//   - {input, questions} 原样到上游，只有 model 允许被 model_mapping 重写；
//   - stream=true 直接 400，上游零调用（官方不支持流式）；
//   - 输出免费：usage 输出 token 在计费时清零，输入照价目表计费。
func TestRawProtocolHandlerDecisionsPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstreamModel := "gpt-6-luna"
	var gotPath, gotRaw, gotAuth string
	var gotBody map[string]any
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		gotPath = r.URL.Path
		gotRaw = string(raw)
		gotAuth = r.Header.Get("Authorization")
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"dec_1","model":"` + upstreamModel + `","answers":{"ship":{"type":"predicate","probability":0.9}},"usage":{"input_tokens":281,"output_tokens":20}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:         "decisions-openai",
		Type:         outbound.OutboundTypeOpenAIChat,
		Enabled:      true,
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "xpl-decisions-key"}},
		Model:        upstreamModel,
		ModelMapping: map[string]string{"decisions-alias": upstreamModel},
		Priority:     1,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	price.SetLLMPrice(upstreamModel, dbmodel.LLMPrice{Input: 0.10, Output: 5.0})
	price.SetLLMPrice("decisions-alias", dbmodel.LLMPrice{Input: 0.10, Output: 5.0})

	newReq := func(body string) (*httptest.ResponseRecorder, *gin.Context) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		c.Request = req
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.127.1")
		return rec, c
	}

	// 1) stream=true 必须 400，且零上行。
	rec, c := newReq(`{"model":"decisions-alias","stream":true,"input":"x","questions":{"ship":{"type":"predicate","instructions":"ship?"}}}`)
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/decisions", Name: "decisions", RejectStream: true}, c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stream=true must be 400, got %d body %s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("stream=true must not reach upstream, calls=%d", calls)
	}

	// 2) 透传：input/questions 原样，上游路径 /v1/decisions，Bearer 是渠道 key。
	rec, c = newReq(`{"model":"decisions-alias","input":"Deploy the fix now?","questions":{"ship":{"type":"predicate","instructions":"Safe to ship?"}}}`)
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/decisions", Name: "decisions", RejectStream: true}, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("decisions passthrough must succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/decisions" {
		t.Fatalf("upstream path = %q, want /v1/decisions", gotPath)
	}
	if gotAuth != "Bearer xpl-decisions-key" {
		t.Fatalf("upstream auth = %q", gotAuth)
	}
	if got := gotBody["model"]; got != upstreamModel {
		t.Fatalf("upstream model = %v, want %q (model_mapping rewrite)", got, upstreamModel)
	}
	if got := gotBody["input"]; got != "Deploy the fix now?" {
		t.Fatalf("upstream input = %v, want original untouched (raw: %s)", got, gotRaw)
	}
	if _, ok := gotBody["questions"].(map[string]any); !ok {
		t.Fatalf("upstream questions missing/wrong shape: %v", gotBody["questions"])
	}

	// 3) 计费：输入照价（281×0.10/M），输出免费——尽管价目表 output 配了 5.0/M。
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil || len(logs) == 0 {
		t.Fatalf("relay log missing: %v len=%d", err, len(logs))
	}
	lg := logs[0]
	if lg.InputTokens != 281 {
		t.Fatalf("input tokens = %d, want 281", lg.InputTokens)
	}
	if lg.OutputTokens != 0 {
		t.Fatalf("decisions output must be free: output tokens = %d, want 0 (zero-counted at billing)", lg.OutputTokens)
	}
	if math.Abs(lg.FinalInputCost-281*0.10*1e-6) > 1e-12 {
		t.Fatalf("input cost = %v, want %v", lg.FinalInputCost, 281*0.10*1e-6)
	}
	if lg.FinalOutputCost != 0 {
		t.Fatalf("output cost = %v, want 0 (decisions output is free)", lg.FinalOutputCost)
	}
	if lg.RequestEndpoint != "decisions" || lg.RequestPath != "/v1/decisions" {
		t.Fatalf("log endpoint = %q path %q", lg.RequestEndpoint, lg.RequestPath)
	}
}
