package modeltest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// fix-1127 返工第 10 条验收：鹈鹕（creative）请求按真实 CLI 默认带思考配置。
// 假上游只截获请求体即回 500——比对的是发往上游的字节，响应无所谓。
func TestCreativeThinkingDefaultsReachUpstream(t *testing.T) {
	ctx := setupModelTestDB(t)

	captureUpstream := func(t *testing.T) (*httptest.Server, func() []byte) {
		t.Helper()
		bodyCh := make(chan []byte, 4)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			select {
			case bodyCh <- body:
			default:
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"capture only"}`))
		}))
		t.Cleanup(server.Close)
		return server, func() []byte {
			select {
			case b := <-bodyCh:
				return b
			default:
				return nil
			}
		}
	}

	run := func(t *testing.T, name string, channelType outbound.OutboundType, endpoint, model string) map[string]any {
		t.Helper()
		upstream, got := captureUpstream(t)
		channel := dbmodel.Channel{
			Name:     "pelican-thinking-" + name,
			Type:     channelType,
			Enabled:  false,
			BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
			Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		}
		if err := op.ChannelCreate(&channel, ctx); err != nil {
			t.Fatalf("create channel: %v", err)
		}
		sender, err := NewRelaySender(channel, model, endpoint)
		if err != nil {
			t.Fatalf("new relay sender: %v", err)
		}
		sender.SetCreativeThinkingDefaults(true)
		_, _ = sender.Ask(ctx, modelverify.Ask{Prompt: "画一只猫"})
		body := got()
		if body == nil {
			t.Fatalf("%s: upstream never saw the request", name)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("%s: captured body not json: %v", name, err)
		}
		return parsed
	}

	t.Run("openai_chat_reasoning_effort_medium", func(t *testing.T) {
		// chat 出站的强度投影按上游模型族分流（chat.go applyReasoningProjection）：
		// 不认识的上游会按设计剥离陌生参数防 400——那是生产正确行为，不是注入缺失。
		// 用已知能力族模型（deepseek-v4 系原样透传归一档位）验证注入确实到达出站体；
		// 真 OpenAI 官方 base 的 o/gpt-5 族同样保留（isOpenAIReasoningChatModel 分支）。
		body := run(t, "chat", outbound.OutboundTypeOpenAIChat, "openai_chat", "deepseek-v4-pro")
		effort, _ := body["reasoning_effort"].(string)
		if effort != "medium" {
			t.Fatalf("openai chat reasoning_effort = %q, want medium (codex CLI 默认)", effort)
		}
	})

	t.Run("openai_responses_reasoning_effort_medium", func(t *testing.T) {
		body := run(t, "responses", outbound.OutboundTypeOpenAIResponse, "openai_responses", "gpt-5-codex")
		reasoning, _ := body["reasoning"].(map[string]any)
		effort, _ := reasoning["effort"].(string)
		if effort != "medium" {
			t.Fatalf("responses reasoning.effort = %q, want medium (codex CLI 默认); reasoning=%v", effort, reasoning)
		}
	})

	t.Run("gemini25_thinking_budget_8192", func(t *testing.T) {
		body := run(t, "gemini25", outbound.OutboundTypeGemini, "gemini_generate_content", "gemini-2.5-pro")
		gc, _ := body["generationConfig"].(map[string]any)
		thinking, _ := gc["thinkingConfig"].(map[string]any)
		if thinking == nil {
			t.Fatalf("gemini-2.5 generationConfig.thinkingConfig missing: %v", gc)
		}
		if budget, _ := thinking["thinkingBudget"].(float64); int(budget) != 8192 {
			t.Fatalf("gemini-2.5 thinkingBudget = %v, want 8192 (gemini-cli DEFAULT_THINKING_MODE)", thinking["thinkingBudget"])
		}
		if inc, _ := thinking["includeThoughts"].(bool); !inc {
			t.Fatalf("gemini-2.5 includeThoughts = %v, want true", thinking["includeThoughts"])
		}
	})

	t.Run("gemini3_thinking_level_high", func(t *testing.T) {
		body := run(t, "gemini3", outbound.OutboundTypeGemini, "gemini_generate_content", "gemini-3-pro")
		gc, _ := body["generationConfig"].(map[string]any)
		thinking, _ := gc["thinkingConfig"].(map[string]any)
		if thinking == nil {
			t.Fatalf("gemini-3 generationConfig.thinkingConfig missing: %v", gc)
		}
		if level, _ := thinking["thinkingLevel"].(string); !strings.EqualFold(level, "HIGH") {
			t.Fatalf("gemini-3 thinkingLevel = %v, want HIGH", thinking["thinkingLevel"])
		}
	})

	t.Run("gemini_unknown_generation_not_injected", func(t *testing.T) {
		body := run(t, "gemini-old", outbound.OutboundTypeGemini, "gemini_generate_content", "gemini-1.5-pro")
		gc, _ := body["generationConfig"].(map[string]any)
		if thinking, ok := gc["thinkingConfig"]; ok {
			t.Fatalf("gemini-1.5 不认识代际不应注入思考配置, got %v", thinking)
		}
	})

	t.Run("non_creative_sender_not_injected", func(t *testing.T) {
		upstream, got := captureUpstream(t)
		channel := dbmodel.Channel{
			Name:     "pelican-thinking-off",
			Type:     outbound.OutboundTypeOpenAIChat,
			Enabled:  false,
			BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
			Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		}
		if err := op.ChannelCreate(&channel, ctx); err != nil {
			t.Fatalf("create channel: %v", err)
		}
		sender, err := NewRelaySender(channel, "gpt-x", "openai_chat")
		if err != nil {
			t.Fatalf("new relay sender: %v", err)
		}
		// 不开旗标（其他检测题的路径）：什么思考字段都不能有。
		_, _ = sender.Ask(ctx, modelverify.Ask{Prompt: "画一只猫"})
		body := got()
		if body == nil {
			t.Fatalf("upstream never saw the request")
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("captured body not json: %v", err)
		}
		if _, ok := parsed["reasoning_effort"]; ok {
			t.Fatalf("非 creative 路径不应带 reasoning_effort: %v", parsed["reasoning_effort"])
		}
	})
}
