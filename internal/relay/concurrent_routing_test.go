package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestConcurrentResponsesStreamingRoundRobinCompletesAllTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayKeyRetryDB(t)

	var leftCount int64
	var rightCount int64
	left := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&leftCount, 1)
		writeCodexShapeResponsesSSE(w, "upstream-concurrent")
	}))
	t.Cleanup(left.Close)
	right := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&rightCount, 1)
		writeCodexShapeResponsesSSE(w, "upstream-concurrent")
	}))
	t.Cleanup(right.Close)

	leftChannel := dbmodel.Channel{
		Name:    "concurrent-left",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: left.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "left-key"}},
	}
	if err := op.ChannelCreate(&leftChannel, ctx); err != nil {
		t.Fatalf("create left channel: %v", err)
	}
	rightChannel := dbmodel.Channel{
		Name:    "concurrent-right",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: right.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "right-key"}},
	}
	if err := op.ChannelCreate(&rightChannel, ctx); err != nil {
		t.Fatalf("create right channel: %v", err)
	}
	group := dbmodel.Group{Name: "gpt-concurrent-stream", Mode: dbmodel.GroupModeRoundRobin}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	for _, channelID := range []int{leftChannel.ID, rightChannel.ID} {
		if err := op.GroupItemAdd(&dbmodel.GroupItem{
			GroupID:   group.ID,
			ChannelID: channelID,
			ModelName: "upstream-concurrent",
			Priority:  1,
			Weight:    1,
		}, ctx); err != nil {
			t.Fatalf("create group item: %v", err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
				"model":"gpt-concurrent-stream",
				"input":"Say OK only",
				"stream":true
			}`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			Handler(inbound.InboundTypeOpenAIResponse, c)

			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Errorf("expected HTTP 200, got %d body %s", rec.Code, body)
				return
			}
			if !strings.Contains(body, `"response.completed"`) || !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, "OK") {
				t.Errorf("expected complete streaming OK response, got %s", body)
			}
		}()
	}
	wg.Wait()

	leftN := atomic.LoadInt64(&leftCount)
	rightN := atomic.LoadInt64(&rightCount)
	if leftN+rightN != 12 {
		t.Fatalf("expected all 12 turns to complete across the two channels, got left=%d right=%d", leftN, rightN)
	}
	// Capacity-aware spread intentionally does not guarantee an exact 6/6 split:
	// the selection reservation nudges concurrent bursts away from a just-picked
	// channel based on live in-flight load, so a small imbalance is expected and
	// healthier than mechanical round-robin. Assert both channels carried a fair
	// share rather than an exact count.
	if leftN < 3 || rightN < 3 {
		t.Fatalf("expected spread to share load across both channels, got left=%d right=%d", leftN, rightN)
	}
}

func TestConcurrentResponsesClientSessionsDoNotCrossStickyChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayKeyRetryDB(t)

	left := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCodexShapeResponsesSSEWithText(w, "resp-session-left", "upstream-session-isolation", "LEFT")
	}))
	t.Cleanup(left.Close)
	right := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCodexShapeResponsesSSEWithText(w, "resp-session-right", "upstream-session-isolation", "RIGHT")
	}))
	t.Cleanup(right.Close)

	leftChannel := dbmodel.Channel{
		Name:    "session-left",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: left.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "left-key"}},
	}
	if err := op.ChannelCreate(&leftChannel, ctx); err != nil {
		t.Fatalf("create left channel: %v", err)
	}
	rightChannel := dbmodel.Channel{
		Name:    "session-right",
		Type:    outbound.OutboundTypeOpenAIResponse,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: right.URL,
		}},
		Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "right-key"}},
	}
	if err := op.ChannelCreate(&rightChannel, ctx); err != nil {
		t.Fatalf("create right channel: %v", err)
	}
	group := dbmodel.Group{
		Name:            "gpt-session-isolation",
		Mode:            dbmodel.GroupModeRoundRobin,
		SessionKeepTime: 300,
	}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	for _, channelID := range []int{leftChannel.ID, rightChannel.ID} {
		if err := op.GroupItemAdd(&dbmodel.GroupItem{
			GroupID:   group.ID,
			ChannelID: channelID,
			ModelName: "upstream-session-isolation",
			Priority:  1,
			Weight:    1,
		}, ctx); err != nil {
			t.Fatalf("create group item: %v", err)
		}
	}

	call := func(sessionID string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
			"model":"gpt-session-isolation",
			"input":"Say the marker only",
			"stream":true
		}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session_id", sessionID)
		c.Request = req
		c.Set("api_key_id", 0)
		c.Set("user_id", 0)
		c.Set("request_ip", "127.0.0.1")

		Handler(inbound.InboundTypeOpenAIResponse, c)

		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200 for %s, got %d body %s", sessionID, rec.Code, body)
		}
		switch {
		case strings.Contains(body, "LEFT"):
			return "LEFT"
		case strings.Contains(body, "RIGHT"):
			return "RIGHT"
		default:
			t.Fatalf("missing upstream marker for %s: %s", sessionID, body)
			return ""
		}
	}

	sessionA := "codex-session-A"
	sessionB := "codex-session-B"
	markerA := call(sessionA)
	markerB := call(sessionB)
	if markerA == markerB {
		t.Fatalf("setup expected round-robin to seed different sticky channels, got A=%s B=%s", markerA, markerB)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		for _, item := range []struct {
			session string
			want    string
		}{
			{session: sessionA, want: markerA},
			{session: sessionB, want: markerB},
		} {
			item := item
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := call(item.session); got != item.want {
					t.Errorf("session %s crossed sticky channel: got %s want %s", item.session, got, item.want)
				}
			}()
		}
	}
	wg.Wait()
}
