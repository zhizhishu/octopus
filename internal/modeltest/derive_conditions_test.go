package modeltest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// fix-1127 第 3 项验收（Hark）：配置推算出的条件必须与真实出站一致。
// 同一条渠道、同一道题：先推算一次，再真发一次（假上游截获请求体），
// 用同一套提取逻辑从截获体里提取条件，两者逐字段相等。
func TestDeriveRelayConditionsMatchesActualOutbound(t *testing.T) {
	ctx := setupModelTestDB(t)

	type capture struct {
		body []byte
	}
	capCh := make(chan capture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case capCh <- capture{body: body}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-derive",
			"object":"chat.completion",
			"created":123,
			"model":"gpt-derive",
			"choices":[{"index":0,"message":{"role":"assistant","content":"7"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}
		}`))
	}))
	t.Cleanup(upstream.Close)

	override := `{"temperature":0.3,"top_p":0.9}`
	channel := dbmodel.Channel{
		Name:          "derive-conditions-channel",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       false,
		ParamOverride: &override,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	derived := DeriveRelayConditions(channel, "gpt-derive", "openai_chat")
	if derived.Completeness == "unknown" {
		t.Fatalf("derivation must not be wholly unknown for a healthy openai chat channel: %+v", derived)
	}

	sender, err := NewRelaySender(channel, "gpt-derive", "openai_chat")
	if err != nil {
		t.Fatalf("new relay sender: %v", err)
	}
	if _, err := sender.Ask(ctx, modelverify.Ask{Prompt: numberdist.Prompt}); err != nil {
		t.Fatalf("relay ask: %v", err)
	}
	var got capture
	select {
	case got = <-capCh:
	default:
		t.Fatalf("upstream never saw the sample request")
	}

	// 用与推算同一套提取逻辑，从真实截获的请求体提取条件。
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, upstream.URL+"/v1/chat/completions", bytes.NewReader(got.body))
	if err != nil {
		t.Fatalf("rebuild captured request: %v", err)
	}
	actual := buildOutboundConditions(probeProtocolOf(channel.Type), req)

	if derived.TemperatureState != actual.TemperatureState {
		t.Fatalf("temperature state: derived=%q actual=%q", derived.TemperatureState, actual.TemperatureState)
	}
	if (derived.Temperature == nil) != (actual.Temperature == nil) ||
		(derived.Temperature != nil && actual.Temperature != nil && *derived.Temperature != *actual.Temperature) {
		t.Fatalf("temperature value mismatch: derived=%v actual=%v", derived.Temperature, actual.Temperature)
	}
	if derived.TopPState != actual.TopPState {
		t.Fatalf("top_p state: derived=%q actual=%q", derived.TopPState, actual.TopPState)
	}
	if (derived.Stream == nil) != (actual.Stream == nil) ||
		(derived.Stream != nil && actual.Stream != nil && *derived.Stream != *actual.Stream) {
		t.Fatalf("stream mismatch: derived=%v actual=%v", derived.Stream, actual.Stream)
	}
	if derived.PromptSHA256 != actual.PromptSHA256 {
		t.Fatalf("prompt sha mismatch: derived=%q actual=%q", derived.PromptSHA256, actual.PromptSHA256)
	}
	if derived.PromptSHA256 == "" {
		t.Fatalf("prompt sha must be derivable (the prompt template is fixed)")
	}
	if (derived.SystemInjected == nil) != (actual.SystemInjected == nil) {
		t.Fatalf("system injected presence mismatch: derived=%v actual=%v", derived.SystemInjected, actual.SystemInjected)
	}
	// param override 的 temperature/top_p 必须真的推算出来（不是 unknown）。
	if derived.TemperatureState == numberdist.StateUnknown || derived.Temperature == nil || *derived.Temperature != 0.3 {
		t.Fatalf("param-override temperature must be derived as 0.3, got state=%q val=%v", derived.TemperatureState, derived.Temperature)
	}
	if derived.ConfigHash == "" {
		t.Fatalf("config hash must be present")
	}
}

// 配置快照 hash：形状决定性配置一变，hash 就变；无关字段（名字）不变 hash。
func TestHlwyChannelConfigHashSensitivity(t *testing.T) {
	base := dbmodel.Channel{
		Name: "hash-channel",
		Type: outbound.OutboundTypeOpenAIChat,
	}
	h1 := HlwyChannelConfigHash(base)
	if h1 == "" {
		t.Fatalf("hash must be non-empty")
	}
	renamed := base
	renamed.Name = "hash-channel-renamed"
	if HlwyChannelConfigHash(renamed) != h1 {
		t.Fatalf("cosmetic rename must not change the shape hash")
	}
	cloaked := base
	cloaked.Cloak.Shape = "claude"
	if HlwyChannelConfigHash(cloaked) == h1 {
		t.Fatalf("cloak shape change must change the hash")
	}
	override := `{"temperature":0.7}`
	overridden := base
	overridden.ParamOverride = &override
	if HlwyChannelConfigHash(overridden) == h1 {
		t.Fatalf("param override must change the hash")
	}
}


// fix-1127 返工第 2 条验收：一致性扩展到 Anthropic / OpenAI Responses / Gemini 三家。
// 假上游只截获请求体就回 500——比对的是「发往上游的字节」，响应内容无关紧要；
// 这样流式强制、错误重试等链路细节都不影响截获的完整性。
func TestDeriveRelayConditionsMatchesActualOutboundMultiProtocol(t *testing.T) {
	ctx := setupModelTestDB(t)

	cases := []struct {
		name        string
		channelType outbound.OutboundType
		endpoint    string
		cloak       dbmodel.ChannelCloak
	}{
		{"anthropic", outbound.OutboundTypeAnthropic, "anthropic_messages", dbmodel.ChannelCloak{}},
		{"openai_responses", outbound.OutboundTypeOpenAIResponse, "openai_responses", dbmodel.ChannelCloak{}},
		{"gemini", outbound.OutboundTypeGemini, "gemini_generate_content", dbmodel.ChannelCloak{}},
		// 带 codex 形状的 Responses 渠道：生产出站走 prepareCodexRequestFingerprint/
		// prepareCodexRequestShape 一整套（include/store=false/默认 effort/agent context），
		// 推算必须与它逐键一致——这正是第 2 轮第 1 条把编排收敛成唯一入口要保的东西。
		{"codex_shaped_responses", outbound.OutboundTypeOpenAIResponse, "openai_responses", dbmodel.ChannelCloak{Mode: "always", Shape: "codex"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			type capture struct {
				url  string
				body []byte
			}
			capCh := make(chan capture, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				select {
				case capCh <- capture{url: r.URL.String(), body: body}:
				default:
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"capture only"}`))
			}))
			t.Cleanup(upstream.Close)

			channel := dbmodel.Channel{
				Name:     "derive-multi-" + tc.name,
				Type:     tc.channelType,
				Enabled:  false,
				Cloak:    tc.cloak,
				BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
				Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
			}
			if err := op.ChannelCreate(&channel, ctx); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			derived := DeriveRelayConditions(channel, "test-model", tc.endpoint)

			sender, err := NewRelaySender(channel, "test-model", tc.endpoint)
			if err != nil {
				t.Fatalf("new relay sender: %v", err)
			}
			// 上游恒 500：发送必然失败，但请求体已被截获，这正是要对比的东西。
			_, _ = sender.Ask(ctx, modelverify.Ask{Prompt: numberdist.Prompt})

			// 截获必须带真实 URL：gemini 的流式体现在 URL 动作
			// （:generateContent vs :streamGenerateContent），只留 body 会丢掉它。
			var got capture
			select {
			case got = <-capCh:
			default:
				t.Fatalf("upstream never saw the sample request")
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, upstream.URL+got.url, bytes.NewReader(got.body))
			if err != nil {
				t.Fatalf("rebuild captured request: %v", err)
			}
			actual := buildOutboundConditions(probeProtocolOf(channel.Type), req)

			if derived.TemperatureState != actual.TemperatureState {
				t.Fatalf("%s temperature state: derived=%q actual=%q", tc.name, derived.TemperatureState, actual.TemperatureState)
			}
			if (derived.Temperature == nil) != (actual.Temperature == nil) ||
				(derived.Temperature != nil && actual.Temperature != nil && *derived.Temperature != *actual.Temperature) {
				t.Fatalf("%s temperature mismatch: derived=%v actual=%v", tc.name, derived.Temperature, actual.Temperature)
			}
			if derived.TopPState != actual.TopPState {
				t.Fatalf("%s top_p state: derived=%q actual=%q", tc.name, derived.TopPState, actual.TopPState)
			}
			if (derived.Stream == nil) != (actual.Stream == nil) ||
				(derived.Stream != nil && actual.Stream != nil && *derived.Stream != *actual.Stream) {
				t.Fatalf("%s stream mismatch: derived=%v actual=%v", tc.name, derived.Stream, actual.Stream)
			}
			if derived.PromptSHA256 != actual.PromptSHA256 {
				t.Fatalf("%s prompt sha mismatch: derived=%q actual=%q", tc.name, derived.PromptSHA256, actual.PromptSHA256)
			}
			if derived.ConfigHash == "" {
				t.Fatalf("%s config hash must be present", tc.name)
			}

			// 第 2 轮第 1 条验收：推算的最终出站体与真实截获体逐键一致——
			// 覆盖 system 注入、tools 注入、thinking/effort 字段等所有形状维度
			// （键集合递归比对，任何一边多一个/少一个键都判失败；随机值如
			// metadata.user_id 只比键不比值）。
			derivedReq, ok := deriveOutboundRequest(channel, "test-model", tc.endpoint)
			if !ok {
				t.Fatalf("%s deriveOutboundRequest failed", tc.name)
			}
			derivedBody, err := io.ReadAll(derivedReq.Body)
			if err != nil {
				t.Fatalf("%s read derived body: %v", tc.name, err)
			}
			var derivedJSON, actualJSON map[string]any
			if err := json.Unmarshal(derivedBody, &derivedJSON); err != nil {
				t.Fatalf("%s derived body not json: %v", tc.name, err)
			}
			if err := json.Unmarshal(got.body, &actualJSON); err != nil {
				t.Fatalf("%s captured body not json: %v", tc.name, err)
			}
			dKeys := jsonKeySet(derivedJSON, "")
			aKeys := jsonKeySet(actualJSON, "")
			if strings.Join(dKeys, ",") != strings.Join(aKeys, ",") {
				t.Fatalf("%s shape key mismatch:\nderived-only/extra-in-derived vs actual\nderived: %v\nactual: %v", tc.name, dKeys, aKeys)
			}
			// 形状关键字段的值必须一致（不止键）：system 注入内容、thinking/effort、
			// temperature、store 等确定性值。随机值路径（metadata.user_id）除外。
			for _, path := range shapeValuePaths(derivedJSON) {
				if jsonValueAt(derivedJSON, path) != jsonValueAt(actualJSON, path) {
					t.Fatalf("%s shape value mismatch at %q: derived=%q actual=%q", tc.name, path, jsonValueAt(derivedJSON, path), jsonValueAt(actualJSON, path))
				}
			}
		})
	}
}

