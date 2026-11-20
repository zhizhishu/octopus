package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// TestClientHelloSpecRouting pins which CLI face each API path wears. The face
// must follow the same family routing as the header-order tables: /responses is
// codex, /messages is claude, everything else keeps the Chrome default.
func TestClientHelloSpecRouting(t *testing.T) {
	cases := []struct {
		path   string
		family string
		isNil  bool
	}{
		{"/v1/responses", tlsFaceCodex, false},
		{"/responses", tlsFaceCodex, false},
		{"/v1/messages", tlsFaceClaude, false},
		{"/v1/messages/count_tokens", tlsFaceClaude, false},
		{"/v1/chat/completions", tlsFaceChrome, true},
		{"/v1/models", tlsFaceChrome, true},
	}
	for _, tc := range cases {
		spec, family := clientHelloSpecForPath(tc.path)
		if family != tc.family {
			t.Fatalf("path %s: family = %q, want %q", tc.path, family, tc.family)
		}
		if (spec == nil) != tc.isNil {
			t.Fatalf("path %s: spec nil = %v, want %v", tc.path, spec == nil, tc.isNil)
		}
	}
}

// TestWithTLSFaceOverrideSelectsFamilySpec pins the per-request TLS face override the
// relay uses for a Chat channel's cloak shape: WithTLSFace("claude") resolves to the Claude
// Node ClientHello spec, an unknown/empty family keeps the Chrome default (nil spec), so a
// stray override value can never select an undefined face.
func TestWithTLSFaceOverrideSelectsFamilySpec(t *testing.T) {
	ctx := WithTLSFace(context.Background(), "claude")
	if got := tlsFaceFromContext(ctx); got != "claude" {
		t.Fatalf("tlsFaceFromContext = %q, want claude", got)
	}
	spec, family := clientHelloSpecForFamily(tlsFaceFromContext(ctx))
	if family != tlsFaceClaude {
		t.Fatalf("family = %q, want %q", family, tlsFaceClaude)
	}
	if spec == nil {
		t.Fatalf("claude override must return a non-nil ClientHello spec")
	}

	ctxCodex := WithTLSFace(context.Background(), "codex")
	codexSpec, codexFamily := clientHelloSpecForFamily(tlsFaceFromContext(ctxCodex))
	if codexFamily != tlsFaceCodex || codexSpec == nil {
		t.Fatalf("codex override: family = %q, spec nil = %v", codexFamily, codexSpec == nil)
	}

	// No override (empty family) keeps the Chrome default: family=chrome, spec=nil
	// (HelloChrome_Auto is the pre-existing non-CLI face).
	chromeSpec, chromeFamily := clientHelloSpecForFamily(tlsFaceFromContext(context.Background()))
	if chromeFamily != tlsFaceChrome || chromeSpec != nil {
		t.Fatalf("empty override must keep the chrome face, got family = %q, spec nil = %v", chromeFamily, chromeSpec == nil)
	}
}

// Wire expectations transcribed from the captured genuine-CLI ClientHellos.
// These are the regression guard: if the specs or uTLS emission drift, the test
// fails with the actual wire shape. SNI is absent because the test dials an IP,
// exactly like the captures.
var (
	codexFaceCiphers = []uint16{
		0x1302, 0x1301, 0x1303, 0xc02c, 0xc02b, 0xcca9, 0xc030, 0xc02f, 0xcca8, 0x00ff,
	}
	codexFaceExtTypes = []uint16{
		43, 16, 51, 23, 13, 35, 11, 10, 45, 5,
	}
	codexFaceSigAlgs = []uint16{
		0x0503, 0x0403, 0x0603, 0x0807, 0x0806, 0x0805, 0x0804,
		0x0601, 0x0501, 0x0401, 0x0904, 0x0905, 0x0906,
	}
	codexFaceGroups = []uint16{0x11ec, 0x001d, 0x0017, 0x0018}

	claudeFaceCiphers = []uint16{
		0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9,
		0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f,
		0x0035,
	}
	claudeFaceExtTypes = []uint16{
		23, 65281, 10, 11, 35, 16, 5, 13, 18, 51, 45, 43,
	}
	claudeFaceSigAlgs = []uint16{
		0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601,
		0x0201,
	}
	claudeFaceGroups = []uint16{0x11ec, 0x001d, 0x0017, 0x0018}

	faceKeyShareGroups = []uint16{0x11ec, 0x001d}
)

