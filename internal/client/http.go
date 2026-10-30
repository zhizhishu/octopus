package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"golang.org/x/net/proxy"
)

var (
	systemDirectClient *http.Client
	systemProxyClient  *http.Client
	systemProxyURL     string
	utlsDirectClient   *http.Client
	// utlsProxyClients caches one uTLS client per proxy URL ("" = direct) so a
	// shaped channel keeps its ClientHello face through a proxy without paying a
	// fresh transport per request. Bounded by the number of configured proxies.
	utlsProxyClients = make(map[string]*http.Client)
	// customProxyClients caches one client (hence one transport, hence one idle
	// connection pool) per channel proxy URL. Keyed by the raw URL string, mirroring
	// the systemProxyURL equality check: a different URL is a different client, an
	// edited URL simply stops being looked up. There is no eviction — channel proxy
	// URLs are a bounded admin config field, so the map tops out at the number of
	// distinct configured proxies.
	customProxyClients = make(map[string]*http.Client)
	clientLock         sync.RWMutex
)

// GetHTTPClientUTLSDirect returns a cached http.Client whose HTTPS transport presents
// a Chrome (uTLS) ClientHello for direct — no-proxy — upstream calls, so a strict
// upstream cannot fingerprint octopus's stock Go TLS handshake. It is opt-in: callers
// gate on SettingKeyUpstreamUTLSFingerprint and only reach here for no-proxy channels.
// Must be verified against the real the relay passthrough before enabling (see the
// utls transport doc comment).
func GetHTTPClientUTLSDirect() (*http.Client, error) {
	clientLock.RLock()
	if utlsDirectClient != nil {
		clientLock.RUnlock()
		return utlsDirectClient, nil
	}
	clientLock.RUnlock()

	clientLock.Lock()
	defer clientLock.Unlock()
	if utlsDirectClient != nil {
		return utlsDirectClient, nil
	}

	fallback, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}
	fallback.Proxy = nil

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	// http:// (and the chrome-face h1 fallback) share this transport across every
	// direct uTLS client. Key it by face so a chat channel dressed as claude does
	// not reuse a connection opened for a codex-shaped request.
	utlsDirectClient = &http.Client{Transport: newUTLSRoundTripper(dialer.DialContext, newFaceKeyedPlainTransport(fallback))}
	return utlsDirectClient, nil
}

// TransportKind names the transport behind an HTTP client for diagnostics and tests:
// "utls" means the request will wear a CLI/browser ClientHello face, anything else
// means a stock Go handshake. Kept intentionally small so a test can assert the
// property that matters instead of reaching into unexported types.
func TransportKind(c *http.Client) string {
	if c == nil {
		return ""
	}
	if _, ok := c.Transport.(*utlsRoundTripper); ok {
		return "utls"
	}
	return "standard"
}

// GetHTTPClientUTLSForProxy returns a cached http.Client that presents the uTLS
// CLI face for a channel that reaches the upstream through proxyURL (nil = direct).
//
// This exists because a channel that committed to a CLI identity must keep that
// identity on every path. Routing the uTLS handshake through a proxy tunnel is the
// whole point: the proxy must not be able to turn a shaped request into an
// unshaped one, which is what happened while shaped channels bypassed uTLS
// whenever a proxy was configured.
//
// Clients are cached per proxy URL — one transport, one idle pool each — and the
// nil entry holds the direct client, so a mixed fleet pays no rebuild cost.
func GetHTTPClientUTLSForProxy(proxyURL *url.URL) (*http.Client, error) {
	key := ""
	if proxyURL != nil {
		key = proxyURL.String()
	}

	clientLock.RLock()
	if cached, ok := utlsProxyClients[key]; ok {
		clientLock.RUnlock()
		return cached, nil
	}
	clientLock.RUnlock()

	clientLock.Lock()
	defer clientLock.Unlock()
	if cached, ok := utlsProxyClients[key]; ok {
		return cached, nil
	}

	fallback, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}
	fallback.Proxy = nil

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	dialContext := dialer.DialContext
	if proxyURL != nil {
		tunnel, err := proxyTunnelDialer(proxyURL)
		if err != nil {
			return nil, err
		}
		dialContext = tunnel
	}

	built := &http.Client{Transport: newUTLSRoundTripper(dialContext, newFaceKeyedPlainTransport(fallback))}
	utlsProxyClients[key] = built
	return built, nil
}

