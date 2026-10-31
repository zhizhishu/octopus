package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

const affinityAnthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_aff","type":"message","role":"assistant","model":"claude-aff-up","content":[],"usage":{"input_tokens":5,"cache_read_input_tokens":4000,"cache_creation_input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// setupAffinityChannels: two same-tier Anthropic channels behind 轮询 with NO session
// keep time configured anywhere (the shape every access-plan route has).
func setupAffinityChannels(t *testing.T) (hits [2]*int64) {
	t.Helper()
	ctx := setupRelayKeyRetryDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatalf("set spread routing: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeySessionKeepTimeDefault, "0"); err != nil {
		t.Fatalf("reset session keep time default: %v", err)
	}
	for i := range hits {
		var n int64
		hits[i] = &n
		counter := hits[i]
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(counter, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(affinityAnthropicSSE))
		}))
		t.Cleanup(srv.Close)
		ch := dbmodel.Channel{
			Name:         fmt.Sprintf("affinity-%d", i),
			Type:         outbound.OutboundTypeAnthropic,
			Enabled:      true,
			Model:        "claude-aff-up",
			ModelMapping: map[string]string{"claude-aff": "claude-aff-up"},
			Priority:     1,
			BaseUrls:     []dbmodel.BaseUrl{{URL: srv.URL}},
			Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: fmt.Sprintf("aff-key-%d", i)}},
		}
		if err := op.ChannelCreate(&ch, ctx); err != nil {
			t.Fatalf("create channel: %v", err)
		}
	}
	return hits
}

// claudeCodeTurn sends turn N of one Claude Code conversation: the history grows, the
// metadata.user_id session stays the same (exactly what the real CLI does).
func claudeCodeTurn(t *testing.T, sessionID string, turn int) {
	t.Helper()
	var msgs []string
	for i := 0; i <= turn; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":"question %d"}`, i))
		if i < turn {
			msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":"answer %d"}`, i))
		}
	}
	userID := fmt.Sprintf(`{\"device_id\":\"%s\",\"account_uuid\":\"\",\"session_id\":\"%s\"}`, strings.Repeat("a", 64), sessionID)
	body := fmt.Sprintf(`{"model":"claude-aff","max_tokens":16,"stream":true,"metadata":{"user_id":"%s"},"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"messages":[%s]}`, userID, strings.Join(msgs, ","))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeAnthropic, c)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("turn %d failed: %d %s", turn, rec.Code, rec.Body.String())
	}
}

// Every turn of one Claude Code session must reach the same upstream under 轮询 even
// though no keep time is configured — that is what keeps cache_read hitting.
func TestClaudeCodeSessionStaysOnOneChannelUnderRoundRobin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hits := setupAffinityChannels(t)

	for turn := 0; turn < 8; turn++ {
		claudeCodeTurn(t, "11111111-2222-4333-8444-555555555555", turn)
	}
	a, b := atomic.LoadInt64(hits[0]), atomic.LoadInt64(hits[1])
	if a+b != 8 || (a != 0 && b != 0) {
		t.Fatalf("one session was split across channels: %d / %d", a, b)
	}
}

// Control: with the escape hatch off the same traffic rotates again — proving the test
// above measures the affinity layer, not a lucky ordering.
func TestClaudeCodeSessionRotatesWhenAffinityDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hits := setupAffinityChannels(t)
	if err := op.SettingSetString(dbmodel.SettingKeySessionAffinityEnabled, "false"); err != nil {
		t.Fatalf("disable affinity: %v", err)
	}
	t.Cleanup(func() { _ = op.SettingSetString(dbmodel.SettingKeySessionAffinityEnabled, "true") })

	for turn := 0; turn < 8; turn++ {
		claudeCodeTurn(t, "99999999-2222-4333-8444-555555555555", turn)
	}
	a, b := atomic.LoadInt64(hits[0]), atomic.LoadInt64(hits[1])
	if a == 0 || b == 0 {
		t.Fatalf("expected legacy rotation across both channels, got %d / %d", a, b)
	}
}