// capturedClientHello is the parsed wire shape of one ClientHello.
type capturedClientHello struct {
	ciphers    []uint16
	extTypes   []uint16
	alpn       []string
	sni        []string
	groups     []uint16
	sigAlgs    []uint16
	shareGrps  []uint16
	sessionLen int
}

// parseClientHelloRecord parses the ClientHello handshake message out of a
// single captured TLS record (test-only mini parser; the captures here, and the
// synthetic case below, always fit one record <16k — a hello split across TLS
// records would carry the next record's 5-byte header mid-`rec` and is not
// handled).
func parseClientHelloRecord(t *testing.T, rec []byte) *capturedClientHello {
	t.Helper()
	if len(rec) < 9 || rec[0] != 0x16 {
		t.Fatalf("not a handshake record: % x", rec[:min(9, len(rec))])
	}
	hsLen := int(rec[6])<<16 | int(rec[7])<<8 | int(rec[8]) // handshake body length, excludes the 4-byte handshake header
	// rec[9:] is the ClientHello body: the 4-byte handshake header (type + length)
	// is already behind us, so the body is exactly hsLen bytes — NOT hsLen-4.
	if len(rec) < 9+hsLen {
		t.Fatalf("captured ClientHello truncated: len(rec)=%d < 9+hsLen=%d", len(rec), 9+hsLen)
	}
	ch := rec[9 : 9+hsLen]
	// ch: version(2) random(32) sidLen(1) sid ciphersLen(2) ciphers compLen(1) comp extLen(2) exts
	pos := 0
	pos += 2 + 32
	sidLen := int(ch[pos])
	pos++
	out := &capturedClientHello{sessionLen: sidLen}
	pos += sidLen
	csLen := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2
	for i := 0; i < csLen; i += 2 {
		out.ciphers = append(out.ciphers, uint16(ch[pos+i])<<8|uint16(ch[pos+i+1]))
	}
	pos += csLen
	compLen := int(ch[pos])
	pos += 1 + compLen
	extTotal := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2
	end := pos + extTotal
	for pos < end {
		etype := uint16(ch[pos])<<8 | uint16(ch[pos+1])
		elen := int(ch[pos+2])<<8 | int(ch[pos+3])
		edata := ch[pos+4 : pos+4+elen]
		out.extTypes = append(out.extTypes, etype)
		switch etype {
		case 0: // server_name
			l := int(edata[0])<<8 | int(edata[1])
			p := 2
			for p < 2+l {
				nt := edata[p]
				nl := int(edata[p+1])<<8 | int(edata[p+2])
				if nt == 0 {
					out.sni = append(out.sni, string(edata[p+3:p+3+nl]))
				}
				p += 3 + nl
			}
		case 16: // ALPN
			p := 2
			listLen := int(edata[0])<<8 | int(edata[1])
			for p < 2+listLen {
				pl := int(edata[p])
				out.alpn = append(out.alpn, string(edata[p+1:p+1+pl]))
				p += 1 + pl
			}
		case 10: // supported groups
			gl := int(edata[0])<<8 | int(edata[1])
			for i := 2; i < 2+gl; i += 2 {
				out.groups = append(out.groups, uint16(edata[i])<<8|uint16(edata[i+1]))
			}
		case 13: // signature algorithms
			sl := int(edata[0])<<8 | int(edata[1])
			for i := 2; i < 2+sl; i += 2 {
				out.sigAlgs = append(out.sigAlgs, uint16(edata[i])<<8|uint16(edata[i+1]))
			}
		case 51: // key share
			gl := int(edata[0])<<8 | int(edata[1])
			p := 2
			for p < 2+gl {
				grp := uint16(edata[p])<<8 | uint16(edata[p+1])
				klen := int(edata[p+2])<<8 | int(edata[p+3])
				out.shareGrps = append(out.shareGrps, grp)
				p += 4 + klen
			}
		}
		pos += 4 + elen
	}
	return out
}

