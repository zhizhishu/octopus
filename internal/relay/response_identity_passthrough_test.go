package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func passthroughTestContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", path, strings.NewReader(`{"model":"sent-B"}`))
	c.Request.Header.Set("X-Identity-Diagnostic", "true")
	return c, w
}

func TestPassthroughIdentityJSONAndOpaque(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			c, w := passthroughTestContext(path)
			headers := c.Request.Header.Clone()
			var evidence passthroughIdentityEvidence
			bindPassthroughIdentity(c, "Public-A[Exact]", &evidence)
			body := `{"id":"resp_real-C","model":"real-C","system_fingerprint":"fp_private","response":{"model":"real-C","id":"cursor-real-C"},"tools":[{"model":"tool-C","signature":"sig"}],"n":9007199254740993,"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`
			u, written, id, err := proxyNonStreamWithResponseID(c, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))})
			if err != nil || !written || id != "resp_real-C" {
				t.Fatalf("proxy: written=%v id=%q err=%v", written, id, err)
			}
			out := w.Body.String()
			for _, want := range []string{`"model":"Public-A[Exact]"`, `"id":"resp_real-C"`, `"id":"cursor-real-C"`, `"model":"tool-C"`, `"n":9007199254740993`} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %s: %s", want, out)
				}
			}
			if strings.Contains(out, "fp_private") || evidence.Model != "real-C" || evidence.Fingerprint != "fp_private" {
				t.Fatalf("identity evidence: %s %+v", out, evidence)
			}
			if u == nil || u.TotalTokens != 18 {
				t.Fatalf("usage: %+v", u)
			}
			req, _ := io.ReadAll(c.Request.Body)
			if string(req) != `{"model":"sent-B"}` || !reflect.DeepEqual(headers, c.Request.Header) {
				t.Fatal("upstream request/header changed")
			}
		})
	}
	c, w := passthroughTestContext("/v1/audio/speech")
	bindPassthroughIdentity(c, "A", nil)
	binary := []byte{0, 1, 255, 'm', 'o', 'd', 'e', 'l', 0}
	_, _, err := proxyOpaqueResponse(c, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(bytes.NewReader(binary))})
	if err != nil || !bytes.Equal(w.Body.Bytes(), binary) {
		t.Fatalf("opaque changed: %v", err)
	}
}

func TestPassthroughIdentityDeepContentPreserved(t *testing.T) {
	content := strings.Repeat(`{"nested":`, 129) + `{"model":"tool-C","signature":"sig-C","n":9007199254740993}` + strings.Repeat("}", 129)
	body := []byte(`{"model":"real-C","tools":[` + content + `]}`)
	if !json.Valid(body) {
		t.Fatal("deep fixture must be valid JSON")
	}
	out, err := rewriteIdentityJSON(body, passthroughIdentity{alias: "public-A"})
	want := []byte(`{"model":"public-A","tools":[` + content + `]}`)
	if err != nil || !bytes.Equal(out, want) {
		t.Fatalf("deep content changed: err=%v", err)
	}

	for _, depth := range []int{identityJSONMaxDepth, identityJSONMaxDepth + 1} {
		payload := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
		out, err := rewriteIdentityJSON(payload, passthroughIdentity{alias: "A"})
		if depth == identityJSONMaxDepth {
			if !json.Valid(payload) || err != nil || !bytes.Equal(out, payload) {
				t.Fatalf("maximum valid depth rejected: %v", err)
			}
		} else if json.Valid(payload) || err == nil || out != nil {
			t.Fatalf("above hard cap accepted: err=%v output=%d", err, len(out))
		}
	}
}

