package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// 嵌入请求（非 CLI 流量）在 relay 主链路上必须与 chat 一样留下审计行。
//
// 这组用例钉的是 GAP A：OCE 这类客户端打 /v1/embeddings 时，请求走的是**共享的主
// relay.Handler**（不是 raw protocol），成功分支在 relay.go 的 metrics.Save 里落库。
// 之前现场"实测无统计"的根因不在这条成功路径（成功路径本来就落库），而在于：
//   - 请求在选路/熔断阶段就被短路（0 上游流量 → 0 token），且
//   - 两个"还没进选路"的窗口（请求体读不出、模型不在白名单）只写 HTTP 响应、一个字节
//     都不落库 —— 本文件 A2 用例就是为这两个窗口补的回归网。
//
// 另一个必须钉住的约束：新增的审计行不得改变客户端可见响应字节（见
// TestPreRoutingRelayLogWritesNoClientBytes）。
func TestEmbeddingRequestWritesRelayLogWithUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("unexpected upstream content type: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"emb-test",
			"object":"list",
			"created":123,
			"model":"test-embed-model",
			"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],
			"usage":{"prompt_tokens":7,"total_tokens":7}
		}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "embedding-channel",
		Type:     outbound.OutboundTypeOpenAIEmbedding,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "embed-key"}},
		Model:    "test-embed-model",
		Priority: 1,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"test-embed-model","input":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIEmbedding, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected embedding request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"embedding":[0.1,0.2,0.3]`) {
		t.Fatalf("client-visible body must carry the upstream vectors unchanged, got %s", rec.Body.String())
	}
	if upstreamHits != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", upstreamHits)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 50, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	// The package shares one DB across tests (db.InitDB is a no-op once open),
	// so count only this test's rows instead of asserting on the whole table.
	mine := make([]dbmodel.RelayLog, 0, 1)
	for _, l := range logs {
		if l.RequestModelName == "test-embed-model" {
			mine = append(mine, l)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("expected exactly 1 relay log row for one embedding request, got %d (%#v)", len(mine), mine)
	}
	log := mine[0]
	if log.RequestEndpoint != "embeddings" {
		t.Fatalf("request_endpoint = %q, want \"embeddings\"", log.RequestEndpoint)
	}
	if log.RequestModelName != "test-embed-model" {
		t.Fatalf("request_model_name = %q, want \"test-embed-model\"", log.RequestModelName)
	}
	if log.InputTokens != 7 {
		t.Fatalf("input_tokens = %d, want 7 (upstream usage)", log.InputTokens)
	}
	if log.UsageSource != dbmodel.RelayLogUsageSourceUpstream {
		t.Fatalf("usage_source = %q, want %q", log.UsageSource, dbmodel.RelayLogUsageSourceUpstream)
	}
	if log.ErrorCode != "" || log.ErrorStatus != 0 {
		t.Fatalf("successful embedding row must carry no error, got code=%q status=%d", log.ErrorCode, log.ErrorStatus)
	}
	if log.IsStream {
		t.Fatalf("embedding responses are never streamed, is_stream must be false")
	}
}

// A2（绿后）：模型不在 API Key 的 supported_models 白名单里时，过去只写 400、不落日志
// —— 管理员在日志页看不到任何痕迹（这正是现场"实测无统计"的一部分）。现在必须留下
// 恰好一行审计，且带上可读的 reason。
//
// 红前证据（本用例在改动前的行为，无法在本进程里复跑，故记录在案）：同一请求在
// relay.go 的 isSupportedRequestModel 分支 `writeRelayErrorPreStream(...); return`
// 直接返回，op.RelayLogList 返回 0 行。
func TestPreRoutingModelNotSupportedWritesRelayLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"test-embed-unsupported","input":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	// 白名单只放行别的模型 → 这个请求必须被本地拒绝，且不发任何上游。
	c.Set("supported_models", "some-other-model")

	Handler(inbound.InboundTypeOpenAIEmbedding, c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a model outside the key allow-list, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_supported") {
		t.Fatalf("client-visible body must keep the model_not_supported error, got %s", rec.Body.String())
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 50, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	// Shared package DB: count only this test's row (unique model name).
	mine := make([]dbmodel.RelayLog, 0, 1)
	for _, l := range logs {
		if l.RequestModelName == "test-embed-unsupported" {
			mine = append(mine, l)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("expected exactly 1 pre-routing audit row, got %d (%#v)", len(mine), mine)
	}
	log := mine[0]
	if log.ErrorCode != dbmodel.RelayLogErrorCodeModelNotSupported {
		t.Fatalf("error_code = %q, want %q", log.ErrorCode, dbmodel.RelayLogErrorCodeModelNotSupported)
	}
	if log.RequestEndpoint != "embeddings" {
		t.Fatalf("request_endpoint = %q, want \"embeddings\"", log.RequestEndpoint)
	}
	if log.RequestModelName != "test-embed-unsupported" {
		t.Fatalf("request_model_name = %q, want \"test-embed-unsupported\"", log.RequestModelName)
	}
	if log.ErrorStatus != http.StatusBadRequest {
		t.Fatalf("error_status = %d, want 400", log.ErrorStatus)
	}
	if !strings.Contains(log.ErrorStrategy, "reason="+preRoutingModelNotSupported) {
		t.Fatalf("error_strategy = %q, want it to carry reason=%s", log.ErrorStrategy, preRoutingModelNotSupported)
	}
	// 这些行没有触达上游，必须落在 local_validation 家族里——op.relayLogExcludedFromModelTelemetry
	// 正是按这个前缀把它们挡在模型健康/排名统计之外（relay_log_filter.go 的语义一个字没改）。
	if !strings.HasPrefix(log.ErrorStrategy, dbmodel.RelayLogErrorStrategyLocalValidationPart) {
		t.Fatalf("error_strategy = %q, want the %q family prefix so model telemetry keeps excluding it", log.ErrorStrategy, dbmodel.RelayLogErrorStrategyLocalValidationPart)
	}
	if log.UsageSource != dbmodel.RelayLogUsageSourceNoUsage {
		t.Fatalf("usage_source = %q, want %q", log.UsageSource, dbmodel.RelayLogUsageSourceNoUsage)
	}
	if log.TotalAttempts != 0 {
		t.Fatalf("total_attempts = %d, want 0 (nothing was forwarded)", log.TotalAttempts)
	}
}

// A2 的另一个窗口：请求体根本读不出来/解析不了（这里用坏 JSON 触发）。过去同样是
// 裸 500 + 零日志。
func TestUnparsableRequestBodyWritesRelayLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIEmbedding, c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for an unparsable body (parseRequest writes it), got %d body %s", rec.Code, rec.Body.String())
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 50, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	// Shared package DB; an unparsable body never yields a model name, so filter
	// by this window's unique error code instead.
	mine := make([]dbmodel.RelayLog, 0, 1)
	for _, l := range logs {
		if l.ErrorCode == dbmodel.RelayLogErrorCodeRequestUnparsable {
			mine = append(mine, l)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("expected exactly 1 pre-routing audit row, got %d (%#v)", len(mine), mine)
	}
	log := mine[0]
	if log.ErrorCode != dbmodel.RelayLogErrorCodeRequestUnparsable {
		t.Fatalf("error_code = %q, want %q", log.ErrorCode, dbmodel.RelayLogErrorCodeRequestUnparsable)
	}
	if log.ErrorStatus != http.StatusInternalServerError {
		t.Fatalf("error_status = %d, want 500", log.ErrorStatus)
	}
	if log.RequestEndpoint != "embeddings" {
		t.Fatalf("request_endpoint = %q, want \"embeddings\"", log.RequestEndpoint)
	}
	// 连模型名都拿不到：req 为 nil 也绝不能 panic，字段留空即可。
	if log.RequestModelName != "" {
		t.Fatalf("request_model_name = %q, want empty (the body never parsed)", log.RequestModelName)
	}
	if !strings.Contains(log.ErrorStrategy, "reason="+preRoutingRequestUnparsable) {
		t.Fatalf("error_strategy = %q, want it to carry reason=%s", log.ErrorStrategy, preRoutingRequestUnparsable)
	}
}

// 新增的落库函数必须零字节落向客户端：直接证据是它自己一行都不写 c.Writer。
// 这是"A.1/A.2 客户端可见响应字节一字不动"的可复现证明（补上审计行只是在
// 请求处理里多了一次 op.RelayLogAdd，响应仍由原来的 writeRelayErrorPreStream 出）。
func TestPreRoutingRelayLogWritesNoClientBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"x","input":"y"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	before := rec.Body.String()
	savePreRoutingFailureRelayLog(ctx, c, inbound.InboundTypeOpenAIEmbedding, nil, preRoutingModelNotSupported, dbmodel.RelayLogErrorCodeModelNotSupported, http.StatusBadRequest, "model not supported")
	if got := rec.Body.String(); got != before {
		t.Fatalf("pre-routing audit row changed client-visible body: before=%q after=%q", before, got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("recorder status changed from its default: %d", rec.Code)
	}

	// req 为 nil 也不能炸（解析失败窗口就是走这条）。
	savePreRoutingFailureRelayLog(ctx, c, inbound.InboundTypeOpenAIEmbedding, nil, preRoutingRequestUnparsable, dbmodel.RelayLogErrorCodeRequestUnparsable, http.StatusInternalServerError, "request body rejected before routing")
	// c 为 nil 直接返回，不 panic。
	savePreRoutingFailureRelayLog(ctx, nil, inbound.InboundTypeOpenAIEmbedding, nil, preRoutingRequestUnparsable, dbmodel.RelayLogErrorCodeRequestUnparsable, http.StatusInternalServerError, "request body rejected before routing")
}
