package relay

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"testing"
)

// TestUnwrapResponseEncodingGzipNonStream verifies FIX B2: a non-stream upstream
// reply carrying `Content-Encoding: gzip` is decompressed in place by
// unwrapResponseEncoding (which handleResponse now calls before
// outAdapter.TransformResponse). The body must read back as the original JSON and
// the now-stale Content-Encoding header must be cleared. Pure unit test — no DB,
// no network — so it can run standalone and avoid the relay package's Windows
// sqlite timeout.
func TestUnwrapResponseEncodingGzipNonStream(t *testing.T) {
	original := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(original)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	resp := &http.Response{
		Header: http.Header{},
		Body:   io.NopCloser(bytes.NewReader(buf.Bytes())),
	}
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Set("Content-Type", "application/json")

	if err := unwrapResponseEncoding(resp); err != nil {
		t.Fatalf("unwrapResponseEncoding returned error: %v", err)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read decompressed body: %v", err)
	}
	if string(got) != original {
		t.Fatalf("expected decompressed body %q, got %q", original, string(got))
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected Content-Encoding to be cleared, got %q", enc)
	}
	// Content-Type must survive; only the encoding headers are stripped.
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type to be preserved, got %q", ct)
	}
}