// TestParseClientHelloRecordKeepsFinalEmptyExtension regresses the truncation
// where the parser sliced the ClientHello body as 9+hsLen-4, dropping the final
// 4 bytes: a zero-length trailing extension then made the extension walk run
// past the slice and panic. The synthetic hello is one record holding a minimal
// ClientHello whose only extension is a final, empty type 23 — the shape the old
// fixed-key-share-last captures happened to hide.
func TestParseClientHelloRecordKeepsFinalEmptyExtension(t *testing.T) {
	var body []byte
	body = append(body, 0x03, 0x03)             // legacy_version
	body = append(body, make([]byte, 32)...)    // random
	body = append(body, 0x00)                   // session_id length (0)
	body = append(body, 0x00, 0x02, 0x13, 0x01) // cipher_suites: len 2, TLS_AES_128_GCM_SHA256
	body = append(body, 0x01, 0x00)             // compression: len 1, null
	body = append(body, 0x00, 0x04)             // extensions length 4
	body = append(body, 0x00, 0x17, 0x00, 0x00) // final extension: type 23, len 0

	rec := []byte{
		0x16, 0x03, 0x03, // record: handshake, TLS 1.2 record version
		byte((len(body) + 4) >> 8), byte(len(body) + 4), // record length = handshake header(4) + body
		0x01,                                                         // handshake type: client_hello
		byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), // handshake body length
	}
	rec = append(rec, body...)

	hello := parseClientHelloRecord(t, rec)
	if !reflect.DeepEqual(hello.extTypes, []uint16{23}) {
		t.Fatalf("extTypes = %v, want [23] (final empty extension dropped?)", hello.extTypes)
	}
	if hello.sessionLen != 0 {
		t.Fatalf("session id length = %d, want 0", hello.sessionLen)
	}
}

// replayConn feeds already-read bytes back to a tls.Server before delegating
// to the live connection.
type replayConn struct {
	net.Conn
	pending []byte
}

func (c *replayConn) Read(p []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// readClientHelloRecords reads whole TLS records off conn until the ClientHello
// handshake message is complete, returning the raw bytes consumed.
func readClientHelloRecords(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	var buf []byte
	deadline := time.Now().Add(10 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for {
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			t.Fatalf("read record header: %v", err)
		}
		recLen := int(hdr[3])<<8 | int(hdr[4])
		body := make([]byte, recLen)
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatalf("read record body: %v", err)
		}
		buf = append(buf, hdr...)
		buf = append(buf, body...)
		if len(buf) >= 9 {
			hsLen := int(buf[6])<<16 | int(buf[7])<<8 | int(buf[8])
			if len(buf) >= 9+hsLen {
				return buf
			}
		}
	}
}

// genTestCert builds a throwaway self-signed ECDSA P-256 certificate for the
// capture servers.
func genTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "upstream.example"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"upstream.example", "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	return pair
}

// newFaceTestRoundTripper builds a utlsRoundTripper dialing plain TCP (no
// proxy) and trusting the self-signed capture cert, so the real RoundTrip path
// (face routing, uTLS dial, h2/h1 selection) is exercised in-process.
func newFaceTestRoundTripper(t *testing.T) *utlsRoundTripper {
	t.Helper()
	fallback, err := clonedDefaultTransport()
	if err != nil {
		t.Fatalf("clone transport: %v", err)
	}
	fallback.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rt := newUTLSRoundTripper(dialer.DialContext, fallback)
	rt.tlsConfigForHost = func(host string) *utls.Config {
		return &utls.Config{ServerName: host, InsecureSkipVerify: true}
	}
	return rt
}

