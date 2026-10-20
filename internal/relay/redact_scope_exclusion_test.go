package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// These tests pin the DELIBERATE exclusions in inboundRedactExcludedRequest.
//
// They exist because that exclusion list reads like unfinished work ("out of scope this
// round"), and a future contributor could reasonably try to "complete" it by wiring
// embedding or image generation into the scanner. Both would be regressions:
//
//   - embedding: redaction corrupts the vector (a vector is the fingerprint of the
//     EXACT text sent) and the response contains no text to restore, so the caller's
//     similarity search silently starts matching the wrong documents;
//   - image/video: the sensitive bytes live inside the artefact or the pixels, which a
//     text detector cannot read — "covering" it would ship unscanned bytes while
//     claiming protection.
//
// If this policy is ever changed on purpose, change these tests together with a
// documented replacement guarantee. Do not delete them to make a build go green.

// newRedactExclusionAttempt builds an attempt that is covered by protocol and fully
// opted in (global master switch + channel flag), so the ONLY thing able to make
// inboundRedactActive false is the exclusion list under test.
func newRedactExclusionAttempt(t *testing.T, internalRequest *model.InternalLLMRequest, path string) *relayAttempt {
	t.Helper()
	req := &relayRequest{
		inboundType:     inbound.InboundTypeOpenAIChat,
		internalRequest: internalRequest,
	}
	if path != "" {
		// isVideoRequest reads the request URL path, so the video case needs a gin
		// context. Other cases leave c nil on purpose: the exclusion helper tolerates
		// nil, and building a context they do not need would only add noise.
		c, _ := gin.CreateTestContext(nil)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		req.c = c
	}
	return &relayAttempt{
		relayRequest: req,
		channel: &dbmodel.Channel{
			ID:            1,
			Name:          "exclusion-channel",
			Type:          outbound.OutboundTypeOpenAIChat,
			Enabled:       true,
			RedactEnabled: true,
		},
	}
}

// TestRedactCoveredChatRequestIsNotExcluded is the control. A plain chat request on an
// opted-in channel MUST be active; without it, a bug that excluded everything would
// make every exclusion test below pass for the wrong reason.
func TestRedactCoveredChatRequestIsNotExcluded(t *testing.T) {
	setupRedactDB(t)
	req := &model.InternalLLMRequest{
		Model:      "upstream-model",
		Messages:   []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtr("hello")}}},
		Modalities: []string{"text"},
	}
	ra := newRedactExclusionAttempt(t, req, "")
	if ra.inboundRedactExcludedRequest() {
		t.Fatalf("a plain chat request must not be excluded from redaction")
	}
	if !ra.inboundRedactActive() {
		t.Fatalf("an opted-in channel with a plain chat request must have redaction active")
	}
}

// TestRedactEmbeddingRequestStaysUnprotected locks exclusion #3.
//
// An embedding response is a VECTOR, not text: there is nothing to restore, and
// redacting the input would change the vector and silently corrupt similarity search.
func TestRedactEmbeddingRequestStaysUnprotected(t *testing.T) {
	setupRedactDB(t)
	req := &model.InternalLLMRequest{
		Model: "text-embedding-3-small",
		// Real-looking credential on purpose: a request that WOULD be redacted as chat
		// must still be skipped because it is an embedding request.
		EmbeddingInput: &model.EmbeddingInput{
			Single: strPtr("my key is sk-ant-api03-QWERTYUIOPASDFGHJKLZXCVBNM"),
		},
	}
	ra := newRedactExclusionAttempt(t, req, "")
	if !ra.inboundRedactExcludedRequest() {
		t.Fatalf("embedding requests must be excluded: redaction would corrupt the vector " +
			"and there is no response text to restore")
	}
	if ra.inboundRedactActive() {
		t.Fatalf("redaction must stay INACTIVE for an embedding request even when the " +
			"channel opted in (redacting it would silently break similarity search)")
	}
}

// TestRedactImageGenerationRequestStaysUnprotected locks exclusion #1 across every way
// oct classifies an image-generation request: model name, image modality, and tool.
func TestRedactImageGenerationRequestStaysUnprotected(t *testing.T) {
	cases := []struct {
		name string
		req  *model.InternalLLMRequest
	}{
		{name: "image model name", req: &model.InternalLLMRequest{Model: "gpt-image-1"}},
		{name: "image modality", req: &model.InternalLLMRequest{Model: "some-model", Modalities: []string{"text", "image"}}},
		{
			name: "image_generation tool",
			req: &model.InternalLLMRequest{
				Model: "some-model",
				Tools: []model.Tool{{Type: "image_generation", ImageGeneration: &model.ImageGeneration{}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupRedactDB(t)
			ra := newRedactExclusionAttempt(t, tc.req, "")
			if !ra.inboundRedactExcludedRequest() {
				t.Fatalf("image generation (%s) must be excluded: the sensitive bytes live "+
					"inside the artefact, which a text detector cannot read", tc.name)
			}
			if ra.inboundRedactActive() {
				t.Fatalf("redaction must stay INACTIVE for image generation (%s)", tc.name)
			}
		})
	}
}

// TestRedactVideoRequestStaysUnprotected locks exclusion #2, which keys off the request
// PATH (/videos) rather than the body.
func TestRedactVideoRequestStaysUnprotected(t *testing.T) {
	setupRedactDB(t)
	req := &model.InternalLLMRequest{Model: "some-video-model"}
	ra := newRedactExclusionAttempt(t, req, "/v1/videos")
	if !ra.inboundRedactExcludedRequest() {
		t.Fatalf("video requests must be excluded: the payload is media, not reducible text")
	}
	if ra.inboundRedactActive() {
		t.Fatalf("redaction must stay INACTIVE for a /videos request")
	}
}

// TestRedactUncoveredProtocolStaysInactive pins the protocol gate: even a plain text
// request is not redacted when the inbound protocol is outside the contract
// (Chat Completions / Responses / Anthropic Messages). This is the "scope by inbound
// protocol" rule that keeps Gemini-native and raw entrypoints on their original path.
func TestRedactUncoveredProtocolStaysInactive(t *testing.T) {
	setupRedactDB(t)
	req := &model.InternalLLMRequest{
		Model:    "some-model",
		Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtr("hello")}}},
	}
	ra := newRedactExclusionAttempt(t, req, "")
	ra.inboundType = inbound.InboundTypeGemini
	if ra.inboundRedactCovered() {
		t.Fatalf("an uncovered inbound protocol must not be treated as covered")
	}
	if ra.inboundRedactActive() {
		t.Fatalf("redaction must stay INACTIVE for an uncovered inbound protocol")
	}
}

// TestRedactExclusionSurvivesStickyFlag proves the exclusions are absolute: a request
// that already pinned protection (redactRequired) must still skip an excluded family.
// Protection is sticky for COVERED requests; it never overrides the scope boundary.
func TestRedactExclusionSurvivesStickyFlag(t *testing.T) {
	setupRedactDB(t)
	req := &model.InternalLLMRequest{
		Model:          "text-embedding-3-small",
		EmbeddingInput: &model.EmbeddingInput{Single: strPtr("hello")},
	}
	ra := newRedactExclusionAttempt(t, req, "")
	ra.redactRequired = true
	if ra.inboundRedactActive() {
		t.Fatalf("the sticky protection flag must not drag an excluded embedding request " +
			"into redaction: the scope boundary still applies")
	}
}
