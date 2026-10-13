package client

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"reflect"
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

// Wire expectations transcribed from the live captures (2026-10-13,
// _artifacts/berobox/codexshape-h2/captures/). These are the regression guard:
// if the specs or uTLS emission drift, the test fails with the actual wire
// shape. SNI is absent because the test dials an IP, exactly like the captures.
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
		0x1302, 0x1303, 0x1301, 0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8, 0xccaa,
		0xc02b, 0xc02f, 0x009e, 0xc024, 0xc028, 0x006b, 0xc023, 0xc027, 0x0067,
		0xc00a, 0xc014, 0x0039, 0xc009, 0xc013, 0x0033, 0x009d, 0x009c, 0x003d,
		0x003c, 0x0035, 0x002f,
	}
	claudeFaceExtTypes = []uint16{
		65281, 11, 10, 35, 22, 23, 13, 43, 45, 51,
	}
	claudeFaceSigAlgs = []uint16{
		0x0905, 0x0906, 0x0904, 0x0403, 0x0503, 0x0603, 0x0807, 0x0808,
		0x081a, 0x081b, 0x081c, 0x0809, 0x080a, 0x080b, 0x0804, 0x0805, 0x0806,
		0x0401, 0x0501, 0x0601, 0x0303, 0x0301, 0x0302, 0x0402, 0x0502, 0x0602,
	}
	claudeFaceGroups = []uint16{0x11ec, 0x001d, 0x0017, 0x001e, 0x0018, 0x0019, 0x0100, 0x0101}

	faceKeyShareGroups = []uint16{0x11ec, 0x001d}
)

// capturedClientHello is the parsed wire shape of one ClientHello.
type capturedClientHello struct {
	ciphers    []uint16
	extTypes   []uint16
	alpn       []string
	groups     []uint16
	sigAlgs    []uint16
	shareGrps  []uint16
	sessionLen int
}

// parseClientHelloRecord parses the ClientHello handshake message out of the
// first TLS record(s) on the wire (test-only mini parser).
func parseClientHelloRecord(t *testing.T, rec []byte) *capturedClientHello {
	t.Helper()
	if len(rec) < 9 || rec[0] != 0x16 {
		t.Fatalf("not a handshake record: % x", rec[:min(9, len(rec))])
	}
	hsLen := int(rec[6])<<16 | int(rec[7])<<8 | int(rec[8])
	ch := rec[9 : 9+hsLen-4] // skip handshake header (type+len) already consumed
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
			if len(buf) >= 9+hsLen-4 {
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
// asserts the wire shape matches the live-captured Claude Code face (no ALPN,
// HTTP/1.1 carried over the uTLS connection).
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
	if len(hello.alpn) != 0 {
		t.Fatalf("claude face must offer no ALPN (CLI speaks h1), got %v", hello.alpn)
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

// TestCodexFaceEmitsCapturedClientHello drives a /responses request through the
// full round tripper against a TLS+h2 server that captures the ClientHello, and
// asserts the wire shape matches the live-captured codex CLI face (ALPN
// h2+http/1.1, request served over the pooled fhttp2 connection).
func TestCodexFaceEmitsCapturedClientHello(t *testing.T) {
	pair := genTestCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

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
		t.Fatalf("build request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("codex face roundtrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("codex face response: status=%d", resp.StatusCode)
	}
	if proto := resp.ProtoMajor; proto != 2 {
		t.Fatalf("codex face must be served over h2, got HTTP/%d", proto)
	}

	hello := parseClientHelloRecord(t, <-helloCh)
	if !reflect.DeepEqual(hello.ciphers, codexFaceCiphers) {
		t.Fatalf("codex face cipher suites mismatch:\n got=%v\nwant=%v", hello.ciphers, codexFaceCiphers)
	}
	if !reflect.DeepEqual(hello.extTypes, codexFaceExtTypes) {
		t.Fatalf("codex face extension order mismatch:\n got=%v\nwant=%v", hello.extTypes, codexFaceExtTypes)
	}
	if !reflect.DeepEqual(hello.alpn, []string{"h2", "http/1.1"}) {
		t.Fatalf("codex face ALPN mismatch: got %v", hello.alpn)
	}
	if !reflect.DeepEqual(hello.groups, codexFaceGroups) {
		t.Fatalf("codex face groups mismatch:\n got=%v\nwant=%v", hello.groups, codexFaceGroups)
	}
	if !reflect.DeepEqual(hello.sigAlgs, codexFaceSigAlgs) {
		t.Fatalf("codex face signature algorithms mismatch:\n got=%v\nwant=%v", hello.sigAlgs, codexFaceSigAlgs)
	}
	if !reflect.DeepEqual(hello.shareGrps, faceKeyShareGroups) {
		t.Fatalf("codex face key share groups mismatch:\n got=%v\nwant=%v", hello.shareGrps, faceKeyShareGroups)
	}
	if hello.sessionLen != 32 {
		t.Fatalf("codex face session id length = %d, want 32", hello.sessionLen)
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
