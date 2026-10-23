package relay

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
)

// fakeGeminiKey is deliberately fake. This test pins that the *shape* of a
// Gemini channel key in a URL query never survives into any log surface.
const fakeGeminiKey = "AIzaSecret123"

// geminiURLKeyError reproduces what Go's net/http transport actually returns
// when a Gemini channel request fails: the error quotes the full request URL,
// including the ?key=<channel key> query parameter.
func geminiURLKeyError() error {
	return fmt.Errorf(
		`Post "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=%s": dial tcp 142.250.0.1:443: connect: connection refused`,
		fakeGeminiKey,
	)
}

// TestGeminiURLKeyRedactedBeforeLogging pins that the API key Gemini carries in
// the URL query (?key=..., see transformer/outbound/gemini/messages.go and
// relay/gemini_images_bridge.go) is scrubbed before it reaches relay_logs.error
// or a per-attempt audit message. Unrelated query parameters and the rest of the
// error text must be preserved — this is a targeted scrub, not a blanket wipe.
func TestGeminiURLKeyRedactedBeforeLogging(t *testing.T) {
	err := geminiURLKeyError()

	// relay_logs.error is filled from errSafeMessage in metrics.go, images.go and
	// raw_protocol.go, so that helper is the persistence gate.
	logged := errSafeMessage(err)
	if strings.Contains(logged, fakeGeminiKey) {
		t.Fatalf("relay_logs.error leaked gemini key: %s", logged)
	}
	if !strings.Contains(logged, "key=***") {
		t.Fatalf("expected redacted key=*** in relay error, got: %s", logged)
	}
	for _, want := range []string{
		"alt=sse",
		"/v1beta/models/gemini-2.5-flash:streamGenerateContent",
		"connection refused",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("redaction broke unrelated text, missing %q in: %s", want, logged)
		}
	}

	// Every per-attempt error is written through AttemptSpan.End, which must
	// redact before appending to the attempt list.
	group := model.Group{Items: []model.GroupItem{{ChannelID: 1, ModelName: "gemini-2.5-flash", Priority: 0}}}
	it := balancer.NewIteratorWithSessionKey(group, 1, "gemini-2.5-flash", "")
	if !it.Next() {
		t.Fatal("expected the single candidate to be available")
	}
	span := it.StartAttempt(1, 0, "gemini-channel")
	span.End(model.AttemptFailed, 502, err.Error())

	attempts := it.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", len(attempts))
	}
	if strings.Contains(attempts[0].Msg, fakeGeminiKey) {
		t.Fatalf("attempt error leaked gemini key: %s", attempts[0].Msg)
	}
	if !strings.Contains(attempts[0].Msg, "key=***") {
		t.Fatalf("expected redacted key=*** in attempt error, got: %s", attempts[0].Msg)
	}
}
