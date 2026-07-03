package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound/authropic"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TestClaudeFingerprintVersionConsistency guards the single drift the two-place
// Claude CLI version can suffer: the CLI user-agent (internal/model) and the
// Anthropic outbound billing-header cc_version (…/outbound/authropic) are declared in
// separate packages because an import cycle forbids sharing one const. This asserts
// they always name the same version, so bumping one without the other fails CI.
func TestClaudeFingerprintVersionConsistency(t *testing.T) {
	if authropic.ClaudeCLIVersion != dbmodel.DefaultClaudeCLIVersion {
		t.Fatalf("billing cc_version %q != user-agent version %q", authropic.ClaudeCLIVersion, dbmodel.DefaultClaudeCLIVersion)
	}
	if !strings.Contains(dbmodel.DefaultClaudeHeaderUserAgent, dbmodel.DefaultClaudeCLIVersion) {
		t.Fatalf("user-agent %q must contain the named version %q", dbmodel.DefaultClaudeHeaderUserAgent, dbmodel.DefaultClaudeCLIVersion)
	}
}

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
	if got := upstreamReq.Header.Get("Session-Id"); got != sessionID {
		t.Fatalf("Session-Id header = %q, want %q", got, sessionID)
	}
	if got := upstreamReq.Header.Get("Session_id"); got != "" {
		t.Fatalf("Session_id (underscore) is an octopus-only tell and must not be emitted, got %q", got)
	}
	if got := upstreamReq.Header.Get("X-Session-Id"); got != "" {
		t.Fatalf("X-Session-Id is an octopus-only tell and must not be emitted, got %q", got)
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

// TestClaudeFingerprintSessionHeaderMatchesBodyUserID pins that the synthesized
// Claude fingerprint matches the real claude-cli/2.1.198 wire shape: the upstream
// User-Agent is the current CLI version, and the X-Claude-Code-Session-Id header is
// a UUID equal to the session_id embedded in body metadata.user_id (real Claude Code
// emits one UUID in both places — a 32-hex header that disagreed with the body was a
// detectable non-CLI tell).
func TestClaudeFingerprintSessionHeaderMatchesBodyUserID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pck := "fp-consistency-session"
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:           c,
			inboundType: inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{
				Model:          "claude-opus-4-8",
				PromptCacheKey: &pck,
			},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}

	ra.ensureClaudeMetadataUserID()
	upstreamReq := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("User-Agent"); got != dbmodel.DefaultClaudeHeaderUserAgent {
		t.Fatalf("user-agent = %q, want %q", got, dbmodel.DefaultClaudeHeaderUserAgent)
	}

	headerSession := upstreamReq.Header.Get("X-Claude-Code-Session-Id")
	if _, err := uuid.Parse(headerSession); err != nil {
		t.Fatalf("X-Claude-Code-Session-Id = %q is not a UUID: %v", headerSession, err)
	}

	var meta struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	raw := ra.internalRequest.Metadata["user_id"]
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("metadata.user_id is not JSON: %q (%v)", raw, err)
	}
	if meta.SessionID != headerSession {
		t.Fatalf("body session_id %q != header session %q (real Claude Code uses one UUID for both)", meta.SessionID, headerSession)
	}
	if len(meta.DeviceID) != 64 {
		t.Fatalf("device_id should be 64-hex like real Claude Code, got %d chars: %q", len(meta.DeviceID), meta.DeviceID)
	}
}

// TestClaudeFingerprintSuppressedWhenCloakOff pins that an Anthropic channel with
// cloak mode "never" gets NO synthetic Claude fingerprint: ensureClaudeMetadataUserID
// injects no body metadata.user_id, and applyHeaderDefaults (via copyHeaders) sets no
// Claude CLI headers. This lets a domestic Anthropic-compatible upstream (GLM/DeepSeek)
// be reached clean. auto/always stay covered by the sibling tests above.
func TestClaudeFingerprintSuppressedWhenCloakOff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pck := "cloak-off-session"
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:           c,
			inboundType: inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{
				Model:          "glm-4.6",
				PromptCacheKey: &pck,
			},
		},
		channel: &dbmodel.Channel{
			Type:  outbound.OutboundTypeAnthropic,
			Cloak: dbmodel.ChannelCloak{Mode: "never"},
		},
	}

	ra.ensureClaudeMetadataUserID()
	if got := ra.internalRequest.Metadata["user_id"]; got != "" {
		t.Fatalf("cloak off must not inject metadata.user_id, got %q", got)
	}

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://glm.example/v1/messages", nil)
	ra.copyHeaders(upstreamReq)
	if got := upstreamReq.Header.Get("X-Claude-Code-Session-Id"); got != "" {
		t.Fatalf("cloak off must not set X-Claude-Code-Session-Id, got %q", got)
	}
	if got := upstreamReq.Header.Get("User-Agent"); got == dbmodel.DefaultClaudeHeaderUserAgent {
		t.Fatalf("cloak off must not set the claude-cli User-Agent")
	}
}

