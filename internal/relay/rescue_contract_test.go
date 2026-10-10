package relay

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// A deterministic request-shape rejection is the CALLER's error, so the relay stops
// retrying it (see isDeterministicClientRejection) and the caller must be able to see the
// upstream's own status, code and reason — hiding them behind the unified public code is
// what made a real 400 come back as a 200 from a failover channel in the rescue matrix.
func TestRelayErrorResponsePassesDeterministicRequestInvalidFourXXThrough(t *testing.T) {
	setupRelayErrorDB(t)

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "redacted_upstream"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	// The unified public code stays configured: the deterministic branch must ignore it,
	// exactly like the sibling context-window branch does.
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorPublicCode, "service_busy"); err != nil {
		t.Fatalf("set public code: %v", err)
	}

	status, code, message := relayErrorResponse(newUpstreamError(
		http.StatusBadRequest,
		[]byte(`{"error":{"type":"invalid_request_error","message":"bad param"}}`),
	))
	if status != http.StatusBadRequest {
		t.Errorf("deterministic 400 must reach the client as 400, got %d", status)
	}
	if code != "invalid_request_error" {
		t.Errorf("deterministic 400 must keep the upstream code, got %q", code)
	}
	if !strings.Contains(message, "bad param") {
		t.Errorf("deterministic 400 must carry the upstream reason, got %q", message)
	}

	// A serde body-deserialize rejection carries no code field; the canonical
	// invalid_request_error code is the fallback and the reason still comes through.
	status, code, message = relayErrorResponse(newUpstreamError(
		http.StatusUnprocessableEntity,
		[]byte(`{"error":{"message":"Failed to deserialize the JSON body into ChatCompletionRequest"}}`),
	))
	if status != http.StatusUnprocessableEntity || code != "invalid_request_error" {
		t.Errorf("serde rejection shape changed: status=%d code=%q", status, code)
	}
	if !strings.Contains(message, "Failed to deserialize") {
		t.Errorf("serde rejection must carry the upstream reason, got %q", message)
	}

	// Control: a 400 with no request-invalid marker is untouched by this branch and keeps
	// the configured public code with the upstream body hidden.
	status, code, message = relayErrorResponse(newUpstreamError(
		http.StatusBadRequest,
		[]byte(`{"error":{"message":"provider detail sk-live"}}`),
	))
	if status != http.StatusBadRequest || code != "service_busy" {
		t.Errorf("unmarked 400 shape changed: status=%d code=%q", status, code)
	}
	if strings.Contains(message, "provider detail") || strings.Contains(message, "sk-live") {
		t.Errorf("unmarked 400 leaked the upstream body: %q", message)
	}
}

// The client-facing message follows the admin body-mode setting, same as every other
// upstream error surface.
func TestRequestInvalidUserMessageHonoursBodyMode(t *testing.T) {
	setupRelayErrorDB(t)

	err := newUpstreamError(http.StatusBadRequest,
		[]byte(`{"error":{"type":"invalid_request_error","message":"bad param"}}`))

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "octopus_standard"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	if msg := requestInvalidUserMessage(err); strings.Contains(msg, "bad param") {
		t.Errorf("octopus_standard must not reveal the upstream reason, got %q", msg)
	}

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "custom_message"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorCustom, "contact support"); err != nil {
		t.Fatalf("set custom message: %v", err)
	}
	if msg := requestInvalidUserMessage(err); msg != "contact support" {
		t.Errorf("custom_message must win, got %q", msg)
	}
}

func TestIsDeterministicClientRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"context window 400", newUpstreamError(http.StatusBadRequest,
			[]byte(`{"error":{"code":"context_length_exceeded","message":"too long"}}`)), true},
		{"invalid_request_error 400", newUpstreamError(http.StatusBadRequest,
			[]byte(`{"error":{"type":"invalid_request_error","message":"bad param"}}`)), true},
		{"serde deserialize 422", newUpstreamError(http.StatusUnprocessableEntity,
			[]byte(`{"error":{"message":"failed to deserialize the JSON body"}}`)), true},
		{"429 stays rescuable", newUpstreamError(http.StatusTooManyRequests,
			[]byte(`{"error":{"message":"rate limit"}}`)), false},
		{"503 stays rescuable", newUpstreamError(http.StatusServiceUnavailable,
			[]byte(`{"error":{"message":"overloaded"}}`)), false},
		{"500 stays rescuable", newUpstreamError(http.StatusInternalServerError,
			[]byte(`{"error":{"message":"boom"}}`)), false},
		{"plain 400 without markers", newUpstreamError(http.StatusBadRequest,
			[]byte(`{"error":{"message":"provider detail"}}`)), false},
		{"404 is not a request-shape rejection", newUpstreamError(http.StatusNotFound,
			[]byte(`{"error":{"message":"resource not found"}}`)), false},
		{"400 strategic supply suspension (zh)", newUpstreamError(http.StatusBadRequest,
			[]byte(`{"error":{"message":"由于 claude 模型供应难以保证，上线 gpt-6-astra 模型"}}`)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDeterministicClientRejection(tc.err); got != tc.want {
				t.Errorf("isDeterministicClientRejection() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 上游「策略性停供」是一个 400，但语义上和参数错同类：换渠道、等一会儿都不会变。判定必须按
// 错误体语义而不是状态码——凡 400 都重试（把真实的 400 吞成别的渠道的 200）和凡 400 都不重试
// （漏掉一个只是措辞成 400 的限流）都是错的。这一条锁住中文停供文案的三件事：不算可救援、
// 不算瞬时容量、真实 400 与文案原样透传给调用端。
func TestStrategicSupplySuspensionFourHundredStaysDeterministic(t *testing.T) {
	setupRelayErrorDB(t)

	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorBodyMode, "redacted_upstream"); err != nil {
		t.Fatalf("set body mode: %v", err)
	}

	err := newUpstreamError(http.StatusBadRequest,
		[]byte(`{"error":{"message":"由于 claude 模型供应难以保证，上线 gpt-6-astra 模型"}}`))

	if !isRequestInvalidUpstreamError(err) {
		t.Error("strategic supply suspension must be recognised as a deterministic rejection")
	}
	if isEligibleForInterventionRescue(err) {
		t.Error("strategic supply suspension must never enter the rescue loop")
	}
	if isTransientCapacityUpstreamError(err) {
		t.Error("strategic supply suspension must not be mistaken for transient capacity")
	}

	status, code, message := relayErrorResponse(err)
	if status != http.StatusBadRequest {
		t.Errorf("real upstream status must survive, got %d", status)
	}
	if code != "invalid_request_error" {
		t.Errorf("expected the deterministic request code, got %q", code)
	}
	if !strings.Contains(message, "供应难以保证") {
		t.Errorf("the upstream reason must reach the caller verbatim, got %q", message)
	}
}

// An automatic rescue that is NOT registered for operator review (the intervention switch
// is off) still has to pace its rounds: waitRescueRound must sleep the backoff and give up
// the moment the rescue deadline fires.
func TestWaitRescueRoundUnregisteredPacesAndHonorsCancel(t *testing.T) {
	start := time.Now()
	if _, preempted, err := waitRescueRound(context.Background(), "", false, 40*time.Millisecond); err != nil || preempted {
		t.Fatalf("unregistered round should just wait: preempted=%v err=%v", preempted, err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("unregistered round returned after %s, want >= 40ms", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := waitRescueRound(ctx, "", false, 5*time.Second); err == nil {
		t.Fatal("a canceled rescue context must abort the round wait")
	}
}
