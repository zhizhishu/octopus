package client

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetHTTPClientCustomProxyUsesHTTPProxy(t *testing.T) {
	var proxyHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			t.Fatalf("expected absolute proxy target URL, got %q", r.URL.String())
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("via-http-proxy"))
	}))
	t.Cleanup(proxy.Close)

	client, err := GetHTTPClientCustomProxy(proxy.URL)
	if err != nil {
		t.Fatalf("GetHTTPClientCustomProxy: %v", err)
	}
	client.Timeout = 5 * time.Second

	resp, err := client.Get("http://provider.test/v1/models")
	if err != nil {
		t.Fatalf("GET through http proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-http-proxy" || atomic.LoadInt32(&proxyHits) != 1 {
		t.Fatalf("unexpected proxy result body=%q hits=%d", string(body), proxyHits)
	}
}

func TestGetHTTPClientCustomProxyUsesSOCKS5Proxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("via-socks5-proxy"))
	}))
	t.Cleanup(target.Close)

	proxyURL, proxyHits := startSOCKS5Proxy(t)
	client, err := GetHTTPClientCustomProxy(proxyURL)
	if err != nil {
		t.Fatalf("GetHTTPClientCustomProxy: %v", err)
	}
	client.Timeout = 5 * time.Second

	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET through socks5 proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-socks5-proxy" || atomic.LoadInt32(proxyHits) == 0 {
		t.Fatalf("unexpected socks result body=%q hits=%d", string(body), atomic.LoadInt32(proxyHits))
	}
}

func TestGetHTTPClientCustomProxyUsesSOCKS5ProxyWithAuth(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("via-socks5-auth-proxy"))
	}))
	t.Cleanup(target.Close)

	const wantUser, wantPass = "alice", "s3cr3t"
	proxyURL, proxyHits, authHits := startSOCKS5ProxyWithAuth(t, wantUser, wantPass)
	// Embed credentials in the channel proxy URL the same way a user would.
	authProxyURL := "socks5://" + wantUser + ":" + wantPass + "@" + proxyURL
	client, err := GetHTTPClientCustomProxy(authProxyURL)
	if err != nil {
		t.Fatalf("GetHTTPClientCustomProxy: %v", err)
	}
	client.Timeout = 5 * time.Second

	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET through authenticated socks5 proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-socks5-auth-proxy" {
		t.Fatalf("unexpected socks auth result body=%q", string(body))
	}
	if atomic.LoadInt32(proxyHits) == 0 {
		t.Fatalf("expected request to traverse socks5 proxy, hits=%d", atomic.LoadInt32(proxyHits))
	}
	if atomic.LoadInt32(authHits) == 0 {
		t.Fatalf("expected username/password auth negotiation, authHits=%d", atomic.LoadInt32(authHits))
	}
}

func startSOCKS5Proxy(t *testing.T) (string, *int32) {
	t.Helper()
	addr, hits, _ := startSOCKS5Server(t, "", "")
	return "socks5://" + addr, hits
}

// startSOCKS5ProxyWithAuth starts a SOCKS5 server requiring RFC 1929
// username/password auth. It returns the listener address, a counter for
// proxied connections, and a counter for successful auth handshakes.
func startSOCKS5ProxyWithAuth(t *testing.T, user, pass string) (string, *int32, *int32) {
	t.Helper()
	return startSOCKS5Server(t, user, pass)
}

func startSOCKS5Server(t *testing.T, user, pass string) (string, *int32, *int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5: %v", err)
	}
	var hits int32
	var authHits int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5Conn(t, conn, &hits, &authHits, user, pass)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return listener.Addr().String(), &hits, &authHits
}

