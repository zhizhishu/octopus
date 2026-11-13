package relay

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type securityBody struct {
	*bytes.Reader
	closed bool
}

func (b *securityBody) Close() error { b.closed = true; return nil }

func TestResponseZstdSecurityUpgradePreservesPayload(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	for _, payload := range [][]byte{
		[]byte("{\"id\":\"sample\",\"choices\":[{\"message\":{\"content\":\"hello 世界\"}}]}\n"),
		[]byte("event: message\ndata: {\"text\":\"hello\"}\n\ndata: [DONE]\n\n"),
	} {
		body := &securityBody{Reader: bytes.NewReader(encoder.EncodeAll(payload, nil))}
		resp := &http.Response{Body: body, Header: http.Header{
			"Content-Encoding": {"zstd"}, "Content-Length": {"123"}, "Content-Type": {"text/event-stream"},
		}}
		if err := unwrapResponseEncoding(resp); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("decoded bytes changed: got %q, want %q", got, payload)
		}
		if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "" {
			t.Fatal("compression metadata was not removed")
		}
		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal("unrelated header changed")
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if !body.closed {
			t.Fatal("underlying response body was not closed")
		}
	}
}

func TestResponseZstdRejectsTwoBlockFSEOverflow(t *testing.T) {
	// Regression frame from GHSA-fpmh-frq8-7rj8:
	// https://github.com/klauspost/compress/security/advisories/GHSA-fpmh-frq8-7rj8
	// Decode through the relay's actual streaming entry point.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38, 0x74, 0x06, 0x00, 0x42, 0xb8, 0x32, 0x0d, 0x1a, 0x00, 0x00, 0x00, 0x90, 0xaf, 0xfa, 0xff, 0x03, 0x00, 0x80, 0xff, 0x7f}
	frame = append(frame, bytes.Repeat([]byte{0xa5, 0x94, 0x52, 0x4a, 0x29}, 37)...)
	frame = append(frame, []byte{0xa5, 0x94, 0x12, 0x00, 0xad, 0x56, 0x03, 0xee, 0xff, 0xdf, 0xb3, 0x1a, 0x1c, 0x1a, 0x00, 0xe0, 0xff, 0x1f, 0x0f, 0xb1, 0x9c, 0x5a, 0xcf, 0x4d, 0xe4, 0x1d, 0x7b, 0xab, 0x99, 0x5e, 0x01, 0x51, 0xa4, 0x73, 0xd8, 0xb1, 0xf3, 0x39, 0x4b, 0x67, 0xf6, 0xab, 0x1a, 0xab, 0x1a, 0xab, 0x1a}...)
	frame = append(frame, bytes.Repeat([]byte{0x8c, 0x31, 0xc6, 0x18, 0x63}, 1365)...)
	frame = append(frame, 0x8c, 0x31)
	frame = append(frame, bytes.Repeat([]byte{0x63, 0x8c, 0x31, 0xc6, 0x18}, 1365)...)
	frame = append(frame, 0x63, 0x2c)
	frame = append(frame, bytes.Repeat([]byte{0x31, 0xc6, 0x18, 0x63, 0x8c}, 1365)...)
	frame = append(frame, 0x31, 0x46)
	frame = append(frame, bytes.Repeat([]byte{0x31, 0xc6, 0x18, 0x63, 0x8c}, 1365)...)
	frame = append(frame, 0x31, 0x06, 0x00)
	body := &securityBody{Reader: bytes.NewReader(frame)}
	resp := &http.Response{Body: body, Header: http.Header{"Content-Encoding": {"zstd"}}}
	err := unwrapResponseEncoding(resp)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil {
		t.Fatal("malformed frame must return a decode error")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("underlying response body was not closed after decode failure")
	}
}
