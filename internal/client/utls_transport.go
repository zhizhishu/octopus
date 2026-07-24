package client

import (
	"context"
	"net"
	"net/http"
	"sync"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttp2 "github.com/bogdanfinn/fhttp/http2"
	utls "github.com/refraction-networking/utls"
)

// utlsRoundTripper makes upstream HTTPS calls present a Chrome TLS ClientHello
// (JA3) AND a genuine-CLI HTTP/2 header order (JA4H / Akamai h2 shape) so a strict
// upstream cannot fingerprint octopus's stock Go TLS+h2 as non-browser/non-CLI
// traffic. It pools one HTTP/2 connection per host:port over a uTLS handshake and
// drives the h2 layer with bogdanfinn/fhttp (a net/http fork that honours an
// explicit header + pseudo-header order — stock x/net/http2 randomises regular
// header order per request). Plain http:// requests, and HTTPS servers that do not
// negotiate h2 over the Chrome ALPN, are delegated to `fallback` (the standard
// transport).
//
// This is opt-in and default-off: presenting a browser JA3 + fixed h2 order changes
// how EVERY upstream sees octopus, so it MUST be verified against the real the relay
// passthrough (a genuine claude/codex CLI routed through octopus) before being
// enabled in production — see docs/upstream-shape-test-sop.md.
type utlsRoundTripper struct {
	// dialContext dials the raw TCP connection (honouring any configured proxy).
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// fallback handles http:// and HTTPS servers that decline h2.
	fallback http.RoundTripper

	mu    sync.Mutex
	conns map[string]*fhttp2.ClientConn
}

func newUTLSRoundTripper(
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error),
	fallback http.RoundTripper,
) *utlsRoundTripper {
	return &utlsRoundTripper{
		dialContext: dialContext,
		fallback:    fallback,
		conns:       make(map[string]*fhttp2.ClientConn),
	}
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return t.fallback.RoundTrip(req)
	}
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)

	cc, err := t.h2ConnForHost(req.Context(), host, addr)
	if err != nil {
		return nil, err
	}
	if cc == nil {
		// Server declined h2 over the Chrome ALPN; use the standard transport. The
		// JA3/h2-order fingerprint is not applied on this rare h1 path (documented).
		return t.fallback.RoundTrip(req)
	}

	freq, err := toFHTTPRequest(req)
	if err != nil {
		return nil, err
	}
	fresp, err := cc.RoundTrip(freq)
	if err != nil {
		return nil, err
	}
	return fromFHTTPResponse(fresp, req), nil
}

// toFHTTPRequest converts a stdlib *http.Request into an *fhttp.Request, stamping the
// genuine-CLI regular-header order (by API path) and the Chrome pseudo-header order so
// fhttp emits the HEADERS frame in that exact field order. The order sentinels use
// fhttp's magic keys, which fhttp strips before hitting the wire.
func toFHTTPRequest(req *http.Request) (*fhttp.Request, error) {
	freq, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, err
	}

	h := make(fhttp.Header, len(req.Header)+2)
	for k, vv := range req.Header {
		cp := make([]string, len(vv))
		copy(cp, vv)
		h[k] = cp
	}
	// Always set the pseudo-header order so fhttp never falls back to its linked
	// (nil) transport's PseudoHeaderOrder. Set the regular-header order only when we
	// have a captured CLI baseline for this path.
	h[fhttp.PHeaderOrderKey] = chromePseudoHeaderOrder
	if order := canonicalHeaderOrderForPath(req.URL.Path); order != nil {
		h[fhttp.HeaderOrderKey] = order
	}
	freq.Header = h
	freq.ContentLength = req.ContentLength
	if req.Host != "" {
		freq.Host = req.Host
	}
	return freq, nil
}

// fromFHTTPResponse converts an *fhttp.Response back into a stdlib *http.Response.
// Response header keys are re-canonicalised (h2 delivers them lowercase) so the relay,
// which reads them with the stdlib case-insensitive-lookup Header.Get, still finds
// Content-Type/Content-Encoding/etc.
func fromFHTTPResponse(fresp *fhttp.Response, req *http.Request) *http.Response {
	stdHeader := make(http.Header, len(fresp.Header))
	for k, vv := range fresp.Header {
		stdHeader[http.CanonicalHeaderKey(k)] = vv
	}
	stdTrailer := http.Header(nil)
	if len(fresp.Trailer) > 0 {
		stdTrailer = make(http.Header, len(fresp.Trailer))
		for k, vv := range fresp.Trailer {
			stdTrailer[http.CanonicalHeaderKey(k)] = vv
		}
	}
	return &http.Response{
		Status:           fresp.Status,
		StatusCode:       fresp.StatusCode,
		Proto:            fresp.Proto,
		ProtoMajor:       fresp.ProtoMajor,
		ProtoMinor:       fresp.ProtoMinor,
		Header:           stdHeader,
		Body:             fresp.Body,
		ContentLength:    fresp.ContentLength,
		TransferEncoding: fresp.TransferEncoding,
		Close:            fresp.Close,
		Uncompressed:     fresp.Uncompressed,
		Trailer:          stdTrailer,
		Request:          req,
	}
}

// h2ConnForHost returns a pooled, usable HTTP/2 connection to addr, dialing a fresh
// uTLS (Chrome ClientHello) connection when needed. It returns (nil, nil) when the
// server negotiates a non-h2 protocol, signalling the caller to use the fallback.
// A double-checked lock keeps concurrent first-dials from racing without holding the
// mutex across the handshake.
func (t *utlsRoundTripper) h2ConnForHost(ctx context.Context, serverName, addr string) (*fhttp2.ClientConn, error) {
	t.mu.Lock()
	if cc, ok := t.conns[addr]; ok {
		if cc.CanTakeNewRequest() {
			t.mu.Unlock()
			return cc, nil
		}
		delete(t.conns, addr)
	}
	t.mu.Unlock()

	rawConn, err := t.dialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	uConn := utls.UClient(rawConn, &utls.Config{ServerName: serverName}, utls.HelloChrome_Auto)
	if err := uConn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	if uConn.ConnectionState().NegotiatedProtocol != fhttp2.NextProtoTLS {
		_ = uConn.Close()
		return nil, nil
	}

	// DisableCompression: never auto-inject "accept-encoding: gzip, deflate, br" on
	// requests that omit the header — real codex sends none, and the relay decompresses
	// any upstream Content-Encoding itself (unwrapResponseEncoding). Claude still sends
	// its explicit gzip,deflate,br,zstd value, so this only suppresses auto-injection.
	h2 := &fhttp2.Transport{DisableCompression: true}
	cc, err := h2.NewClientConn(uConn)
	if err != nil {
		_ = uConn.Close()
		return nil, err
	}

	t.mu.Lock()
	if existing, ok := t.conns[addr]; ok && existing.CanTakeNewRequest() {
		t.mu.Unlock()
		_ = cc.Close() // lost the race — keep the connection that got there first
		return existing, nil
	}
	t.conns[addr] = cc
	t.mu.Unlock()
	return cc, nil
}
