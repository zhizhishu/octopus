package relay

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// URLSecrets-only call sites (each passes the raw URL or err.Error() through
// xredact.URLSecrets, not the broader Secrets scrubber):
//
//	upstream_error.go errSafeMessage, both branches — TestErrSafeMessageRedactsAPIKeyQuery
//	image_generation_bridge.go fetch-failure warn   — TestImageBridgeLogRedactsAPIKeyQuery
//	relay.go sendRequest warn                        — TestSendRequestWarnLogRedactsAPIKeyQuery
//	relay.go forward TransformRequest warn           — same URLSecrets(err.Error()) call; the
//	  "failed to parse … ?apikey=" shape is pinned in xredact.TestURLSecretsTable and driven
//	  through errSafeMessage below, which is where that wrapped error reaches API users.

func TestErrSafeMessageRedactsAPIKeyQuery(t *testing.T) {
	const (
		atStart = "channel-apikey-secret"
		inQuery = "channel-apiKey-secret"
	)
	plain := fmt.Errorf(
		`Post "https://upstream.example/v1/models?apikey=%s&foo=1&apiKey=%s": dial tcp 203.0.113.1:443: connect: connection refused`,
		atStart, inQuery,
	)
	assertAPIKeyQueryRedacted(t, errSafeMessage(plain), atStart, inQuery)
	if got := errSafeMessage(plain); !strings.Contains(got, "foo=1") || !strings.Contains(got, "connection refused") {
		t.Fatalf("redaction changed the rest of the transport error: %s", got)
	}

	// upstreamError.Error() is the message field. A transport failure wrapped as
	// one still has to be scrubbed on the branch that reads upErr.Error().
	wrapped := &upstreamError{message: plain.Error()}
	assertAPIKeyQueryRedacted(t, errSafeMessage(wrapped), atStart, inQuery)

	// The warn at relay.go "failed to create request" logs err.Error() only.
	// The error returned to the caller is that same string wrapped, and
	// errSafeMessage is what API users see when status passthrough is on.
	createErr := fmt.Errorf(
		`failed to create request: parse "https://upstream.example/v1?tokens=12&keyword=cat&monkey=see&apikey=%s": invalid URL`,
		atStart,
	)
	got := errSafeMessage(createErr)
	assertAPIKeyQueryRedacted(t, got, atStart)
	for _, keep := range []string{"tokens=12", "keyword=cat", "monkey=see", "failed to create request"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("errSafeMessage dropped %q: %s", keep, got)
		}
	}

	jsonErr := fmt.Errorf(`{"url":"https://upstream.example/v1?foo=1&apiKey=%s"}`, inQuery)
	assertAPIKeyQueryRedacted(t, errSafeMessage(jsonErr), inQuery)

	if errSafeMessage(nil) != "" {
		t.Fatal("nil error should produce an empty safe message")
	}
}

func TestImageBridgeLogRedactsAPIKeyQuery(t *testing.T) {
	buf := captureRelayLog(t)
	// allowPrivateImageDownloadHosts stays false, so the loopback dial is refused
	// before any bytes move and the fetch-failure warn still runs.
	ra := &relayAttempt{
		relayRequest: &relayRequest{inboundType: inbound.InboundTypeOpenAIResponse},
		channel:      &dbmodel.Channel{ID: 7, Name: "image-channel"},
	}
	const (
		atStart = "img-apikey-secret"
		atEnd   = "img-apiKey-secret"
	)
	u := "http://127.0.0.1:1/gen/img.png?apikey=" + atStart + "&x=1&apiKey=" + atEnd
	response := &model.InternalLLMResponse{
		Choices: []model.Choice{{
			Message: &model.Message{
				Content: model.MessageContent{
					MultipleContent: []model.MessageContentPart{{
						Type:     "image_url",
						ImageURL: &model.ImageURL{URL: u},
					}},
				},
			},
		}},
	}
	if err := ra.ensureResponsesImageResultsBase64(context.Background(), response); err != nil {
		t.Fatalf("a failed image fetch must not fail the response: %v", err)
	}
	if got := response.Choices[0].Message.Content.MultipleContent[0].ImageURL.URL; got != u {
		t.Fatalf("a failed fetch must leave the original URL in place, got %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "could not fetch") {
		t.Fatalf("expected the fetch failure to be logged, got: %q", out)
	}
	assertAPIKeyQueryRedacted(t, out, atStart, atEnd)
	if !strings.Contains(out, "x=1") {
		t.Fatalf("harmless query param was dropped from the log: %q", out)
	}
}

func TestSendRequestWarnLogRedactsAPIKeyQuery(t *testing.T) {
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamHeaderTimeoutSec, "0"); err != nil {
		t.Fatalf("disable header timeout: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	const (
		inMiddle = "send-apikey-secret"
		atEnd    = "send-apiKey-secret"
	)
	raw := fmt.Sprintf("http://%s/v1?model=m&apikey=%s&apiKey=%s", addr, inMiddle, atEnd)
	req, err := http.NewRequest(http.MethodPost, raw, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	buf := captureRelayLog(t)
	ra := &relayAttempt{channel: &dbmodel.Channel{Name: "query-redact"}}
	_, sendErr := ra.sendRequest(req)
	if sendErr == nil {
		t.Fatal("expected the dial to fail")
	}
	out := buf.String()
	if !strings.Contains(out, "failed to send request") {
		t.Fatalf("expected the send-failure warn, got: %q", out)
	}
	assertAPIKeyQueryRedacted(t, out, inMiddle, atEnd)
	if !strings.Contains(out, "model=m") {
		t.Fatalf("harmless query param was dropped from the warn log: %q", out)
	}
}

func assertAPIKeyQueryRedacted(t *testing.T, got string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(got, secret) {
			t.Fatalf("query credential %q leaked: %s", secret, got)
		}
	}
	if strings.Contains(got, "apikey=") && !strings.Contains(got, "apikey=***") && !strings.Contains(got, "apiKey=***") {
		t.Fatalf("expected an apikey/apiKey value to be masked, got: %s", got)
	}
}
