package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	model "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// TestHandleNonStreamResponseAsStreamDecompressesGzip locks the fallback-path
// decompression contract: when an anthropic stream request is retried as
// non-stream (upstream answered the stream attempt with 502/503/504/520) and
// the upstream then answers 200 with a gzip-compressed JSON body, the fallback
// must decompress it before the outbound parses. The claude outbound advertises
// the genuine claude-cli Accept-Encoding and the shared transport has
// DisableCompression=true, so nothing else decodes the body; without the unwrap
// call the anthropic TransformResponse sees raw gzip bytes, fails to parse, and
// the whole stream-to-non-stream fallback feature silently breaks. Pure unit
// test — no DB, no network — so it can run standalone and avoid the relay
// package's Windows sqlite timeout.
func TestHandleNonStreamResponseAsStreamDecompressesGzip(t *testing.T) {
	original := `{"id":"msg_fb","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"fallback ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(original)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(buf.Bytes())),
	}
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Set("Content-Type", "application/json")

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	stream := true
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeAnthropic,
			inAdapter:       inbound.Get(inbound.InboundTypeAnthropic),
			internalRequest: &model.InternalLLMRequest{Model: "claude-opus-5-5", Stream: &stream},
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic},
	}

	if err := ra.handleNonStreamResponseAsStream(context.Background(), resp, outbound.Get(outbound.OutboundTypeAnthropic)); err != nil {
		t.Fatalf("fallback should parse the compressed 200 body, got error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "fallback ok") {
		trimmed := body
		if len(trimmed) > 300 {
			trimmed = trimmed[:300]
		}
		t.Fatalf("expected SSE body to carry the decompressed text, got: %q", trimmed)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected Content-Encoding cleared after unwrap, got %q", enc)
	}
}
