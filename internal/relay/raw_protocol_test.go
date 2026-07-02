package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestRawProtocolHandlerRewritesJSONModelAndLogsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu       sync.Mutex
		gotPath  string
		gotQuery string
		gotAuth  string
		gotModel string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("trace")
		gotAuth = r.Header.Get("Authorization")
		gotModel, _ = body["model"].(string)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results":[],
			"usage":{
				"prompt_tokens":7,
				"completion_tokens":2,
				"total_tokens":9,
				"prompt_tokens_details":{"cached_tokens":3}
			}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-rerank", "upstream-rerank", "raw-json-key")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank?trace=1", strings.NewReader(`{
		"model":"request-rerank",
		"query":"hello",
		"documents":["world"]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/rerank", Name: "rerank"}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected raw protocol request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, query, auth, modelName := gotPath, gotQuery, gotAuth, gotModel
	mu.Unlock()
	if path != "/v1/rerank" || query != "1" {
		t.Fatalf("unexpected upstream path/query: path=%q query=%q", path, query)
	}
	if auth != "Bearer raw-json-key" {
		t.Fatalf("unexpected upstream auth: %q", auth)
	}
	if modelName != "upstream-rerank" {
		t.Fatalf("expected upstream model rewrite, got %q", modelName)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	log := logs[0]
	if log.RequestModelName != "request-rerank" || log.ActualModelName != "upstream-rerank" {
		t.Fatalf("unexpected log models: request=%q actual=%q", log.RequestModelName, log.ActualModelName)
	}
	if log.InputTokens != 7 || log.OutputTokens != 2 || log.CacheHitTokens != 3 || log.CacheInputTokens != 7 {
		t.Fatalf("unexpected usage log: input=%d output=%d cache_hit=%d cache_input=%d", log.InputTokens, log.OutputTokens, log.CacheHitTokens, log.CacheInputTokens)
	}
	if math.Abs(log.CacheHitRate-(3.0/7.0)) > 1e-9 {
		t.Fatalf("unexpected cache hit rate: %f", log.CacheHitRate)
	}
	if log.BillingModel != "request-rerank" {
		t.Fatalf("expected request-model billing, got %q", log.BillingModel)
	}
	if log.RequestEndpoint != "rerank" || log.RequestPath != "/v1/rerank" {
		t.Fatalf("unexpected log endpoint: endpoint=%q path=%q", log.RequestEndpoint, log.RequestPath)
	}
}

func TestRawResponsesCompactUsesCanonicalOpenAIPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu      sync.Mutex
		gotPath string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact","usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := createRawProtocolGroup(t, ctx, upstream.URL+"/v1/responses", "request-compact", "upstream-compact", "raw-compact-key")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses/compact", strings.NewReader(`{
		"model":"request-compact",
		"input":[{"role":"user","content":"compact this"}],
		"previous_response_id":"resp_prev"
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", "codex-compact-session")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/responses/compact", Name: "responses_compact", NonBilling: true}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected raw compact request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path := gotPath
	mu.Unlock()
	if path != "/v1/responses/compact" {
		t.Fatalf("expected canonical compact path, got %q", path)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].RequestEndpoint != "responses_compact" || logs[0].RequestPath != "/backend-api/codex/responses/compact" {
		t.Fatalf("unexpected compact log endpoint/path: endpoint=%q path=%q", logs[0].RequestEndpoint, logs[0].RequestPath)
	}
	if logs[0].SessionSource != "header:Session_id" || logs[0].SessionKey == "" {
		t.Fatalf("expected compact log to record client session source/key, got source=%q key=%q", logs[0].SessionSource, logs[0].SessionKey)
	}
	if logs[0].InputTokens != 0 || logs[0].OutputTokens != 0 || logs[0].Cost != 0 {
		t.Fatalf("compact utility endpoint should not bill usage, got input=%d output=%d cost=%f", logs[0].InputTokens, logs[0].OutputTokens, logs[0].Cost)
	}
	owner, ok := responsesSessionOwner("resp_compact")
	if !ok {
		t.Fatalf("expected compact response id owner to be recorded")
	}
	if owner.channelID != channel.ID || owner.channelKeyID == 0 {
		t.Fatalf("unexpected compact owner: %#v channel=%#v", owner, channel)
	}
}

