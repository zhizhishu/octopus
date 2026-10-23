package relay

// Two audit gaps around the redaction module:
//
//  1. The exclusion list in inboundRedactExclusionReason keys off request SHAPE, and the
//     client controls that shape: an ordinary chat body that merely declares an "image"
//     modality (or carries an image_generation tool, or targets /videos) is exempted from
//     scanning. That is a client-side off switch for credential redaction, so it MUST
//     leave a trace — otherwise a request can ship cleartext with nothing anywhere
//     recording why it was not masked.
//  2. The image bridge logs the UPSTREAM image URL when a fetch fails; those URLs carry
//     signed query parameters (?key= / ?sig= / ?token=), so the log line must be redacted.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// captureRelayLog swaps the process logger (zap -> os.Stdout in production) for a buffer
// so a test can read what would have hit stdout. Restored by t.Cleanup.
func captureRelayLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := log.Logger
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(zapcore.EncoderConfig{MessageKey: "msg"}),
		zapcore.AddSync(buf),
		zapcore.DebugLevel,
	)
	log.Logger = zap.New(core).Sugar()
	t.Cleanup(func() { log.Logger = previous })
	return buf
}

// TestRedactClientShapedRequestLeavesAuditTrace: a chat request that the CLIENT marks as
// image generation is not scanned; that must be recorded, with the reason and the request
// text never echoed.
func TestRedactClientShapedRequestLeavesAuditTrace(t *testing.T) {
	setupRedactDB(t)
	buf := captureRelayLog(t)

	req := &model.InternalLLMRequest{
		Model:    "gpt-4o",
		Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtr("my password is hunter2")}}},
		// Client-supplied, no upstream involvement: enough to silence redaction.
		Modalities: []string{"image"},
	}
	ra := newRedactExclusionAttempt(t, req, "")
	if ra.inboundRedactActive() {
		t.Fatal("precondition: an image-modality request must stay outside the redaction scope")
	}
	if err := ra.applyInboundRedaction(); err != nil {
		t.Fatalf("applyInboundRedaction must stay a no-op for an excluded request, got %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "credential redaction skipped for this request") {
		t.Fatalf("a client-shaped exclusion must leave an audit trace, stdout had: %q", out)
	}
	if !strings.Contains(out, "image_generation") {
		t.Fatalf("the trace must name which exclusion applied, got: %q", out)
	}
	if !strings.Contains(out, "exclusion-channel") {
		t.Fatalf("the trace must name the channel the operator opted in, got: %q", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("the audit trace must never echo request text, got: %q", out)
	}
}

// TestRedactSkipTraceSilentWhenModuleNotOptedIn is the control: with redaction configured
// OFF (master switch + channel flag) the skip is an operator setting, not a client action,
// so it must not spray a warning on every image/video request.
func TestRedactSkipTraceSilentWhenModuleNotOptedIn(t *testing.T) {
	setupRedactDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRedactEnabled, "false"); err != nil {
		t.Fatalf("disable redaction: %v", err)
	}
	buf := captureRelayLog(t)

	req := &model.InternalLLMRequest{
		Model:      "gpt-4o",
		Messages:   []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtr("hello")}}},
		Modalities: []string{"image"},
	}
	ra := newRedactExclusionAttempt(t, req, "")
	ra.channel.RedactEnabled = false
	if err := ra.applyInboundRedaction(); err != nil {
		t.Fatalf("applyInboundRedaction: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "credential redaction skipped") {
		t.Fatalf("a configuration-off skip must not be reported as a client-side bypass, got: %q", out)
	}
}

// TestRedactCoveredRequestLeavesNoSkipTrace is the other control: a plain chat request is
// actually scanned, so nothing may claim it was skipped.
func TestRedactCoveredRequestLeavesNoSkipTrace(t *testing.T) {
	setupRedactDB(t)
	buf := captureRelayLog(t)

	req := &model.InternalLLMRequest{
		Model:      "gpt-4o",
		Messages:   []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtr("hello")}}},
		Modalities: []string{"text"},
		RawRequest: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`),
	}
	ra := newRedactExclusionAttempt(t, req, "")
	if err := ra.applyInboundRedaction(); err != nil {
		t.Fatalf("applyInboundRedaction: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "credential redaction skipped") {
		t.Fatalf("a covered request must not be reported as skipped, got: %q", out)
	}
}

// TestImageBridgeFetchFailureLogRedactsSignedURL pins that the upstream image URL logged by
// the base64 bridge carries no query credential into the log/stdout.
func TestImageBridgeFetchFailureLogRedactsSignedURL(t *testing.T) {
	setupRedactDB(t)

	previous := allowPrivateImageDownloadHosts
	allowPrivateImageDownloadHosts = true
	t.Cleanup(func() { allowPrivateImageDownloadHosts = previous })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	buf := captureRelayLog(t)

	ra := &relayAttempt{
		relayRequest: &relayRequest{inboundType: inbound.InboundTypeOpenAIResponse},
		channel:      &dbmodel.Channel{ID: 7, Name: "image-channel"},
	}
	signedURL := upstream.URL + "/gen/img.png?key=SECRET123&sig=SIGNED456&expires=99"
	response := &model.InternalLLMResponse{
		Choices: []model.Choice{{
			Message: &model.Message{
				Content: model.MessageContent{
					MultipleContent: []model.MessageContentPart{{
						Type:     "image_url",
						ImageURL: &model.ImageURL{URL: signedURL},
					}},
				},
			},
		}},
	}
	if err := ra.ensureResponsesImageResultsBase64(context.Background(), response); err != nil {
		t.Fatalf("a failed image fetch must not fail the response: %v", err)
	}
	if got := response.Choices[0].Message.Content.MultipleContent[0].ImageURL.URL; got != signedURL {
		t.Fatalf("a failed fetch must leave the original URL in place, got %q", got)
	}

	out := buf.String()
	if !strings.Contains(out, "could not fetch") {
		t.Fatalf("expected the fetch failure to be logged, got: %q", out)
	}
	for _, secret := range []string{"SECRET123", "SIGNED456"} {
		if strings.Contains(out, secret) {
			t.Fatalf("image URL query credential %q leaked into the log: %q", secret, out)
		}
	}
	if !strings.Contains(out, "key=***") {
		t.Fatalf("the URL should stay readable with the credential masked, got: %q", out)
	}
}
