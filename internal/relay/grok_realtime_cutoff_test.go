package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/server/middleware"
)

// 用完断开：/v1/realtime 准入先查、中途分段扣、不够下一份就发 error 事件并 1008 关两边。
// 全部用假上游，不碰真 key。

const rtCutModel = "grok-voice-latest"

// 每次 response.done：input 10、output 10 token；Input=Output=100000/百万 → 每次 0.001 + 0.001 = 0.002。
const (
	rtCutUsageEvent  = `{"type":"response.done","response":{"usage":{"total_tokens":20,"input_tokens":10,"output_tokens":10}}}`
	rtCutPerResponse = 0.002
)

func shrinkRealtimeBillingClock(t *testing.T, interval, unit time.Duration) {
	t.Helper()
	oldInterval, oldUnit := grokRealtimeBillingInterval, grokRealtimeMinUnit
	grokRealtimeBillingInterval, grokRealtimeMinUnit = interval, unit
	t.Cleanup(func() { grokRealtimeBillingInterval, grokRealtimeMinUnit = oldInterval, oldUnit })
}

func createRealtimeUser(t *testing.T, ctx context.Context, name string, role dbmodel.UserRole, balance float64) dbmodel.User {
	t.Helper()
	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: name,
		Password: "test-password-123",
		Role:     role,
		Status:   dbmodel.UserStatusActive,
		Balance:  balance,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

func userBalance(t *testing.T, id int) float64 {
	t.Helper()
	u, err := op.UserGet(id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	return u.Balance
}

// newRealtimeGatewayFor 与 newRealtimeGateway 相同，只是带上 api key id（测 key 花费上限）。
func newRealtimeGatewayFor(t *testing.T, userID, apiKeyID int) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/realtime", func(c *gin.Context) {
		c.Set("api_key_id", apiKeyID)
		c.Set("user_id", userID)
		c.Set("request_ip", "127.0.0.1")
		GrokRealtimeHandler(c)
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv
}

// tokenUpstream：每收到一条 response.create 回一条带 usage 的 response.done。
func tokenUpstream(t *testing.T) *fakeRealtimeUpstream {
	return newFakeRealtimeUpstream(t, 0, nil, func(conn *websocket.Conn, mt int, data []byte) {
		if mt == websocket.TextMessage && strings.Contains(string(data), `"response.create"`) {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(rtCutUsageEvent))
		}
	})
}

// audioUpstream：收到任何东西就回一块音频 delta（触发按分钟计费）。
func audioUpstream(t *testing.T) *fakeRealtimeUpstream {
	return newFakeRealtimeUpstream(t, 0, nil, func(conn *websocket.Conn, mt int, data []byte) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_audio.delta","delta":"UklGRg=="}`))
	})
}

func readRealtimeEvent(t *testing.T, conn *websocket.Conn, timeout time.Duration) (map[string]any, error) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("bad json %s", data)
	}
	return ev, nil
}