func TestPassthroughTaskFailureMessagesKeepStoredEvidence(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := database.AutoMigrate(&model.ImageTask{}, &model.VideoTask{}); err != nil {
		t.Fatal(err)
	}
	image := model.ImageTask{TaskID: "failure-image", Status: model.ImageTaskStatusFailed, ErrorMessage: "private-c provider/model failure", Model: "sent-B", RequestModel: "public-A"}
	video := model.VideoTask{UpstreamTaskID: "failure-video", Status: model.VideoTaskFailure, FailReason: "private-c provider/model failure", Model: "sent-B", RequestModel: "public-A"}
	if err := database.Create(&image).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	native := buildImageTaskResponse(image)
	if native.Status != model.ImageTaskStatusFailed || native.Error == nil || native.Error.Code != "image_generation_failed" || native.Error.Message != "image generation failed" {
		t.Fatalf("native failure changed: %+v", native)
	}
	mj := midjourneyRowView(model.MidjourneyTask{UpstreamTaskID: "failure-mj", Model: "sent-B", RequestModel: "public-A"})
	applyMidjourneyUpstream(&mj, midjourneyUpstreamTask{Status: "FAILURE", FailReason: "private-c provider/model failure"})
	for _, public := range []any{native, imageTaskView(image), videoTaskView(video), mj} {
		body, err := json.Marshal(public)
		if err != nil || bytes.Contains(body, []byte("private-c")) {
			t.Fatalf("failure leaked: %s %v", body, err)
		}
	}
	var storedImage model.ImageTask
	var storedVideo model.VideoTask
	if err := database.First(&storedImage, image.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.First(&storedVideo, video.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedImage.ErrorMessage != image.ErrorMessage || storedVideo.FailReason != video.FailReason {
		t.Fatal("stored failure evidence changed")
	}
}

func TestPassthroughMediaErrorSlots(t *testing.T) {
	original := []byte(`{"status":"failed","id":"task_C","code":42,"fail_reason":"private-c","failReason":"private-c","error":{"code":"unchanged","message":"private-c"},"description":"private-c","tools":[{"error":{"message":"tool-content"}}],"data":{"error":{"message":"business-content"}}}`)
	out, err := rewriteIdentityJSON(original, passthroughIdentity{alias: "A", mediaEnvelopes: true, failureDescription: true})
	if err != nil || bytes.Contains(out, []byte("private-c")) {
		t.Fatalf("media failure leaked: %s %v", out, err)
	}
	for _, want := range []string{`"status":"failed"`, `"id":"task_C"`, `"code":42`, `"code":"unchanged"`, `"message":"tool-content"`, `"message":"business-content"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("non-error content changed: %s", out)
		}
	}
	success := []byte(`{"status":"success","description":"successful image description","content":{"failReason":"content text"}}`)
	out, err = rewriteIdentityJSON(success, passthroughIdentity{alias: "A", mediaEnvelopes: true})
	if err != nil || !bytes.Equal(out, success) {
		t.Fatalf("successful description changed: %s %v", out, err)
	}
	c, w := passthroughTestContext("/mj/task/task_C/fetch")
	c.Request = c.Request.WithContext(WithOriginalUpstreamIdentity(c.Request.Context()))
	bindPassthroughIdentity(c, "A", nil)
	writeVideoResponse(c, http.StatusOK, "application/json", original)
	if !bytes.Equal(w.Body.Bytes(), original) {
		t.Fatal("diagnostic error evidence changed")
	}
	cached, err := prepareIdentityBody(io.NopCloser(strings.NewReader(`{"model":"C","bad":`)), passthroughIdentity{alias: "A"})
	if cached != nil {
		cached.Close()
	}
	if !isPassthroughIdentityError(err) {
		t.Fatalf("truncated EOF lost local error type: %T %v", err, err)
	}
}

func TestPassthroughIdentityNullAndBoundaries(t *testing.T) {
	var evidence passthroughIdentityEvidence
	body := []byte(`{"model":null,"system_fingerprint":null,"message":{"model":"C","content":[{"model":"content-C"}]},"data":{"model":"business-C"},"tools":[{"model":"tool-C"}],"model":"C2"}`)
	out, err := rewriteIdentityJSON(body, passthroughIdentity{alias: "A", evidence: &evidence})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Model != "C" || bytes.Contains(out, []byte("system_fingerprint")) || !bytes.Contains(out, []byte(`"model":"business-C"`)) || !bytes.Contains(out, []byte(`"model":"content-C"`)) {
		t.Fatalf("scope: %s %+v", out, evidence)
	}
	for _, bad := range []string{`{"model":"C",}`, `{"model":123}`, `{"model":"C"}garbage`, `{"x":[1,]}`, `{"x":"\q"}`} {
		if _, err := rewriteIdentityJSON([]byte(bad), passthroughIdentity{alias: "A"}); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	c, w := passthroughTestContext("/v1/images/generations")
	bindPassthroughIdentity(c, "A", nil)
	_, written, _, err := proxyNonStreamWithResponseID(c, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"C",broken`))})
	if err == nil || written || w.Body.Len() != 0 {
		t.Fatalf("invalid JSON leaked: %v %s", err, w.Body.String())
	}
}

