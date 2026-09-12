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
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestChatStreamEOFRetainsFieldsAndSynthSingleDone(t *testing.T) {
	choices := []string{
		`{"delta":{"role":"assistant","reasoning_content":"THINK_ONLY"}}`,
		`{"delta":{"content":"BODY_ONLY"}}`,
		`{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calculate","arguments":"{\"value\":"}}]}}`,
		`{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"3031}"}}]}}`,
		`{"delta":{},"finish_reason":"tool_calls"}`,
	}
	for _, heartbeat := range []bool{false, true} {
		name := "eof"
		if heartbeat {
			name = "heartbeats_then_eof"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayErrorDB(t)
			var script strings.Builder
			for _, choice := range choices {
				event := `{"id":"chatcmpl_boundary","object":"chat.completion.chunk","model":"reasoning-model","choices":[` + choice + `]}`
				if !json.Valid([]byte(event)) {
					t.Fatalf("invalid test event: %s", event)
				}
				script.WriteString("data: " + event + "\n\n")
				if heartbeat {
					script.WriteString(": keepalive\n\ndata: \n\n")
				}
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected upstream path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(script.String())) // Deliberately omit [DONE].
			}))
			t.Cleanup(upstream.Close)
			channel := dbmodel.Channel{
				Name: "chat-eof-boundary", Type: outbound.OutboundTypeOpenAIChat, Enabled: true,
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
				Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
				Model:    "reasoning-model", Priority: 1,
			}
			if err := op.ChannelCreate(&channel, ctx); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"reasoning-model","messages":[{"role":"user","content":"calculate"}],"stream":true,"tools":[{"type":"function","function":{"name":"calculate","parameters":{"type":"object"}}}]}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")
			Handler(inbound.InboundTypeOpenAIChat, c)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var reasoning, text, arguments, toolID, toolName, finish string
			doneCount := 0
			for _, line := range strings.Split(rec.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "" {
					continue
				}
				if data == "[DONE]" {
					doneCount++
					continue
				}
				var event struct {
					Choices []struct {
						Delta struct {
							Reasoning string `json:"reasoning_content"`
							Content   string `json:"content"`
							Tools     []struct {
								ID       string `json:"id"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
						Finish string `json:"finish_reason"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatalf("invalid downstream event: %v", err)
				}
				for _, choice := range event.Choices {
					reasoning += choice.Delta.Reasoning
					text += choice.Delta.Content
					if choice.Finish != "" {
						finish = choice.Finish
					}
					for _, tool := range choice.Delta.Tools {
						arguments += tool.Function.Arguments
						if tool.ID != "" {
							toolID = tool.ID
						}
						if tool.Function.Name != "" {
							toolName = tool.Function.Name
						}
					}
				}
			}
			if doneCount != 1 || !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") || reasoning != "THINK_ONLY" || text != "BODY_ONLY" || arguments != `{"value":3031}` || toolID != "call_1" || toolName != "calculate" || finish != "tool_calls" {
				t.Fatalf("stream boundary or fields lost: %s", rec.Body.String())
			}
		})
	}
}
