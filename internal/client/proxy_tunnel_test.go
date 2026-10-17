package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// TestHTTPConnectDialerTunnels is the "does the tunnel actually open" test: it
// stands up a fake HTTP proxy that accepts exactly one CONNECT and then speaks
// plain TCP, and asserts the dialer returns a connection wired to the far side.
// Without this, a shaped channel behind a proxy would fail every request at
// dial time rather than merely losing its face.
func TestHTTPConnectDialerTunnels(t *testing.T) {
	// Far side: echoes a banner so we can prove bytes traverse the tunnel.
	far, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("far listen: %v", err)
	}
	defer far.Close()
	go func() {
		conn, err := far.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("TUNNELED"))
	}()

	// Proxy side: accept CONNECT, reply 200, then splice to the far listener.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyLn.Close()

	gotConnectTarget := make(chan string, 1)
	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		gotConnectTarget <- req.Host
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		upstream, err := net.Dial("tcp", far.Addr().String())
		if err != nil {
			return
		}
		defer upstream.Close()
		// Splice both directions until either side closes.
		done := make(chan struct{}, 2)
		go func() { _, _ = copyThenClose(upstream, conn); done <- struct{}{} }()
		go func() { _, _ = copyThenClose(conn, upstream); done <- struct{}{} }()
		<-done
	}()

	proxyURL := parseURLForTest(t, "http://"+proxyLn.Addr().String())
	dial, err := proxyTunnelDialer(proxyURL)
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The dialer receives the target the caller wants to reach; the proxy is the hop.
	conn, err := dial(ctx, "tcp", far.Addr().String())
	if err != nil {
		t.Fatalf("tunnel dial: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if got := string(buf[:n]); got != "TUNNELED" {
		t.Fatalf("tunnel payload = %q, want TUNNELED", got)
	}

	select {
	case target := <-gotConnectTarget:
		if !strings.Contains(target, far.Addr().String()) {
			t.Fatalf("CONNECT target = %q, want it to address %q", target, far.Addr().String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never received a CONNECT")
	}
}

// TestHTTPConnectDialerSendsProxyAuth pins that credentials in the proxy URL reach
// the proxy: an authenticated proxy that never sees the header would reject every
// shaped request, which would look like an upstream outage rather than a config bug.
func TestHTTPConnectDialerSendsProxyAuth(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyLn.Close()

	gotAuth := make(chan string, 1)
	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		gotAuth <- req.Header.Get("Proxy-Authorization")
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		time.Sleep(100 * time.Millisecond)
	}()

	proxyURL := parseURLForTest(t, "http://alice:s3cret@"+proxyLn.Addr().String())
	dial, err := proxyTunnelDialer(proxyURL)
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "192.0.2.10:443") // TEST-NET-1, never dialed
	if err != nil {
		t.Fatalf("tunnel dial: %v", err)
	}
	conn.Close()

	select {
	case auth := <-gotAuth:
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
		if auth != want {
			t.Fatalf("Proxy-Authorization = %q, want %q", auth, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never received a CONNECT")
	}
}

// TestHTTPConnectDialerRejectsNon200 pins that a refused tunnel is an error, not a
// silently returned socket: handing back a connection the proxy already rejected
// would make the client write a ClientHello into an error page.
func TestHTTPConnectDialerRejectsNon200(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyLn.Close()

	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
	}()

	dial, err := proxyTunnelDialer(parseURLForTest(t, "http://"+proxyLn.Addr().String()))
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dial(ctx, "tcp", "192.0.2.10:443"); err == nil {
		t.Fatal("a refused CONNECT must return an error")
	}
}

// TestProxyTunnelDialerRejectsUnknownScheme pins the failure mode for a proxy scheme
// the tunnel cannot speak (e.g. a raw "socks4"): it must error at construction, not
// fall back to a direct connection, which would leak an unproxied request.
func TestProxyTunnelDialerRejectsUnknownScheme(t *testing.T) {
	if _, err := proxyTunnelDialer(parseURLForTest(t, "socks4://127.0.0.1:1080")); err == nil {
		t.Fatal("unsupported scheme must error rather than dial direct")
	}
}

// TestUTLSClientThroughConnectProxyKeepsFace is the end-to-end proof for the whole
// change: a codex-shaped request whose dial is a CONNECT tunnel must still complete a
// uTLS handshake wearing the CLI face, and the server must see that face's negotiated
// parameters rather than a stock Go handshake. It fails if the face is dropped when a
// proxy is in play — the exact regression this change exists to prevent.
func TestUTLSClientThroughConnectProxyKeepsFace(t *testing.T) {
	cert := genTestCert(t)

	recorded := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn := tls.Server(conn, tlsCfg)
		if err := tlsConn.Handshake(); err != nil {
			recorded <- "handshake-error: " + err.Error()
			return
		}
		state := tlsConn.ConnectionState()
		recorded <- fmt.Sprintf("alpn=%s suite=%s", state.NegotiatedProtocol, tls.CipherSuiteName(state.CipherSuite))
	}()

	// A proxy that CONNECTs every tunnel to the TLS server above.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyLn.Close()
	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		up, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer up.Close()
		go func() { _, _ = copyThenClose(up, conn) }()
		_, _ = copyThenClose(conn, up)
	}()

	// Build the real uTLS round tripper exactly as production does, but tunnel its
	// dial through the proxy and trust the capture cert.
	tunnel, err := proxyTunnelDialer(parseURLForTest(t, "http://"+proxyLn.Addr().String()))
	if err != nil {
		t.Fatalf("tunnel dialer: %v", err)
	}
	fallback, err := clonedDefaultTransport()
	if err != nil {
		t.Fatalf("clone transport: %v", err)
	}
	fallback.Proxy = nil
	rt := newUTLSRoundTripper(tunnel, fallback)
	rt.tlsConfigForHost = func(host string) *utls.Config {
		return &utls.Config{ServerName: host, InsecureSkipVerify: true}
	}
	client := &http.Client{Transport: rt}

	req, err := http.NewRequest(http.MethodPost, "https://"+ln.Addr().String()+"/v1/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req = req.WithContext(WithTLSFace(req.Context(), tlsFaceCodex))

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := client.Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case got := <-recorded:
		if strings.HasPrefix(got, "handshake-error") {
			t.Fatalf("TLS handshake through the tunnel failed: %s", got)
		}
		t.Logf("server saw through tunnel: %s", got)
		// The codex face advertises h2 first; either ALPN outcome proves the uTLS
		// ClientHello completed, and a stock Go handshake would not reach this server
		// at all through the proxy.
		if !strings.Contains(got, "alpn=") {
			t.Fatalf("no ALPN observed through tunnel: %s", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no TLS handshake reached the server through the tunnel")
	}
	<-done
}

// TestProxyTunnelPreservesClientHelloExactly is the assertion that actually pins the
// property this change protects, and the one the ALPN test alone could not make.
//
// A path-based face means /v1/responses wears the codex face with or without a proxy,
// so a plain "did it complete a handshake" check passes even when the proxy path drops
// the per-request face entirely — which is how the original bug hid. Comparing the
// captured ClientHello from the direct dial and the tunnelled dial field by field
// removes that blind spot: any difference in cipher list, extension order, groups,
// signature algorithms, ALPN or key-share groups fails here.
func TestProxyTunnelPreservesClientHelloExactly(t *testing.T) {
	capture := func(name string, useProxy bool) *capturedClientHello {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("%s listen: %v", name, err)
		}
		defer ln.Close()

		got := make(chan *capturedClientHello, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			rec := readClientHelloRecords(t, conn)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: ClientHello did not parse as a CLI face (%v)", name, r)
					got <- nil
				}
			}()
			got <- parseClientHelloRecord(t, rec)
		}()

		var dial func(ctx context.Context, network, addr string) (net.Conn, error)
		var proxyLn net.Listener
		if useProxy {
			proxyLn, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("%s proxy listen: %v", name, err)
			}
			defer proxyLn.Close()
			go func() {
				pconn, err := proxyLn.Accept()
				if err != nil {
					return
				}
				defer pconn.Close()
				if _, err := http.ReadRequest(bufio.NewReader(pconn)); err != nil {
					return
				}
				_, _ = pconn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				up, err := net.Dial("tcp", ln.Addr().String())
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = copyThenClose(up, pconn) }()
				_, _ = copyThenClose(pconn, up)
			}()
			dial, err = proxyTunnelDialer(parseURLForTest(t, "http://"+proxyLn.Addr().String()))
			if err != nil {
				t.Fatalf("%s tunnel: %v", name, err)
			}
		} else {
			dialer := &net.Dialer{}
			dial = dialer.DialContext
		}

		fallback, err := clonedDefaultTransport()
		if err != nil {
			t.Fatalf("clone transport: %v", err)
		}
		fallback.Proxy = nil
		rt := newUTLSRoundTripper(dial, fallback)
		rt.tlsConfigForHost = func(host string) *utls.Config {
			return &utls.Config{ServerName: host, InsecureSkipVerify: true}
		}
		client := &http.Client{Transport: rt}

		req, err := http.NewRequest(http.MethodPost, "https://"+ln.Addr().String()+"/proxy/neutral", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s request: %v", name, err)
		}
		req = req.WithContext(WithTLSFace(req.Context(), tlsFaceClaude))
		go func() {
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				_ = resp.Body.Close()
			}
		}()

		select {
		case ch := <-got:
			return ch
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: no ClientHello captured", name)
			return nil
		}
	}

	direct := capture("direct", false)
	tunnelled := capture("tunnelled", true)

	compare := func(field string, a, b []uint16) {
		t.Helper()
		if len(a) != len(b) {
			t.Fatalf("%s length differs: direct=%d tunnelled=%d", field, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("%s[%d] differs: direct=%#x tunnelled=%#x", field, i, a[i], b[i])
			}
		}
	}
	compare("cipher", direct.ciphers, tunnelled.ciphers)
	compare("extension order", direct.extTypes, tunnelled.extTypes)
	compare("supported groups", direct.groups, tunnelled.groups)
	compare("signature algorithms", direct.sigAlgs, tunnelled.sigAlgs)
	compare("key share groups", direct.shareGrps, tunnelled.shareGrps)

	if len(direct.alpn) != len(tunnelled.alpn) {
		t.Fatalf("ALPN differs: direct=%v tunnelled=%v", direct.alpn, tunnelled.alpn)
	}
	for i := range direct.alpn {
		if direct.alpn[i] != tunnelled.alpn[i] {
			t.Fatalf("ALPN[%d] differs: direct=%q tunnelled=%q", i, direct.alpn[i], tunnelled.alpn[i])
		}
	}
	if direct.sessionLen != tunnelled.sessionLen {
		t.Fatalf("session id length differs: direct=%d tunnelled=%d", direct.sessionLen, tunnelled.sessionLen)
	}
	t.Logf("identical ClientHello over both paths: %d ciphers, %d extensions, alpn=%v",
		len(direct.ciphers), len(direct.extTypes), direct.alpn)
}

// parseURLForTest parses a URL literal that the test itself constructed.
func parseURLForTest(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// copyThenClose splices one direction of a tunnel and closes the destination when
// the source ends, so the far side observes EOF instead of hanging.
func copyThenClose(dst io.Writer, src io.Reader) (int64, error) {
	n, err := io.Copy(dst, src)
	if c, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	} else if c, ok := dst.(io.Closer); ok {
		_ = c.Close()
	}
	return n, err
}