func TestPassthroughIdentitySSEMultilineTerminal(t *testing.T) {
	setupRelayErrorDB(t)
	c, w := passthroughTestContext("/v1/responses")
	var evidence passthroughIdentityEvidence
	bindPassthroughIdentity(c, "A", &evidence)
	body := "id: cursor-C\nevent: response.completed\ndata: {\ndata: \"response\":{\"id\":\"resp_C\",\"model\":\"C\",\"system_fingerprint\":\"fp_C\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n" + "data: {\"model\":\"tail-C\"}\n\n"
	u, _, id, err := proxySSEWithOptions(context.Background(), c, &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, 0, nil, proxySSEOptions{ScanAllData: true, CaptureResponsesID: true, StopOnResponsesTerminal: true})
	if err != nil || id != "resp_C" || u == nil || u.TotalTokens != 5 {
		t.Fatalf("stream: %q %+v %v", id, u, err)
	}
	if !strings.Contains(w.Body.String(), "id: cursor-C") || strings.Contains(w.Body.String(), "tail-C") || strings.Contains(w.Body.String(), "fp_C") || evidence.Model != "C" || evidence.Fingerprint != "fp_C" {
		t.Fatalf("stream identity: %s %+v", w.Body.String(), evidence)
	}
	// A complete terminal data line must not wait for the next blank line.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := readIdentitySSEEvent(bufio.NewReader(pr), passthroughIdentity{alias: "A"}, true)
		done <- err
	}()
	_, _ = io.WriteString(pw, "event: response.completed\ndata: {\"response\":{\"id\":\"resp_C\",\"model\":\"C\"}}\n")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = pw.Close()
		t.Fatal("terminal waited for blank line")
	}
	_ = pw.Close()
	_ = pr.Close()
}

