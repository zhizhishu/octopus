package authropic_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/inbound/anthropic"
	"github.com/zhizhishu/octopus/internal/transformer/outbound/authropic"
)

// pi-ai / DSH on claude-opus-5.5 class models pins the top-level output_config at "high" and puts
// the turn's real effort on a trailing empty system message (mid-conversation-output-config beta).
// Before the fix the outbound hoisted that message: its output_config was dropped and an empty text
// block landed in system, so the upstream ran at "high" whatever the client chose, and the log said
// "high" too.
func TestMidConversationOutputConfigEffortReachesUpstream(t *testing.T) {
	for _, effort := range []string{"xhigh", "max", "low"} {
		t.Run(effort, func(t *testing.T) {
			body := `{"model":"claude-opus-5.5","max_tokens":128000,"stream":true,
"betas":["mid-conversation-output-config-2026-07-01","thinking-binding-controls-2026-08-01"],
"thinking":{"type":"adaptive","display":"summarized"},
"output_config":{"effort":"high"},
"messages":[{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"effort":"` + effort + `"}}]}`
			req, err := (&anthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(body))
			if err != nil {
				t.Fatalf("inbound: %v", err)
			}
			if req.ReasoningEffort != effort {
				t.Fatalf("internal effort (logs, cross-protocol) = %q, want the turn's %q", req.ReasoningEffort, effort)
			}
			httpReq, err := (&authropic.MessageOutbound{}).TransformRequest(context.Background(), req, "https://relay.example.com", "k")
			if err != nil {
				t.Fatalf("outbound: %v", err)
			}
			out, _ := io.ReadAll(httpReq.Body)
			var decoded struct {
				Messages []struct {
					Role         string          `json:"role"`
					OutputConfig json.RawMessage `json:"output_config"`
				} `json:"messages"`
				System []struct {
					Text string `json:"text"`
				} `json:"system"`
				OutputConfig json.RawMessage `json:"output_config"`
			}
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("decode: %v\n%s", err, out)
			}
			if n := len(decoded.Messages); n != 2 || decoded.Messages[1].Role != "system" ||
				string(decoded.Messages[1].OutputConfig) != `{"effort":"`+effort+`"}` {
				t.Fatalf("trailing system message with the turn effort must stay in messages, got %s", out)
			}
			if string(decoded.OutputConfig) != `{"effort":"high"}` {
				t.Fatalf("client's top-level output_config must be forwarded untouched, got %s", decoded.OutputConfig)
			}
			for _, s := range decoded.System {
				if s.Text == "" {
					t.Fatalf("no empty text block may be hoisted into system: %s", out)
				}
			}
		})
	}
}

// A client that sends no per-message output_config keeps the old reading (top-level effort, else high).
func TestAdaptiveEffortWithoutPerMessageConfigUnchanged(t *testing.T) {
	cases := map[string]string{
		`{"model":"claude-opus-4-8","max_tokens":64000,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`: "max",
		`{"model":"claude-opus-4-8","max_tokens":64000,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`:                                  "high",
	}
	for body, want := range cases {
		req, err := (&anthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(body))
		if err != nil {
			t.Fatalf("inbound: %v", err)
		}
		if req.ReasoningEffort != want {
			t.Fatalf("effort = %q, want %q for %s", req.ReasoningEffort, want, body)
		}
	}
}