// TestClaudeOneMillionPlainClientCloakOffEmitsNoClaudeIdentity locks the F1 fix on the
// cloak=never side: a plain (non-CLI) [1m] request routed to a cloak-off Anthropic
// channel must carry NO Claude identity in the body. prepareClaudeOneMillionPlainClientShape
// no longer synthesises metadata.user_id or the "You are a Claude agent" system block
// itself — that was the F1 leak, an ungated (and malformed) re-implementation of the
// cloak-gated canonical paths. With cloak off, ensureClaudeMetadataUserID stays a no-op,
// so neither identity survives on the relay side. (The [1m] model still trips
// AnthropicRequestWantsOneMillionBeta, so this exercises the real branch, not an early
// return.)
func TestClaudeOneMillionPlainClientCloakOffEmitsNoClaudeIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	userContent := "ping"
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:           c,
			inboundType: inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{
				Model: "claude-opus-4-8[1m]",
				Messages: []transformermodel.Message{{
					Role:    "user",
					Content: transformermodel.MessageContent{Content: &userContent},
				}},
			},
		},
		channel: &dbmodel.Channel{
			Type:  outbound.OutboundTypeAnthropic,
			Cloak: dbmodel.ChannelCloak{Mode: "never"},
		},
	}

	// The two relay-side identity calls applyTransformOptions runs, in the same order.
	ra.ensureClaudeMetadataUserID()
	ra.prepareClaudeOneMillionPlainClientShape()

	if got := ra.internalRequest.Metadata["user_id"]; got != "" {
		t.Fatalf("cloak=never [1m] must not inject metadata.user_id, got %q", got)
	}
	for _, msg := range ra.internalRequest.Messages {
		if msg.Content.Content != nil && strings.Contains(*msg.Content.Content, "built on Anthropic's Claude Agent SDK") {
			t.Fatalf("cloak=never [1m] must not inject the Claude agent-identity system block (role %q)", msg.Role)
		}
	}
}

// TestClaudeHeaderDefaultsUniformUAIgnoresInboundVersion pins the UNIFORM-UA
// requirement: even when a genuine claude-cli downstream reports its OWN version
// (UA + X-Stainless-* version/os/arch), octopus must NOT mirror it onto the upstream.
// Every upstream sees ONE pinned static fingerprint, so traffic relayed through
// octopus never looks like many different devices/versions. (This supersedes the
// earlier per-request version adoption — see relay.inboundClaudeClientVersion.)
func TestClaudeHeaderDefaultsUniformUAIgnoresInboundVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "claude-cli/9.9.9 (external, sdk-cli)")
	c.Request.Header.Set("X-Stainless-Package-Version", "1.2.3")
	c.Request.Header.Set("X-Stainless-Runtime-Version", "v22.0.0")
	c.Request.Header.Set("X-Stainless-OS", "MacOS")
	c.Request.Header.Set("X-Stainless-Arch", "arm64")

	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{Model: "claude-opus-4-8"},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}
	up := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	ra.copyHeaders(up)

	// Upstream must see the single pinned static UA, NOT the downstream's 9.9.9.
	if got := up.Header.Get("User-Agent"); got != defaultClaudeUserAgent {
		t.Fatalf("User-Agent = %q, want uniform static %q (must NOT adopt downstream version)", got, defaultClaudeUserAgent)
	}
	// None of the downstream-reported version values may leak onto the upstream.
	leaks := map[string]string{
		"X-Stainless-Package-Version": "1.2.3",
		"X-Stainless-Runtime-Version": "v22.0.0",
		"X-Stainless-OS":              "MacOS",
		"X-Stainless-Arch":            "arm64",
	}
	for h, downstream := range leaks {
		if got := up.Header.Get(h); got == downstream {
			t.Fatalf("%s = %q leaked the downstream value; must be uniform static across all traffic", h, got)
		}
	}
	if up.Header.Get("X-App") != "cli" {
		t.Fatalf("canonical X-App must remain cli")
	}
}

