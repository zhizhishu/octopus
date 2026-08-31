package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func setupRaceTestRequest(t *testing.T, baseURL string) (*httptest.ResponseRecorder, *relayRequest, *dbmodel.Channel) {
	t.Helper()

	balancer.ResetRuntimeTelemetry()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	)
	ginContext.Request.Header.Set("Content-Type", "application/json")

	messageText := "hi"
	internalRequest := &transformerModel.InternalLLMRequest{
		Model: "gpt-4o",
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &messageText},
		}},
	}
	group := dbmodel.Group{Items: []dbmodel.GroupItem{{ChannelID: 1, ModelName: "gpt-4o"}}}
	iterator := balancer.NewIterator(group, 0, "gpt-4o")
	if !iterator.Next() {
		t.Fatal("expected race test iterator candidate")
	}

	request := &relayRequest{
		c:               ginContext,
		inboundType:     inbound.InboundTypeOpenAIChat,
		inAdapter:       inbound.Get(inbound.InboundTypeOpenAIChat),
		internalRequest: internalRequest,
		metrics:         NewRelayMetrics(0, 0, "127.0.0.1", "gpt-4o", internalRequest),
		requestModel:    "gpt-4o",
		iter:            iterator,
	}
	channel := &dbmodel.Channel{
		ID:                 1,
		Name:               "race-channel",
		Type:               outbound.OutboundTypeOpenAIChat,
		Enabled:            true,
		BaseUrls:           []dbmodel.BaseUrl{{URL: baseURL}},
		RaceMode:           true,
		RaceKeyConcurrency: 2,
	}
	return recorder, request, channel
}

func TestRaceEligibilityAndConcurrencyClamp(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request := &relayRequest{c: ginContext, internalRequest: &transformerModel.InternalLLMRequest{Model: "gpt-4o"}}
	channel := &dbmodel.Channel{RaceMode: true}
	keys := []dbmodel.ChannelKey{{ID: 1}, {ID: 2}}

	if !canChannelRace(request, channel, keys) {
		t.Fatal("expected eligible text request with two keys to race")
	}
	request.interventionKeyID = 1
	if canChannelRace(request, channel, keys) {
		t.Fatal("manual intervention must force one selected key")
	}
	request.interventionKeyID = 0
	ginContext.Request.URL.Path = "/v1/videos"
	if canChannelRace(request, channel, keys) {
		t.Fatal("video requests must not race")
	}
	if clampRaceConcurrency(1) != 2 || clampRaceConcurrency(3) != 3 || clampRaceConcurrency(9) != 5 {
		t.Fatal("race concurrency must clamp to 2..5")
	}
}

func TestPrepareRacerAttemptDoesNotMutateParentRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	_, request, channel := setupRaceTestRequest(t, server.URL)
	parentInternalRequest := request.internalRequest

	preparedAttempt, outboundRequest, _, err := prepareRacerAttempt(
		context.Background(),
		request,
		channel,
		dbmodel.ChannelKey{ID: 1, ChannelID: channel.ID, Enabled: true, ChannelKey: "key-one"},
		outbound.Get(outbound.OutboundTypeOpenAIChat),
	)
	if err != nil {
		t.Fatalf("prepare racer: %v", err)
	}
	if preparedAttempt.internalRequest == parentInternalRequest {
		t.Fatal("racer must own an isolated internal request")
	}
	if request.internalRequest != parentInternalRequest {
		t.Fatal("preparing a racer mutated the parent request pointer")
	}
	if got := outboundRequest.Header.Get("Authorization"); got != "Bearer key-one" {
		t.Fatalf("authorization = %q, want racer key", got)
	}
}