// TestClaudeFaceEmitsCapturedClientHello drives a /messages request through the
// full round tripper against a TLS server that captures the ClientHello, and
// asserts the wire shape matches the captured business Claude Code face (ALPN
// http/1.1, HTTP/1.1 carried over the uTLS connection).
func TestClaudeFaceEmitsCapturedClientHello(t *testing.T) {
	pair := genTestCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	helloCh := make(chan []byte, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		hello := readClientHelloRecords(t, raw)
		helloCh <- hello
		replay := &replayConn{Conn: raw, pending: hello}
		_ = raw.SetReadDeadline(time.Time{})
		tlsConn := tls.Server(replay, &tls.Config{Certificates: []tls.Certificate{pair}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		// Serve exactly one HTTP/1.1 request.
		_ = tlsConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		req, err := http.ReadRequest(bufio.NewReader(tlsConn))
		if err != nil {
			return
		}
		_ = req.Body.Close()
		_, _ = tlsConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi"))
		_ = tlsConn.Close()
	}()

	rt := newFaceTestRoundTripper(t)
	req, err := http.NewRequest(http.MethodPost, "https://"+ln.Addr().String()+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("claude face roundtrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hi" {
		t.Fatalf("claude face response: status=%d body=%q", resp.StatusCode, body)
	}

	hello := parseClientHelloRecord(t, <-helloCh)
	if !reflect.DeepEqual(hello.ciphers, claudeFaceCiphers) {
		t.Fatalf("claude face cipher suites mismatch:\n got=%v\nwant=%v", hello.ciphers, claudeFaceCiphers)
	}
	if !reflect.DeepEqual(hello.extTypes, claudeFaceExtTypes) {
		t.Fatalf("claude face extension order mismatch:\n got=%v\nwant=%v", hello.extTypes, claudeFaceExtTypes)
	}
	if !reflect.DeepEqual(hello.alpn, []string{"http/1.1"}) {
		t.Fatalf("claude face ALPN mismatch:\n got=%v\nwant=[http/1.1]", hello.alpn)
	}
	if !reflect.DeepEqual(hello.groups, claudeFaceGroups) {
		t.Fatalf("claude face groups mismatch:\n got=%v\nwant=%v", hello.groups, claudeFaceGroups)
	}
	if !reflect.DeepEqual(hello.sigAlgs, claudeFaceSigAlgs) {
		t.Fatalf("claude face signature algorithms mismatch:\n got=%v\nwant=%v", hello.sigAlgs, claudeFaceSigAlgs)
	}
	if !reflect.DeepEqual(hello.shareGrps, faceKeyShareGroups) {
		t.Fatalf("claude face key share groups mismatch:\n got=%v\nwant=%v", hello.shareGrps, faceKeyShareGroups)
	}
	if hello.sessionLen != 32 {
		t.Fatalf("claude face session id length = %d, want 32", hello.sessionLen)
	}
}

// TestClaudeFaceHostnameSendsSNI pins that hostname targets still prepend the
// server_name extension (IP targets omit it, covered by the captured-shape test
// above) while keeping the Claude face's cipher and extension shape. The dial
// trusts the throwaway capture cert (test-only InsecureSkipVerify), so this
// asserts the ClientHello wire shape only — it makes no claim about a verified
// production handshake.
func TestClaudeFaceHostnameSendsSNI(t *testing.T) {
	pair := genTestCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	helloCh := make(chan []byte, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		hello := readClientHelloRecords(t, raw)
		helloCh <- hello
		replay := &replayConn{Conn: raw, pending: hello}
		_ = raw.SetReadDeadline(time.Time{})
		tlsConn := tls.Server(replay, &tls.Config{Certificates: []tls.Certificate{pair}})
		_ = tlsConn.Handshake()
		_ = tlsConn.Close()
	}()

	rt := newFaceTestRoundTripper(t)
	uConn, err := rt.dialUTLS(context.Background(), "tcp", ln.Addr().String(), "upstream.example", tlsFaceClaude, claudeCodeNodeClientHelloSpec())
	if err != nil {
		t.Fatalf("hostname dial failed: %v", err)
	}
	_ = uConn.Close()

	hello := parseClientHelloRecord(t, <-helloCh)
	if len(hello.extTypes) == 0 || hello.extTypes[0] != 0 {
		t.Fatalf("hostname target must send server_name first, ext types = %v", hello.extTypes)
	}
	if !reflect.DeepEqual(hello.sni, []string{"upstream.example"}) {
		t.Fatalf("SNI = %v, want [upstream.example]", hello.sni)
	}
	wantExt := append([]uint16{0}, claudeFaceExtTypes...)
	if !reflect.DeepEqual(hello.extTypes, wantExt) {
		t.Fatalf("hostname target extension order mismatch:\n got=%v\nwant=%v", hello.extTypes, wantExt)
	}
	if !reflect.DeepEqual(hello.ciphers, claudeFaceCiphers) {
		t.Fatalf("hostname target cipher suites mismatch:\n got=%v\nwant=%v", hello.ciphers, claudeFaceCiphers)
	}
}

// sortedExtTypes returns a sorted copy of an extension-type list, for
// order-independent (multiset) comparison of a shuffled ClientHello.
func sortedExtTypes(exts []uint16) []uint16 {
	cp := append([]uint16(nil), exts...)
	slices.Sort(cp)
	return cp
}

// TestCodexFaceEmitsCapturedClientHello drives /responses requests through the
// full round tripper against a TLS+h2 server, captures each ClientHello, and
// asserts the codex face matches the captured genuine-CLI shape. The codex CLI
// reorders its extension order per connection from a fresh rustls order seed, so
// each dial is checked for a stable cipher/group/sig/ALPN/key-share shape, an
// identical extension multiset and membership in the rustls 0.23.45 seed space,
// while the raw extension order is expected to vary across dials.
func TestCodexFaceEmitsCapturedClientHello(t *testing.T) {
	const dials = 8
	pair := genTestCert(t)

	hellos := make([]*capturedClientHello, 0, dials)
	for i := 0; i < dials; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		helloCh := make(chan []byte, 1)
		orderCh := make(chan []string, 1)
		go func() {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			hello := readClientHelloRecords(t, raw)
			helloCh <- hello
			replay := &replayConn{Conn: raw, pending: hello}
			_ = raw.SetReadDeadline(time.Time{})
			tlsConn := tls.Server(replay, &tls.Config{
				Certificates: []tls.Certificate{pair},
				NextProtos:   []string{"h2"},
			})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			serveH2Frames(tlsConn, orderCh)
		}()

		rt := newFaceTestRoundTripper(t)
		req, err := http.NewRequest(http.MethodPost, "https://"+ln.Addr().String()+"/v1/responses", strings.NewReader("{}"))
		if err != nil {
			_ = ln.Close()
			t.Fatalf("build request: %v", err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			_ = ln.Close()
			t.Fatalf("codex face roundtrip %d: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			_ = ln.Close()
			t.Fatalf("codex face response %d: status=%d", i+1, resp.StatusCode)
		}
		if proto := resp.ProtoMajor; proto != 2 {
			_ = ln.Close()
			t.Fatalf("codex face must be served over h2, got HTTP/%d", proto)
		}
		hello := parseClientHelloRecord(t, <-helloCh)
		hellos = append(hellos, hello)
		_ = ln.Close()
	}

	// Ciphers, groups, sig algs and ALPN stay pinned; only the extension order
	// is randomized per connection. JA3 includes that order, so it differs
	// between connections — the invariants are the extension multiset and
	// membership in the rustls 0.23.45 reachable seed space, not any one order
	// and not equality with another independent random sample.
	wantExtSet := sortedExtTypes(codexFaceExtTypes)
	reachableOrders := rustlsReachableCodexOrders(t)
	orders := make(map[string]bool, dials)
	for i, hello := range hellos {
		if !reflect.DeepEqual(hello.ciphers, codexFaceCiphers) {
			t.Fatalf("codex face cipher suites mismatch (dial %d):\n got=%v\nwant=%v", i+1, hello.ciphers, codexFaceCiphers)
		}
		if !reflect.DeepEqual(hello.alpn, []string{"h2", "http/1.1"}) {
			t.Fatalf("codex face ALPN mismatch (dial %d): got %v", i+1, hello.alpn)
		}
		if !reflect.DeepEqual(hello.groups, codexFaceGroups) {
			t.Fatalf("codex face groups mismatch (dial %d):\n got=%v\nwant=%v", i+1, hello.groups, codexFaceGroups)
		}
		if !reflect.DeepEqual(hello.sigAlgs, codexFaceSigAlgs) {
			t.Fatalf("codex face signature algorithms mismatch (dial %d):\n got=%v\nwant=%v", i+1, hello.sigAlgs, codexFaceSigAlgs)
		}
		if !reflect.DeepEqual(hello.shareGrps, faceKeyShareGroups) {
			t.Fatalf("codex face key share groups mismatch (dial %d):\n got=%v\nwant=%v", i+1, hello.shareGrps, faceKeyShareGroups)
		}
		if hello.sessionLen != 32 {
			t.Fatalf("codex face session id length = %d, want 32 (dial %d)", hello.sessionLen, i+1)
		}
		if got := sortedExtTypes(hello.extTypes); !reflect.DeepEqual(got, wantExtSet) {
			t.Fatalf("codex face extension multiset mismatch (dial %d):\n got=%v\nwant=%v", i+1, got, wantExtSet)
		}
		if !reachableOrders[fmt.Sprint(hello.extTypes)] {
			t.Fatalf("codex face extension order not reachable from the rustls 0.23.45 seed space (dial %d): %v", i+1, hello.extTypes)
		}
		t.Logf("codex dial %d raw extension order: %v", i+1, hello.extTypes)
		orders[fmt.Sprint(hello.extTypes)] = true
	}
	if len(orders) < 2 {
		t.Fatalf("codex face extension order never varied across %d dials (per-connection ordering missing): %v", dials, orders)
	}
}

// serveH2Frames speaks just enough HTTP/2 to answer one request with a 200 (the
// frame loop from h2CaptureServer, over an already-established TLS connection).
func serveH2Frames(conn net.Conn, orderCh chan<- []string) {
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		return
	}
	if string(preface) != http2.ClientPreface {
		return
	}
	fr := http2.NewFramer(conn, conn)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := fr.WriteSettings(); err != nil {
		return
	}
	responded := false
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return
		}
		switch mf := f.(type) {
		case *http2.SettingsFrame:
			if !mf.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.MetaHeadersFrame:
			if responded {
				continue
			}
			responded = true
			order := make([]string, 0, len(mf.Fields))
			for _, hf := range mf.Fields {
				order = append(order, hf.Name)
			}
			orderCh <- order
			var buf bytes.Buffer
			enc := hpack.NewEncoder(&buf)
			_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
			_ = fr.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      mf.StreamID,
				BlockFragment: buf.Bytes(),
				EndHeaders:    true,
				EndStream:     true,
			})
		}
	}
}