// jsonKeySet 递归列出 JSON 对象的所有键路径（数组按位置展开），排序后返回。
// 只比键不比值——随机化值（user_id 哈希等）不影响形状判定。
func jsonKeySet(v any, prefix string) []string {
	var out []string
	switch node := v.(type) {
	case map[string]any:
		for k, sub := range node {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out = append(out, p)
			out = append(out, jsonKeySet(sub, p)...)
		}
	case []any:
		for i, sub := range node {
			p := prefix + "[]"
			if i > 0 {
				// 同构数组（messages/tools）元素键集相同，只展开第一个，
				// 避免消息条数差异造成假阳性——条数由模板固定，两边本就相同。
				continue
			}
			out = append(out, jsonKeySet(sub, p)...)
		}
	}
	sort.Strings(out)
	return out
}

// shapeValuePaths 返回对象中所有「确定性形状值」的叶子路径：跳过明显的随机/
// 会话相关字段（user_id、prompt_cache_key、session id），其余叶子全部纳入。
func shapeValuePaths(m map[string]any) []string {
	var out []string
	var walk func(v any, prefix string)
	walk = func(v any, prefix string) {
		switch node := v.(type) {
		case map[string]any:
			for k, sub := range node {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(sub, p)
			}
		case []any:
			if len(node) > 0 {
				walk(node[0], prefix+"[]")
			}
		default:
			lower := strings.ReplaceAll(strings.ToLower(prefix), "-", "_")
			if strings.Contains(lower, "user_id") || strings.Contains(lower, "prompt_cache_key") || strings.Contains(lower, "session_id") ||
				strings.Contains(lower, "turn_metadata") || strings.Contains(lower, "turn_id") || strings.Contains(lower, "turn_started_at") {
				// codex 指纹里的 turn_id / 时间戳 / installation 元数据逐请求随机，
				// 键集合仍参与比对，值不比。
				return
			}
			out = append(out, prefix)
		}
	}
	walk(m, "")
	sort.Strings(out)
	return out
}

func jsonValueAt(m map[string]any, path string) string {
	cur := any(m)
	for _, part := range strings.Split(path, ".") {
		arr := strings.HasSuffix(part, "[]")
		name := strings.TrimSuffix(part, "[]")
		asMap, ok := cur.(map[string]any)
		if !ok {
			return "<missing>"
		}
		cur, ok = asMap[name]
		if !ok {
			return "<missing>"
		}
		if arr {
			list, ok := cur.([]any)
			if !ok || len(list) == 0 {
				return "<missing>"
			}
			cur = list[0]
		}
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(raw)
}
