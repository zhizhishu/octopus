package behavior

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

type recordProgress struct {
	mu     sync.Mutex
	events []string
}

func (r *recordProgress) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}
func (r *recordProgress) ProbeStarted(id ProbeID) { r.add("start:" + string(id)) }
func (r *recordProgress) RequestSent()            { r.add("sent") }
func (r *recordProgress) ProbeFinished(id ProbeID, ok bool) {
	if ok {
		r.add("ok:" + string(id))
	} else {
		r.add("fail:" + string(id))
	}
}

// 观察者只看事件：有没有它，Run 的结果必须一模一样。
func TestRunReportsProgressWithoutChangingResults(t *testing.T) {
	ids := []ProbeID{ProbeLiveness, ProbeLiveness}
	plain := Run(context.Background(), ids, echoLivenessSender(), "gpt-test")

	obs := &recordProgress{}
	watched := Run(WithProgress(context.Background(), obs), ids, echoLivenessSender(), "gpt-test")

	if len(plain) != len(watched) {
		t.Fatalf("result count changed: %d vs %d", len(plain), len(watched))
	}
	for i := range plain {
		if plain[i].OK != watched[i].OK || plain[i].ProbeID != watched[i].ProbeID || (plain[i].Err == nil) != (watched[i].Err == nil) {
			t.Fatalf("result %d changed under observation: %+v vs %+v", i, plain[i], watched[i])
		}
	}
	want := []string{"start:liveness", "sent", "ok:liveness", "start:liveness", "sent", "ok:liveness"}
	if !reflect.DeepEqual(obs.events, want) {
		t.Fatalf("events = %v, want %v", obs.events, want)
	}
}

func TestRunWithCanceledContextReportsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	obs := &recordProgress{}
	Run(WithProgress(ctx, obs), []ProbeID{ProbeLiveness}, echoLivenessSender(), "gpt-test")
	if !reflect.DeepEqual(obs.events, []string{"fail:liveness"}) {
		t.Fatalf("events = %v", obs.events)
	}
}