func TestRawResponsesCompactAppliesCodexDefaultsForBackendAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var saw struct {
		userAgent   string
		originator  string
		sessionID   string
		trace       string
		betaFeature string
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw.userAgent = r.Header.Get("User-Agent")
		saw.originator = r.Header.Get("Originator")
		saw.sessionID = r.Header.Get("Session_id")
		saw.trace = r.Header.Get("AH-Trace-Id")
		saw.betaFeature = r.Header.Get("X-Codex-Beta-Features")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact_codex","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-compact-codex", "upstream-compact-codex", "raw-compact-key")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses/compact", strings.NewReader(`{
		"model":"request-compact-codex",
		"prompt_cache_key":"compact-session-1",
		"input":[{"role":"user","content":"compact this"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "client-should-not-leak")
	req.Header.Set("AH-Trace-Id", "trace-should-not-leak")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/responses/compact", Name: "responses_compact", NonBilling: true}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected raw compact request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if saw.userAgent != defaultCodexUserAgent || saw.originator != defaultCodexOriginator || saw.betaFeature != defaultCodexBetaFeatures {
		t.Fatalf("expected Codex defaults, ua=%q originator=%q beta=%q", saw.userAgent, saw.originator, saw.betaFeature)
	}
	if saw.sessionID != "compact-session-1" {
		t.Fatalf("expected compact prompt_cache_key to become Session_id, got %q", saw.sessionID)
	}
	if saw.trace != "" {
		t.Fatalf("client trace header leaked upstream: %q", saw.trace)
	}
}

func TestRawResponsesCompactPrioritizesKnownResponseOwnerChannelKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var firstCalls, ownerCalls int
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"wrong channel"}}`))
	}))
	t.Cleanup(firstUpstream.Close)
	var ownerAuth string
	ownerUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ownerCalls++
		ownerAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact_owner","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(ownerUpstream.Close)

	firstChannel := createRawProtocolChannel(t, ctx, "first-compact-channel", firstUpstream.URL, []string{"first-key"})
	ownerChannel := createRawProtocolChannel(t, ctx, "owner-compact-channel", ownerUpstream.URL, []string{"wrong-owner-key", "actual-owner-key"})
	group := dbmodel.Group{Name: "request-compact-owner", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	for _, item := range []struct {
		channelID int
		modelName string
	}{
		{firstChannel.ID, "upstream-first"},
		{ownerChannel.ID, "upstream-owner"},
	} {
		if err := op.GroupItemAdd(&dbmodel.GroupItem{
			GroupID:   group.ID,
			ChannelID: item.channelID,
			ModelName: item.modelName,
			Priority:  1,
			Weight:    1,
		}, ctx); err != nil {
			t.Fatalf("create group item: %v", err)
		}
	}
	ownerChannelPtr, err := op.ChannelGet(ownerChannel.ID, ctx)
	if err != nil {
		t.Fatalf("reload owner channel: %v", err)
	}
	if len(ownerChannelPtr.Keys) < 2 {
		t.Fatalf("expected two owner keys, got %#v", ownerChannelPtr.Keys)
	}
	recordResponsesSession("resp_prev_compact_owner", ownerChannel.ID, ownerChannelPtr.Keys[1].ID)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{
		"model":"request-compact-owner",
		"previous_response_id":"resp_prev_compact_owner",
		"input":[{"role":"user","content":"compact this"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/responses/compact", Name: "responses_compact", NonBilling: true}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected owner compact request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if firstCalls != 0 || ownerCalls != 1 {
		t.Fatalf("expected owner channel only, first=%d owner=%d", firstCalls, ownerCalls)
	}
	if ownerAuth != "Bearer actual-owner-key" {
		t.Fatalf("expected owner key to be first attempt, got auth %q", ownerAuth)
	}
}

func TestRawResponsesCompactDoesNotForwardOwnerCursorToNonOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact_non_owner","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-compact-non-owner", "upstream-non-owner", "non-owner-key")
	ownerChannel := createRawProtocolChannel(t, ctx, "unrelated-compact-owner", upstream.URL, []string{"owner-key"})
	ownerChannelPtr, err := op.ChannelGet(ownerChannel.ID, ctx)
	if err != nil {
		t.Fatalf("reload owner channel: %v", err)
	}
	if len(ownerChannelPtr.Keys) != 1 {
		t.Fatalf("expected one owner key, got %#v", ownerChannelPtr.Keys)
	}
	recordResponsesSession("resp_prev_elsewhere", ownerChannel.ID, ownerChannelPtr.Keys[0].ID)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{
		"model":"request-compact-non-owner",
		"previous_response_id":"resp_prev_elsewhere",
		"input":[{"role":"user","content":"compact this"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/responses/compact", Name: "responses_compact", NonBilling: true}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected compact request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if _, ok := upstreamBody["previous_response_id"]; ok {
		t.Fatalf("non-owner compact attempt forwarded previous_response_id: %#v", upstreamBody)
	}
}

func TestRawResponsesCompactUnknownCursorNeedsTrustedClientSession(t *testing.T) {
	clearResponsesSessionCacheForTest()
	group := dbmodel.Group{
		SessionKeepTime: 300,
		Items: []dbmodel.GroupItem{{
			ChannelID: 6,
			ModelName: "gpt-5.5",
			Priority:  1,
			Weight:    1,
		}},
	}
	balancer.SetSticky(1, "gpt-5.5", 6, 7)
	iter := balancer.NewIterator(group, 1, "gpt-5.5")
	if !iter.Next() {
		t.Fatalf("expected sticky candidate")
	}

	if shouldForwardRawProtocolResponsesCursor(context.Background(), iter, "resp_unknown_raw", 6, 7, "", 0, 0) {
		t.Fatalf("raw compact must not forward unknown cursor from legacy sticky alone")
	}
	if shouldForwardRawProtocolResponsesCursor(context.Background(), iter, "resp_unknown_raw", 6, 7, "body:previous_response_id", 0, 0) {
		t.Fatalf("raw compact must not trust previous_response_id fallback as ownership proof")
	}
	if !shouldForwardRawProtocolResponsesCursor(context.Background(), iter, "resp_unknown_raw", 6, 7, "header:Session_id", 0, 0) {
		t.Fatalf("raw compact should forward unknown cursor when explicit client session sticky matches")
	}
}

func TestDeriveRawProtocolClientSessionInfoUsesCodexClientMetadata(t *testing.T) {
	info := deriveRawProtocolClientSessionInfo(map[string]any{
		"client_metadata": map[string]any{
			"x-codex-window-id":       "raw-codex-session:2",
			"x-codex-installation-id": "install-should-lose",
		},
	})
	want := hashRouteSessionKey("codex-session", "raw-codex-session")
	if info.Key != want || info.Source != "body:client_metadata:x-codex-window-id" {
		t.Fatalf("raw codex client metadata session = %+v, want key %q", info, want)
	}
}

// P2 #3: a raw-protocol payload carrying no session identifier must fall back to a
// stable per-request fingerprint instead of collapsing to the coarse api-key+model
// sticky slot shared by every unrelated request.
func TestDeriveRawProtocolClientSessionInfoFingerprintFallback(t *testing.T) {
	payload := map[string]any{"model": "gpt-5.5", "input": "no session metadata here"}
	info := deriveRawProtocolClientSessionInfo(payload)
	if info.Key == "" || info.Source != "octopus:request_fingerprint" {
		t.Fatalf("expected request-fingerprint fallback, got %+v", info)
	}
	// Stable for the same payload (so a request's retries stay on one channel)...
	again := deriveRawProtocolClientSessionInfo(map[string]any{"model": "gpt-5.5", "input": "no session metadata here"})
	if again.Key != info.Key {
		t.Fatalf("fingerprint must be stable for identical payloads, got %q vs %q", again.Key, info.Key)
	}
	// ...and distinct for a different request.
	other := deriveRawProtocolClientSessionInfo(map[string]any{"model": "gpt-5.5", "input": "an entirely different request"})
	if other.Key == info.Key {
		t.Fatalf("different payloads must produce different fingerprints, both %q", info.Key)
	}
}

func TestRawResponsesCompactStreamStopsAfterTerminalAndRecordsOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		if r.URL.Path != "/v1/responses/compact" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.completed` + "\n" +
			`data: {"type":"response.completed","response":{"id":"resp_compact_stream","object":"response","created_at":123,"model":"upstream-compact-stream","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-compact-stream", "upstream-compact-stream", "raw-compact-stream-key")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{
		"model":"request-compact-stream",
		"input":[{"role":"user","content":"compact this"}],
		"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	start := time.Now()
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/responses/compact", Name: "responses_compact", NonBilling: true}, c)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected raw compact stream request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("compact stream waited for post-terminal upstream data: elapsed=%s body=%s", elapsed, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"type":"ping"`) {
		t.Fatalf("compact stream should stop after terminal event, got %s", rec.Body.String())
	}
	if _, ok := responsesSessionOwner("resp_compact_stream"); !ok {
		t.Fatalf("expected compact stream response owner")
	}
	<-upstreamDone
}

func TestRawResponsesCompactTerminalEventNameWithoutTypedPayloadStops(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte(`data: {"response":{"id":"resp_compact_event_only","object":"response","created_at":123,"model":"upstream-compact-terminal","status":"completed","output":[]}}` + "\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte("\nevent: ping\ndata: {\"type\":\"ping\"}\n\n"))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-compact-terminal", "upstream-compact-terminal", "raw-compact-terminal-key")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{
		"model":"request-compact-terminal",
		"input":[{"role":"user","content":"compact this"}],
		"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	start := time.Now()
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/responses/compact", Name: "responses_compact", NonBilling: true}, c)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected compact stream request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("compact stream waited for boundary after terminal event name: elapsed=%s body=%s", elapsed, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"type":"ping"`) {
		t.Fatalf("compact stream should stop before post-terminal ping, got %s", rec.Body.String())
	}
}

func TestRawProtocolHandlerRewritesMultipartModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu          sync.Mutex
		gotModel    string
		gotFileBody string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("get multipart file: %v", err)
		}
		var fileBody []byte
		if file != nil {
			fileBody, _ = io.ReadAll(file)
			_ = file.Close()
		}

		mu.Lock()
		gotModel = r.FormValue("model")
		gotFileBody = string(fileBody)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-transcribe", "upstream-transcribe", "raw-multipart-key")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "request-transcribe")
	_ = mw.WriteField("response_format", "json")
	fw, err := mw.CreateFormFile("file", "voice.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = fw.Write([]byte("voice-bytes"))
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/audio/transcriptions", Name: "audio_transcriptions"}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected multipart raw protocol request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	modelName, fileBody := gotModel, gotFileBody
	mu.Unlock()
	if modelName != "upstream-transcribe" {
		t.Fatalf("expected upstream multipart model rewrite, got %q", modelName)
	}
	if fileBody != "voice-bytes" {
		t.Fatalf("expected multipart file to be preserved, got %q", fileBody)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if !strings.Contains(logs[0].RequestContent, "multipart request content omitted") || strings.Contains(logs[0].RequestContent, "voice-bytes") {
		t.Fatalf("multipart request log should omit file content, got %s", logs[0].RequestContent)
	}
	if logs[0].InputTokens != 3 || logs[0].OutputTokens != 1 {
		t.Fatalf("unexpected usage log: input=%d output=%d", logs[0].InputTokens, logs[0].OutputTokens)
	}
}

func TestRawProtocolHandlerScansStreamingUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"cmpl\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1,\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-completion", "upstream-completion", "raw-stream-key")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{
		"model":"request-completion",
		"prompt":"hello",
		"stream":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	RawProtocolHandler(RawProtocolOptions{Endpoint: "/completions", Name: "completions"}, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected streaming raw protocol request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("expected raw SSE body passthrough, got %s", rec.Body.String())
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].InputTokens != 4 || logs[0].OutputTokens != 1 {
		t.Fatalf("unexpected streaming usage log: input=%d output=%d", logs[0].InputTokens, logs[0].OutputTokens)
	}
}

func TestProxySSEClientCancelReturnsAbortError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	respUp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       pr,
	}
	respUp.Header.Set("Content-Type", "text/event-stream")

	cancel()
	_, _, err := proxySSE(ctx, c, respUp, 0, &rawProtocolRelayMetrics{}, true)
	if err == nil {
		t.Fatalf("expected client cancel error")
	}
	if !isClientAbortError(err) {
		t.Fatalf("expected client abort classification, got %v", err)
	}
}

func TestImagesHandlerRewritesModelAndLogsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	var (
		mu       sync.Mutex
		gotPath  string
		gotAuth  string
		gotModel string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotModel, _ = body["model"].(string)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created":123,
			"data":[{"url":"https://example.com/image.png"}],
			"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}
		}`))
	}))
	t.Cleanup(upstream.Close)

	createRawProtocolGroup(t, ctx, upstream.URL, "request-image", "upstream-image", "image-key")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"request-image",
		"prompt":"draw a small octopus"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	ImagesHandler("/images/generations", c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected image request to succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, auth, modelName := gotPath, gotAuth, gotModel
	mu.Unlock()
	if path != "/v1/images/generations" {
		t.Fatalf("unexpected upstream image path: %q", path)
	}
	if auth != "Bearer image-key" {
		t.Fatalf("unexpected upstream image auth: %q", auth)
	}
	if modelName != "upstream-image" {
		t.Fatalf("expected image model rewrite, got %q", modelName)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay log")
	}
	if logs[0].InputTokens != 5 || logs[0].OutputTokens != 2 {
		t.Fatalf("unexpected image usage log: input=%d output=%d", logs[0].InputTokens, logs[0].OutputTokens)
	}
	if logs[0].RequestEndpoint != "images_generations" || logs[0].RequestPath != "/v1/images/generations" {
		t.Fatalf("unexpected image log endpoint: endpoint=%q path=%q", logs[0].RequestEndpoint, logs[0].RequestPath)
	}
	if !strings.Contains(logs[0].ResponseContent, "image data omitted") {
		t.Fatalf("expected image response content to omit image data, got %s", logs[0].ResponseContent)
	}
}

func createRawProtocolGroup(t *testing.T, ctx context.Context, upstreamURL string, requestModel string, upstreamModel string, key string) dbmodel.Channel {
	t.Helper()
	channel := createRawProtocolChannel(t, ctx, requestModel+"-channel", upstreamURL, []string{key})
	group := dbmodel.Group{
		Name: requestModel,
		Mode: dbmodel.GroupModeFailover,
	}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: upstreamModel,
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("create group item: %v", err)
	}
	return channel
}

func createRawProtocolChannel(t *testing.T, ctx context.Context, name string, upstreamURL string, keys []string) dbmodel.Channel {
	t.Helper()
	channelKeys := make([]dbmodel.ChannelKey, 0, len(keys))
	for _, key := range keys {
		channelKeys = append(channelKeys, dbmodel.ChannelKey{Enabled: true, ChannelKey: key})
	}
	channel := dbmodel.Channel{
		Name:    name,
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{
			URL: upstreamURL,
		}},
		Keys: channelKeys,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return channel
}
