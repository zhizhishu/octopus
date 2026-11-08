package modeltest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestChannelTestSelectedUpstreamEndpoint(t *testing.T) {
	ctx := setupModelTestDB(t)
	for _, direction := range []struct {
		name             string
		stored, selected outbound.OutboundType
		endpoint, model  string
	}{
		{"responses-to-claude", outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeAnthropic, "anthropic_messages", "claude-opus-5-5"},
		{"claude-to-responses", outbound.OutboundTypeAnthropic, outbound.OutboundTypeOpenAIResponse, "openai_responses", "gpt-6-astra"},
	} {
		for _, cloak := range []string{"auto", "never"} {
			for _, selectUpstream := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/selected=%t", direction.name, cloak, selectUpstream)
				t.Run(name, func(t *testing.T) {
					wantType := direction.stored
					if selectUpstream {
						wantType = direction.selected
					}
					wantPath := "/v1/responses"
					if wantType == outbound.OutboundTypeAnthropic {
						wantPath = "/v1/messages"
					}
					wantStream := wantType == outbound.OutboundTypeOpenAIResponse || cloak == "auto"
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							http.Error(w, "invalid JSON", 400)
							return
						}
						if r.URL.Path != wantPath {
							t.Errorf("path=%s want %s", r.URL.Path, wantPath)
							http.Error(w, "wrong endpoint", 400)
							return
						}
						if body["model"] != direction.model {
							t.Errorf("model changed: %v", body["model"])
						}
						if got := body["stream"] == true; got != wantStream {
							t.Errorf("stream=%t want %t", got, wantStream)
						}
						if wantType == outbound.OutboundTypeAnthropic {
							if lenValue(body["messages"]) == 0 || body["input"] != nil {
								t.Error("wrong Anthropic body")
								http.Error(w, "invalid messages", 400)
								return
							}
							if r.Header.Get("Authorization") != "Bearer test-key" {
								t.Error("Anthropic proxy auth header missing")
							}
							if cloak == "auto" && r.Header.Get("X-Api-Key") != "" {
								t.Error("cloaked Claude must not send duplicate auth headers")
							}
							if selectUpstream && direction.model == "claude-opus-5-5" {
								thinking, _ := body["thinking"].(map[string]any)
								if thinking["type"] == "disabled" {
									t.Error("adaptive-only Claude received disabled thinking")
								}
							}
							if wantStream {
								w.Header().Set("Content-Type", "text/event-stream")
								fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", direction.model)
							} else {
								w.Header().Set("Content-Type", "application/json")
								fmt.Fprintf(w, `{"id":"msg_test","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`, direction.model)
							}
						} else {
							if lenValue(body["input"]) == 0 || body["messages"] != nil || body["thinking"] != nil {
								t.Error("wrong Responses body")
								http.Error(w, "invalid input", 400)
								return
							}
							if r.Header.Get("Authorization") != "Bearer test-key" {
								t.Error("Responses auth header missing")
							}
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n", direction.model, direction.model)
						}
					}))
					defer upstream.Close()
					channel := dbmodel.Channel{Name: name, Type: direction.stored, Enabled: true, Model: direction.model, BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}}, Cloak: dbmodel.ChannelCloak{Mode: cloak}}
					if err := op.ChannelCreate(&channel, ctx); err != nil {
						t.Fatal(err)
					}
					stream := false
					req := dbmodel.ModelTestRequest{Model: direction.model, ChannelID: channel.ID, Endpoint: direction.endpoint, TestUpstreamEndpoint: selectUpstream, Prompt: "Reply OK.", Stream: &stream}
					for _, draft := range []bool{false, true} {
						var result dbmodel.ModelTestResponse
						var err error
						if draft {
							result, err = RunChannel(ctx, channel, req)
						} else {
							result, err = Run(ctx, req)
						}
						if err != nil {
							t.Fatal(err)
						}
						if len(result.Results) != 1 || !result.Results[0].Success || result.Results[0].UpstreamPath != wantPath || result.Results[0].ResponsePreview != "OK" {
							t.Fatalf("draft=%t result=%#v", draft, result)
						}
					}
					if calls.Load() != 2 {
						t.Fatalf("actual upstream requests=%d want2", calls.Load())
					}
					stored, err := op.ChannelGet(channel.ID, ctx)
					if err != nil {
						t.Fatal(err)
					}
					if channel.Type != direction.stored || stored.Type != direction.stored {
						t.Fatal("test modified saved channel type")
					}
				})
			}
		}
	}
}

func lenValue(value any) int {
	if items, ok := value.([]any); ok {
		return len(items)
	}
	return 0
}

func TestChannelTestCustomURLDoesNotGuessAnotherProtocol(t *testing.T) {
	ctx := setupModelTestDB(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "must not be called", 500) }))
	defer upstream.Close()
	channel := dbmodel.Channel{Type: outbound.OutboundTypeCustomOpenAIChat, BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL + "/fixed/chat"}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}}}
	result, err := RunChannel(ctx, channel, dbmodel.ModelTestRequest{Model: "gpt-6-astra", Endpoint: "openai_responses", TestUpstreamEndpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(result.Results) != 1 || result.Results[0].Success || !strings.Contains(result.Results[0].Error, "完整地址") {
		t.Fatalf("unexpected result=%#v calls=%d", result, calls.Load())
	}
}