// TestClaudeHeaderDefaultsUsesStaticForNonCLIClient pins that a non-claude-cli client
// (no genuine claude-cli UA) still gets the pinned static fingerprint version, i.e. the
// adopt path is strictly gated and prior behaviour is unchanged for non-CLI callers.
func TestClaudeHeaderDefaultsUsesStaticForNonCLIClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "python-requests/2.31")

	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{Model: "claude-opus-4-8"},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}
	up := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	ra.copyHeaders(up)

	if got := up.Header.Get("User-Agent"); got != dbmodel.DefaultClaudeHeaderUserAgent {
		t.Fatalf("non-CLI client must get the static claude UA, got %q", got)
	}
	if got := up.Header.Get("X-Stainless-OS"); got != defaultClaudeOS {
		t.Fatalf("non-CLI client must get the static pinned OS %q, got %q", defaultClaudeOS, got)
	}
}

// TestClaudeHeaderDefaultsSendsStainlessOSArchWhenStabilizeOff pins F3: a genuine
// claude-cli sends X-Stainless-OS and X-Stainless-Arch on EVERY request, and the
// downstream client's own x-stainless-* are stripped hop-by-hop (shouldForwardClientHeader),
// so octopus must re-emit the pair even when the (now backward-compat-only) stabilize
// toggle is OFF. Previously stabilize=false dropped both headers entirely — a non-CLI
// tell (a request missing two headers the real CLI always carries).
func TestClaudeHeaderDefaultsSendsStainlessOSArchWhenStabilizeOff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t) // sqlite + op cache (seeds the default header settings)
	if err := op.SettingSetString(dbmodel.SettingKeyClaudeHeaderStabilize, "false"); err != nil {
		t.Fatalf("set stabilize=false: %v", err)
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{Model: "claude-opus-4-8"},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}
	up := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	ra.copyHeaders(up)

	gotOS := up.Header.Get("X-Stainless-OS")
	gotArch := up.Header.Get("X-Stainless-Arch")
	if gotOS != defaultClaudeOS {
		t.Fatalf("stabilize=false: X-Stainless-OS = %q, want default %q (must still be sent)", gotOS, defaultClaudeOS)
	}
	if gotArch != defaultClaudeArch {
		t.Fatalf("stabilize=false: X-Stainless-Arch = %q, want default %q (must still be sent)", gotArch, defaultClaudeArch)
	}
	if gotOS == "" || gotArch == "" {
		t.Fatalf("stabilize=false: X-Stainless-OS/Arch must be non-empty, got OS=%q Arch=%q", gotOS, gotArch)
	}
}

// TestClaudeBetaHeaderMatchesGenuineCliOrder pins that the upstream anthropic-beta
// header is emitted in the EXACT order a genuine claude-cli request uses — in
// particular context-1m-2025-08-07 sits in its real position (7th, after
// mid-conversation-system), NOT prepended. Regression guard: a transform-injected
// 1m beta must not reorder the canonical set (AnyRouter shape checks can key on beta
// order/set). Also pins that X-Client-Request-Id stays absent (genuine cli omits it).
func TestClaudeBetaHeaderMatchesGenuineCliOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	ir := &transformermodel.InternalLLMRequest{Model: "claude-opus-4-8"}
	ir.TransformOptions.AnthropicOneMillionBeta = true
	// Reproduce the bug trigger: 1m also present in the transform beta list.
	ir.TransformOptions.AnthropicBetas = []string{transformermodel.AnthropicOneMillionBeta}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			internalRequest: ir,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	// Reproduce the real wire condition: the outbound transformer already appended a
	// lone context-1m beta to the request before relay header defaults run. The
	// rebuild must override this so 1m does NOT stay stuck at position 1.
	upstreamReq.Header.Set("Anthropic-Beta", transformermodel.AnthropicOneMillionBeta)
	ra.copyHeaders(upstreamReq)

	gotBeta := upstreamReq.Header.Get("Anthropic-Beta")
	wantBeta := strings.Join(transformermodel.AnthropicClaudeCodeBetas(true), ",")
	if gotBeta != wantBeta {
		t.Fatalf("anthropic-beta order mismatch:\n got=%q\nwant=%q", gotBeta, wantBeta)
	}
	if strings.HasPrefix(gotBeta, transformermodel.AnthropicOneMillionBeta) {
		t.Fatalf("1m beta must not be prepended (genuine cli puts it 7th): %q", gotBeta)
	}
	if got := upstreamReq.Header.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id must be absent to match genuine claude-cli, got %q", got)
	}
}