func TestPassthroughIdentityInvalidResponseDoesNotReroute(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"private-C","bad":`)
	}))
	defer upstream.Close()
	for _, name := range []string{"identity-first", "identity-second"} {
		createRawProtocolChannelWithModel(t, ctx, name, upstream.URL, []string{"test-key"}, "sent-B", map[string]string{"public-A": "sent-B"})
	}
	c, w := passthroughTestContext("/v1/rerank")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"model":"public-A","query":"keep","documents":["same"]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	RawProtocolHandler(RawProtocolOptions{Endpoint: "/rerank", Name: "rerank"}, c)
	if calls.Load() != 1 || w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "private-C") {
		t.Fatalf("reroute/leak: calls=%d status=%d body=%s", calls.Load(), w.Code, w.Body.String())
	}
}

func TestPassthroughIdentityBodyLimit(t *testing.T) {
	t.Setenv("OCTOPUS_IMAGES_BODY_MAX_MB", "1")
	t.Setenv("OCTOPUS_IMAGES_BODY_MEMORY_THRESHOLD_MB", "1")
	t.Setenv("OCTOPUS_IMAGES_BODY_TMP_DIR", t.TempDir())
	c, w := passthroughTestContext("/v1/images/generations")
	bindPassthroughIdentity(c, "A", nil)
	body := io.NopCloser(io.MultiReader(strings.NewReader(`{"model":"C","data":[{"b64_json":"`), strings.NewReader(strings.Repeat("A", 2<<20)), strings.NewReader(`"}]}`)))
	_, written, _, err := proxyNonStreamWithResponseID(c, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: body})
	if err == nil || written || w.Body.Len() != 0 {
		t.Fatalf("oversize leaked: written=%v err=%v", written, err)
	}
}

func TestPassthroughIdentityInternalDiagnostic(t *testing.T) {
	c, w := passthroughTestContext("/v1/images/generations")
	c.Request = c.Request.WithContext(WithOriginalUpstreamIdentity(c.Request.Context()))
	bindPassthroughIdentity(c, "A", nil)
	original := `{"model":"C","system_fingerprint":"fp_C"}`
	_, _, err := proxyNonStream(c, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(original))})
	if err != nil || w.Body.String() != original {
		t.Fatalf("diagnostic changed: %s %v", w.Body.String(), err)
	}
}

func TestPassthroughTaskAliasMigration(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	for _, table := range []string{"video_tasks", "midjourney_tasks"} {
		if err := database.Exec("CREATE TABLE " + table + " (id integer PRIMARY KEY, model text)").Error; err != nil {
			t.Fatal(err)
		}
		if err := database.Exec("INSERT INTO " + table + " (id,model) VALUES (1,'sent-B')").Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.AutoMigrate(&model.VideoTask{}, &model.MidjourneyTask{}); err != nil {
		t.Fatal(err)
	}
	var video model.VideoTask
	if err := database.First(&video, 1).Error; err != nil {
		t.Fatal(err)
	}
	var mj model.MidjourneyTask
	if err := database.First(&mj, 1).Error; err != nil {
		t.Fatal(err)
	}
	if video.Model != "sent-B" || mj.Model != "sent-B" || video.RequestModel != "" || mj.RequestModel != "" {
		t.Fatalf("legacy altered: %+v %+v", video, mj)
	}
	for _, table := range []string{"video_tasks", "midjourney_tasks"} {
		if err := database.Table(table).Where("id = ?", 1).Update("request_model", "public-A").Error; err != nil {
			t.Fatal(err)
		}
		var old struct {
			ID    int64
			Model string
		}
		if err := database.Table(table).First(&old).Error; err != nil {
			t.Fatal(err)
		}
		if old.Model != "sent-B" {
			t.Fatalf("old code reader changed: %+v", old)
		}
		if err := database.Exec("UPDATE " + table + " SET request_model=NULL WHERE id=1").Error; err != nil {
			t.Fatalf("nullable alias required: %v", err)
		}
	}
}

func TestPassthroughTaskAliasSurvivesOwnerReload(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	recordVideoTaskRow(ctx, []string{"identity-video"}, 11, 12, "sent-B", 1, 2, "", map[string]any{"model": "public-A"})
	entry, ok := videoTaskOwnerFromDB(ctx, "identity-video")
	if !ok || entry.model != "sent-B" || entry.requestModel != "public-A" {
		t.Fatalf("video reload: %+v %v", entry, ok)
	}
	recordMidjourneyTaskOwner(ctx, "identity-mj", midjourneyTaskEntry{channelID: 11, channelKeyID: 12, model: "sent-B", requestModel: "public-A", ownerTokenID: 1, ownerUserID: 2}, "IMAGINE")
	midjourneyTaskStore.Lock()
	delete(midjourneyTaskStore.items, "identity-mj")
	midjourneyTaskStore.Unlock()
	mj, ok := midjourneyTaskOwner(ctx, "identity-mj")
	if !ok || mj.model != "sent-B" || mj.requestModel != "public-A" {
		t.Fatalf("MJ reload: %+v %v", mj, ok)
	}
	for _, v := range []TaskView{videoTaskView(model.VideoTask{Model: "B", RequestModel: "A"}), imageTaskView(model.ImageTask{Model: "B", RequestModel: "A"}), midjourneyRowView(model.MidjourneyTask{Model: "B", RequestModel: "A"})} {
		if v.Model != "A" {
			t.Fatalf("task model: %+v", v)
		}
	}
	old, _ := json.Marshal(videoTaskView(model.VideoTask{Model: "private-B"}))
	if bytes.Contains(old, []byte(`"model"`)) || bytes.Contains(old, []byte("private-B")) {
		t.Fatalf("legacy identity: %s", old)
	}
}
