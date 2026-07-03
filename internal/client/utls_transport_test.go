package client

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	fhttp2 "github.com/bogdanfinn/fhttp/http2"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type recordingRoundTripper struct{ called bool }

func (r *recordingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.called = true
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

// Plain http:// must bypass the uTLS/JA3 path entirely and use the fallback
// transport — the fingerprint only applies to HTTPS.
func TestUTLSRoundTripperDelegatesPlainHTTP(t *testing.T) {
	fb := &recordingRoundTripper{}
	rt := newUTLSRoundTripper(nil, fb) // dialContext is never reached on the http:// path
	req, err := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("plain http roundtrip failed: %v", err)
	}
	if !fb.called {
		t.Fatalf("plain http:// must be delegated to the fallback transport")
	}
}

// GetHTTPClientUTLSDirect builds a cached client whose transport is the uTLS
// round tripper, so the opt-in path is wired end to end.
func TestGetHTTPClientUTLSDirectCaches(t *testing.T) {
	c1, err := GetHTTPClientUTLSDirect()
	if err != nil || c1 == nil {
		t.Fatalf("uTLS direct client build failed: c=%v err=%v", c1, err)
	}
	c2, err := GetHTTPClientUTLSDirect()
	if err != nil {
		t.Fatalf("second uTLS direct client build failed: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("uTLS direct client must be cached (same instance)")
	}
	if _, ok := c1.Transport.(*utlsRoundTripper); !ok {
		t.Fatalf("uTLS direct client transport must be *utlsRoundTripper, got %T", c1.Transport)
	}
}

// TestUTLSEmitsCanonicalHeaderOrder is the T4 "real verification": it stands up an
// in-process HTTP/2 endpoint (over TCP loopback), drives a request through the exact
// fhttp path the uTLS transport uses (toFHTTPRequest -> fhttp2 ClientConn.RoundTrip),
// captures the on-wire HEADERS-frame field order via the h2 Framer, and asserts the
// regular-header order matches the genuine-CLI canonical order — regardless of the
// (scrambled) order the headers were set in on the stdlib request.
func TestUTLSEmitsCanonicalHeaderOrder(t *testing.T) {
	cases := []struct {
		name string
		path string
		want []string
	}{
		{"claude/messages", "https://anyrouter.top/v1/messages?beta=true", claudeCanonicalHeaderOrder},
		{"codex/responses", "https://anyrouter.top/v1/responses", codexCanonicalHeaderOrder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build a stdlib request carrying exactly the canonical headers, but set
			// in REVERSE order so a passing test proves fhttp reordered them.
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header = http.Header{}
			for i := len(tc.want) - 1; i >= 0; i-- {
				req.Header.Set(tc.want[i], "v")
			}

			got := roundTripCaptureRegularOrder(t, req)

			// Filter the captured order to the canonical members (fhttp may append
			// transport headers like content-length that are not part of the CLI set).
			want := make(map[string]bool, len(tc.want))
			for _, h := range tc.want {
				want[h] = true
			}
			filtered := make([]string, 0, len(tc.want))
			for _, h := range got {
				if want[h] {
					filtered = append(filtered, h)
				}
			}
			if !reflect.DeepEqual(filtered, tc.want) {
				t.Fatalf("HEADERS-frame regular-header order mismatch:\n got=%v\nwant=%v\n(full captured=%v)", filtered, tc.want, got)
			}
		})
	}
}

// roundTripCaptureRegularOrder runs req through fhttp2 over a loopback h2 connection
// and returns the ordered list of REGULAR (non-pseudo) header field names as the
// server received them in the HEADERS frame.
func roundTripCaptureRegularOrder(t *testing.T, req *http.Request) []string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	orderCh := make(chan []string, 1)
	go h2CaptureServer(ln, orderCh)

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tr := &fhttp2.Transport{}
	cc, err := tr.NewClientConn(clientConn)
	if err != nil {
		t.Fatalf("NewClientConn: %v", err)
	}
	freq, err := toFHTTPRequest(req)
	if err != nil {
		t.Fatalf("toFHTTPRequest: %v", err)
	}
	resp, err := cc.RoundTrip(freq)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case order := <-orderCh:
		return order
	default:
		t.Fatalf("server captured no HEADERS frame")
		return nil
	}
}

// h2CaptureServer accepts one connection and speaks just enough HTTP/2 to receive the
// client's request HEADERS frame (capturing field order) and return a 200, so the
// client RoundTrip completes.
func h2CaptureServer(ln net.Listener, orderCh chan<- []string) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		return
	}
	if string(preface) != http2.ClientPreface {
		return
	}

	fr := http2.NewFramer(conn, conn)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := fr.WriteSettings(); err != nil {
		return
	}

	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return
		}
		switch mf := f.(type) {
		case *http2.SettingsFrame:
			if !mf.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.MetaHeadersFrame:
			order := make([]string, 0, len(mf.Fields))
			for _, hf := range mf.Fields {
				if !strings.HasPrefix(hf.Name, ":") {
					order = append(order, hf.Name)
				}
			}
			orderCh <- order
			var buf bytes.Buffer
			enc := hpack.NewEncoder(&buf)
			_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
			_ = fr.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      mf.StreamID,
				BlockFragment: buf.Bytes(),
				EndHeaders:    true,
				EndStream:     true,
			})
			return
		}
	}
}
