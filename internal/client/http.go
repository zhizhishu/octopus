package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"golang.org/x/net/proxy"
)

var (
	systemDirectClient *http.Client
	systemProxyClient  *http.Client
	systemProxyURL     string
	utlsDirectClient   *http.Client
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
	utlsDirectClient = &http.Client{Transport: newUTLSRoundTripper(dialer.DialContext, fallback)}
	return utlsDirectClient, nil
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
	return cloned, nil
}

func newHTTPClientNoProxy() (*http.Client, error) {
	cloned, err := clonedDefaultTransport()
	if err != nil {
		return nil, err
	}
	cloned.Proxy = nil
	return &http.Client{Transport: cloned}, nil
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
		cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
	}

	return &http.Client{Transport: cloned}, nil
}