// TestClaudeBetaHeaderPreservesGenuineClientSet pins the 2026-07-02 fix: when a real
// claude-cli downstream already sent its OWN anthropic-beta (forwarded onto the outbound
// request by copyHeaders), the relay MUST preserve that set+order verbatim instead of
// rebuilding it into the canonical flagship set. The genuine 2.1.198 haiku agentic set
// carries advisor-tool, places claude-code-20250219 near the end, and omits
// mid-conversation-system/effort — a fixed flagship 7-set here is the exact non-CLI tell
// a wire A/B showed AnyRouter rejects on a haiku request.
func TestClaudeBetaHeaderPreservesGenuineClientSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			internalRequest: &transformermodel.InternalLLMRequest{Model: "claude-haiku-4-5-20251001"},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}

	// The genuine claude-cli 2.1.198 haiku agentic beta set, captured on the wire
	// (order-sensitive: claude-code-20250219 is 5th, advisor-tool is present).
	clientBeta := "interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,claude-code-20250219,advisor-tool-2026-03-01"
	upstreamReq := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	// Mirror the real wire condition: the client's own beta is already on the outbound
	// request (copyHeaders forwards it there) before relay header defaults run.
	upstreamReq.Header.Set("Anthropic-Beta", clientBeta)
	ra.copyHeaders(upstreamReq)

	if got := upstreamReq.Header.Get("Anthropic-Beta"); got != clientBeta {
		t.Fatalf("genuine client beta must be preserved verbatim (not rebuilt):\n got=%q\nwant=%q", got, clientBeta)
	}
}

// TestClaudeBetaStripFlagsEscapeHatch pins the opt-in beta-strip escape hatch
// (SettingKeyClaudeBetaStripFlags): with the default empty value the genuine client
// beta is forwarded verbatim, and with a configured flag list those flags are removed
// from the outbound anthropic-beta while every other flag survives in order. This is
// the escape valve for anyrouter's intermittent 520 on prompt-caching-scope-2026-01-05.
func TestClaudeBetaStripFlagsEscapeHatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t) // sqlite + op cache; seeds SettingKeyClaudeBetaStripFlags="" (OFF)

	// The genuine claude-cli 2.1.198 haiku agentic beta set captured on the wire, which
	// carries the anyrouter-tripping prompt-caching-scope-2026-01-05.
	clientBeta := "interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,claude-code-20250219,advisor-tool-2026-03-01"

	newAttempt := func() *relayAttempt {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		return &relayAttempt{
			relayRequest: &relayRequest{
				c:               c,
				inboundType:     inbound.InboundTypeAnthropic,
				internalRequest: &transformermodel.InternalLLMRequest{Model: "claude-haiku-4-5-20251001"},
			},
			channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
		}
	}

	// Default (empty setting) must forward the client beta verbatim — zero behaviour change.
	reqDefault := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	reqDefault.Header.Set("Anthropic-Beta", clientBeta)
	newAttempt().copyHeaders(reqDefault)
	if got := reqDefault.Header.Get("Anthropic-Beta"); got != clientBeta {
		t.Fatalf("default (empty strip) must preserve beta verbatim:\n got=%q\nwant=%q", got, clientBeta)
	}

	// Configure the escape hatch to strip the tripping flag.
	if err := op.SettingSetString(dbmodel.SettingKeyClaudeBetaStripFlags, "prompt-caching-scope-2026-01-05"); err != nil {
		t.Fatalf("set claude_beta_strip_flags: %v", err)
	}

	reqStripped := httptest.NewRequest(http.MethodPost, "https://anyrouter.top/v1/messages", nil)
	reqStripped.Header.Set("Anthropic-Beta", clientBeta)
	newAttempt().copyHeaders(reqStripped)

	wantStripped := "interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,claude-code-20250219,advisor-tool-2026-03-01"
	got := reqStripped.Header.Get("Anthropic-Beta")
	if strings.Contains(got, "prompt-caching-scope-2026-01-05") {
		t.Fatalf("configured flag must be stripped from anthropic-beta: %q", got)
	}
	if got != wantStripped {
		t.Fatalf("stripped anthropic-beta mismatch:\n got=%q\nwant=%q", got, wantStripped)
	}
}
