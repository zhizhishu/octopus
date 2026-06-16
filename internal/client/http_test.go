package client

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

func startSOCKS5Proxy(t *testing.T) (string, *int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5: %v", err)
	}
	var hits int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5Conn(t, conn, &hits)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return "socks5://" + listener.Addr().String(), &hits
}

func handleSOCKS5Conn(t *testing.T, conn net.Conn, hits *int32) {
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
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		t.Errorf("write socks greeting response: %v", err)
		return
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
