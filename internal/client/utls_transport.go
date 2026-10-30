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

// utlsRoundTripper makes upstream HTTPS calls present a genuine-CLI TLS
// ClientHello (JA3/JA4) AND a genuine-CLI HTTP/2 header order (JA4H / Akamai h2
// shape) so a strict upstream cannot fingerprint octopus's stock Go TLS+h2 as
// non-CLI traffic. The ClientHello face is routed per CLI family from the
// request path (see tls_faces.go): /responses wears the codex CLI's rustls
// face, /messages wears the Claude Code Node/OpenSSL face, everything else
// keeps the Chrome face.
//
// Transport selection follows the face:
//   - codex face (ALPN h2+http/1.1): one pooled fhttp2 connection per
//     family+host:port (fhttp honours the explicit pseudo + regular header
//     order). If the server declines h2, HTTP/1.1 is carried over a uTLS
//     connection wearing the same face — never a second, stock-Go handshake —
//     wrapped in the plaintext wire-order rewriter.
//   - claude face (no ALPN — the CLI speaks HTTP/1.1 over TLS): always the
//     HTTP/1.1-over-uTLS path.
//   - chrome face: pooled fhttp2 when the server negotiates h2, otherwise the
//     standard transport fallback (pre-existing, documented behaviour).
//
// Plain http:// requests are delegated to `fallback` (the standard transport).
//
// This is opt-in and default-off: presenting CLI/browser JA3 + fixed header
// order changes how EVERY upstream sees octopus, so it MUST be verified against
// the real the relay passthrough (a genuine claude/codex CLI routed through
// octopus) before being enabled in production — see
// docs/upstream-shape-test-sop.md.
type utlsRoundTripper struct {
	// dialContext dials the raw TCP connection (honouring any configured proxy).
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// fallback handles http:// and HTTPS servers that decline h2 on the chrome face.
	fallback http.RoundTripper
	// tlsConfigForHost optionally builds the uTLS config for one dial (tests
	// inject trust for self-signed capture servers). nil → ServerName only.
	tlsConfigForHost func(host string) *utls.Config

	mu           sync.Mutex
	conns        map[string]*fhttp2.ClientConn // key: family \x00 host:port
	h1Transports map[string]*http.Transport    // key: family
	// h1Only remembers (family \x00 host:port) targets whose server completed a
	// TLS handshake but declined h2, so later requests skip the h2 probe instead
	// of burning a dial+handshake per request. Only set for CLI faces: their h1
	// path keeps the face, so the fingerprint never depends on the cache.
	h1Only map[string]bool
}

func newUTLSRoundTripper(
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error),
	fallback http.RoundTripper,
) *utlsRoundTripper {
	return &utlsRoundTripper{
		dialContext:  dialContext,
		fallback:     fallback,
		conns:        make(map[string]*fhttp2.ClientConn),
		h1Transports: make(map[string]*http.Transport),
		h1Only:       make(map[string]bool),
	}
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return t.fallback.RoundTrip(req)
	}
	spec, family := tlsFamilyForDial(req.Context(), req.URL.Path)
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)

	// The claude face carries no ALPN: the genuine CLI speaks HTTP/1.1 over
	// TLS, so h2 can never be negotiated — skip the h2 probe entirely.
	if family == tlsFaceClaude {
		return t.h1RoundTrip(req, family, spec)
	}

	poolKey := family + "\x00" + addr
	if !t.isKnownH1(poolKey) {
		cc, err := t.h2ConnForHost(req.Context(), host, addr, poolKey, spec)
		if err != nil {
			return nil, err
		}
		if cc != nil {
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
		// Server declined h2 (handshake succeeded, non-h2 ALPN). CLI faces keep
		// their face on the h1 path; chrome keeps the documented stdlib fallback.
		if family == tlsFaceChrome {
			return t.fallback.RoundTrip(req)
		}
		t.markH1Only(poolKey)
	}
	return t.h1RoundTrip(req, family, spec)
}

// h1RoundTrip serves req over HTTP/1.1 on a uTLS connection wearing the
// family's ClientHello face, through a pooled standard transport whose TLS dial
// is replaced by the uTLS dial (the same pattern as the reference project's
// claudeCodeRoundTripper). The connection is wrapped in the plaintext
// wire-order rewriter so the h1 request header block keeps the genuine-CLI
// order; pooling, timeouts and response handling stay standard library.
func (t *utlsRoundTripper) h1RoundTrip(req *http.Request, family string, spec *utls.ClientHelloSpec) (*http.Response, error) {
	transport, err := t.h1TransportForFamily(family, spec)
	if err != nil {
		return nil, err
	}
	return transport.RoundTrip(req)
}

// h1OrderFuncForFace returns the plaintext header-order hook for one TLS family.
//
// face 必须一路传到头块：握手已经穿了这个 face 的 ClientHello，头块也得穿同一套，
// 否则就是"TLS 说 A、头块说 Go"的自相矛盾 —— 比不伪装更容易被 shape check 抓。
// 拆成独立函数是为了能被测试直接钉住：只测 http1WireHeaderOrderForFace 守不住
// 拨号闭包真的用了它（变异实测：闭包退回 http1RequestHeaderOrder 时那些测试全绿）。
func h1OrderFuncForFace(family string) requestHeaderOrderFunc {
	return func(method, target string) []string {
		return http1WireHeaderOrderForFace(method, target, family)
	}
}

