package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	model "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// newCodexAnthropicAttempt builds a relayAttempt for an OpenAI-Responses (codex)
// inbound targeting an Anthropic channel; codexClient toggles the codex CLI header.
func newCodexAnthropicAttempt(t *testing.T, req *model.InternalLLMRequest, codexClient bool) *relayAttempt {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if codexClient {
		c.Request.Header.Set("Originator", defaultCodexOriginator)
	}
	return &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}
}

// A codex continuation (previous_response_id set, tools omitted) mapped to Anthropic
// must get the codex tool set restored, else the Claude model loses its tools
// mid-conversation and stalls ("stopped calling tools").
func TestRestoreCodexToolsForAnthropicOnContinuation(t *testing.T) {
	prev := "resp_prev_1"
	req := &model.InternalLLMRequest{PreviousResponseID: &prev}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForAnthropic()
	if len(req.Tools) == 0 {
		t.Fatalf("codex continuation to Anthropic must restore tools, got none")
	}
	if req.ToolChoice == nil {
		t.Fatalf("tool_choice should default to auto when tools are restored")
	}
}

// A continuation whose prior tool output is already replayed into messages (role=tool)
// but carries no tools and no previous_response_id must also get tools restored.
func TestRestoreCodexToolsForAnthropicOnToolOutputHistory(t *testing.T) {
	req := &model.InternalLLMRequest{Messages: []model.Message{{Role: "tool"}}}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForAnthropic()
	if len(req.Tools) == 0 {
		t.Fatalf("codex continuation with tool history must restore tools, got none")
	}
}

// A non-codex responses client (e.g. Cursor) must NOT get the codex tool set injected;
// its tools differ, so silently attaching codex tools would be wrong.
func TestRestoreCodexToolsForAnthropicSkipsNonCodexClient(t *testing.T) {
	prev := "resp_prev_1"
	req := &model.InternalLLMRequest{PreviousResponseID: &prev}
	ra := newCodexAnthropicAttempt(t, req, false)
	ra.restoreCodexToolsForAnthropic()
	if len(req.Tools) != 0 {
		t.Fatalf("non-codex responses client must not get codex tools, got %d", len(req.Tools))
	}
}

// A genuine first turn (no previous_response_id, no tool history) that legitimately
// carries no tools must be left alone — do not fabricate tools.
func TestRestoreCodexToolsForAnthropicSkipsFirstTurn(t *testing.T) {
	req := &model.InternalLLMRequest{}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForAnthropic()
	if len(req.Tools) != 0 {
		t.Fatalf("genuine no-tools first turn must not be fabricated, got %d", len(req.Tools))
	}
}

// A continuation that DID resend its tools must keep them, never be overwritten.
func TestRestoreCodexToolsForAnthropicKeepsExistingTools(t *testing.T) {
	prev := "resp_prev_1"
	req := &model.InternalLLMRequest{
		PreviousResponseID: &prev,
		Tools:              []model.Tool{{Type: "function", Function: model.Function{Name: "my_tool"}}},
	}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForAnthropic()
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "my_tool" {
		t.Fatalf("existing tools must be preserved, got %#v", req.Tools)
	}
}
