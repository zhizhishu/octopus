package client

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// proxyTunnelDialer builds a dial function that opens a raw TCP tunnel to
// addr through proxyURL, so a uTLS handshake can then run end to end over it.
//
// This is the piece that lets a shape-carrying channel keep its ClientHello face
// even when it reaches the upstream through a proxy: uTLS needs a plain net.Conn
// to write its own ClientHello onto, and a proxy by definition sits in front of
// that connection. Without it the only choices would be "lose the face" or "lose
// the proxy", which is exactly the trade the shape rule forbids.
//
// Supported schemes mirror the rest of the proxy handling: socks5/socks (via
// x/net/proxy, which does the SOCKS handshake itself) and http/https (via a
// CONNECT tunnel). Credentials embedded in the URL are forwarded.
func proxyTunnelDialer(proxyURL *url.URL) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if proxyURL == nil {
		return nil, errors.New("proxy url is nil")
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "socks", "socks5", "socks5h":
		dialer, err := xproxy.FromURL(proxyURL, xproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("invalid socks proxy: %w", err)
		}
		// x/net/proxy exposes a context-aware dialer for socks5; prefer it so a
		// cancelled request aborts the handshake instead of parking a goroutine.
		if cd, ok := dialer.(xproxy.ContextDialer); ok {
			return cd.DialContext, nil
		}
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}, nil
	case "http", "https":
		return httpConnectDialer(proxyURL), nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme for tunnel dial: %s", proxyURL.Scheme)
	}
}

// httpConnectDialer returns a dial function that establishes an HTTP CONNECT
// tunnel through the given proxy. The returned conn is the raw tunnel: the
// caller (uTLS) performs its own handshake through it, so the proxy never sees
// the ClientHello as plaintext and cannot alter the presented face.
func httpConnectDialer(proxyURL *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		if strings.EqualFold(proxyURL.Scheme, "https") {
			proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "80")
		}
	}
	authHeader := ""
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		authHeader = "Basic " + token
	}

	return func(ctx context.Context, network, target string) (net.Conn, error) {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
		}

		// A CONNECT exchange must not outlive its request; without this a stalled
		// proxy would hold the caller forever (net/http would normally cover this,
		// but here we own the handshake).
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		}

		var req strings.Builder
		req.WriteString("CONNECT " + target + " HTTP/1.1\r\n")
		req.WriteString("Host: " + target + "\r\n")
		if authHeader != "" {
			req.WriteString("Proxy-Authorization: " + authHeader + "\r\n")
		}
		req.WriteString("\r\n")
		if _, err := io.WriteString(conn, req.String()); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("write CONNECT: %w", err)
		}

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("read CONNECT response: %w", err)
		}
		// The body of a CONNECT response is the tunnel itself; never close it here.
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			_ = conn.Close()
			return nil, fmt.Errorf("proxy CONNECT to %s failed: %s", target, resp.Status)
		}

		// Clear the handshake deadline: the tunnel is now a normal connection and
		// the caller's own timeouts take over.
		_ = conn.SetDeadline(time.Time{})

		// ReadResponse may have buffered bytes beyond the headers; hand them back
		// so a fast TLS server response cannot be lost.
		if br.Buffered() > 0 {
			buffered, _ := br.Peek(br.Buffered())
			return &bufferedConn{Conn: conn, buffered: buffered}, nil
		}
		return conn, nil
	}
}

// bufferedConn replays bytes that were already read off the socket (the CONNECT
// response parser can read ahead into the tunnel) before delegating to the real
// connection.
type bufferedConn struct {
	net.Conn
	buffered []byte
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if len(c.buffered) > 0 {
		n := copy(p, c.buffered)
		c.buffered = c.buffered[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// String is used only in diagnostics so a buffered tunnel is still identifiable.
func (c *bufferedConn) String() string {
	return "bufferedConn(" + c.Conn.RemoteAddr().String() + ", buffered=" + strconv.Itoa(len(c.buffered)) + ")"
}
