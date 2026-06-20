package balancer

import (
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// TestDecisionTraceRendersCandidates pins that DecisionTrace renders every candidate
// with the bands it sorted on (tier/rank) and does not panic on a plain iterator.
func TestDecisionTraceRendersCandidates(t *testing.T) {
	group := model.Group{
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "m", Priority: 0},
			{ChannelID: 2, ModelName: "m", Priority: 0},
		},
	}
	it := NewIteratorWithSessionKey(group, 1, "m", "")
	trace := it.DecisionTrace()
	for _, want := range []string{"candidates", "ch=1", "ch=2", "tier=", "rank="} {
		if !strings.Contains(trace, want) {
			t.Fatalf("trace missing %q: %s", want, trace)
		}
	}
}

// TestDecisionTraceEmpty pins the no-candidate path is safe.
func TestDecisionTraceEmpty(t *testing.T) {
	var it *Iterator
	if got := it.DecisionTrace(); got != "no candidates" {
		t.Fatalf("nil iterator trace = %q", got)
	}
	it2 := &Iterator{}
	if got := it2.DecisionTrace(); got != "no candidates" {
		t.Fatalf("empty iterator trace = %q", got)
	}
}

// TestDecisionLogHookInvoked pins the hook fires during iterator construction.
func TestDecisionLogHookInvoked(t *testing.T) {
	prev := DecisionLogHook
	defer func() { DecisionLogHook = prev }()

	var gotModel string
	var gotTrace string
	DecisionLogHook = func(requestModel string, it *Iterator) {
		gotModel = requestModel
		gotTrace = it.DecisionTrace()
	}
	group := model.Group{Items: []model.GroupItem{{ChannelID: 7, ModelName: "gpt-5.5", Priority: 0}}}
	NewIteratorWithSessionKey(group, 1, "gpt-5.5", "")
	if gotModel != "gpt-5.5" {
		t.Fatalf("hook model = %q", gotModel)
	}
	if !strings.Contains(gotTrace, "ch=7") {
		t.Fatalf("hook trace = %q", gotTrace)
	}
}
