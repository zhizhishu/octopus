package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestShouldForwardClientHeaderFiltersTimeoutHeaders(t *testing.T) {
	blocked := []string{
		"X-Stainless-Timeout",
		"x-stainless-read-timeout",
		"X-Stainless-Connect-Timeout",
		"X-Stainless-Lang",
		"X-Request-Timeout",
		"Request-Timeout",
		"Grpc-Timeout",
		"User-Agent",
		"AH-Thread-Id",
		"AH-Trace-Id",
		"Session_id",
		"Conversation_id",
	}
	for _, key := range blocked {
		if shouldForwardClientHeader(key) {
			t.Fatalf("expected %s to be filtered", key)
		}
	}

	if shouldForwardClientHeader("Authorization") {
		t.Fatalf("expected authorization to remain filtered")
	}
}

func TestRelayCopyHeadersAppliesClaudeDefaultsAndAllowsCustomOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "filtered-client")
	c.Request.Header.Set("X-Stainless-Timeout", "30")

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{Model: "claude-opus-4-8[1m]"},
		},
		channel: &dbmodel.Channel{
			Type: outbound.OutboundTypeAnthropic,
			CustomHeader: []dbmodel.CustomHeader{{
				HeaderKey:   "X-Stainless-Timeout",
				HeaderValue: "900",
			}},
		},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("User-Agent"); got != defaultClaudeUserAgent {
		t.Fatalf("claude default user-agent = %q, want %q", got, defaultClaudeUserAgent)
	}
	if got := upstreamReq.Header.Get("X-Stainless-Package-Version"); got != defaultClaudePackageVersion {
		t.Fatalf("claude default package version = %q, want %q", got, defaultClaudePackageVersion)
	}
	if got := upstreamReq.Header.Get("X-Stainless-Os"); got != defaultClaudeOS {
		t.Fatalf("claude default os = %q, want %q", got, defaultClaudeOS)
	}
	if got := upstreamReq.Header.Get("X-Stainless-Timeout"); got != "900" {
		t.Fatalf("custom timeout should override default, got %q", got)
	}
	if got := upstreamReq.Header.Get("Anthropic-Beta"); !strings.Contains(got, defaultClaudeOneMillionBeta) || !strings.Contains(got, "claude-code-20250219") {
		t.Fatalf("claude [1m] beta header = %q, want 1m + Claude Code betas", got)
	}
}

func TestRelayCopyHeadersAppliesOneMillionBetaForOpusShortcutAfterAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:           c,
			inboundType: inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{
				Model: "claude-opus-4-8",
				TransformOptions: transformermodel.TransformOptions{
					AnthropicOneMillionBeta: true,
				},
			},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("Anthropic-Beta"); !strings.Contains(got, defaultClaudeOneMillionBeta) || !strings.Contains(got, "claude-code-20250219") {
		t.Fatalf("opus[1m] compatibility beta header = %q, want 1m + Claude Code betas", got)
	}
}

func TestRelayCopyHeadersDoesNotApplyOneMillionBetaToRegularClaude(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{Model: "claude-sonnet-4-5"},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("Anthropic-Beta"); strings.Contains(got, defaultClaudeOneMillionBeta) {
		t.Fatalf("regular claude should not get 1m beta header, got %q", got)
	}
}

func TestRouteModelCandidatesIncludeAnthropicAlias(t *testing.T) {
	got := routeModelCandidates("opus[1m]", "claude-opus-4-8")
	if len(got) != 2 || got[0] != "opus[1m]" || got[1] != "claude-opus-4-8" {
		t.Fatalf("unexpected route candidates: %#v", got)
	}
	if !isSupportedRequestModel("claude-opus-4-8", "opus[1m]", []string{"claude-opus-4-8"}) {
		t.Fatalf("expected supported model check to accept the normalized Anthropic alias")
	}
}

func TestRelayCopyHeadersAppliesCodexDefaultsForResponsesChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "filtered-codex")

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, inboundType: inbound.InboundTypeOpenAIResponse},
		channel:      &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("User-Agent"); got != defaultCodexUserAgent {
		t.Fatalf("codex default user-agent = %q, want %q", got, defaultCodexUserAgent)
	}
	if got := upstreamReq.Header.Get("X-Codex-Beta-Features"); got != defaultCodexBetaFeatures {
		t.Fatalf("codex beta features = %q, want %q", got, defaultCodexBetaFeatures)
	}
}

func TestRelayCopyHeadersAppliesCodexSessionFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Accept", "application/json")
	sessionID := "019e8d7b-0690-7a91-a60f-b642269c3439"

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	upstreamReq.Header.Set("Accept", "text/event-stream")
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: &transformermodel.InternalLLMRequest{PromptCacheKey: &sessionID},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("expected upstream SSE accept to survive client accept copy, got %q", got)
	}
	if got := upstreamReq.Header.Get("Session_id"); got != sessionID {
		t.Fatalf("session header = %q, want %q", got, sessionID)
	}
	if got := upstreamReq.Header.Get("X-Codex-Window-Id"); got != sessionID+":0" {
		t.Fatalf("window id = %q", got)
	}
	if got := upstreamReq.Header.Get("X-Codex-Turn-Metadata"); !strings.Contains(got, `"session_id":"`+sessionID+`"`) {
		t.Fatalf("turn metadata missing session id: %s", got)
	}
}

func TestRelayCopyHeadersSkipsDefaultsWhenCloakNever(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "filtered-codex")

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, inboundType: inbound.InboundTypeOpenAIResponse},
		channel: &dbmodel.Channel{
			Type:  outbound.OutboundTypeOpenAIResponse,
			Cloak: dbmodel.ChannelCloak{Mode: "never"},
		},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("User-Agent"); got != "" {
		t.Fatalf("cloak=never user-agent = %q, want empty", got)
	}
	if got := upstreamReq.Header.Get("X-Codex-Beta-Features"); got != "" {
		t.Fatalf("cloak=never beta features = %q, want empty", got)
	}
}

