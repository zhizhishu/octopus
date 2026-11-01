package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func oneMillionHeaderFor(t *testing.T, model string, cloak dbmodel.ChannelCloak) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ir := &transformermodel.InternalLLMRequest{Model: model}
	ir.TransformOptions.AnthropicOneMillionBeta = true // channel 1M switch on
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, inboundType: inbound.InboundTypeAnthropic, internalRequest: ir},
		channel:      &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic, Cloak: cloak, AnthropicContext1M: true},
	}
	up := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil)
	// what the outbound transformer leaves behind before header defaults run
	if transformermodel.AnthropicRequestWantsOneMillionBeta(ir) {
		up.Header.Set("Anthropic-Beta", transformermodel.AnthropicOneMillionBeta)
	}
	ra.copyHeaders(up)
	return up.Header.Get("Anthropic-Beta")
}

// Shape-off path (cloak=never): haiku gets no context-1m; opus still does.
func TestCloakOffOneMillionSkipsHaiku(t *testing.T) {
	never := dbmodel.ChannelCloak{Mode: "never"}
	if got := oneMillionHeaderFor(t, "claude-haiku-4-5-20251001", never); strings.Contains(got, transformermodel.AnthropicOneMillionBeta) {
		t.Fatalf("cloak=never haiku must not carry context-1m, got %q", got)
	}
	if got := oneMillionHeaderFor(t, "claude-opus-4-8", never); got != transformermodel.AnthropicOneMillionBeta {
		t.Fatalf("cloak=never opus beta = %q, want lone context-1m", got)
	}
}

// CLI-shape synthesis path (non-CLI client): haiku gets the plain haiku set;
// opus keeps the byte-identical canonical 1M order (slot 2, no dup).
func TestClaudeShapeOneMillionSkipsHaiku(t *testing.T) {
	got := oneMillionHeaderFor(t, "claude-haiku-4-5-20251001", dbmodel.ChannelCloak{})
	if strings.Contains(got, transformermodel.AnthropicOneMillionBeta) {
		t.Fatalf("claude shape haiku must not carry context-1m, got %q", got)
	}
	if !strings.HasPrefix(got, "interleaved-thinking-2025-05-14,") {
		t.Fatalf("claude shape haiku must use the reduced haiku set, got %q", got)
	}
	opus := oneMillionHeaderFor(t, "claude-opus-4-8", dbmodel.ChannelCloak{})
	if want := strings.Join(transformermodel.AnthropicClaudeCodeBetas(true), ","); opus != want {
		t.Fatalf("opus 1M order changed:\n got=%q\nwant=%q", opus, want)
	}
}