// keyShareRawBytes returns the raw key_share extension payload from a captured
// ClientHello record (nil when absent).
func keyShareRawBytes(t *testing.T, rec []byte) []byte {
	t.Helper()
	if len(rec) < 9 || rec[0] != 0x16 {
		return nil
	}
	hsLen := int(rec[6])<<16 | int(rec[7])<<8 | int(rec[8])
	// The 4-byte handshake header is already behind us, so the body is exactly
	// hsLen bytes — NOT hsLen-4 (see parseClientHelloRecord above). Guard the
	// length first: a record split across TLS records (or a short read) must
	// report "absent", not panic on an out-of-range slice.
	if len(rec) < 9+hsLen {
		return nil
	}
	ch := rec[9 : 9+hsLen]
	pos := 0
	pos += 2 + 32
	sidLen := int(ch[pos])
	pos += 1 + sidLen
	csLen := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2 + csLen
	compLen := int(ch[pos])
	pos += 1 + compLen
	extTotal := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2
	end := pos + extTotal
	for pos < end {
		etype := uint16(ch[pos])<<8 | uint16(ch[pos+1])
		elen := int(ch[pos+2])<<8 | int(ch[pos+3])
		if etype == 51 {
			return append([]byte(nil), ch[pos+4:pos+4+elen]...)
		}
		pos += 4 + elen
	}
	return nil
}

