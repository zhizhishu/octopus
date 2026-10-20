package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/gin-gonic/gin"
)

func testRaceContender(channelID, keyID, raceConcurrency int) raceContender {
	return raceContender{
		channel: &dbmodel.Channel{ID: channelID, RaceKeyConcurrency: raceConcurrency, RaceMode: true, Enabled: true},
		key:     dbmodel.ChannelKey{ID: keyID, ChannelID: channelID, Enabled: true},
	}
}

// First wave takes one key per channel, then fills the remaining global slots.
func TestNextRaceWaveOnePerChannelThenFill(t *testing.T) {
	queue := []raceContender{
		testRaceContender(1, 11, 5),
		testRaceContender(2, 21, 5),
		testRaceContender(3, 31, 5),
		testRaceContender(1, 12, 5),
		testRaceContender(2, 22, 5),
	}
	wave, rest := nextRaceWave(queue)
	if len(wave) != 5 {
		t.Fatalf("wave size = %d, want 5", len(wave))
	}
	if wave[0].channel.ID != 1 || wave[1].channel.ID != 2 || wave[2].channel.ID != 3 {
		t.Fatalf("first three must be one key per channel, got %d/%d/%d", wave[0].channel.ID, wave[1].channel.ID, wave[2].channel.ID)
	}
	if len(rest) != 0 {
		t.Fatalf("rest = %d, want 0", len(rest))
	}
}

// Global concurrency cap is 5.
func TestNextRaceWaveGlobalCapFive(t *testing.T) {
	queue := []raceContender{
		testRaceContender(1, 11, 5),
		testRaceContender(2, 21, 5),
		testRaceContender(3, 31, 5),
		testRaceContender(4, 41, 5),
		testRaceContender(5, 51, 5),
		testRaceContender(6, 61, 5),
	}
	wave, rest := nextRaceWave(queue)
	if len(wave) != 5 {
		t.Fatalf("global cap must be 5, got %d", len(wave))
	}
	if len(rest) != 1 || rest[0].channel.ID != 6 {
		t.Fatalf("rest = %+v, want a single channel-6 remainder", rest)
	}
}

// Per-channel clamp limits how many of a channel's keys enter one wave.
func TestNextRaceWavePerChannelClamp(t *testing.T) {
	queue := []raceContender{
		testRaceContender(1, 11, 2),
		testRaceContender(2, 21, 2),
		testRaceContender(1, 12, 2),
		testRaceContender(2, 22, 2),
		testRaceContender(1, 13, 2),
		testRaceContender(2, 23, 2),
	}
	wave, rest := nextRaceWave(queue)
	if len(wave) != 4 {
		t.Fatalf("per-channel clamp 2 across 2 channels -> wave 4, got %d", len(wave))
	}
	if len(rest) != 2 {
		t.Fatalf("rest = %d, want 2", len(rest))
	}
}

// Hedge delay staggers a channel's keys in wave order.
func TestApplyWaveDelaysPerChannelHedge(t *testing.T) {
	wave := []raceContender{
		testRaceContender(1, 11, 5),
		testRaceContender(2, 21, 5),
		testRaceContender(1, 12, 5),
	}
	for i := range wave {
		wave[i].channel.RaceDelayMs = 100
	}
	applyWaveDelays(wave)
	if wave[0].delay != 0 {
		t.Fatalf("first key delay = %v, want 0", wave[0].delay)
	}
	if wave[1].delay != 0 {
		t.Fatalf("first key of a second channel delay = %v, want 0", wave[1].delay)
	}
	if wave[2].delay != 100*time.Millisecond {
		t.Fatalf("second key of channel 1 delay = %v, want 100ms", wave[2].delay)
	}
}

// Content budget uses the explicit timeout when set, else a positive shipped default.
func TestRaceContentBudgetExplicitAndFallback(t *testing.T) {
	if got := raceContentBudget(30); got != 30*time.Second {
		t.Fatalf("explicit budget = %v, want 30s", got)
	}
	if got := raceContentBudget(0); got <= 0 {
		t.Fatalf("zero must fall back to a positive default, got %v", got)
	}
}

