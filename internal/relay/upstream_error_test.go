package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestHandlerPassesThroughUpstreamStatusWithSanitizedError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorStatusPass, "true"); err != nil {
		t.Fatalf("set status passthrough: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorPublicCode, ""); err != nil {
		t.Fatalf("clear public code: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"secret token sk-live-sensitive leaked by provider"}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "sanitized-error-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "upstream-model",
		ModelMapping: map[string]string{
			"request-model": "upstream-model",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "bad-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"request-model",
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected upstream status 401, got %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error_code":"octopus_upstream_auth_failed"`) {
		t.Fatalf("expected standard error code in body, got %s", body)
	}
	if strings.Contains(body, "sk-live-sensitive") || strings.Contains(body, "leaked by provider") {
		t.Fatalf("response leaked upstream body: %s", body)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].ErrorStatus != http.StatusUnauthorized {
		t.Fatalf("expected relay log error status 401, got %d", logs[0].ErrorStatus)
	}
	if logs[0].ErrorCode != "octopus_upstream_auth_failed" {
		t.Fatalf("expected relay log standard error code, got %q", logs[0].ErrorCode)
	}
	if !strings.Contains(logs[0].ErrorStrategy, "preserve_status_sanitize_body_provider_code_observed") ||
		!strings.Contains(logs[0].ErrorStrategy, "body_mode=redacted_upstream") {
		t.Fatalf("expected relay log strategy, got %q", logs[0].ErrorStrategy)
	}
	if strings.Contains(logs[0].Error, "sk-live-sensitive") || strings.Contains(logs[0].Error, "leaked by provider") {
		t.Fatalf("relay log error leaked upstream body: %s", logs[0].Error)
	}
}

func TestHandlerUsesCustomUpstreamErrorMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorStatusPass, "false"); err != nil {
		t.Fatalf("set status passthrough: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "custom_message"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorCustom, "Provider busy, retry later."); err != nil {
		t.Fatalf("set custom message: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorPublicCode, "service_busy"); err != nil {
		t.Fatalf("set public code: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"secret token sk-live-sensitive leaked by provider"}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:    "custom-error-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "upstream-model",
		ModelMapping: map[string]string{
			"custom-error-model": "upstream-model",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "bad-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"custom-error-model",
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status passthrough disabled to return 502, got %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error_code":"service_busy"`) ||
		!strings.Contains(body, "Provider busy, retry later.") {
		t.Fatalf("expected custom sanitized error body, got %s", body)
	}
	if strings.Contains(body, "sk-live-sensitive") || strings.Contains(body, "leaked by provider") {
		t.Fatalf("response leaked upstream body: %s", body)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].ErrorStatus != http.StatusInternalServerError || logs[0].ErrorCode != "octopus_upstream_unavailable" {
		t.Fatalf("expected admin log to keep upstream audit details, got status=%d code=%q", logs[0].ErrorStatus, logs[0].ErrorCode)
	}
}

func TestRelayErrorResponseHidesRawStatusWhenPassthroughDisabled(t *testing.T) {
	setupRelayErrorDB(t)

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorStatusPass, "false"); err != nil {
		t.Fatalf("set status passthrough: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "redacted_upstream"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorPublicCode, "service_busy"); err != nil {
		t.Fatalf("set public code: %v", err)
	}

	// Use a non-429 upstream status: a 429 is always passed through (see
	// TestRelayErrorResponseAlwaysPassesThrough429); every other status is still hidden
	// behind a friendly 502 when passthrough is disabled.
	status, code, message := relayErrorResponse(newUpstreamError(http.StatusInternalServerError, []byte(`{"error":{"code":"500","message":"cpu overloaded"}}`)))
	if status != http.StatusBadGateway || code != "service_busy" {
		t.Fatalf("expected friendly public status/code, got status=%d code=%q message=%q", status, code, message)
	}
	if strings.Contains(message, "500") || strings.Contains(message, "cpu overloaded") {
		t.Fatalf("expected message to hide raw upstream details, got %q", message)
	}
}

// A 429 rate-limit must always reach the client as a 429 — even with status
// passthrough disabled — so the caller (claude-code / codex) backs off and retries
// exactly as it would against the upstream directly, instead of treating a masked 502
// as a hard failure and giving up mid-session. The body/code stay redacted.
func TestRelayErrorResponseAlwaysPassesThrough429(t *testing.T) {
	setupRelayErrorDB(t)

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorStatusPass, "false"); err != nil {
		t.Fatalf("set status passthrough: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "redacted_upstream"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorPublicCode, "service_busy"); err != nil {
		t.Fatalf("set public code: %v", err)
	}

	status, code, message := relayErrorResponse(newUpstreamError(http.StatusTooManyRequests, []byte(`{"error":{"code":"429","message":"cpu overloaded"}}`)))
	if status != http.StatusTooManyRequests {
		t.Fatalf("a 429 must pass through as 429 even with passthrough disabled, got status=%d", status)
	}
	if code != "service_busy" {
		t.Fatalf("429 public code should still be the redacted admin code, got %q", code)
	}
	if strings.Contains(message, "cpu overloaded") {
		t.Fatalf("429 upstream body detail must stay redacted, got %q", message)
	}
}

func TestRelayErrorDetailsClassifiesLocalRouteSelection(t *testing.T) {
	err := routeSelectionErrorFromAttempts([]dbmodel.ChannelAttempt{{
		ChannelID:   4,
		ChannelName: "Claude-CPA",
		Status:      dbmodel.AttemptCircuitBreak,
		Msg:         "circuit breaker tripped, remaining cooldown: 30s",
	}})

	status, code, strategy, ok := relayErrorDetails(err)
	if !ok || status != http.StatusServiceUnavailable || code != "octopus_channel_circuit_open" {
		t.Fatalf("unexpected local route details: ok=%t status=%d code=%q", ok, status, code)
	}
	if !strings.Contains(strategy, "local_route_selection") || !strings.Contains(strategy, "circuit_break") {
		t.Fatalf("unexpected local route strategy: %q", strategy)
	}

	respStatus, respCode, message := relayErrorResponse(err)
	if respStatus != http.StatusServiceUnavailable || respCode != "octopus_channel_circuit_open" {
		t.Fatalf("unexpected local route response: status=%d code=%q message=%q", respStatus, respCode, message)
	}
	// The user-facing message must NOT leak the channel name or circuit/cooldown state;
	// that internal detail stays in the audit log (strategy/attempts), not the response.
	if strings.Contains(message, "Claude-CPA") || strings.Contains(strings.ToLower(message), "circuit") || strings.Contains(strings.ToLower(message), "cooldown") {
		t.Fatalf("local route response must not leak channel/circuit detail, got %q", message)
	}
}

func TestRelayErrorDetailsClassifiesClientAbort(t *testing.T) {
	err := fmt.Errorf("channel Claude-CPA failed: %w", context.Canceled)
	status, code, strategy, ok := relayErrorDetails(err)
	if !ok || status != statusClientClosedRequest || code != "octopus_client_canceled" {
		t.Fatalf("unexpected client abort details: ok=%t status=%d code=%q", ok, status, code)
	}
	if !strings.Contains(strategy, "breaker_counted=false") {
		t.Fatalf("expected breaker_counted=false strategy, got %q", strategy)
	}
	if shouldRecordBreakerFailure(0, err) {
		t.Fatalf("client abort must not count against circuit breaker")
	}
}

func TestRelayErrorResponseClassifiesEmptyUpstreamStream(t *testing.T) {
	// Every channel opened an SSE stream then ended it without content. The client
	// must see a distinct, actionable code instead of the opaque generic all-failed,
	// and the message must explain the likely cause (oversized/overloaded) so the
	// caller shortens the request rather than blindly retrying the same payload.
	cases := []error{
		errors.New("upstream stream ended without internal response"),
		fmt.Errorf("channel maomao failed: %w", errors.New("upstream stream ended without internal response")),
	}
	for _, err := range cases {
		status, code, message := relayErrorResponse(err)
		if status != http.StatusBadGateway {
			t.Fatalf("empty-stream status = %d, want 502 (err=%v)", status, err)
		}
		if code != "octopus_upstream_empty_response" {
			t.Fatalf("empty-stream code = %q, want octopus_upstream_empty_response (err=%v)", code, err)
		}
		if code == "octopus_all_channels_failed" || strings.Contains(message, "service temporarily unavailable") {
			t.Fatalf("empty-stream response must not fall back to the opaque generic (err=%v): code=%q msg=%q", err, code, message)
		}
		if !strings.Contains(message, "empty response") {
			t.Fatalf("empty-stream message should explain the cause, got %q", message)
		}
	}
}

func TestHandlerReturnsToOriginalGroupAfterRouteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	routeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"route failed"}}`))
	}))
	t.Cleanup(routeUpstream.Close)

	fallbackUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-fallback",
			"object":"chat.completion",
			"created":123,
			"model":"original-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"fallback ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	t.Cleanup(fallbackUpstream.Close)

	routeChannel := dbmodel.Channel{
		Name:    "route-fail-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "route-upstream",
		BaseUrls: []dbmodel.BaseUrl{{
			URL: routeUpstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "route-key"}},
	}
	if err := op.ChannelCreate(&routeChannel, ctx); err != nil {
		t.Fatalf("create route channel: %v", err)
	}
	fallbackChannel := dbmodel.Channel{
		Name:    "original-group-channel",
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		Model:   "original-upstream",
		ModelMapping: map[string]string{
			"return-group-model": "original-upstream",
		},
		Priority: 1,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: fallbackUpstream.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "fallback-key"}},
	}
	if err := op.ChannelCreate(&fallbackChannel, ctx); err != nil {
		t.Fatalf("create fallback channel: %v", err)
	}

	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list access plans: %v", err)
	}
	var vip dbmodel.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "vip" {
			vip = plan
			break
		}
	}
	if vip.ID == 0 {
		t.Fatalf("vip plan not found")
	}
	rule := dbmodel.AccessRouteRule{
		RouteProfileID: vip.RouteProfileID,
		RequestModel:   "return-group-model",
		FallbackMode:   dbmodel.AccessRouteFallbackReturnGroup,
	}
	if err := op.AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}
	if err := op.AccessRouteTargetCreate(&dbmodel.AccessRouteTarget{
		RouteRuleID:   rule.ID,
		ChannelID:     routeChannel.ID,
		UpstreamModel: "route-upstream",
		Priority:      1,
		Weight:        1,
		Enabled:       true,
	}, ctx); err != nil {
		t.Fatalf("create route target: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"return-group-model",
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected original group fallback to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fallback ok") {
		t.Fatalf("expected fallback response, got %s", rec.Body.String())
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].ActualModelName != "original-upstream" {
		t.Fatalf("expected original group model in log, got %q", logs[0].ActualModelName)
	}
	if logs[0].TotalAttempts != 2 {
		t.Fatalf("expected route + original group attempts, got %d", logs[0].TotalAttempts)
	}
}

func TestRelayErrorResponseRedactsLocalRouteDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)

	// A local route-selection error whose underlying message carries a channel name +
	// circuit/cooldown detail (the audit-log form). The user-facing response must not.
	leaky := &localRelayError{
		status:   http.StatusServiceUnavailable,
		code:     "octopus_channel_circuit_open",
		strategy: "local_route_selection;reason=circuit_break;upstream_forwarded=false",
		message:  "no available channel: tmp-relay-claude-no1m (circuit breaker tripped, remaining cooldown: 30s)",
	}

	status, code, message := relayErrorResponse(leaky)
	if status != http.StatusServiceUnavailable || code != "octopus_channel_circuit_open" {
		t.Fatalf("status/code must be preserved: %d %q", status, code)
	}
	for _, leak := range []string{"tmp-relay", "circuit", "cooldown", "no available channel"} {
		if strings.Contains(strings.ToLower(message), strings.ToLower(leak)) {
			t.Fatalf("user-facing message leaked internal route detail %q: %q", leak, message)
		}
	}

	// A configured custom message unifies upstream + local error surfaces to one line.
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "custom_message"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorCustom, "服务繁忙，请稍后再试"); err != nil {
		t.Fatalf("set custom message: %v", err)
	}
	if _, _, message = relayErrorResponse(leaky); message != "服务繁忙，请稍后再试" {
		t.Fatalf("custom message must apply to local errors too, got %q", message)
	}
}

// The admin audit message must surface the REAL upstream reason (so an admin can
// see "only Claude Code clients" / "model overloaded" instead of an opaque
// service_busy), across the upstream error-body shapes octopus sees in the wild.
func TestUpstreamErrorDetailedMessageSurfacesRealReason(t *testing.T) {
	upErr := newUpstreamError(http.StatusServiceUnavailable, []byte(`{"error":{"message":"No available accounts: this group only allows Claude Code clients","type":"api_error"}}`))
	detailed := upErr.DetailedMessage()
	if !strings.Contains(detailed, "only allows Claude Code clients") {
		t.Fatalf("admin detail must surface upstream reason, got %q", detailed)
	}
	if !strings.Contains(detailed, "octopus_upstream_unavailable") {
		t.Fatalf("admin detail must keep the octopus wrapper, got %q", detailed)
	}

	cases := map[string]string{
		`{"error":{"message":"当前模型 gpt-5.5 负载已经达到上限，请稍后重试","code":"get_channel_failed"}}`: "负载已经达到上限",
		`{"error":"当前 API 不支持所选模型 gpt-5.5","type":"error"}`:                               "不支持所选模型",
		`{"message":"Service Unavailable"}`: "Service Unavailable",
	}
	for body, want := range cases {
		if got := upstreamBodySummary(body); !strings.Contains(got, want) {
			t.Fatalf("body %q: expected reason %q, got %q", body, want, got)
		}
	}
	if got := upstreamBodySummary("  plain text boom \n"); got != "plain text boom" {
		t.Fatalf("non-JSON body should fall back to trimmed raw, got %q", got)
	}
}

// Even in the admin audit, upstream-echoed credentials must be redacted.
func TestUpstreamErrorDetailRedactsSecrets(t *testing.T) {
	upErr := newUpstreamError(http.StatusUnauthorized, []byte(`{"error":{"message":"bad key sk-live-sensitive-1234 rejected"}}`))
	detailed := upErr.DetailedMessage()
	if strings.Contains(detailed, "sk-live-sensitive") {
		t.Fatalf("admin detail must redact secrets, got %q", detailed)
	}
	if !strings.Contains(detailed, "[redacted]") {
		t.Fatalf("expected redaction marker, got %q", detailed)
	}
}

// The admin audit carries the real reason; the API-client response must not.
func TestAuditErrorMessageDoesNotChangePublicResponse(t *testing.T) {
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "redacted_upstream"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorPublicCode, "service_busy"); err != nil {
		t.Fatalf("set public code: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorStatusPass, "false"); err != nil {
		t.Fatalf("set passthrough: %v", err)
	}
	err := fmt.Errorf("forward: %w", newUpstreamError(http.StatusServiceUnavailable,
		[]byte(`{"error":{"message":"No available accounts: this group only allows Claude Code clients"}}`)))

	if audit := auditErrorMessage(err); !strings.Contains(audit, "only allows Claude Code clients") {
		t.Fatalf("audit (admin) must carry the upstream reason, got %q", audit)
	}
	_, code, message := relayErrorResponse(err)
	if code != "service_busy" {
		t.Fatalf("expected public code service_busy, got %q", code)
	}
	if strings.Contains(message, "Claude Code clients") {
		t.Fatalf("public message must hide the upstream reason, got %q", message)
	}
}

func setupRelayErrorDB(t *testing.T) context.Context {
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

// A route left on any fallback mode except an explicit "none" must spill to the model
// pool after its targeted candidates fail at runtime — including the gorm default
// "failover" (AccessRouteFallbackGroup), whose name means "fall back to the group".
// Only "none" dead-ends. Guards the fix that stopped a default-mode route from
// dead-ending on its primary while a healthy redundant channel sat in the pool.
func TestShouldReturnToOriginalGroupSpillsUnlessNone(t *testing.T) {
	cases := []struct {
		mode dbmodel.AccessRouteFallbackMode
		want bool
	}{
		{dbmodel.AccessRouteFallbackReturnGroup, true},
		{dbmodel.AccessRouteFallbackGroup, true}, // the gorm default value ("failover")
		{"", true},                               // unset behaves like the default
		{dbmodel.AccessRouteFallbackNone, false}, // only "none" opts out of the pool fallback
	}
	for _, tc := range cases {
		rr := routeGroupResult{
			AccessRouteUsed: true,
			AccessRouteRule: &dbmodel.AccessRouteRule{FallbackMode: tc.mode},
		}
		if got := shouldReturnToOriginalGroup(rr, false); got != tc.want {
			t.Fatalf("fallback mode %q: want %v, got %v", tc.mode, tc.want, got)
		}
	}

	// alreadyTried must prevent a second pool fallback regardless of mode.
	tried := routeGroupResult{
		AccessRouteUsed: true,
		AccessRouteRule: &dbmodel.AccessRouteRule{FallbackMode: dbmodel.AccessRouteFallbackReturnGroup},
	}
	if shouldReturnToOriginalGroup(tried, true) {
		t.Fatalf("alreadyTried must prevent a second fallback")
	}

	// A request that never used a route has nothing to spill from.
	if shouldReturnToOriginalGroup(routeGroupResult{AccessRouteUsed: false}, false) {
		t.Fatalf("a request that did not use a route must not trigger group fallback")
	}
}
