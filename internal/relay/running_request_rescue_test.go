package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
)

func TestRunningRescueCommitAndIdentityGuard(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	s := newRequestStateWithCancel("model", "chat", 1, 1, cancelParent)
	attempt, cancelAttempt := context.WithCancel(parent)
	s.bindAttemptCancel(cancelAttempt, true)
	if err := RescueRunningRequest(s.ID, s.StartedAt.Add(time.Second)); !errors.Is(err, ErrRunningRequestConflict) {
		t.Fatalf("stale id accepted: %v", err)
	}
	if err := RescueRunningRequest(s.ID, s.StartedAt); err != nil {
		t.Fatal(err)
	}
	if attempt.Err() == nil || parent.Err() != nil {
		t.Fatal("rescue must cancel only attempt")
	}
	if err := RescueRunningRequest(s.ID, s.StartedAt); err != nil {
		t.Fatal("repeated rescue is not idempotent")
	}
	if err := s.commitBusiness(); !errors.Is(err, errRunningRequestRescue) {
		t.Fatalf("business write accepted after rescue: %v", err)
	}
	if !s.consumeRescueRequest() {
		t.Fatal("rescue signal lost")
	}
	s.BindInterventionID("test-rescue")
	if err := CancelRunningRequest(s.ID, s.StartedAt); err != nil || parent.Err() == nil {
		t.Fatal("abort after rescue must cancel parent")
	}

	s2 := newRequestState("model", "chat", 1, 1)
	s2.bindAttemptCancel(func() { t.Error("committed stream was canceled") }, true)
	if err := s2.commitBusiness(); err != nil {
		t.Fatal(err)
	}
	if err := RescueRunningRequest(s2.ID, s2.StartedAt); !errors.Is(err, ErrRunningRequestConflict) {
		t.Fatalf("committed request transferred: %v", err)
	}
}

func TestRunningRequestTransferToRescue(t *testing.T) {
	setupRescueDeadlineDB(t)
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "10"); err != nil {
		t.Fatal(err)
	}
	entered, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-r.Context().Done():
				close(stopped)
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"done\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"rescued\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"done\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(func() { close(release); upstream.CloseClientConnections(); upstream.Close() })
	newRescueChannel(t, upstream.URL)
	rec, c := newChatGin()
	parent := c.Request.Context()
	done := make(chan struct{})
	go func() { Handler(inbound.InboundTypeOpenAIChat, c); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not entered")
	}
	state := GetRequestStateSnapshot()[0]
	if err := RescueRunningRequest(state.ID, state.StartedAt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("current attempt still running")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rescue did not complete")
	}
	if parent.Err() != nil || calls.Load() != 2 || !strings.Contains(rec.Body.String(), "rescued") {
		t.Fatalf("rescue lifecycle failed: calls=%d body=%s", calls.Load(), rec.Body.String())
	}
	final := GetRequestStateSnapshot()[0]
	if final.Status != "success" || final.InterventionID == "" || final.Rescuable || len(intervention.List()) != 0 {
		t.Fatalf("rescue state leaked: %+v", final)
	}
}
