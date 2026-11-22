package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestRescueExhaustionRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name            string
		upstreamStatus  int
		upstreamHint    string
		passthrough     bool
		seedKeyCooldown bool
		wantStatus      int
		wantHint        string
	}{
		{name: "503 passthrough", upstreamStatus: 503, passthrough: true, wantStatus: 503, wantHint: "1"},
		{name: "503 mapped to 502", upstreamStatus: 503, wantStatus: 502, wantHint: "1"},
		{name: "key cooldown capped", upstreamStatus: 503, passthrough: true, seedKeyCooldown: true, wantStatus: 503, wantHint: "60"},
		{name: "provider hint wins unclamped", upstreamStatus: 503, upstreamHint: "90", passthrough: true, wantStatus: 503, wantHint: "90"},
		{name: "429 remains visible", upstreamStatus: 429, wantStatus: 429, wantHint: "1"},
		{name: "unrelated 5xx unchanged", upstreamStatus: 500, wantStatus: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupRescueDeadlineDB(t)
			withAutoRescueCap(t, 2200*time.Millisecond)
			setRetryAfterTestSetting(t, dbmodel.SettingKeyRelayInterventionEnabled, "true")
			setRetryAfterTestSetting(t, dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "300")
			setRetryAfterTestSetting(t, dbmodel.SettingKeyUpstreamErrorStatusPass, strconv.FormatBool(tc.passthrough))

			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				if tc.upstreamHint != "" {
					w.Header().Set("Retry-After", tc.upstreamHint)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"fake upstream unavailable"}}`))
			}))
			t.Cleanup(upstream.Close)

			channel := newRetryAfterTestChannel(t, upstream.URL, !tc.seedKeyCooldown)
			if tc.seedKeyCooldown {
				for range 6 {
					balancer.RecordRuntimeFailure(channel.ID, channel.Keys[0].ID, "upstream-model", tc.upstreamStatus, time.Second, 0)
				}
			}
			rec, c := newChatGin()
			Handler(inbound.InboundTypeOpenAIChat, c)

			if got := hits.Load(); got < 2 {
				t.Errorf("got %d upstream attempts; expected automatic rescue to retry the single key", got)
			}
			if got := rec.Result().Header.Get("Retry-After"); rec.Code != tc.wantStatus || got != tc.wantHint {
				t.Errorf("status=%d Retry-After=%q body=%s; want status=%d Retry-After=%q",
					rec.Code, got, rec.Body.String(), tc.wantStatus, tc.wantHint)
			}
		})
	}
}

func TestRelayShapeRejectionLeavesChannelHealthUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rejectBody   string
		inboundBody  string
		wantForwards int32
	}{
		{name: "empty inbound messages", inboundBody: `{"model":"request-model","messages":[]}`},
		{name: "upstream invalid request", rejectBody: `{"error":{"type":"invalid_request_error","message":"Invalid schema for text.format"}}`, wantForwards: 1},
		{name: "upstream context window", rejectBody: `{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`, wantForwards: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupRescueDeadlineDB(t)
			setRetryAfterTestSetting(t, dbmodel.SettingKeyCircuitBreakerThreshold, "1")
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.rejectBody))
			}))
			t.Cleanup(upstream.Close)

			channel := newRetryAfterTestChannel(t, upstream.URL, false)
			rec, c := newChatGin()
			if tc.inboundBody != "" {
				c.Request = httptest.NewRequest(http.MethodPost, c.Request.URL.Path, strings.NewReader(tc.inboundBody))
				c.Request.Header.Set("Content-Type", "application/json")
			}
			Handler(inbound.InboundTypeOpenAIChat, c)

			if got := rec.Result().Header.Get("Retry-After"); rec.Code != http.StatusBadRequest || got != "" {
				t.Errorf("status=%d Retry-After=%q body=%s; want 400 without Retry-After", rec.Code, got, rec.Body.String())
			}
			if got := hits.Load(); got != tc.wantForwards {
				t.Errorf("got %d forwards, want %d", got, tc.wantForwards)
			}
			if got := balancer.SnapshotKeyRuntime(channel.ID, channel.Keys[0].ID, "upstream-model"); got.RequestFailed != 0 || got.CooldownRemainingMs != 0 {
				t.Errorf("shape rejection changed key health: %+v", got)
			}
			if got := balancer.ListChannelCircuits(channel.ID); len(got) != 0 {
				t.Errorf("shape rejection tripped a breaker: %+v", got)
			}
		})
	}
}

func newRetryAfterTestChannel(t *testing.T, upstreamURL string, disableBreaker bool) dbmodel.Channel {
	t.Helper()
	channel := dbmodel.Channel{
		Name:                  "retry-after-test-channel",
		Type:                  outbound.OutboundTypeOpenAIChat,
		Enabled:               true,
		DisableCircuitBreaker: disableBreaker,
		BaseUrls:              []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:                  []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}},
		Model:                 "upstream-model",
		ModelMapping:          map[string]string{"request-model": "upstream-model"},
		Priority:              1,
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create test channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)
	t.Cleanup(func() { balancer.ResetChannel(channel.ID) })
	return channel
}

func setRetryAfterTestSetting(t *testing.T, key dbmodel.SettingKey, value string) {
	t.Helper()
	if err := op.SettingSetString(key, value); err != nil {
		t.Fatalf("set %s to %s: %v", key, value, err)
	}
}