// h1TransportForFamily lazily builds one standard transport per CLI family. The
// transport pools connections per host itself, so the map is keyed by family
// only. The uTLS dial already completed the handshake, so the transport must
// never attempt its own h2 (that would also bypass the fhttp2 order path).
func (t *utlsRoundTripper) h1TransportForFamily(family string, spec *utls.ClientHelloSpec) (*http.Transport, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.h1Transports[family]; ok {
		return tr, nil
	}
	cloned, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}
	cloned.Proxy = nil
	cloned.ForceAttemptHTTP2 = false
	cloned.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			host = addr
		}
		uConn, dialErr := t.dialUTLS(ctx, network, addr, host, spec)
		if dialErr != nil {
			return nil, dialErr
		}
		return newOrderedRequestConn(uConn, h1OrderFuncForFace(family)), nil
	}
	t.h1Transports[family] = cloned
	return cloned, nil
}

// dialUTLS dials addr and completes a uTLS handshake wearing spec. A nil spec
// keeps the Chrome preset (the pre-existing default face for non-CLI paths).
func (t *utlsRoundTripper) dialUTLS(ctx context.Context, network, addr, serverName string, spec *utls.ClientHelloSpec) (*utls.UConn, error) {
	rawConn, err := t.dialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{ServerName: serverName}
	if t.tlsConfigForHost != nil {
		cfg = t.tlsConfigForHost(serverName)
	}
	// Clone per dial: utls mutates the spec's KeyShareExtension.Data and
	// SNIExtension.ServerName in place, so a spec pinned by a long-lived
	// caller (the per-family h1 transport closure) would poison every dial
	// after the first with stale key-share bytes and no private key.
	spec = specForDial(spec)
	var uConn *utls.UConn
	if spec != nil {
		uConn = utls.UClient(rawConn, cfg, utls.HelloCustom)
		if err := uConn.ApplyPreset(spec); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
		// Fill the spec's SNI extension for hostname targets. hostnameInSNI
		// omits the extension for IP targets — the same behaviour as the real
		// CLIs (the captures, taken against an IP, show no SNI). The config's
		// ServerName is left untouched so certificate verification still has
		// the original host.
		for _, ext := range uConn.Extensions {
			if sni, ok := ext.(*utls.SNIExtension); ok {
				sni.ServerName = serverName
			}
		}
	} else {
		uConn = utls.UClient(rawConn, cfg, utls.HelloChrome_Auto)
	}
	if err := uConn.HandshakeContext(ctx); err != nil {
		_ = uConn.Close()
		return nil, err
	}
	return uConn, nil
}

// toFHTTPRequest converts a stdlib *http.Request into an *fhttp.Request, stamping the
// genuine-CLI regular-header order (by API path) and the CLI pseudo-header order so
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
	// Always set a pseudo-header order so fhttp never falls back to its linked
	// (nil) transport's PseudoHeaderOrder. A face override (a Chat channel dressed as
	// codex/claude) takes precedence over the path, so the field order follows the
	// identity the ClientHello already presents instead of falling back to Chrome; with
	// no override the historical path rule applies unchanged.
	face := tlsFaceFromContext(req.Context())
	h[fhttp.PHeaderOrderKey] = h2PseudoHeaderOrderForFace(req.URL.Path, face)
	if order := canonicalHeaderOrderForFace(req.URL.Path, face); order != nil {
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

// h2ConnForHost returns a pooled, usable HTTP/2 connection for poolKey, dialing
// a fresh uTLS connection (wearing the family's ClientHello face) when needed.
// It returns (nil, nil) when the server negotiates a non-h2 protocol, signalling
// the caller to use the h1 path. A double-checked lock keeps concurrent
// first-dials from racing without holding the mutex across the handshake.
func (t *utlsRoundTripper) h2ConnForHost(ctx context.Context, serverName, addr, poolKey string, spec *utls.ClientHelloSpec) (*fhttp2.ClientConn, error) {
	t.mu.Lock()
	if cc, ok := t.conns[poolKey]; ok {
		if cc.CanTakeNewRequest() {
			t.mu.Unlock()
			return cc, nil
		}
		delete(t.conns, poolKey)
	}
	t.mu.Unlock()

	uConn, err := t.dialUTLS(ctx, "tcp", addr, serverName, spec)
	if err != nil {
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
	if existing, ok := t.conns[poolKey]; ok && existing.CanTakeNewRequest() {
		t.mu.Unlock()
		_ = cc.Close() // lost the race — keep the connection that got there first
		return existing, nil
	}
	t.conns[poolKey] = cc
	t.mu.Unlock()
	return cc, nil
}

func (t *utlsRoundTripper) isKnownH1(poolKey string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.h1Only[poolKey]
}

func (t *utlsRoundTripper) markH1Only(poolKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.h1Only[poolKey] = true
}