func TestChannelRaceFast200WinsAfter429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer rate-limited":
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.WriteHeader(http.StatusTooManyRequests)
			_, _ = responseWriter.Write([]byte(`{"error":{"message":"busy"}}`))
		case "Bearer healthy":
			responseWriter.Header().Set("Content-Type", "application/json")
			_, _ = responseWriter.Write([]byte(`{"id":"race-win","choices":[{"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`))
		case "Bearer slow":
			select {
			case <-request.Context().Done():
			case <-time.After(2 * time.Second):
				responseWriter.WriteHeader(http.StatusGatewayTimeout)
			}
		default:
			responseWriter.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	recorder, request, channel := setupRaceTestRequest(t, server.URL)
	channel.RaceKeyConcurrency = 3
	keys := []dbmodel.ChannelKey{
		{ID: 1, ChannelID: channel.ID, Enabled: true, ChannelKey: "rate-limited"},
		{ID: 2, ChannelID: channel.ID, Enabled: true, ChannelKey: "healthy", Remark: "winner"},
		{ID: 3, ChannelID: channel.ID, Enabled: true, ChannelKey: "slow"},
	}

	result, remainingKeys := runChannelRace(
		request,
		channel,
		keys,
		outbound.Get(outbound.OutboundTypeOpenAIChat),
		"chat",
		"",
		0,
	)
	if !result.Success {
		t.Fatalf("race failed: %v", result.Err)
	}
	if len(remainingKeys) != 0 {
		t.Fatalf("remaining keys = %d, want 0", len(remainingKeys))
	}
	if !strings.Contains(recorder.Body.String(), "recovered") {
		t.Fatalf("winner response missing: %s", recorder.Body.String())
	}
	if request.metrics.ChannelKeyRemark != "winner" {
		t.Fatalf("winner key remark = %q", request.metrics.ChannelKeyRemark)
	}
	loserWasRecordedAsRacedOut := false
	for _, attempt := range request.iter.Attempts() {
		if attempt.ChannelKeyID == 3 && attempt.Msg == "raced_out" {
			loserWasRecordedAsRacedOut = true
			break
		}
	}
	if !loserWasRecordedAsRacedOut {
		t.Fatalf("slow loser was not recorded as raced_out: %#v", request.iter.Attempts())
	}
}

func TestChannelRaceHedgeSkipsSecondKeyAfterFastWinner(t *testing.T) {
	var secondKeyHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer second" {
			secondKeyHits.Add(1)
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"id":"hedge-win","choices":[{"message":{"role":"assistant","content":"fast"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	_, request, channel := setupRaceTestRequest(t, server.URL)
	channel.RaceDelayMs = 200
	result, _ := runChannelRace(
		request,
		channel,
		[]dbmodel.ChannelKey{
			{ID: 1, ChannelID: channel.ID, Enabled: true, ChannelKey: "first"},
			{ID: 2, ChannelID: channel.ID, Enabled: true, ChannelKey: "second"},
		},
		outbound.Get(outbound.OutboundTypeOpenAIChat),
		"chat",
		"",
		0,
	)
	if !result.Success {
		t.Fatalf("hedged race failed: %v", result.Err)
	}
	if secondKeyHits.Load() != 0 {
		t.Fatalf("delayed hedge launched after winner: hits=%d", secondKeyHits.Load())
	}
}

func TestChannelRaceAllFailedReturnsRemainingKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.WriteHeader(http.StatusTooManyRequests)
		_, _ = responseWriter.Write([]byte(`{"error":{"message":"busy"}}`))
	}))
	defer server.Close()

	_, request, channel := setupRaceTestRequest(t, server.URL)
	result, remainingKeys := runChannelRace(
		request,
		channel,
		[]dbmodel.ChannelKey{
			{ID: 1, ChannelID: channel.ID, Enabled: true, ChannelKey: "one"},
			{ID: 2, ChannelID: channel.ID, Enabled: true, ChannelKey: "two"},
			{ID: 3, ChannelID: channel.ID, Enabled: true, ChannelKey: "three"},
		},
		outbound.Get(outbound.OutboundTypeOpenAIChat),
		"chat",
		"",
		0,
	)
	if result.Success || result.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("result = %#v, want 429 failure", result)
	}
	if len(remainingKeys) != 1 || remainingKeys[0].ID != 3 {
		t.Fatalf("remaining keys = %#v, want key 3", remainingKeys)
	}
}