// TestKeyShareRawBytesBodyAndTruncation pins keyShareRawBytes to the exact
// ClientHello body length (9+hsLen) and to safe behaviour on a short record.
//
// History: the helper used to slice rec[9 : 9+hsLen-4]. That did NOT panic in
// the captures we have: Go bounds-checks a slice expression against cap, not
// len, and append-built records carry spare capacity, so the walk stayed inside
// cap and merely read the wrong logical extent. It only became observable on a
// record that is itself short — where the correct slice panics unless guarded.
// Both halves are asserted here so the helper cannot regress either way.
func TestKeyShareRawBytesBodyAndTruncation(t *testing.T) {
	key := bytes.Repeat([]byte{0xa5}, 32)
	var ks []byte
	ks = append(ks, 0x00, 0x00) // client_shares length, patched below
	ks = append(ks, 0x00, 0x1d) // group: x25519
	ks = append(ks, 0x00, byte(len(key)))
	ks = append(ks, key...)
	ks[0], ks[1] = byte((len(ks)-2)>>8), byte(len(ks)-2)

	var body []byte
	body = append(body, 0x03, 0x03)                            // legacy_version
	body = append(body, make([]byte, 32)...)                   // random
	body = append(body, 0x00)                                  // session_id length (0)
	body = append(body, 0x00, 0x02, 0x13, 0x01)                // cipher_suites
	body = append(body, 0x01, 0x00)                            // compression: null
	body = append(body, byte((4+len(ks))>>8), byte(4+len(ks))) // extensions length
	body = append(body, 0x00, 0x33)                            // final extension: key_share
	body = append(body, byte(len(ks)>>8), byte(len(ks)))
	body = append(body, ks...)

	// Exact-size buffer: append() leaves spare capacity, and spare cap is what
	// let the old 9+hsLen-4 slice silently "work" (Go checks cap, not len).
	rec := make([]byte, 9+len(body))
	copy(rec, []byte{0x16, 0x03, 0x03, byte((len(body) + 4) >> 8), byte(len(body) + 4), 0x01})
	rec[6], rec[7], rec[8] = byte(len(body)>>16), byte(len(body)>>8), byte(len(body))
	copy(rec[9:], body)

	if got := keyShareRawBytes(t, rec); !bytes.Equal(got, ks) {
		t.Fatalf("key_share payload = % x, want % x", got, ks)
	}

	// A record shorter than its declared handshake length must yield nil, never a
	// panic (a ClientHello split across TLS records lands here).
	short := make([]byte, 9+len(body)-8)
	copy(short, rec)
	if got := keyShareRawBytes(t, short); got != nil {
		t.Fatalf("truncated record: got %d bytes, want nil", len(got))
	}
}