func handleSOCKS5Conn(t *testing.T, conn net.Conn, hits, authHits *int32, wantUser, wantPass string) {
	t.Helper()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Errorf("read socks greeting: %v", err)
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		t.Errorf("read socks methods: %v", err)
		return
	}

	requireAuth := wantUser != "" || wantPass != ""
	if requireAuth {
		if !slices.Contains(methods, byte(0x02)) { // username/password
			t.Errorf("client did not offer username/password auth method: %#v", methods)
			return
		}
		// Select username/password auth.
		if _, err := conn.Write([]byte{0x05, 0x02}); err != nil {
			t.Errorf("write socks auth method selection: %v", err)
			return
		}
		if !negotiateSOCKS5Auth(t, conn, authHits, wantUser, wantPass) {
			return
		}
	} else {
		// No auth required.
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
			t.Errorf("write socks greeting response: %v", err)
			return
		}
	}

	reqHead := make([]byte, 4)
	if _, err := io.ReadFull(conn, reqHead); err != nil {
		t.Errorf("read socks request: %v", err)
		return
	}
	if reqHead[0] != 0x05 || reqHead[1] != 0x01 {
		t.Errorf("unexpected socks request header: %#v", reqHead)
		return
	}

	host, err := readSOCKS5Host(conn, reqHead[3])
	if err != nil {
		t.Errorf("read socks host: %v", err)
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		t.Errorf("read socks port: %v", err)
		return
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	atomic.AddInt32(hits, 1)
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Errorf("write socks connect response: %v", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	go func() {
		_, _ = io.Copy(upstream, conn)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(conn, upstream)
}

// negotiateSOCKS5Auth performs the RFC 1929 username/password sub-negotiation
// and reports whether the supplied credentials matched. On success it bumps
// authHits so tests can assert the auth handshake actually happened.
func negotiateSOCKS5Auth(t *testing.T, conn net.Conn, authHits *int32, wantUser, wantPass string) bool {
	t.Helper()
	ver := make([]byte, 1)
	if _, err := io.ReadFull(conn, ver); err != nil {
		t.Errorf("read socks auth version: %v", err)
		return false
	}
	if ver[0] != 0x01 {
		t.Errorf("unexpected socks auth version: %#v", ver)
		return false
	}
	ulen := make([]byte, 1)
	if _, err := io.ReadFull(conn, ulen); err != nil {
		t.Errorf("read socks auth ulen: %v", err)
		return false
	}
	uname := make([]byte, int(ulen[0]))
	if _, err := io.ReadFull(conn, uname); err != nil {
		t.Errorf("read socks auth uname: %v", err)
		return false
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		t.Errorf("read socks auth plen: %v", err)
		return false
	}
	passwd := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(conn, passwd); err != nil {
		t.Errorf("read socks auth passwd: %v", err)
		return false
	}
	if string(uname) != wantUser || string(passwd) != wantPass {
		_, _ = conn.Write([]byte{0x01, 0x01}) // failure
		t.Errorf("socks auth mismatch: got %q/%q", string(uname), string(passwd))
		return false
	}
	if _, err := conn.Write([]byte{0x01, 0x00}); err != nil { // success
		t.Errorf("write socks auth success: %v", err)
		return false
	}
	atomic.AddInt32(authHits, 1)
	return true
}

func readSOCKS5Host(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		raw := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return "", err
		}
		return net.IP(raw).String(), nil
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", err
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return "", err
		}
		return string(raw), nil
	case 0x04:
		raw := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return "", err
		}
		return net.IP(raw).String(), nil
	default:
		return "", strconv.ErrSyntax
	}
}

// TestNoProxyClientDoesNotAutoInjectAcceptEncoding proves the stock upstream transport
// runs with DisableCompression: a request that omits Accept-Encoding reaches the server
// with NO Accept-Encoding header. Without DisableCompression, Go silently adds
// "gzip" — a fingerprint tell against a genuine codex CLI, which sends none.
func TestNoProxyClientDoesNotAutoInjectAcceptEncoding(t *testing.T) {
	var gotAE string
	var hadAE bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		_, hadAE = r.Header["Accept-Encoding"]
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	client, err := newHTTPClientNoProxy()
	if err != nil {
		t.Fatalf("newHTTPClientNoProxy: %v", err)
	}
	client.Timeout = 5 * time.Second

	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if hadAE {
		t.Fatalf("stock client auto-injected Accept-Encoding = %q; want absent (DisableCompression)", gotAE)
	}
}

// TestNoProxyClientForwardsExplicitAcceptEncoding proves DisableCompression only
// suppresses auto-injection: an explicitly set Accept-Encoding (the claude-cli value)
// is still delivered verbatim, so the claude path is unaffected by the codex fix.
func TestNoProxyClientForwardsExplicitAcceptEncoding(t *testing.T) {
	const want = "gzip, deflate, br, zstd"
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept-Encoding")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	client, err := newHTTPClientNoProxy()
	if err != nil {
		t.Fatalf("newHTTPClientNoProxy: %v", err)
	}
	client.Timeout = 5 * time.Second

	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept-Encoding", want)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got != want {
		t.Fatalf("explicit Accept-Encoding = %q, want %q", got, want)
	}
}
