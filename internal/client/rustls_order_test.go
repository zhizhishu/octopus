package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"sort"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Literal vectors for the rustls 0.23.45 codex ClientHello extension order.
// They were produced by the supervisor by compiling and running the upstream
// Rust ordering function (exit 0) — NOT recomputed here — so this test cannot
// self-confirm a wrong port of the algorithm. withSNI is the full 11-extension
// order (SNI id 0 included); noSNI is the same order with SNI removed, i.e. the
// order an IP target actually puts on the wire (uTLS omits SNI for IPs).
var rustlsCodexOrderLiterals = []struct {
	seed    uint16
	withSNI []uint16
	noSNI   []uint16
}{
	{0, []uint16{5, 16, 43, 11, 10, 0, 13, 35, 23, 51, 45}, []uint16{5, 16, 43, 11, 10, 13, 35, 23, 51, 45}},
	{1, []uint16{16, 10, 45, 13, 0, 43, 23, 5, 35, 11, 51}, []uint16{16, 10, 45, 13, 43, 23, 5, 35, 11, 51}},
	{42, []uint16{45, 0, 11, 43, 16, 51, 35, 10, 5, 23, 13}, []uint16{45, 11, 43, 16, 51, 35, 10, 5, 23, 13}},
	{65535, []uint16{13, 5, 11, 10, 45, 23, 43, 16, 51, 0, 35}, []uint16{13, 5, 11, 10, 45, 23, 43, 16, 51, 35}},
}

