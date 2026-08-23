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

func strPtrRT(s string) *string { return &s }

// storeSessionTools records a transcript + tools under responseID with owner 0/0,
// matching the test relayAttempt's default apiKeyID/userID (readable by anyone).
func storeSessionTools(responseID string, tools []model.Tool) {
	msgs := []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtrRT("hi")}}}
	recordResponsesSessionTranscriptOwned(responseID, msgs, tools, 0, 0)
}

// A codex continuation (previous_response_id set, tools omitted) mapped to Anthropic
// must recover the client's REAL tools from the session — not a hardcoded default —
// else the mapped Claude model loses its tools mid-conversation and stalls.
func TestRestoreCodexToolsForAnthropicRestoresSessionTools(t *testing.T) {
	prev := "resp_prev_sess_restore"
	storeSessionTools(prev, []model.Tool{{Type: "function", Function: model.Function{Name: "shell"}}})
	req := &model.InternalLLMRequest{PreviousResponseID: &prev}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForStatelessOutbound()
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "shell" {
		t.Fatalf("expected client's real session tool 'shell' restored, got %#v", req.Tools)
	}
	if req.ToolChoice == nil {
		t.Fatalf("tool_choice should default to auto when tools restored")
	}
}

// A non-codex responses client (e.g. Cursor) must not trigger the codex restore path.
func TestRestoreCodexToolsForAnthropicSkipsNonCodexClient(t *testing.T) {
	prev := "resp_prev_sess_noncodex"
	storeSessionTools(prev, []model.Tool{{Type: "function", Function: model.Function{Name: "shell"}}})
	req := &model.InternalLLMRequest{PreviousResponseID: &prev}
	ra := newCodexAnthropicAttempt(t, req, false)
	ra.restoreCodexToolsForStatelessOutbound()
	if len(req.Tools) != 0 {
		t.Fatalf("non-codex client must not restore codex session tools, got %d", len(req.Tools))
	}
}

// A genuine first turn (no previous_response_id) is left alone.
func TestRestoreCodexToolsForAnthropicSkipsFirstTurn(t *testing.T) {
	req := &model.InternalLLMRequest{}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForStatelessOutbound()
	if len(req.Tools) != 0 {
		t.Fatalf("no-previous-id first turn must not be touched, got %d", len(req.Tools))
	}
}

// A continuation that DID resend its tools keeps them, never overwritten.
func TestRestoreCodexToolsForAnthropicKeepsExistingTools(t *testing.T) {
	prev := "resp_prev_sess_existing"
	storeSessionTools(prev, []model.Tool{{Type: "function", Function: model.Function{Name: "shell"}}})
	req := &model.InternalLLMRequest{
		PreviousResponseID: &prev,
		Tools:              []model.Tool{{Type: "function", Function: model.Function{Name: "my_tool"}}},
	}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForStatelessOutbound()
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "my_tool" {
		t.Fatalf("existing tools must be preserved, got %#v", req.Tools)
	}
}

// A continuation whose session is gone (expired / never stored) must NOT get a
// wrong-named default injected — leave tools empty (the old defaultCodexTools bug).
func TestRestoreCodexToolsForAnthropicSkipsWhenNoSession(t *testing.T) {
	prev := "resp_prev_sess_absent_never_stored"
	req := &model.InternalLLMRequest{PreviousResponseID: &prev}
	ra := newCodexAnthropicAttempt(t, req, true)
	ra.restoreCodexToolsForStatelessOutbound()
	if len(req.Tools) != 0 {
		t.Fatalf("missing session must not inject default tools, got %#v", req.Tools)
	}
}

func TestRestoreCodexToolsForStatelessChatOutbounds(t *testing.T) {
	tests := []struct {
		name        string
		channelType outbound.OutboundType
	}{
		{
			name:        "OpenAIChat",
			channelType: outbound.OutboundTypeOpenAIChat,
		},
		{
			name:        "CustomOpenAIChat",
			channelType: outbound.OutboundTypeCustomOpenAIChat,
		},
		{
			name:        "Gemini",
			channelType: outbound.OutboundTypeGemini,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := "resp_prev_sess_chat_" + tt.name
			storeSessionTools(prev, []model.Tool{{Type: "function", Function: model.Function{Name: "shell"}}})
			req := &model.InternalLLMRequest{PreviousResponseID: &prev}

			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("Originator", defaultCodexOriginator)

			ra := &relayAttempt{
				relayRequest: &relayRequest{
					c:               c,
					inboundType:     inbound.InboundTypeOpenAIResponse,
					internalRequest: req,
				},
				channel: &dbmodel.Channel{Type: tt.channelType},
			}

			ra.restoreCodexToolsForStatelessOutbound()
			if len(req.Tools) == 0 {
				t.Fatalf("[%s] expected non-empty req.Tools restored from session", tt.name)
			}
			if req.ToolChoice == nil {
				t.Fatalf("[%s] expected non-nil ToolChoice when tools restored", tt.name)
			}
		})
	}
}