func assertCutoffEventAndClose(t *testing.T, client *websocket.Conn, wantCode, wantReason string) {
	t.Helper()
	var ev map[string]any
	for {
		var err error
		ev, err = readRealtimeEvent(t, client, 3*time.Second)
		if err != nil {
			t.Fatalf("expected error event before close, got %v", err)
		}
		if ev["type"] == "error" {
			break
		}
	}
	errObj, _ := ev["error"].(map[string]any)
	if errObj["code"] != wantCode {
		t.Fatalf("error code %v, want %s (event %v)", errObj["code"], wantCode, ev)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "realtime session was ended") {
		t.Fatalf("error message should say plainly why the session ended: %q", msg)
	}
	if id, _ := ev["event_id"].(string); id == "" {
		t.Fatalf("error event needs an event_id: %v", ev)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := client.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.ClosePolicyViolation || ce.Text != wantReason {
		t.Fatalf("client should get close 1008 %q, got %v", wantReason, err)
	}
}

func waitUpstreamClosed(t *testing.T, f *fakeRealtimeUpstream, wantCode int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		err := f.readErr
		f.mu.Unlock()
		if err != nil {
			var ce *websocket.CloseError
			if wantCode != 0 && (!errors.As(err, &ce) || ce.Code != wantCode) {
				t.Fatalf("upstream close %v, want code %d", err, wantCode)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("upstream connection was not closed")
}

func TestGrokRealtimeRejectsZeroBalanceBeforeConnect(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	dialed := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, rtCutModel, rtCutModel, "k")
	price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{PerCall: 0.08})
	t.Cleanup(func() { price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{}) })

	for _, tc := range []struct {
		name    string
		balance float64
	}{
		{"zero balance", 0},
		// 0.08/分钟时 1 秒音频约 0.00133：不够第一份。
		{"less than the first unit", 0.001},
	} {
		user := createRealtimeUser(t, ctx, "rt-broke-"+strings.ReplaceAll(tc.name, " ", "-"), dbmodel.UserRoleUser, tc.balance)
		gw := newRealtimeGatewayFor(t, user.ID, 0)
		conn, resp, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("%s: handshake must be refused", tc.name)
		}
		if resp == nil || resp.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("%s: want HTTP 402, got resp=%v err=%v", tc.name, resp, err)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), grokRealtimeCutoffCodeBalance) {
			t.Fatalf("%s: body %s", tc.name, body)
		}
	}
	if dialed != 0 {
		t.Fatalf("upstream must not be dialed when the balance cannot pay, dialed=%d", dialed)
	}
}

func TestGrokRealtimeCutsOffMidSessionWhenTokenFundsRunOut(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	shrinkRealtimeBillingClock(t, time.Hour, time.Second)
	upstream := tokenUpstream(t)
	createRawProtocolGroup(t, ctx, upstream.srv.URL, rtCutModel, rtCutModel, "k")
	price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{Input: 100, Output: 100})
	t.Cleanup(func() { price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{}) })

	// 够 2.5 次回复：第 1 次后剩 1.5 份（够下一份），第 2 次后剩 0.5 份（不够）→ 断。
	start := 2.5 * rtCutPerResponse
	user := createRealtimeUser(t, ctx, "rt-token-cut", dbmodel.UserRoleUser, start)
	gw := newRealtimeGatewayFor(t, user.ID, 0)
	client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	for i := 1; i <= 2; i++ {
		if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
			t.Fatal(err)
		}
		ev, err := readRealtimeEvent(t, client, 3*time.Second)
		if err != nil || ev["type"] != "response.done" {
			t.Fatalf("response %d: %v %v", i, ev, err)
		}
		if i == 1 {
			// 中途就扣了，不等会话结束。
			waitBalance(t, user.ID, start-rtCutPerResponse)
		}
	}
	assertCutoffEventAndClose(t, client, grokRealtimeCutoffCodeBalance, grokRealtimeCloseReasonBalance)
	waitUpstreamClosed(t, upstream, websocket.ClosePolicyViolation)

	entry := rtLogs.next(t)
	if want := 2 * rtCutPerResponse; math.Abs(entry.Cost-want) > 1e-12 {
		t.Fatalf("log cost %v, want %v", entry.Cost, want)
	}
	if got := start - userBalance(t, user.ID); math.Abs(got-2*rtCutPerResponse) > 1e-12 {
		t.Fatalf("charged %v, want exactly 2 responses %v (no double charge on close)", got, 2*rtCutPerResponse)
	}
	detail, err := op.RelayLogGetByID(ctx, entry.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.ResponseContent, `"cutoff":"insufficient_quota"`) || !strings.Contains(detail.ResponseContent, `"close_code":1008`) {
		t.Fatalf("session log should record the cutoff: %s", detail.ResponseContent)
	}
}

