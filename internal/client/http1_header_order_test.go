package client

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// claudeHTTP1WireHeaderOrderWant is the 21-item wire block a genuine claude-cli emits on
// its plaintext HTTP/1.1 leg (captured 2026-10-10). Positions 18-21 come from the CLI's
// own HTTP stack: only `Connection` is ADDED by us, the other three already exist in Go's
// serialisation and are moved. Written out as a literal on purpose — this is the
// byte-exact contract, so deriving it from claudeHTTP1WireHeaderOrder would let any typo
// in the table agree with itself.
var claudeHTTP1WireHeaderOrderWant = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

const (
	testClaudeBody      = `{"model":"claude-opus-5-5","messages":[]}`
	testClaudeCLIUA     = "claude-cli/2.1.198 (external, cli)"
	testClaudeSessionID = "6f1c0f4e-8b5a-4a2f-9b0f-2f6a1c9d7e33"
)

// testClaudeHeaderValues is keyed by the WIRE spelling in claudeHTTP1WireHeaderOrderWant
// so the loop below can look every entry up. Header.Set canonicalises the key it stores,
// which is the point: the rewriter has to put the wire casing back.
var testClaudeHeaderValues = map[string]string{
	"Accept":                                    "application/json",
	"Authorization":                             "Bearer test-key",
	"Content-Type":                              "application/json",
	"User-Agent":                                testClaudeCLIUA,
	"X-Claude-Code-Session-Id":                  testClaudeSessionID,
	"X-Stainless-Arch":                          "x64",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-OS":                            "Windows",
	"X-Stainless-Package-Version":               "0.70.0",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Runtime-Version":               "v22.14.0",
	"X-Stainless-Timeout":                       "600",
	"anthropic-beta":                            "oauth-2025-04-20",
	"anthropic-dangerous-direct-browser-access": "true",
	"anthropic-version":                         "2023-06-01",
	"x-app":                                     "cli",
	"Accept-Encoding":                           "gzip, deflate, br, zstd",
}

// claudeWireRequest builds the outbound request the relay would send for the claude leg.
// Names are set through the CANONICAL spelling Go produces (Header.Set) and in REVERSE
// wire order, so a passing assertion proves the reordering and the casing rewrite
// happened on the wire rather than in the map. `Connection` is deliberately NOT set:
// the real CLI's stack emits it, Go never does, so the rewriter must synthesize it.
func claudeWireRequest(url string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(testClaudeBody))
	if err != nil {
		return nil, err
	}
	for i := len(claudeHTTP1WireHeaderOrderWant) - 1; i >= 0; i-- {
		if value, ok := testClaudeHeaderValues[claudeHTTP1WireHeaderOrderWant[i]]; ok {
			req.Header.Set(claudeHTTP1WireHeaderOrderWant[i], value)
		}
	}
	req.ContentLength = int64(len(testClaudeBody))
	return req, nil
}

// orderedTransport is the production dial wiring with the order function injected, so the
// byte contract can be pinned without touching settings storage.
func orderedTransport(order requestHeaderOrderFunc) *http.Transport {
	tr := &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 2}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return newOrderedRequestConn(conn, order), nil
	}
	return tr
}

// wireOrderFunc adapts the pure target -> order lookup to the wrapper's
// (method, requestTarget) hook signature.
func wireOrderFunc() requestHeaderOrderFunc {
	return func(_, requestTarget string) []string { return http1WireHeaderOrderForTarget(requestTarget) }
}

func readHTTP1HeaderBlock(br *bufio.Reader) ([]string, error) {
	var lines []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return lines, err
		}
		lines = append(lines, line)
		if line == "\r\n" {
			return lines, nil
		}
	}
}