// GetHTTPClientSystemProxy returns a cached http.Client.
// - useProxy=false: bypass proxy
// - useProxy=true: use proxy settings from system/app settings (setting key: proxy_url)
func GetHTTPClientSystemProxy(useProxy bool) (*http.Client, error) {
	if useProxy {
		currentProxyURL, err := op.SettingGetString(model.SettingKeyProxyURL)
		if err != nil {
			return nil, err
		}
		if currentProxyURL == "" {
			return nil, fmt.Errorf("proxy url is empty")
		}

		clientLock.RLock()
		if systemProxyClient != nil && systemProxyURL == currentProxyURL {
			clientLock.RUnlock()
			return systemProxyClient, nil
		}
		clientLock.RUnlock()

		clientLock.Lock()
		defer clientLock.Unlock()

		// Re-check after acquiring write lock.
		if systemProxyClient != nil && systemProxyURL == currentProxyURL {
			return systemProxyClient, nil
		}

		client, err := newHTTPClientCustomProxy(currentProxyURL)
		if err != nil {
			return nil, err
		}
		systemProxyClient = client
		systemProxyURL = currentProxyURL
		return systemProxyClient, nil
	}

	clientLock.RLock()
	if !useProxy && systemDirectClient != nil {
		clientLock.RUnlock()
		return systemDirectClient, nil
	}
	clientLock.RUnlock()

	clientLock.Lock()
	defer clientLock.Unlock()

	if systemDirectClient != nil {
		return systemDirectClient, nil
	}
	client, err := newHTTPClientNoProxy()
	if err != nil {
		return nil, err
	}
	systemDirectClient = client
	return systemDirectClient, nil
}

// GetHTTPClientCustomProxy returns a cached http.Client per proxy URL. Building a
// fresh client per call also built a fresh transport, so every request through a
// channel proxy started from an empty connection pool and paid a full TCP+TLS
// handshake; callers with the same URL now share one transport and reuse its idle
// connections.
// proxyURL supports: http, https, socks, socks5
func GetHTTPClientCustomProxy(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return nil, fmt.Errorf("proxy url is empty")
	}

	clientLock.RLock()
	if cached, ok := customProxyClients[proxyURL]; ok {
		clientLock.RUnlock()
		return cached, nil
	}
	clientLock.RUnlock()

	clientLock.Lock()
	defer clientLock.Unlock()

	// Re-check after acquiring write lock.
	if cached, ok := customProxyClients[proxyURL]; ok {
		return cached, nil
	}

	client, err := newHTTPClientCustomProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	customProxyClients[proxyURL] = client
	return client, nil
}

func clonedDefaultTransport() (*http.Transport, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default transport is not *http.Transport")
	}
	cloned := transport.Clone()
	// Do not let Go auto-inject "Accept-Encoding: gzip" on requests that omit the
	// header. The genuine codex CLI sends no Accept-Encoding, so its outbound request
	// must carry none; Go would otherwise silently add gzip and re-introduce a
	// fingerprint tell. With compression disabled, "header not set" means "header not
	// on the wire". The claude path still sends its explicit gzip,deflate,br,zstd value,
	// and the relay decompresses any upstream Content-Encoding itself
	// (unwrapResponseEncoding), so responses are unaffected.
	cloned.DisableCompression = true
	// Go's default MaxIdleConnsPerHost is 2: beyond two concurrent in-flight requests
	// to one host, every extra connection is closed instead of parked, so the next
	// request re-does TCP+TLS. octopus is the opposite of the browser workload that
	// default assumes — many concurrent requests to a handful of upstream hosts — so
	// keep a real per-host pool. Pool sizing only; nothing here touches the handshake
	// or any header, so the outbound fingerprint is unchanged.
	cloned.MaxIdleConnsPerHost = 32
	// Header order is NOT installed here. This transport is a template: plaintext
	// clients wrap it in faceKeyedPlainTransport, which clones one pool per CLI face
	// and only then wraps the dial. Installing the rewriter on the template would
	// pin a single order onto every request that later reused a cached connection.
	return cloned, nil
}

