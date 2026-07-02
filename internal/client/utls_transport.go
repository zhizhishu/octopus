package client

import (
	"context"
	"net"
	"net/http"
	"sync"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// utlsRoundTripper makes upstream HTTPS calls present a Chrome TLS ClientHello
// (JA3) so a strict upstream cannot fingerprint octopus's Go net/http handshake as
// non-browser/non-CLI traffic. It pools one HTTP/2 connection per host:port. Plain
// http:// requests, and HTTPS servers that do not negotiate h2 over the Chrome
// ALPN, are delegated to `fallback` (the standard transport).
//
// This is opt-in and default-off: presenting a browser JA3 changes how EVERY
// upstream sees octopus, so it MUST be verified against the real anyrouter
// passthrough (a genuine claude/codex CLI routed through octopus) before being
// enabled in production — see docs/anyrouter-shape-test-sop.md.
type utlsRoundTripper struct {
	// dialContext dials the raw TCP connection (honouring any configured proxy).
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// fallback handles http:// and HTTPS servers that decline h2.
	fallback http.RoundTripper

	mu    sync.Mutex
	conns map[string]*http2.ClientConn
}

func newUTLSRoundTripper(
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error),
	fallback http.RoundTripper,
) *utlsRoundTripper {
	return &utlsRoundTripper{
		dialContext: dialContext,
		fallback:    fallback,
		conns:       make(map[string]*http2.ClientConn),
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
		// JA3 fingerprint is not applied on this rare h1 path (documented limitation).
		return t.fallback.RoundTrip(req)
	}
	return cc.RoundTrip(req)
}

// h2ConnForHost returns a pooled, usable HTTP/2 connection to addr, dialing a fresh
// uTLS (Chrome ClientHello) connection when needed. It returns (nil, nil) when the
// server negotiates a non-h2 protocol, signalling the caller to use the fallback.
// A double-checked lock keeps concurrent first-dials from racing without holding the
// mutex across the handshake (the reference implementation's cond.Wait had a
// double-unlock; this avoids the pattern entirely).
func (t *utlsRoundTripper) h2ConnForHost(ctx context.Context, serverName, addr string) (*http2.ClientConn, error) {
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
	if uConn.ConnectionState().NegotiatedProtocol != http2.NextProtoTLS {
		_ = uConn.Close()
		return nil, nil
	}

	h2 := &http2.Transport{}
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
