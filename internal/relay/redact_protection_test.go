package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// redactProtWriteChatOK writes a minimal valid OpenAI chat completion body.
func redactProtWriteChatOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"rp","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
}

// TestRedactProtectionFailureFallsThroughFailClosed inverts the audit diagnostic
// TestAuditReviewFailureFallsThroughUnprotected: an opted-in channel whose
// redaction engine is broken must NOT let the request fall through to an opted-out
// channel and leak cleartext. The early sticky pin (defect #1) forces every later
// attempt to re-enter redaction, which also fails closed — so the upstream sees zero
// requests and the client gets a non-200.
func TestRedactProtectionFailureFallsThroughFailClosed(t *testing.T) {
	setupRedactDB(t)
	redactEngineTestErr = errors.New("synthetic audit engine failure")
	defer func() { redactEngineTestErr = nil }()

	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		redactProtWriteChatOK(w)
	}))
	defer upstream.Close()

	newRedactChannelFull(t, "audit-enabled", upstream.URL, outbound.OutboundTypeOpenAIChat, true, "E", 1)
	newRedactChannelFull(t, "audit-disabled", upstream.URL, outbound.OutboundTypeOpenAIChat, false, "", 2)

	rec := httptest.NewRecorder()
	newRedactGinEngine().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"a@example.com"}]}`)))

	mu.Lock()
	defer mu.Unlock()
	if rec.Code == http.StatusOK {
		t.Fatalf("engine-down must not succeed even with an opted-out fallback channel: status=%d", rec.Code)
	}
	if len(bodies) != 0 {
		t.Fatalf("fail-closed violated: upstream received %d request(s): %#v", len(bodies), bodies)
	}
}

// TestRedactInboundRedactActiveStickyBeatsMasterFlip inverts the audit diagnostic
// TestAuditReviewMasterFlipDefeatsSticky: once a request pins protection, flipping
// the global master switch off mid-request must not cancel it for later attempts.
// A brand-new request with the master off stays inactive (总闸 semantics).
func TestRedactInboundRedactActiveStickyBeatsMasterFlip(t *testing.T) {
	setupRedactDB(t)

	req := &relayRequest{
		inboundType: inbound.InboundTypeOpenAIChat,
		internalRequest: &model.InternalLLMRequest{
			RawRequest: []byte(`{"messages":[{"role":"user","content":"a@example.com"}]}`),
			Messages:   []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtr("a@example.com")}}},
		},
	}
	first := &relayAttempt{relayRequest: req, channel: &dbmodel.Channel{RedactEnabled: true, RedactFlags: "E"}}
	if err := first.applyInboundRedaction(); err != nil {
		t.Fatal(err)
	}
	defer first.redactClose()
	if !req.redactRequired {
		t.Fatal("first attempt did not pin protection")
	}

	if err := op.SettingSetString(dbmodel.SettingKeyRedactEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	next := &relayAttempt{relayRequest: req, channel: &dbmodel.Channel{RedactEnabled: true, RedactFlags: "E"}}
	if !next.inboundRedactActive() {
		t.Fatal("sticky protection must survive a mid-request master-switch flip")
	}

	// 总闸语义: master off + opt-in + no sticky must be inactive for a NEW request.
	fresh := &relayRequest{
		inboundType:     inbound.InboundTypeOpenAIChat,
		internalRequest: &model.InternalLLMRequest{RawRequest: []byte(`{"messages":[]}`)},
	}
	freshAttempt := &relayAttempt{relayRequest: fresh, channel: &dbmodel.Channel{RedactEnabled: true, RedactFlags: "E"}}
	if freshAttempt.inboundRedactActive() {
		t.Fatal("master switch off must keep a new request inactive")
	}
}

// TestRedactInboundRedactActiveTruthTable locks the full 9-cell decision matrix
// (global on/off x channel opt-in/out x sticky yes/no, plus the nil-channel guard).
func TestRedactInboundRedactActiveTruthTable(t *testing.T) {
	setupRedactDB(t) // master ON initially

	mkReq := func(sticky bool) *relayRequest {
		r := &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIChat,
			internalRequest: &model.InternalLLMRequest{RawRequest: []byte(`{"messages":[]}`)},
		}
		r.redactRequired = sticky
		return r
	}
	setMaster := func(on bool) {
		v := "false"
		if on {
			v = "true"
		}
		if err := op.SettingSetString(dbmodel.SettingKeyRedactEnabled, v); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		global, optIn, sticky, want bool
	}{
		{false, false, false, false},
		{false, false, true, true},
		{false, true, false, false},
		{false, true, true, true},
		{true, false, false, false},
		{true, false, true, true},
		{true, true, false, true},
		{true, true, true, true},
	}
	for _, c := range cases {
		setMaster(c.global)
		ra := &relayAttempt{relayRequest: mkReq(c.sticky), channel: &dbmodel.Channel{RedactEnabled: c.optIn, RedactFlags: "E"}}
		if got := ra.inboundRedactActive(); got != c.want {
			t.Fatalf("global=%t optIn=%t sticky=%t: got %t want %t", c.global, c.optIn, c.sticky, got, c.want)
		}
	}

	// Row 9: nil channel is always inactive.
	setMaster(true)
	raNil := &relayAttempt{relayRequest: mkReq(false), channel: nil}
	if raNil.inboundRedactActive() {
		t.Fatal("nil channel must be inactive")
	}
}

// TestRedactJSONTextNumericToolArgumentsValid inverts the audit diagnostic
// TestAuditReviewNumericToolArguments: JSON tool arguments must go through the
// JSON-aware walker, so a numeric value stays a number and the arguments stay valid
// JSON, while the tool name is untouched.
func TestRedactJSONTextNumericToolArgumentsValid(t *testing.T) {
	setupRedactDB(t)

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession("P", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	messages, err := redactMessagesText(session, []model.Message{{
		Role: "assistant",
		ToolCalls: []model.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: model.FunctionCall{Name: "lookup", Arguments: `{"phone":13800138000}`},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := messages[0].ToolCalls[0].Function.Arguments
	if !json.Valid([]byte(got)) {
		t.Fatalf("tool arguments must remain valid JSON with a numeric value, got %q", got)
	}
	if got != `{"phone":13800138000}` {
		t.Fatalf("numeric value / key must be preserved (core semantics), got %q", got)
	}
	if name := messages[0].ToolCalls[0].Function.Name; name != "lookup" {
		t.Fatalf("tool name must not change, got %q", name)
	}
}

// TestRedactJSONTextMatchesBodyScan asserts RedactJSONText(args) equals the output
// the core produces for that same args when it scans a whole body containing it, for
// all three covered inbound protocols (chat/responses put args in a string field;
// anthropic puts them in a tool_use input object). A non-JSON payload must reduce
// to the plain-text RedactText path.
func TestRedactJSONTextMatchesBodyScan(t *testing.T) {
	setupRedactDB(t)

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	args := `{"phone":13800138000,"email":"a@example.com","user":"c@example.com","name":"d@example.com","tools":"e@example.com","nested":{"contact":"b@example.com"}}`
	quotedArgsRaw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	quotedArgs := string(quotedArgsRaw) // JSON string literal of args

	cases := []struct {
		name     string
		protocol string
		body     string
		extract  func(t *testing.T, body []byte) string
	}{
		{
			name:     "openai_chat",
			protocol: "openai_chat",
			body:     `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":` + quotedArgs + `}}]}]}`,
			extract: func(t *testing.T, body []byte) string {
				var v struct {
					Messages []struct {
						ToolCalls []struct {
							Function struct {
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatal(err)
				}
				return v.Messages[0].ToolCalls[0].Function.Arguments
			},
		},
		{
			name:     "openai_responses",
			protocol: "openai_responses",
			body:     `{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"lookup","arguments":` + quotedArgs + `}]}`,
			extract: func(t *testing.T, body []byte) string {
				var v struct {
					Input []struct {
						Arguments string `json:"arguments"`
					} `json:"input"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatal(err)
				}
				return v.Input[0].Arguments
			},
		},
		{
			name:     "anthropic_messages",
			protocol: "anthropic_messages",
			body:     `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"lookup","input":` + args + `}]}]}`,
			extract: func(t *testing.T, body []byte) string {
				var v struct {
					Messages []struct {
						Content []struct {
							Input json.RawMessage `json:"input"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(body, &v); err != nil {
					t.Fatal(err)
				}
				return string(v.Messages[0].Content[0].Input)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ONE session, exactly as the relay uses it: the per-attempt session scans
			// the client body AND redacts the per-field tool arguments, so a value is
			// tokenized once (idempotent tokenFor) and both outputs are byte-identical.
			// (Two separate sessions would borrow different VMs from the engine pool,
			// each with its own salted runtimeSalt, producing different-but-equivalent
			// tokens — a non-behavioral difference, not a divergence.)
			s, err := engine.NewSession("E", tc.protocol, false)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			bodyOut, err := s.RedactJSONBody([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			bodyArgs := tc.extract(t, bodyOut)

			textArgs, err := s.RedactJSONText(args)
			if err != nil {
				t.Fatal(err)
			}

			if bodyArgs != textArgs {
				t.Fatalf("RedactJSONText != body-scan output for %s:\n body=%q\n text=%q", tc.name, bodyArgs, textArgs)
			}
			var parsedArgs map[string]any
			if err := json.Unmarshal([]byte(bodyArgs), &parsedArgs); err != nil {
				t.Fatalf("body args not JSON for %s: %v (%q)", tc.name, err, bodyArgs)
			}
			if got, ok := parsedArgs["phone"].(float64); !ok || got != 13800138000 {
				t.Fatalf("numeric value must be preserved as a number for %s: %#v", tc.name, parsedArgs["phone"])
			}
			// Top-level-skip and envelope keys must still be redacted as business-payload
			// leaves (guards against a path=[] / businessPayload regression).
			for _, k := range []string{"email", "user", "name", "tools"} {
				got, _ := parsedArgs[k].(string)
				if !redactTokenRe.MatchString(got) {
					t.Fatalf("%s: key %q must be redacted, got %q", tc.name, k, got)
				}
			}
		})
	}
}