// TestSpecForDialGivesFreshKeySharesPerDial regression-tests shared-spec
// poisoning: utls's ApplyPreset writes the generated key-share bytes into the
// spec's KeyShareExtension.Data in place, so a spec pinned by a long-lived
// caller (the per-family h1 transport closure) made every dial after the first
// send the first connection's public key with no matching private key — the
// TLS 1.3 handshake then aborted with internal_error (observed live: the first
// claude-face connection succeeded, all later ones failed). Two dials on the
// same spec object must both complete and present different key-share
// material, exactly like the genuine CLIs.
func TestSpecForDialGivesFreshKeySharesPerDial(t *testing.T) {
	pair := genTestCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	helloCh := make(chan []byte, 2)
	go func() {
		for i := 0; i < 2; i++ {
			raw, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			hello := readClientHelloRecords(t, raw)
			select {
			case helloCh <- hello:
			default:
			}
			replay := &replayConn{Conn: raw, pending: hello}
			_ = raw.SetReadDeadline(time.Time{})
			tlsConn := tls.Server(replay, &tls.Config{Certificates: []tls.Certificate{pair}})
			_ = tlsConn.Handshake()
			_ = tlsConn.Close()
			_ = raw.Close()
		}
	}()

	rt := newFaceTestRoundTripper(t)
	spec := claudeCodeNodeClientHelloSpec()
	addr := ln.Addr().String()
	var hellos [2][]byte
	for i := 0; i < 2; i++ {
		uConn, dialErr := rt.dialUTLS(context.Background(), "tcp", addr, "127.0.0.1", tlsFaceClaude, spec)
		if dialErr != nil {
			t.Fatalf("dial %d failed (shared-spec poisoning regression): %v", i+1, dialErr)
		}
		_ = uConn.Close()
		select {
		case h := <-helloCh:
			hellos[i] = h
		case <-time.After(5 * time.Second):
			t.Fatalf("dial %d: no ClientHello captured", i+1)
		}
	}
	ks1 := keyShareRawBytes(t, hellos[0])
	ks2 := keyShareRawBytes(t, hellos[1])
	if len(ks1) == 0 || len(ks2) == 0 {
		t.Fatalf("missing key_share extension: ks1=%d ks2=%d", len(ks1), len(ks2))
	}
	if bytes.Equal(ks1, ks2) {
		t.Fatal("key-share material identical across dials — shared-spec poisoning regression")
	}
}
