package modeltest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// The probe budget used to be 8 tokens, widened to 256 only when the model NAME
// matched a hardcoded thinking-model keyword list. A channel's model name is
// arbitrary (ModelMapping routes vanity names to reasoning upstreams), so the list
// silently missed them: those probes burned all 8 tokens on reasoning and the test
// reported a bogus "stream ended without any content". These tests pin the budget
// to the model-name-independent contract so nobody reintroduces the guess.

func TestProbeSendsFullTokenBudgetForVanityModelName(t *testing.T) {
	ctx := setupModelTestDB(t)

	var seenMaxTokens atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if raw, ok := payload["max_tokens"].(float64); ok {
			seenMaxTokens.Store(int64(raw))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "vanity-name-budget",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// "stealth/ox-alpha" matches none of the old keywords (think/reasoner/glm/qwen/...)
	// yet can map to a reasoning upstream. It must still get the full budget.
	if _, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:     "stealth/ox-alpha",
		ChannelID: channel.ID,
		Endpoint:  "openai_chat",
	}); err != nil {
		t.Fatalf("run model test: %v", err)
	}

	got, _ := seenMaxTokens.Load().(int64)
	if got != defaultProbeMaxTokens {
		t.Fatalf("probe max_tokens = %d, want %d regardless of model name", got, defaultProbeMaxTokens)
	}
}

func TestProbeKeepsClaudeCLIBudgetOnAnthropicEndpoint(t *testing.T) {
	ctx := setupModelTestDB(t)

	var seenMaxTokens atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if raw, ok := payload["max_tokens"].(float64); ok {
			seenMaxTokens.Store(int64(raw))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "anthropic-cli-budget",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	if _, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:     "claude-opus-4-8",
		ChannelID: channel.ID,
		Endpoint:  "anthropic_messages",
	}); err != nil {
		t.Fatalf("run model test: %v", err)
	}

	// A tiny budget is itself a non-claude-cli tell on this wire, so Anthropic keeps
	// mirroring the real CLI's 64000 instead of the generic probe budget.
	got, _ := seenMaxTokens.Load().(int64)
	if got != anthropicProbeMaxTokens {
		t.Fatalf("anthropic probe max_tokens = %d, want %d (claude-cli byte shape)", got, anthropicProbeMaxTokens)
	}
}

// A single-model probe is the shape the web UI actually sends: the dialog fans out
// one request per endpoint × model, so run() almost always receives len(models)==1.
// A previous fix added a 750ms stagger inside run()'s dispatch loop guarded by
// `if index > 0`, which therefore never fired on that path. This test pins the
// single-model path so a future stagger/pacing attempt is measured where the real
// traffic is, not in a loop that only multi-model callers reach.
func TestSingleModelProbeIsNotDelayedByDispatchLoop(t *testing.T) {
	ctx := setupModelTestDB(t)

	var requestCount atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "single-model-dispatch",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:     "gpt-4o-mini",
		ChannelID: channel.ID,
		Endpoint:  "openai_chat",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}

	if response.Summary.Total != 1 {
		t.Fatalf("summary total = %d, want 1 (web UI sends one model per request)", response.Summary.Total)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("upstream request count = %d, want 1", got)
	}
}

// The empty-result messages must describe what was observed (upstream finished but
// produced no text) instead of asserting a cause. The previous wording blamed
// "shared-quota upstream rejecting overlapping probes", which sent debugging down
// the concurrency path while the real cause was the token budget.
func TestEmptyResultErrorDescribesObservationNotGuessedCause(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Terminal event with zero content deltas: a "completed but empty" stream.
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:     "empty-stream-message",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  false,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	response, err := Run(ctx, dbmodel.ModelTestRequest{
		Model:     "gpt-4o-mini",
		ChannelID: channel.ID,
		Endpoint:  "openai_chat",
	})
	if err != nil {
		t.Fatalf("run model test: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(response.Results))
	}

	result := response.Results[0]
	if result.Success {
		t.Fatal("probe with no content must not be reported as success")
	}
	for _, banned := range []string{"shared-quota", "overlapping probes", "try again solo"} {
		if strings.Contains(result.Error, banned) {
			t.Fatalf("error message must not guess a cause, found %q in: %s", banned, result.Error)
		}
	}
	if !strings.Contains(result.Error, "without producing any content") {
		t.Fatalf("error message should state what was observed, got: %s", result.Error)
	}
}