func waitBalance(t *testing.T, userID int, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got float64
	for time.Now().Before(deadline) {
		got = userBalance(t, userID)
		if math.Abs(got-want) < 1e-12 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("balance %v, want %v (incremental charge)", got, want)
}

func TestGrokRealtimeCutsOffPerMinuteSessionWhenFundsRunOut(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	shrinkRealtimeBillingClock(t, 50*time.Millisecond, 10*time.Millisecond)
	upstream := audioUpstream(t)
	createRawProtocolGroup(t, ctx, upstream.srv.URL, rtCutModel, rtCutModel, "k")
	// 60/分钟 = 1/秒：余额 0.3 ≈ 0.3 秒音频。
	price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{PerCall: 60})
	t.Cleanup(func() { price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{}) })

	user := createRealtimeUser(t, ctx, "rt-minute-cut", dbmodel.UserRoleUser, 0.3)
	gw := newRealtimeGatewayFor(t, user.ID, 0)
	client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	started := time.Now()
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)); err != nil {
		t.Fatal(err)
	}
	assertCutoffEventAndClose(t, client, grokRealtimeCutoffCodeBalance, grokRealtimeCloseReasonBalance)
	elapsed := time.Since(started)
	waitUpstreamClosed(t, upstream, websocket.ClosePolicyViolation)
	// 用完才断：不会提前一个周期断，也不会拖着不断。
	if elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("cut off after %v, want about 0.3s of audio", elapsed)
	}
	entry := rtLogs.next(t)
	if entry.Cost < 0.29 {
		t.Fatalf("log cost %v, should be about the whole balance", entry.Cost)
	}
	if got := userBalance(t, user.ID); got > 0.01 {
		t.Fatalf("balance %v should be used up", got)
	}
}

func TestGrokRealtimeNeverCutsAdmin(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	shrinkRealtimeBillingClock(t, 20*time.Millisecond, 10*time.Millisecond)
	upstream := audioUpstream(t)
	createRawProtocolGroup(t, ctx, upstream.srv.URL, rtCutModel, rtCutModel, "k")
	price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{PerCall: 60})
	t.Cleanup(func() { price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{}) })

	admin := createRealtimeUser(t, ctx, "rt-admin", dbmodel.UserRoleAdmin, 0)
	gw := newRealtimeGatewayFor(t, admin.ID, 0)
	client, resp, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
	if err != nil {
		t.Fatalf("admin with balance 0 must still connect: %v (resp=%v)", err, resp)
	}
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)); err != nil {
		t.Fatal(err)
	}
	if ev, err := readRealtimeEvent(t, client, 3*time.Second); err != nil || ev["type"] != "response.output_audio.delta" {
		t.Fatalf("audio not relayed: %v %v", ev, err)
	}
	// 跑过很多个结算周期也不断。
	if ev, err := readRealtimeEvent(t, client, 400*time.Millisecond); err == nil || !isTimeoutErr(err) {
		t.Fatalf("admin session must stay open, got event=%v err=%v", ev, err)
	}
	_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = client.Close()
	entry := rtLogs.next(t)
	if entry.Cost <= 0 {
		t.Fatalf("admin usage still logged with cost, got %v", entry.Cost)
	}
	if got := userBalance(t, admin.ID); got != 0 {
		t.Fatalf("admin must never be charged, balance %v", got)
	}
}

