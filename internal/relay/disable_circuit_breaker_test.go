package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// TestHandlerDisableCircuitBreakerBypassesShortCircuit proves the per-channel
// "no circuit breaker" switch makes a channel forward EVERY request to the upstream
// like a direct client. Both variants run the same two-request sequence against an
// upstream that always fails (500), with the trip threshold lowered to 1:
//
//   - default (flag=false): request 1 forwards and its failure trips the breaker, so
//     request 2 is short-circuited locally (503 octopus_channel_circuit_open) WITHOUT
//     touching the upstream — the existing behaviour, unchanged.
//   - flag=true: request 1's failure records no breaker state, so request 2 is forwarded
//     to the upstream again instead of being short-circuited.
//
// The discriminator is the upstream hit count (1 vs 2) plus whether request 2 comes back
// as a synthetic circuit-open rejection.
func TestHandlerDisableCircuitBreakerBypassesShortCircuit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	run := func(t *testing.T, disableCircuitBreaker bool) (upstreamHits int, secondCode int, secondBody string) {
		ctx := setupRelayCircuitBypassDB(t)
		// Lower the trip threshold so a single upstream failure opens the breaker on the
		// default path — otherwise it would take getThreshold()=10 failing requests.
		if err := op.SettingSetString(dbmodel.SettingKeyCircuitBreakerThreshold, "1"); err != nil {
			t.Fatalf("set circuit threshold: %v", err)
		}

		var mu sync.Mutex
		hits := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
		}))
		t.Cleanup(upstream.Close)

		channel := dbmodel.Channel{
			Name:                  "circuit-bypass-channel",
			Type:                  outbound.OutboundTypeOpenAIChat,
			Enabled:               true,
			DisableCircuitBreaker: disableCircuitBreaker,
			BaseUrls:              []dbmodel.BaseUrl{{URL: upstream.URL}},
			Keys:                  []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
			Model:                 "upstream-model",
			ModelMapping:          map[string]string{"request-model": "upstream-model"},
			Priority:              1,
		}
		if err := op.ChannelCreate(&channel, ctx); err != nil {
			t.Fatalf("create channel: %v", err)
		}
		// The breaker store is a package global keyed by channel ID; clear any leftover
		// state (e.g. a channel ID reused across tests) so this channel starts closed.
		balancer.ResetChannel(channel.ID)

		doRequest := func() (int, string) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
				"model":"request-model",
				"messages":[{"role":"user","content":"ping"}]
			}`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")
			Handler(inbound.InboundTypeOpenAIChat, c)
			return rec.Code, rec.Body.String()
		}

		// Request 1: forwards to the upstream and gets a 500.
		doRequest()
		// Request 2: the discriminating one.
		code, body := doRequest()

		mu.Lock()
		total := hits
		mu.Unlock()
		return total, code, body
	}

	t.Run("default trips breaker and short-circuits request 2", func(t *testing.T) {
		hits, code, body := run(t, false)
		if hits != 1 {
			t.Fatalf("default: expected upstream hit ONLY on request 1 (breaker short-circuits request 2), got %d hits", hits)
		}
		if code != http.StatusServiceUnavailable {
			t.Fatalf("default: expected request 2 short-circuited with 503, got %d body %s", code, body)
		}
		if !strings.Contains(body, "octopus_channel_circuit_open") {
			t.Fatalf("default: expected circuit-open error code on request 2, got %s", body)
		}
	})

	t.Run("disable_circuit_breaker forwards every request", func(t *testing.T) {
		hits, _, body := run(t, true)
		if hits != 2 {
			t.Fatalf("disable_circuit_breaker: expected BOTH requests forwarded to upstream, got %d hits", hits)
		}
		if strings.Contains(body, "octopus_channel_circuit_open") {
			t.Fatalf("disable_circuit_breaker: request 2 must NOT be short-circuited as circuit_open, got %s", body)
		}
	})
}

func setupRelayCircuitBypassDB(t *testing.T) context.Context {
	t.Helper()

	balancer.ResetRuntimeTelemetry()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	return context.Background()
}