func TestRuntimeModelForChannelMapping(t *testing.T) {
	channel := &dbmodel.Channel{ModelMapping: map[string]string{"alias": "upstream"}}
	if got := runtimeModelForChannel(channel, &transformerModel.InternalLLMRequest{Model: "alias"}); got != "upstream" {
		t.Fatalf("mapped runtime model = %q, want upstream", got)
	}
	if got := runtimeModelForChannel(channel, &transformerModel.InternalLLMRequest{Model: "plain"}); got != "plain" {
		t.Fatalf("unmapped runtime model = %q, want plain", got)
	}
}

// The racer copy must carry scalars and the atomic suppress flag (by value, never a struct
// copy) and get its own metrics instance.
func TestCopyRelayRequestForRacerCopiesSuppressFlagAndMetrics(t *testing.T) {
	var parent relayRequest
	parent.apiKeyID = 7
	parent.suppressPreContentKeepalive.Store(true)
	parent.metrics = &RelayMetrics{APIKeyID: 7}

	racer := copyRelayRequestForRacer(&parent, nil)
	if racer == nil {
		t.Fatal("racer copy is nil")
	}
	if racer.apiKeyID != 7 {
		t.Fatalf("scalar field not copied: apiKeyID=%d", racer.apiKeyID)
	}
	if !racer.suppressPreContentKeepalive.Load() {
		t.Fatal("atomic suppress flag must be copied by value")
	}
	if racer.metrics == parent.metrics {
		t.Fatal("racer must own an independent metrics copy")
	}
	if racer.metrics.APIKeyID != 7 {
		t.Fatalf("metrics copy value = %d, want 7", racer.metrics.APIKeyID)
	}
	racer.apiKeyID = 9
	if parent.apiKeyID != 7 {
		t.Fatal("racer copy must not mutate the parent")
	}
}

// The key filter must use the SAME breaker scope the attempt records under (runtime model,
// endpoint, capability); a trip on that exact tuple removes the key, an unrelated scope
// does not, and a no-breaker channel is never filtered.
func TestRaceKeysForChannelUsesRecordConsistentBreakerScope(t *testing.T) {
	const channelID = 7701
	const keyID = 77011
	item := dbmodel.GroupItem{ModelName: "alias"}
	preset := []dbmodel.ChannelKey{
		{ID: keyID, ChannelID: channelID, Enabled: true, ChannelKey: "tripped"},
		{ID: keyID + 1, ChannelID: channelID, Enabled: true, ChannelKey: "healthy"},
	}
	channel := &dbmodel.Channel{ID: channelID, Enabled: true, RaceMode: true}

	for range 10 {
		balancer.RecordFailureWithStatusScoped(channelID, keyID, "upstream-m", "chat", "stream", http.StatusServiceUnavailable)
	}

	keys := raceKeysForChannel(nil, item, channel, "chat", "upstream-m", "stream", preset)
	if len(keys) != 1 || keys[0].ID != keyID+1 {
		t.Fatalf("scoped-trip filter = %+v, want only the healthy key", keys)
	}

	// A scope that was never recorded must keep both keys.
	keys = raceKeysForChannel(nil, item, channel, "responses", "upstream-m", "stream", preset)
	if len(keys) != 2 {
		t.Fatalf("unrelated scope must keep both keys, got %+v", keys)
	}

	// No-breaker channel: never filtered even when the scope is tripped.
	noBreaker := &dbmodel.Channel{ID: channelID, Enabled: true, RaceMode: true, DisableCircuitBreaker: true}
	keys = raceKeysForChannel(nil, item, noBreaker, "chat", "upstream-m", "stream", preset)
	if len(keys) != 2 {
		t.Fatalf("no-breaker channel must keep both keys, got %+v", keys)
	}
}