// TestPlainHTTP1EmitsClaudeWireHeaderOrder is the byte-level self-check for the plaintext
// HTTP/1.1 leg: a real request through a real net/http transport over a loopback socket,
// asserting the captured request line + 21 header lines are byte-for-byte the
// genuine-claude-cli sequence — order, name casing and the ADDED `Connection: keep-alive`
// included.
func TestPlainHTTP1EmitsClaudeWireHeaderOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	req, err := claudeWireRequest("http://" + ln.Addr().String() + "/v1/messages?beta=true")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	got := roundTripCaptureBlock(t, orderedTransport(wireOrderFunc()), req, ln)

	want := []string{
		"POST /v1/messages?beta=true HTTP/1.1\r\n",
		"Accept: application/json\r\n",
		"Authorization: Bearer test-key\r\n",
		"Content-Type: application/json\r\n",
		"User-Agent: " + testClaudeCLIUA + "\r\n",
		"X-Claude-Code-Session-Id: " + testClaudeSessionID + "\r\n",
		"X-Stainless-Arch: x64\r\n",
		"X-Stainless-Lang: js\r\n",
		"X-Stainless-OS: Windows\r\n",
		"X-Stainless-Package-Version: 0.70.0\r\n",
		"X-Stainless-Retry-Count: 0\r\n",
		"X-Stainless-Runtime: node\r\n",
		"X-Stainless-Runtime-Version: v22.14.0\r\n",
		"X-Stainless-Timeout: 600\r\n",
		"anthropic-beta: oauth-2025-04-20\r\n",
		"anthropic-dangerous-direct-browser-access: true\r\n",
		"anthropic-version: 2023-06-01\r\n",
		"x-app: cli\r\n",
		"Connection: keep-alive\r\n", // ADDED by the rewriter: Go never emits it
		"Host: " + ln.Addr().String() + "\r\n",
		"Accept-Encoding: gzip, deflate, br, zstd\r\n",
		fmt.Sprintf("Content-Length: %d\r\n", len(testClaudeBody)),
		"\r\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plaintext HTTP/1.1 request block mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestPlainHTTP1OrderingDisabledKeepsGoCanonicalOrder is the anti-regression guard for the
// switch: with the rewriter inert (which is exactly what the production hook returns when
// the setting is off) the outbound bytes must be IDENTICAL to a stock net/http transport.
// Asserted against a real un-wrapped transport rather than a hand-written copy of Go's
// order, which would rot with the toolchain.
func TestPlainHTTP1OrderingDisabledKeepsGoCanonicalOrder(t *testing.T) {
	inert := func(method, requestTarget string) []string { return nil }

	lnStock, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lnStock.Close()
	stockReq, err := claudeWireRequest("http://" + lnStock.Addr().String() + "/v1/messages?beta=true")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	stock := roundTripCaptureBlock(t, &http.Transport{DisableCompression: true}, stockReq, lnStock)

	lnOurs, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lnOurs.Close()
	oursReq, err := claudeWireRequest("http://" + lnOurs.Addr().String() + "/v1/messages?beta=true")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ours := roundTripCaptureBlock(t, orderedTransport(inert), oursReq, lnOurs)

	if len(stock) != len(ours) {
		t.Fatalf("inert rewriter changed the outbound block:\n stock=%q\n  ours=%q", stock, ours)
	}
	for i := range stock {
		// Host carries the ephemeral port, which differs between the two listeners.
		if strings.HasPrefix(stock[i], "Host: ") {
			continue
		}
		if stock[i] != ours[i] {
			t.Fatalf("inert rewriter changed outbound bytes at line %d:\n stock=%q\n  ours=%q", i, stock[i], ours[i])
		}
	}
	// The stock block must NOT already be the target order, or the guard passes vacuously.
	if !strings.HasPrefix(stock[1], "Host: ") {
		t.Fatalf("unexpected stock Go header order (expected Host first): %q", stock)
	}
	for _, line := range stock {
		if strings.HasPrefix(line, "Connection: ") {
			t.Fatalf("stock Go unexpectedly emitted a Connection header: %q", stock)
		}
	}
}

// TestPlainHTTP1ReordersSecondRequestAfterChunkedBody covers the piece that makes the
// rewrite safe on a reused connection: the ported chunked tracker must locate the end of a
// chunked request body exactly, so the NEXT request's header block is recognised and
// reordered while every body byte is passed through verbatim. A tracker bug here would
// corrupt request bodies, not merely mis-order headers.
func TestPlainHTTP1ReordersSecondRequestAfterChunkedBody(t *testing.T) {
	const chunkedBody = `{"model":"claude-opus-5-5","stream":true}`

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type capture struct {
		firstBlock  []string
		rawBody     []byte
		secondBlock []string
		err         error
	}
	resultCh := make(chan capture, 1)
	go func() {
		var out capture
		conn, err := ln.Accept()
		if err != nil {
			resultCh <- capture{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		br := bufio.NewReader(conn)

		if out.firstBlock, err = readHTTP1HeaderBlock(br); err != nil {
			out.err = err
			resultCh <- out
			return
		}
		// Skip the chunked body, keeping its raw bytes so we can prove they are intact.
		for {
			sizeLine, errRead := br.ReadString('\n')
			if errRead != nil {
				out.err = errRead
				resultCh <- out
				return
			}
			out.rawBody = append(out.rawBody, sizeLine...)
			sizeText := strings.TrimSpace(sizeLine)
			if extension := strings.IndexByte(sizeText, ';'); extension >= 0 {
				sizeText = strings.TrimSpace(sizeText[:extension])
			}
			size, errParse := strconv.ParseInt(sizeText, 16, 64)
			if errParse != nil {
				out.err = fmt.Errorf("bad chunk size %q: %w", sizeText, errParse)
				resultCh <- out
				return
			}
			if size == 0 {
				blank, errBlank := br.ReadString('\n')
				if errBlank != nil {
					out.err = errBlank
					resultCh <- out
					return
				}
				out.rawBody = append(out.rawBody, blank...)
				break
			}
			chunk := make([]byte, size+2) // payload + CRLF
			if _, errFull := io.ReadFull(br, chunk); errFull != nil {
				out.err = errFull
				resultCh <- out
				return
			}
			out.rawBody = append(out.rawBody, chunk...)
		}
		if _, errWrite := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")); errWrite != nil {
			out.err = errWrite
			resultCh <- out
			return
		}
		if out.secondBlock, err = readHTTP1HeaderBlock(br); err != nil {
			out.err = err
			resultCh <- out
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		resultCh <- out
	}()

	client := &http.Client{Transport: orderedTransport(wireOrderFunc())}
	base := "http://" + ln.Addr().String() + "/v1/messages?beta=true"

	// First request: unknown body length => Go frames it as chunked.
	first, err := claudeWireRequest(base)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	first.Body = io.NopCloser(strings.NewReader(chunkedBody))
	first.ContentLength = -1
	first.Header.Del("Content-Length")
	resp, err := client.Do(first)
	if err != nil {
		t.Fatalf("first round trip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Second request on the SAME pooled connection, this time with a Content-Length body.
	second, err := claudeWireRequest(base)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp2, err := client.Do(second)
	if err != nil {
		t.Fatalf("second round trip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()

	var out capture
	select {
	case out = <-resultCh:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the captured chunked exchange")
	}
	if out.err != nil {
		t.Fatalf("capture: %v", out.err)
	}

	// The chunked request carries no Content-Length (Go frames the body with
	// Transfer-Encoding, which is not part of the captured CLI block and so lands after
	// the listed headers); the second request does. Both must be in candidate order.
	chunkedWant := make([]string, 0, len(claudeHTTP1WireHeaderOrderWant))
	for _, name := range claudeHTTP1WireHeaderOrderWant {
		if name == "Content-Length" {
			continue
		}
		chunkedWant = append(chunkedWant, name)
	}
	chunkedWant = append(chunkedWant, "Transfer-Encoding")

	for i, tc := range []struct {
		label string
		block []string
		want  []string
	}{
		{"chunked request", out.firstBlock, chunkedWant},
		{"second request on the reused connection", out.secondBlock, claudeHTTP1WireHeaderOrderWant},
	} {
		// The name as it appeared on the wire, so this also asserts the casing rewrite.
		names := make([]string, 0, len(tc.block))
		for _, line := range tc.block[1 : len(tc.block)-1] {
			names = append(names, strings.SplitN(line, ":", 2)[0])
		}
		if !reflect.DeepEqual(names, tc.want) {
			t.Fatalf("case %d (%s): header order mismatch\n got=%v\nwant=%v", i, tc.label, names, tc.want)
		}
	}
	if !strings.Contains(string(out.rawBody), chunkedBody) {
		t.Fatalf("chunked body was modified in transit: %q", out.rawBody)
	}
}

// TestHTTP1WireHeaderOrderTargetSelection pins which legs the HTTP/1.1 rewrite covers.
// The codex /v1/responses leg must stay untouched (nil == pass-through), and an HTTP
// proxy's absolute-form target must still be recognised.
func TestHTTP1WireHeaderOrderTargetSelection(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   []string
	}{
		{"claude origin-form", "/v1/messages?beta=true", claudeHTTP1WireHeaderOrder},
		{"claude count_tokens", "/v1/messages/count_tokens", claudeHTTP1WireHeaderOrder},
		{"claude via http proxy absolute-form", "http://upstream.example/v1/messages?beta=true", claudeHTTP1WireHeaderOrder},
		{"codex responses untouched", "/v1/responses", nil},
		{"other path untouched", "/v1/chat/completions", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := http1WireHeaderOrderForTarget(tc.target)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("target %q: got %v want %v", tc.target, got, tc.want)
			}
		})
	}
}

// TestClaudeHTTP1WireHeaderOrderIsTheCaptured21Items guards the table itself, so a future
// edit cannot silently drop or reorder an entry. It also pins that the h2 table — a
// different contract for a different layer — was not touched by this change.
func TestClaudeHTTP1WireHeaderOrderIsTheCaptured21Items(t *testing.T) {
	if !reflect.DeepEqual(claudeHTTP1WireHeaderOrder, claudeHTTP1WireHeaderOrderWant) {
		t.Fatalf("claudeHTTP1WireHeaderOrder drifted from the captured 21-item sequence:\n got=%v\nwant=%v",
			claudeHTTP1WireHeaderOrder, claudeHTTP1WireHeaderOrderWant)
	}
	if len(claudeCanonicalHeaderOrder) != 18 || claudeCanonicalHeaderOrder[17] != "accept-encoding" {
		t.Fatalf("claudeCanonicalHeaderOrder (h2 path) must stay unchanged, got %v", claudeCanonicalHeaderOrder)
	}
	for _, name := range claudeCanonicalHeaderOrder {
		if name != strings.ToLower(name) {
			t.Fatalf("claudeCanonicalHeaderOrder must stay lowercase for the h2 path, got %q", name)
		}
	}
}

// TestPlainHTTP1OrderingDefaultsOff asserts the switch ships disabled, so a fresh install
// keeps byte-for-byte the current outbound requests.
func TestPlainHTTP1OrderingDefaultsOff(t *testing.T) {
	if upstreamHTTP1HeaderOrderEnabled() {
		t.Fatal("plaintext HTTP/1.1 header-order rewrite must default to off")
	}
	if order := http1RequestHeaderOrder(http.MethodPost, "/v1/messages?beta=true"); order != nil {
		t.Fatalf("with the switch off the order must be nil (block untouched), got %v", order)
	}
}

// TestPlainHTTP1OpaqueConnPassesTLSLikeBytesThrough pins the scope guard: a connection
// whose first bytes are not an HTTP method token (a TLS ClientHello record, h2 ciphertext)
// must latch to pass-through, so encrypted streams are never buffered or rewritten.
func TestPlainHTTP1OpaqueConnPassesTLSLikeBytesThrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ordered := newOrderedRequestConn(client, func(method, requestTarget string) []string {
		t.Fatalf("order function must not be consulted for non-HTTP bytes (method=%q target=%q)", method, requestTarget)
		return nil
	})

	// A TLS 1.0+ handshake record header starts with 0x16, which is not an HTTP token
	// char, followed by ciphertext that happens to contain a CRLFCRLF sequence.
	payload := append([]byte{0x16, 0x03, 0x01, 0x00, 0x05}, []byte("ab\r\n\r\ncd")...)
	readCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(server, buf); err != nil {
			readCh <- nil
			return
		}
		readCh <- buf
	}()

	if _, err := ordered.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := <-readCh
	if !reflect.DeepEqual(got, payload) {
		t.Fatalf("non-HTTP bytes were altered:\n got=%q\nwant=%q", got, payload)
	}
	conn, ok := ordered.(*orderedRequestConn)
	if !ok || !conn.opaque {
		t.Fatal("connection did not latch to pass-through after non-HTTP bytes")
	}
}

// roundTripCaptureBlock sends req through tr to the caller-owned listener ln and returns
// the raw request header block exactly as it arrived on the wire.
func roundTripCaptureBlock(t *testing.T, tr http.RoundTripper, req *http.Request, ln net.Listener) []string {
	t.Helper()
	type result struct {
		lines []string
		err   error
	}
	linesCh := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			linesCh <- result{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		lines, err := readHTTP1HeaderBlock(bufio.NewReader(conn))
		linesCh <- result{lines: lines, err: err}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	}()

	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case got := <-linesCh:
		if got.err != nil {
			t.Fatalf("read header block: %v", got.err)
		}
		return got.lines
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the captured request header block")
		return nil
	}
}