func isTimeoutErr(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

func TestGrokRealtimeIncrementalChargesSumToTotal(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		ctx := setupRelayErrorDB(t)
		rtLogs := newRealtimeLogWatcher(t, ctx)
		shrinkRealtimeBillingClock(t, time.Hour, time.Second)
		upstream := tokenUpstream(t)
		createRawProtocolGroup(t, ctx, upstream.srv.URL, rtCutModel, rtCutModel, "k")
		price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{Input: 100, Output: 100})
		t.Cleanup(func() { price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{}) })

		const start = 10.0
		user := createRealtimeUser(t, ctx, "rt-sum-token", dbmodel.UserRoleUser, start)
		gw := newRealtimeGatewayFor(t, user.ID, 0)
		client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		for i := 1; i <= 3; i++ {
			_ = client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`))
			if ev, err := readRealtimeEvent(t, client, 3*time.Second); err != nil || ev["type"] != "response.done" {
				t.Fatalf("response %d: %v %v", i, ev, err)
			}
			waitBalance(t, user.ID, start-float64(i)*rtCutPerResponse)
		}
		_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = client.Close()
		entry := rtLogs.next(t)
		want := 3 * rtCutPerResponse
		if math.Abs(entry.Cost-want) > 1e-12 {
			t.Fatalf("log cost %v, want %v", entry.Cost, want)
		}
		if got := start - userBalance(t, user.ID); math.Abs(got-want) > 1e-12 {
			t.Fatalf("total charged %v, want %v (final settle must not re-charge)", got, want)
		}
	})
	t.Run("per minute", func(t *testing.T) {
		ctx := setupRelayErrorDB(t)
		rtLogs := newRealtimeLogWatcher(t, ctx)
		shrinkRealtimeBillingClock(t, 30*time.Millisecond, 10*time.Millisecond)
		upstream := audioUpstream(t)
		createRawProtocolGroup(t, ctx, upstream.srv.URL, rtCutModel, rtCutModel, "k")
		price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{PerCall: 60})
		t.Cleanup(func() { price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{}) })

		const start = 100.0
		user := createRealtimeUser(t, ctx, "rt-sum-minute", dbmodel.UserRoleUser, start)
		gw := newRealtimeGatewayFor(t, user.ID, 0)
		client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = client.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
		if _, err := readRealtimeEvent(t, client, 3*time.Second); err != nil {
			t.Fatal(err)
		}
		// 中途已经开始扣了。
		deadline := time.Now().Add(2 * time.Second)
		for userBalance(t, user.ID) >= start && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if userBalance(t, user.ID) >= start {
			t.Fatalf("per-minute session was not charged mid-session")
		}
		time.Sleep(200 * time.Millisecond)
		_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = client.Close()
		entry := rtLogs.next(t)
		charged := start - userBalance(t, user.ID)
		if entry.Cost <= 0 || math.Abs(charged-entry.Cost) > 1e-9 {
			t.Fatalf("total charged %v must equal the session total %v (incremental + final remainder, no double charge)", charged, entry.Cost)
		}
	})
}

func TestGrokRealtimeCutsOffAtAPIKeySpendCap(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	shrinkRealtimeBillingClock(t, time.Hour, time.Second)
	upstream := tokenUpstream(t)
	createRawProtocolGroup(t, ctx, upstream.srv.URL, rtCutModel, rtCutModel, "k")
	price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{Input: 100, Output: 100})
	t.Cleanup(func() { price.SetLLMPrice(rtCutModel, dbmodel.LLMPrice{}) })

	user := createRealtimeUser(t, ctx, "rt-keycap", dbmodel.UserRoleUser, 10)
	key := &dbmodel.APIKey{UserID: user.ID, Name: "capped", APIKey: fmt.Sprintf("sk-octopus-rtcap-%d", time.Now().UnixNano()), Enabled: true, MaxCost: 1.5 * rtCutPerResponse}
	if err := op.APIKeyCreate(key, ctx); err != nil {
		t.Fatalf("create key: %v", err)
	}
	gw := newRealtimeGatewayFor(t, user.ID, key.ID)
	client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`))
	if ev, err := readRealtimeEvent(t, client, 3*time.Second); err != nil || ev["type"] != "response.done" {
		t.Fatalf("response: %v %v", ev, err)
	}
	// 第 1 次后 key 只剩 0.5 份：不够下一份 → 断。
	assertCutoffEventAndClose(t, client, grokRealtimeCutoffCodeKeyCap, grokRealtimeCloseReasonKeyCap)
	waitUpstreamClosed(t, upstream, websocket.ClosePolicyViolation)
}

func TestQuotaReservationContextKeyMatchesMiddleware(t *testing.T) {
	if quotaReservationContextKey != middleware.QuotaReservationContextKey {
		t.Fatalf("relay reads %q but APIKeyAuth writes %q", quotaReservationContextKey, middleware.QuotaReservationContextKey)
	}
}