// faceKeyedPlainTransport is the plaintext (and standard-HTTPS) RoundTripper behind
// every cached HTTP client. utlsRoundTripper already keeps one HTTP/1.1 transport
// per CLI family because its dial closure captures the face; a process-wide
// *http.Transport cannot do that — the next request pulled from the idle pool
// would keep the first request's header order.
//
// The family comes from tlsFamilyForDial, the same function the uTLS dial uses:
// WithTLSFace (relay channelTLSFace) wins, otherwise the URL path. Each family
// gets its own *http.Transport, and that transport's dial closure captures the
// family and calls http1WireHeaderOrderForFace.
type faceKeyedPlainTransport struct {
	base   *http.Transport
	mu     sync.Mutex
	byFace map[string]*http.Transport
}

func newFaceKeyedPlainTransport(base *http.Transport) *faceKeyedPlainTransport {
	return &faceKeyedPlainTransport{
		base:   base,
		byFace: make(map[string]*http.Transport),
	}
}

func (p *faceKeyedPlainTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if p == nil || p.base == nil {
		return nil, fmt.Errorf("client: plaintext transport is not configured")
	}
	path := ""
	ctx := context.Background()
	if req != nil {
		if req.URL != nil {
			path = req.URL.Path
		}
		if req.Context() != nil {
			ctx = req.Context()
		}
	}
	_, family := tlsFamilyForDial(ctx, path)
	return p.transportFor(family).RoundTrip(req)
}

// transportFor returns the cached *http.Transport for family, building it on
// first use. The dial closure captures family the same way
// utlsRoundTripper.h1TransportForFamily does.
func (p *faceKeyedPlainTransport) transportFor(family string) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tr, ok := p.byFace[family]; ok {
		return tr
	}
	cloned := p.base.Clone()
	cloned.DialContext = plainHTTP1OrderingDial(cloned.DialContext, family)
	p.byFace[family] = cloned
	return cloned
}

// plainHTTP1HeaderOrder is the order func a plaintext dial closure must use.
// family is the tlsFamilyForDial result captured by that closure — not read
// from the dial context, which belongs only to the request that opened the
// connection. A reused connection would otherwise keep the first request's face.
//
// CONNECT is left untouched. Its request target is "host:port", and a CLI face
// order would rewrite the proxy handshake (and synthesize Connection: keep-alive).
// The historical path rule returns nil for that target; keep those bytes. The
// tunneled request, if it is plaintext, is a later write on the same conn and
// still goes through http1WireHeaderOrderForFace.
func plainHTTP1HeaderOrder(family string) requestHeaderOrderFunc {
	order := h1OrderFuncForFace(family)
	return func(method, target string) []string {
		if strings.EqualFold(method, http.MethodConnect) {
			return nil
		}
		return order(method, target)
	}
}

// plainHTTP1OrderingDial wraps a transport dial function so the connection it
// returns rewrites HTTP/1.1 request header blocks for family. A nil inner dial
// (the transport would then dial with the package default) falls back to
// net.Dialer, which is what net/http uses in that case anyway.
//
// family is part of the transport cache key (see faceKeyedPlainTransport). The
// setting inside http1WireHeaderOrderForFace is still read per request, so
// turning upstream_http1_header_order off takes effect on pooled connections.
func plainHTTP1OrderingDial(inner func(ctx context.Context, network, addr string) (net.Conn, error), family string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if inner == nil {
		inner = (&net.Dialer{}).DialContext
	}
	// Captured here, not inside the dial func from ctx: DialContext runs only
	// when a connection is opened, and the ordered conn then serves every later
	// request the pool reuses it for.
	order := plainHTTP1HeaderOrder(family)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := inner(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return newOrderedRequestConn(conn, order), nil
	}
}

func newHTTPClientNoProxy() (*http.Client, error) {
	cloned, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}
	cloned.Proxy = nil
	return &http.Client{Transport: newFaceKeyedPlainTransport(cloned)}, nil
}

func newHTTPClientCustomProxy(proxyURLStr string) (*http.Client, error) {
	cloned, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}

	proxyURL, err := url.Parse(proxyURLStr)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy url: %w", err)
	}

	switch proxyURL.Scheme {
	case "http", "https":
		cloned.Proxy = http.ProxyURL(proxyURL)
	case "socks", "socks5":
		socksDialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("invalid socks proxy: %w", err)
		}
		cloned.Proxy = nil
		// Socks replaces the dial. Leave it unwrapped: faceKeyedPlainTransport
		// clones this transport per face and installs plainHTTP1OrderingDial on
		// the clone, so the rewriter stays in the chain without pinning one face
		// onto every request that shares the proxy.
		cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
	}

	return &http.Client{Transport: newFaceKeyedPlainTransport(cloned)}, nil
}
