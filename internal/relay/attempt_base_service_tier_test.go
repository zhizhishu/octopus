package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestAttemptBaseRestoresClientServiceTier(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyCodexFastMode, "true"); err != nil {
		t.Fatalf("enable Codex Fast: %v", err)
	}

	for _, tier := range []string{"<absent>", "auto", "default", "flex", "priority"} {
		t.Run(tier, func(t *testing.T) {
			var original *string
			if tier != "<absent>" {
				original = &tier
			}
			req := &model.InternalLLMRequest{
				Model:           "gpt-6-astra",
				ServiceTier:     original,
				ReasoningEffort: "high",
			}
			base := captureAttemptBase(req)

			for attempt := 0; attempt < 2; attempt++ {
				applyCodexFastMode(req)
				if original == nil {
					if req.ServiceTier == nil || *req.ServiceTier != "priority" {
						t.Fatal("positive control: Codex Fast did not inject priority")
					}
				} else if req.ServiceTier != original || *req.ServiceTier != tier {
					t.Fatalf("Codex Fast replaced the client's %q tier", tier)
				}
				if req.ReasoningEffort != "high" {
					t.Fatalf("Codex Fast changed reasoning effort to %q", req.ReasoningEffort)
				}

				base.restore(req, "gpt-6-astra")
				if req.ServiceTier != original {
					t.Fatalf("attempt %d: restore did not recover the original service-tier pointer", attempt+1)
				}
			}
		})
	}
}

func TestAttemptBaseServiceTierOutboundRetryAndFailover(t *testing.T) {
	setupRescueDeadlineDB(t)

	for _, channelType := range []outbound.OutboundType{outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeAnthropic} {
		for _, tier := range []string{"<absent>", "auto", "default", "flex", "priority"} {
			name := "responses/" + tier
			if channelType == outbound.OutboundTypeAnthropic {
				name = "anthropic/" + tier
			}
			t.Run(name, func(t *testing.T) {
				var original *string
				if tier != "<absent>" {
					original = &tier
				}
				instruction, text := "Answer briefly.", "ping"
				stream := true
				session := "service-tier-test-session"
				template := &model.InternalLLMRequest{
					Model:        "gpt-6-astra",
					RawAPIFormat: model.APIFormatOpenAIResponse,
					ServiceTier:  original,
					Stream:       &stream,
					Messages: []model.Message{
						{Role: "developer", Content: model.MessageContent{Content: &instruction}},
						{Role: "user", Content: model.MessageContent{Content: &text}},
					},
					ResponsesInputRaw: json.RawMessage(`[{"role":"developer","content":[{"type":"input_text","text":"Answer briefly."}]},{"role":"user","content":[{"type":"input_text","text":"ping"}]}]`),
					ReasoningEffort:   "high",
					PromptCacheKey:    &session,
					// Fixed client metadata keeps the Fast on/off comparison free of random turn IDs.
					ClientMetadata: json.RawMessage(`{"x-codex-installation-id":"test-installation","x-codex-window-id":"service-tier-test-session:0","x-codex-turn-metadata":"{\"turn_id\":\"test-turn\"}"}`),
				}

				var expectedBodies [3][]byte
				var expectedHeaders [3]http.Header
				for _, fast := range []string{"false", "true"} {
					if err := op.SettingSetString(dbmodel.SettingKeyCodexFastMode, fast); err != nil {
						t.Fatalf("set Codex Fast=%s: %v", fast, err)
					}
					req := cloneInternalRequestForRetry(template)
					base := captureAttemptBase(req)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					c.Request.Header.Set("User-Agent", "test-client/1.0")
					c.Request.Header.Set("Content-Type", "application/json")
					ra := &relayAttempt{
						relayRequest: &relayRequest{
							c:                c,
							internalRequest:  req,
							inboundType:      inbound.InboundTypeOpenAIResponse,
							requestModel:     "gpt-6-astra",
							clientSessionKey: session,
						},
						channel: &dbmodel.Channel{
							ID:   1,
							Type: outbound.OutboundTypeOpenAIResponse,
						},
						usedKey: dbmodel.ChannelKey{ID: 1, ChannelKey: "test-key"},
					}

					// First hop, retry with the same channel/key, then a non-Codex failover.
					for step := 0; step < 3; step++ {
						if step == 2 {
							ra.channel = &dbmodel.Channel{
								ID:    2,
								Type:  channelType,
								Cloak: dbmodel.ChannelCloak{Mode: "never"},
							}
						}
						base.restore(req, "gpt-6-astra")
						if req.ServiceTier != original {
							t.Fatalf("Fast=%s step=%d: original service-tier pointer was not restored", fast, step)
						}
						wire, body := serviceTierOutboundRequest(t, ra)
						var payload map[string]any
						if err := json.Unmarshal(body, &payload); err != nil {
							t.Fatalf("decode outbound body: %v", err)
						}
						wantTier := original
						if fast == "true" && step < 2 && original == nil {
							priority := "priority"
							wantTier = &priority
						}
						gotTier, present := payload["service_tier"]
						if wantTier == nil {
							if present {
								t.Fatalf("Fast=%s step=%d: unexpected service_tier=%v", fast, step, gotTier)
							}
						} else if !present || gotTier != *wantTier {
							t.Fatalf("Fast=%s step=%d: service_tier=%v, want %q", fast, step, gotTier, *wantTier)
						}
						if fast == "false" {
							expectedBodies[step] = body
							expectedHeaders[step] = wire.Header.Clone()
							continue
						}
						if !reflect.DeepEqual(wire.Header, expectedHeaders[step]) {
							t.Errorf("step=%d: Fast changed outbound headers: got %v, want %v", step, wire.Header, expectedHeaders[step])
						}
						if step == 2 || original != nil {
							if !bytes.Equal(body, expectedBodies[step]) {
								t.Errorf("step=%d: body differs from Fast off:\ngot  %s\nwant %s", step, body, expectedBodies[step])
							}
						} else {
							var expected map[string]any
							if err := json.Unmarshal(expectedBodies[step], &expected); err != nil {
								t.Fatalf("decode Fast-off body: %v", err)
							}
							delete(payload, "service_tier")
							if !reflect.DeepEqual(payload, expected) {
								t.Errorf("step=%d: Fast changed body fields other than service_tier", step)
							}
						}
					}
				}
			})
		}
	}
}

func serviceTierOutboundRequest(t *testing.T, ra *relayAttempt) (*http.Request, []byte) {
	t.Helper()
	if err := ra.applyTransformOptionsWithInboundSetter(false); err != nil {
		t.Fatalf("apply transform options: %v", err)
	}
	wire, err := outbound.Get(ra.channel.Type).TransformRequest(
		ra.context(), ra.internalRequest, "https://upstream.example", ra.usedKey.ChannelKey)
	if err != nil {
		t.Fatalf("transform outbound request: %v", err)
	}
	ra.copyHeaders(wire)
	defer wire.Body.Close()
	body, err := io.ReadAll(wire.Body)
	if err != nil {
		t.Fatalf("read outbound body: %v", err)
	}
	return wire, body
}