// An existing Responses owner cursor must disable cross-channel grouping entirely.
func TestBuildGroupRaceContendersOwnerCursorDisablesCrossChannel(t *testing.T) {
	previous := "resp_owner"
	req := &relayRequest{internalRequest: &transformerModel.InternalLLMRequest{Model: "m", PreviousResponseID: &previous}}
	contenders, additional := buildGroupRaceContenders(
		context.Background(), req,
		dbmodel.GroupItem{ChannelID: 1, ModelName: "m"}, &dbmodel.Channel{ID: 1, RaceMode: true, Enabled: true},
		[]dbmodel.ChannelKey{{ID: 11}}, routeGroupResult{}, "chat",
	)
	if contenders != nil || additional != 0 {
		t.Fatalf("owner cursor must disable cross-channel racing: contenders=%v additional=%d", contenders, additional)
	}
}

// Manual intervention (single pinned key) must disable cross-channel grouping.
func TestBuildGroupRaceContendersInterventionDisablesCrossChannel(t *testing.T) {
	req := &relayRequest{
		internalRequest:   &transformerModel.InternalLLMRequest{Model: "m"},
		interventionKeyID: 5,
	}
	contenders, additional := buildGroupRaceContenders(
		context.Background(), req,
		dbmodel.GroupItem{ChannelID: 1, ModelName: "m"}, &dbmodel.Channel{ID: 1, RaceMode: true, Enabled: true},
		[]dbmodel.ChannelKey{{ID: 11}}, routeGroupResult{}, "chat",
	)
	if contenders != nil || additional != 0 {
		t.Fatalf("intervention must disable cross-channel racing: contenders=%v additional=%d", contenders, additional)
	}
}

// Video requests must never race across channels.
func TestBuildGroupRaceContendersRejectsVideoRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	req := &relayRequest{c: ginContext, internalRequest: &transformerModel.InternalLLMRequest{Model: "m"}}

	contenders, additional := buildGroupRaceContenders(
		context.Background(), req,
		dbmodel.GroupItem{ChannelID: 1, ModelName: "m"}, &dbmodel.Channel{ID: 1, RaceMode: true, Enabled: true},
		[]dbmodel.ChannelKey{{ID: 11}}, routeGroupResult{}, "chat",
	)
	if contenders != nil || additional != 0 {
		t.Fatalf("video request must not race across channels: contenders=%v additional=%d", contenders, additional)
	}
}

// A nil internal request is safe and produces no contenders.
func TestBuildGroupRaceContendersNilRequest(t *testing.T) {
	contenders, additional := buildGroupRaceContenders(
		context.Background(), &relayRequest{},
		dbmodel.GroupItem{ChannelID: 1}, &dbmodel.Channel{ID: 1}, nil, routeGroupResult{}, "chat",
	)
	if contenders != nil || additional != 0 {
		t.Fatalf("nil request must be a no-op: contenders=%v additional=%d", contenders, additional)
	}
}

// Interleaving flattens channels round-robin by key index.
func TestInterleaveGroupRaceContendersRoundRobin(t *testing.T) {
	candidates := []groupRaceCandidate{
		{
			item:     dbmodel.GroupItem{ModelName: "m"},
			channel:  &dbmodel.Channel{ID: 1},
			keys:     []dbmodel.ChannelKey{{ID: 11}, {ID: 12}},
			template: &transformerModel.InternalLLMRequest{Model: "m"},
		},
		{
			item:     dbmodel.GroupItem{ModelName: "m"},
			channel:  &dbmodel.Channel{ID: 2},
			keys:     []dbmodel.ChannelKey{{ID: 21}},
			template: &transformerModel.InternalLLMRequest{Model: "m"},
		},
	}
	got := interleaveGroupRaceContenders(candidates)
	if len(got) != 3 {
		t.Fatalf("interleaved length = %d, want 3", len(got))
	}
	want := []struct{ channel, key int }{{1, 11}, {2, 21}, {1, 12}}
	for i, w := range want {
		if got[i].channel.ID != w.channel || got[i].key.ID != w.key {
			t.Fatalf("interleave[%d] = (%d,%d), want (%d,%d)", i, got[i].channel.ID, got[i].key.ID, w.channel, w.key)
		}
	}
	// Group contenders always mark their prompt snapshot as authoritative.
	for i := range got {
		if !got[i].promptOverridesApplied {
			t.Fatalf("interleave[%d] must carry promptOverridesApplied", i)
		}
	}
}
