package relay

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
)

func TestRunningRequestAbortStopsUpstreamBeforeRescue(t *testing.T) {
	setupRescueDeadlineDB(t)
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "10"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release // Block until test cleanup releases
	}))
	t.Cleanup(func() { close(release); upstream.CloseClientConnections(); upstream.Close() })
	newRescueChannel(t, upstream.URL)
	done := make(chan struct{})
	_, c := newChatGin()
	go func() { Handler(inbound.InboundTypeOpenAIChat, c); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not reached")
	}
	states := GetRequestStateSnapshot()
	if len(states) != 1 || states[0].Status != "running" {
		t.Fatalf("unexpected states: %+v", states)
	}
	if err := CancelRunningRequest(states[0].ID, states[0].StartedAt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("abort did not release relay")
	}
	// Abort must cancel the attempt context, causing the HTTP client to fail with context.Canceled.
	// The upstream handler may still block on <-release, but the relay has already returned with
	// status=canceled and no retry. This is the intended behavior: abort stops the client-side
	// request lifecycle without waiting for server-side TCP cleanup.
	if calls.Load() != 1 {
		t.Fatalf("canceled request retried: %d", calls.Load())
	}
	if len(intervention.List()) != 0 {
		t.Fatal("canceled request leaked into rescue")
	}
	if got := GetRequestStateSnapshot()[0].Status; got != "canceled" {
		t.Fatalf("status=%s", got)
	}
}

func TestRunningRequestStateRedaction(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)
	state := newRequestState("test-model", "messages", 1, 1)
	state.BindInterventionID("private-rescue")
	state.startRound("private-channel", "internal-model")
	state.finishRound("private-attempt-error", 12)
	admin := GetRequestStateSnapshotForUser(1, true)[0]
	user := GetRequestStateSnapshotForUser(1, false)[0]
	if admin.InterventionID == "" || len(admin.Attempts) != 1 || admin.Error != "private-attempt-error" {
		t.Fatal("admin audit data lost")
	}
	for _, snapshot := range []*RequestState{user, RedactRequestState(admin, false)} {
		if snapshot.InterventionID != "" || snapshot.TargetChannel != "" || snapshot.TargetModel != "" || len(snapshot.Attempts) != 0 {
			t.Fatalf("internal details leaked: %+v", snapshot)
		}
	}
	if user.Error != "" || RedactRequestState(admin, false).Error != "" {
		t.Fatal("raw upstream error leaked to normal-user state")
	}
	if len(GetRequestStateSnapshotForUser(2, false)) != 0 {
		t.Fatal("cross-user state leaked")
	}
}