// rustlsIDList maps an extension list to its rustls type-id list (fails the
// test on an unmapped type).
func rustlsIDList(t *testing.T, exts []utls.TLSExtension) []uint16 {
	t.Helper()
	ids := make([]uint16, 0, len(exts))
	for _, ext := range exts {
		id, err := rustlsExtTypeID(ext)
		if err != nil {
			t.Fatalf("rustlsExtTypeID(%T): %v", ext, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// rustlsCodexBaseNoSNI returns the codex spec's extensions with SNI removed.
func rustlsCodexBaseNoSNI(t *testing.T) []utls.TLSExtension {
	t.Helper()
	var out []utls.TLSExtension
	for _, ext := range codexRustlsClientHelloSpec().Extensions {
		if _, isSNI := ext.(*utls.SNIExtension); isSNI {
			continue
		}
		out = append(out, ext)
	}
	return out
}

// TestRustlsOrderLiteralSeedVectors pins the port against the supervisor's
// upstream-compiled literal vectors, for both the full 11-extension order and
// the SNI-stripped order an IP target sends.
func TestRustlsOrderLiteralSeedVectors(t *testing.T) {
	for _, tc := range rustlsCodexOrderLiterals {
		got, err := rustlsOrderExtensions(codexRustlsClientHelloSpec().Extensions, tc.seed)
		if err != nil {
			t.Fatalf("seed %d with SNI: %v", tc.seed, err)
		}
		if ids := rustlsIDList(t, got); !reflect.DeepEqual(ids, tc.withSNI) {
			t.Fatalf("seed %d with SNI order mismatch:\n got=%v\nwant=%v", tc.seed, ids, tc.withSNI)
		}

		gotNoSNI, err := rustlsOrderExtensions(rustlsCodexBaseNoSNI(t), tc.seed)
		if err != nil {
			t.Fatalf("seed %d no SNI: %v", tc.seed, err)
		}
		if ids := rustlsIDList(t, gotNoSNI); !reflect.DeepEqual(ids, tc.noSNI) {
			t.Fatalf("seed %d no SNI order mismatch:\n got=%v\nwant=%v", tc.seed, ids, tc.noSNI)
		}
	}
}

// TestRustlsOrderAllSeedsArePermutations sweeps every one of the 2^16 seeds and
// checks the output is always a permutation of the codex extension set (same
// length, same multiset) and that the seed genuinely moves extensions. It also
// guards the reachable-order space is bounded by 2^16 — no exact distinct count
// is pinned (that would be brittle), only that ordering is not a no-op.
func TestRustlsOrderAllSeedsArePermutations(t *testing.T) {
	base := codexRustlsClientHelloSpec().Extensions
	want := rustlsIDList(t, base)
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

	distinct := make(map[string]bool)
	for seed := 0; seed <= 0xffff; seed++ {
		got, err := rustlsOrderExtensions(base, uint16(seed))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		ids := rustlsIDList(t, got)
		if len(ids) != len(base) {
			t.Fatalf("seed %d: length %d, want %d", seed, len(ids), len(base))
		}
		sorted := append([]uint16(nil), ids...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		if !reflect.DeepEqual(sorted, want) {
			t.Fatalf("seed %d: not a permutation:\n got=%v\nwant=%v", seed, sorted, want)
		}
		distinct[fmt.Sprint(ids)] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("only %d distinct orders across all seeds — ordering is a no-op", len(distinct))
	}
}

// TestRustlsOrderRejectsUnknownExtension pins the "unknown → error" contract:
// an extension type outside the codex set is never silently defaulted.
func TestRustlsOrderRejectsUnknownExtension(t *testing.T) {
	base := codexRustlsClientHelloSpec().Extensions
	withUnknown := append(append([]utls.TLSExtension{}, base...), &utls.SCTExtension{})
	if _, err := rustlsOrderExtensions(withUnknown, 0); err == nil {
		t.Fatalf("unmapped extension type must be rejected, got nil error")
	}
	if _, err := rustlsOrderExtensions([]utls.TLSExtension{&utls.SCTExtension{}}, 0); err == nil {
		t.Fatalf("unmapped-only extension list must be rejected, got nil error")
	}
}

// errReader always fails, to exercise the seed-reader error path.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestReadRustlsOrderSeed pins big-endian decoding and the error-on-short-read
// contract of the injectable seed helper.
func TestReadRustlsOrderSeed(t *testing.T) {
	seed, err := readRustlsOrderSeed(bytes.NewReader([]byte{0x12, 0x34}))
	if err != nil {
		t.Fatalf("read 2 bytes: %v", err)
	}
	if seed != 0x1234 {
		t.Fatalf("seed = 0x%04x, want 0x1234 (big-endian)", seed)
	}
	if _, err := readRustlsOrderSeed(errReader{err: io.ErrUnexpectedEOF}); err == nil {
		t.Fatalf("failing reader must return an error")
	}
	if _, err := readRustlsOrderSeed(bytes.NewReader([]byte{0x12})); err == nil {
		t.Fatalf("short read (1 byte) must return an error")
	}
}

// rustlsReachableCodexOrders returns the set of codex extension-type orders (as
// fmt.Sprint strings) reachable from the rustls 0.23.45 ordering across every
// one of the 2^16 seeds, with SNI stripped (an IP target omits it on the wire).
// A captured ClientHello order must be a member for the face to be genuine.
func rustlsReachableCodexOrders(t *testing.T) map[string]bool {
	t.Helper()
	base := codexRustlsClientHelloSpec().Extensions
	reachable := make(map[string]bool, 1<<16)
	for seed := 0; seed <= 0xffff; seed++ {
		ordered, err := rustlsOrderExtensions(base, uint16(seed))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		ids := make([]uint16, 0, len(ordered))
		for _, ext := range ordered {
			id, idErr := rustlsExtTypeID(ext)
			if idErr != nil {
				t.Fatalf("seed %d: %v", seed, idErr)
			}
			if id == 0x0000 {
				continue // IP targets omit SNI on the wire
			}
			ids = append(ids, id)
		}
		reachable[fmt.Sprint(ids)] = true
	}
	return reachable
}

// TestCodexFaceOrderErrorClosesRawConn pins that when rustls ordering fails on
// the codex face the already-dialed raw connection is closed and the dial
// fails, instead of leaking a half-open socket. The failure is triggered with a
// deterministically unmapped extension (no global test hook), since a real
// crypto/rand seed read is not forceable from a test.
func TestCodexFaceOrderErrorClosesRawConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	closedCh := make(chan error, 1)
	go func() {
		raw, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 1)
		_, readErr := raw.Read(buf)
		closedCh <- readErr
		_ = raw.Close()
	}()

	rt := newFaceTestRoundTripper(t)
	spec := codexRustlsClientHelloSpec()
	spec.Extensions = append(spec.Extensions, &utls.SCTExtension{}) // unmapped → order error
	if _, dialErr := rt.dialUTLS(context.Background(), "tcp", ln.Addr().String(), "127.0.0.1", tlsFaceCodex, spec); dialErr == nil {
		t.Fatalf("codex dial with an unmapped extension must fail")
	}
	select {
	case readErr := <-closedCh:
		if readErr == nil {
			t.Fatalf("server read succeeded; rawConn was not closed on the order error")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server never observed the rawConn close")
	}
}