// TestRedactJSONTextFreeformCustomToolUsesText locks that a non-JSON (custom tool)
// payload is treated as plain text, exactly like the current RedactText path.
func TestRedactJSONTextFreeformCustomToolUsesText(t *testing.T) {
	setupRedactDB(t)

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	payload := "please email a@example.com and use sk-" + strings.Repeat("A", 60)

	// Same session for both calls so the salted token identity matches; the point is
	// that RedactJSONText reduces a non-JSON payload to the plain-text path.
	s, err := engine.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.RedactJSONText(payload)
	if err != nil {
		t.Fatal(err)
	}

	want, err := s.RedactText(payload)
	if err != nil {
		t.Fatal(err)
	}

	if got != want {
		t.Fatalf("non-JSON payload must follow RedactText: got %q want %q", got, want)
	}
	if strings.Contains(got, "a@example.com") {
		t.Fatalf("freeform email not redacted: %q", got)
	}
}

// TestRedactToolArgumentsEndpointEndToEnd drives a chat request whose assistant
// tool call carries a JSON arguments object with an email and a numeric value: the
// upstream arguments must be a redacted, still-valid JSON object with the tool name
// unchanged and the number preserved.
func TestRedactToolArgumentsEndpointEndToEnd(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		redactProtWriteChatOK(w)
	}))
	defer upstream.Close()
	newRedactChannelFull(t, "redact-toolargs", upstream.URL, outbound.OutboundTypeOpenAIChat, true, "E", 1)

	args := `{"phone":13800138000,"email":"a@example.com"}`
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"request-model","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":` + string(argsJSON) + `}}]}]}`

	rec := httptest.NewRecorder()
	newRedactGinEngine().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	up := gotBody
	mu.Unlock()
	if strings.Contains(up, "a@example.com") {
		t.Fatalf("email leaked inside tool args: %s", up)
	}
	var parsed struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(up), &parsed); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, up)
	}
	if len(parsed.Messages) == 0 || len(parsed.Messages[0].ToolCalls) == 0 {
		t.Fatalf("tool_calls missing upstream: %s", up)
	}
	fn := parsed.Messages[0].ToolCalls[0].Function
	if fn.Name != "lookup" {
		t.Fatalf("tool name changed: %q", fn.Name)
	}
	if !json.Valid([]byte(fn.Arguments)) {
		t.Fatalf("arguments no longer valid JSON: %q", fn.Arguments)
	}
	if !redactTokenRe.MatchString(fn.Arguments) {
		t.Fatalf("arguments missing placeholder: %q", fn.Arguments)
	}
	if !strings.Contains(fn.Arguments, "13800138000") {
		t.Fatalf("numeric value not preserved: %q", fn.Arguments)
	}
}

// TestRestoreInternalResponseTexts covers the #6 restore helper: it returns a copy
// with the four supported text kinds restored, leaves reasoning/signature drift
// untouched, never mutates its input, and is an identity no-op when the session is
// absent or nothing was applied.
func TestRestoreInternalResponseTexts(t *testing.T) {
	setupRedactDB(t)

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	token, err := session.RedactText("a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !redactTokenRe.MatchString(token) {
		t.Fatalf("expected a placeholder token, got %q", token)
	}

	ra := &relayAttempt{redactSession: session, redactApplied: true}

	mk := func() *model.InternalLLMResponse {
		content := "contact " + token
		part := "part " + token
		reasoning := "reason " + token
		sig := "sig " + token
		fcArgs := `{"email":"` + token + `"}`
		return &model.InternalLLMResponse{
			ID: "resp-x",
			Choices: []model.Choice{{
				Index: 0,
				Message: &model.Message{
					Role:               "assistant",
					Content:            model.MessageContent{Content: &content, MultipleContent: []model.MessageContentPart{{Type: "text", Text: &part}}},
					ToolCalls:          []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "lookup", Arguments: fcArgs}}},
					FunctionCall:       &model.FunctionCall{Name: "legacy", Arguments: fcArgs},
					ReasoningContent:   &reasoning,
					ReasoningSignature: &sig,
				},
			}},
		}
	}

	in := mk()
	out := ra.restoreInternalResponseTexts(in)
	if out == in {
		t.Fatal("expected a copy, got the same pointer")
	}
	msg := out.Choices[0].Message
	if got := *msg.Content.Content; got != "contact a@example.com" {
		t.Fatalf("message content not restored: %q", got)
	}
	if got := *msg.Content.MultipleContent[0].Text; got != "part a@example.com" {
		t.Fatalf("content part text not restored: %q", got)
	}
	if got := msg.ToolCalls[0].Function.Arguments; got != `{"email":"a@example.com"}` {
		t.Fatalf("tool-call arguments not restored: %q", got)
	}
	if got := msg.FunctionCall.Arguments; got != `{"email":"a@example.com"}` {
		t.Fatalf("function_call arguments not restored: %q", got)
	}
	// Drift fields must be untouched in the copy.
	if got := *msg.ReasoningContent; got != "reason "+token {
		t.Fatalf("reasoning must not be restored: %q", got)
	}
	if got := *msg.ReasoningSignature; got != "sig "+token {
		t.Fatalf("reasoning signature must not be restored: %q", got)
	}
	// The input must not be mutated.
	inMsg := in.Choices[0].Message
	if got := *inMsg.Content.Content; got != "contact "+token {
		t.Fatalf("input mutated (content): %q", got)
	}
	if got := inMsg.ToolCalls[0].Function.Arguments; got != `{"email":"`+token+`"}` {
		t.Fatalf("input mutated (tool-call arguments): %q", got)
	}
	if got := *inMsg.ReasoningContent; got != "reason "+token {
		t.Fatalf("input mutated (reasoning): %q", got)
	}

	// No-op conditions.
	if got := ra.restoreInternalResponseTexts(nil); got != nil {
		t.Fatal("nil response must pass through")
	}
	notApplied := &relayAttempt{redactSession: session}
	if got := notApplied.restoreInternalResponseTexts(in); got != in {
		t.Fatal("must be identity when redactApplied=false")
	}
	noSession := &relayAttempt{redactApplied: true}
	if got := noSession.restoreInternalResponseTexts(in); got != in {
		t.Fatal("must be identity when redactSession=nil")
	}
}