func TestRelayCopyHeadersDoesNotApplyCodexDefaultsToChatChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/chat/completions", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, inboundType: inbound.InboundTypeOpenAIChat},
		channel:      &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("User-Agent"); got != "" {
		t.Fatalf("openai chat default user-agent = %q, want empty", got)
	}
	if got := upstreamReq.Header.Get("X-Codex-Beta-Features"); got != "" {
		t.Fatalf("openai chat beta features = %q, want empty", got)
	}
}

func TestRelayCopyHeadersAppliesCodexDefaultsWhenResponsesUseChatChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "filtered-codex")

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/chat/completions", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, inboundType: inbound.InboundTypeOpenAIResponse},
		channel:      &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("User-Agent"); got != defaultCodexUserAgent {
		t.Fatalf("codex default user-agent = %q, want %q", got, defaultCodexUserAgent)
	}
	if got := upstreamReq.Header.Get("X-Codex-Beta-Features"); got != defaultCodexBetaFeatures {
		t.Fatalf("codex beta features = %q, want %q", got, defaultCodexBetaFeatures)
	}
}

func TestRelayCopyHeadersFiltersClientTimeoutHeadersButAllowsAdminCustomHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("X-Stainless-Timeout", "120000")
	c.Request.Header.Set("X-Request-Timeout", "30")
	c.Request.Header.Set("Grpc-Timeout", "30S")
	c.Request.Header.Set("X-Stainless-Lang", "go")
	c.Request.Header.Set("User-Agent", "claude-cli/test")
	c.Request.Header.Set("Authorization", "Bearer client")
	c.Request.Header.Set("X-Octopus-Plan", "vip")

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/messages", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c},
		channel: &dbmodel.Channel{
			CustomHeader: []dbmodel.CustomHeader{{
				HeaderKey:   "X-Stainless-Timeout",
				HeaderValue: "180000",
			}, {
				HeaderKey:   "User-Agent",
				HeaderValue: "admin-upstream-agent",
			}},
		},
	}

	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("X-Stainless-Timeout"); got != "180000" {
		t.Fatalf("expected explicit admin custom timeout header to win, got %q", got)
	}
	if got := upstreamReq.Header.Get("X-Request-Timeout"); got != "" {
		t.Fatalf("expected client x-request-timeout to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("Grpc-Timeout"); got != "" {
		t.Fatalf("expected client grpc-timeout to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("X-Stainless-Lang"); got != "" {
		t.Fatalf("expected client x-stainless-lang to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("User-Agent"); got != "admin-upstream-agent" {
		t.Fatalf("expected explicit admin custom user-agent to win, got %q", got)
	}
	if got := upstreamReq.Header.Get("Authorization"); got != "" {
		t.Fatalf("expected client authorization to remain filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("X-Octopus-Plan"); got != "" {
		t.Fatalf("expected octopus routing header to remain filtered, got %q", got)
	}
}

func TestClaudeHeaderDefaultsUseCurrentCLIBetaShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	reqModel := "claude-opus-4-8[1m]"
	internalReq := &transformermodel.InternalLLMRequest{
		Model: reqModel,
		TransformOptions: transformermodel.TransformOptions{
			AnthropicOneMillionBeta: true,
		},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			internalRequest: internalReq,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.URL.Query().Get("beta"); got != "true" {
		t.Fatalf("expected Claude CLI beta query, got %q", got)
	}
	if got := upstreamReq.Header.Get("User-Agent"); got != defaultClaudeUserAgent {
		t.Fatalf("claude default user-agent = %q, want %q", got, defaultClaudeUserAgent)
	}
	beta := upstreamReq.Header.Get("Anthropic-Beta")
	for _, want := range []string{
		"claude-code-20250219",
		defaultClaudeOneMillionBeta,
		"context-management-2025-06-27",
		"thinking-token-count-2026-05-13",
		"effort-2025-11-24",
		"structured-outputs-2025-12-15",
	} {
		if !strings.Contains(beta, want) {
			t.Fatalf("expected beta %q in %q", want, beta)
		}
	}
}

func TestCopyHeadersToUpstreamFiltersClientTimeoutHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-Stainless-Timeout", "120000")
	c.Request.Header.Set("Request-Timeout", "30")
	c.Request.Header.Set("X-Stainless-Lang", "go")
	c.Request.Header.Set("User-Agent", "codex-test")
	c.Request.Header.Set("AH-Thread-Id", "thread-1")
	c.Request.Header.Set("Session_id", "session-1")

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	copyHeadersToUpstream(upstreamReq, c, &dbmodel.Channel{}, "upstream-key", "application/json", true)

	if got := upstreamReq.Header.Get("X-Stainless-Timeout"); got != "" {
		t.Fatalf("expected client x-stainless-timeout to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("Request-Timeout"); got != "" {
		t.Fatalf("expected client request-timeout to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("X-Stainless-Lang"); got != "" {
		t.Fatalf("expected client x-stainless-lang to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("User-Agent"); got != "" {
		t.Fatalf("expected client user-agent to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("AH-Thread-Id"); got != "" {
		t.Fatalf("expected client ah-thread-id to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("Session_id"); got != "" {
		t.Fatalf("expected client session_id to be filtered, got %q", got)
	}
	if got := upstreamReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("expected stream accept header, got %q", got)
	}
	if got := upstreamReq.Header.Get("Authorization"); got != "Bearer upstream-key" {
		t.Fatalf("expected upstream authorization to be set, got %q", got)
	}
}
