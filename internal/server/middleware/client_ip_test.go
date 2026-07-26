package middleware

import (
	"net"
	"sync"
	"testing"
)

func mustNets(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	nets := parseNets(entries)
	if len(nets) != len(entries) {
		t.Fatalf("parseNets(%v) parsed %d of %d entries", entries, len(nets), len(entries))
	}
	return nets
}

// hdr builds a header lookup for the pure resolver.
func hdr(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestResolveClientIPUntrustedPeerIgnoresForgedHeaders is the core spoofing guard:
// when the direct peer is NOT a trusted proxy, a client-supplied X-Forwarded-For /
// CF-Connecting-IP must be ignored and the real peer returned. Otherwise anyone
// could forge their audit IP and bypass per-IP rate limiting.
func TestResolveClientIPUntrustedPeerIgnoresForgedHeaders(t *testing.T) {
	nets := mustNets(t, "172.16.0.0/12")
	got := resolveClientIP("203.0.113.9", hdr(map[string]string{
		"X-Forwarded-For":  "9.9.9.9",
		"CF-Connecting-IP": "8.8.8.8",
	}), nets)
	if got != "203.0.113.9" {
		t.Fatalf("untrusted peer must return the real peer, ignoring forged headers; got %q", got)
	}
}

// TestResolveClientIPTrustedPeerResolvesRealClient verifies that behind a trusted
// proxy the client is taken from X-Forwarded-For via the right-to-left trusted walk
// (the rightmost hop is the trusted proxy; the next is the real client).
func TestResolveClientIPTrustedPeerResolvesRealClient(t *testing.T) {
	nets := mustNets(t, "172.16.0.0/12")
	got := resolveClientIP("172.20.0.5", hdr(map[string]string{
		"X-Forwarded-For": "203.0.113.9, 172.20.0.5",
	}), nets)
	if got != "203.0.113.9" {
		t.Fatalf("trusted peer must resolve the real client from XFF; got %q", got)
	}
}

// TestResolveClientIPTrustedPeerCFConnectingIP verifies CF-Connecting-IP is honored
// (and preferred, matching the header order) for a trusted peer.
func TestResolveClientIPTrustedPeerCFConnectingIP(t *testing.T) {
	nets := mustNets(t, "172.16.0.0/12")
	got := resolveClientIP("172.20.0.5", hdr(map[string]string{
		"CF-Connecting-IP": "203.0.113.9",
		"X-Forwarded-For":  "10.0.0.1",
	}), nets)
	if got != "203.0.113.9" {
		t.Fatalf("trusted peer should resolve CF-Connecting-IP first; got %q", got)
	}
}

// TestResolveClientIPTrustedPeerNoHeaders falls back to the peer when a trusted
// proxy forwards no client headers.
func TestResolveClientIPTrustedPeerNoHeaders(t *testing.T) {
	nets := mustNets(t, "172.16.0.0/12")
	got := resolveClientIP("172.20.0.5", hdr(map[string]string{}), nets)
	if got != "172.20.0.5" {
		t.Fatalf("trusted peer with no forwarding headers should return the peer; got %q", got)
	}
}

// TestResolveClientIPIPv6Peer verifies an IPv6 peer/CIDR works end to end.
func TestResolveClientIPIPv6Peer(t *testing.T) {
	nets := mustNets(t, "fd00::/8")
	got := resolveClientIP("fd00::1", hdr(map[string]string{
		"X-Forwarded-For": "2001:db8::1, fd00::1",
	}), nets)
	if got != "2001:db8::1" {
		t.Fatalf("trusted IPv6 peer must resolve the real IPv6 client; got %q", got)
	}
}

// TestValidateForwardedHeaderBreaksOnMalformed mirrors gin: a malformed entry stops
// the walk (so a garbage segment cannot let a spoofed left value through).
func TestValidateForwardedHeaderBreaksOnMalformed(t *testing.T) {
	nets := mustNets(t, "172.16.0.0/12")
	if _, ok := validateForwardedHeader("garbage, 172.20.0.5", nets); ok {
		t.Fatal("a malformed rightmost entry must invalidate the header (gin break semantics)")
	}
}

// TestStaticTrustedProxiesDefault verifies the safe default: with no env set the
// static base is exactly the Cloudflare ranges (trust is never widened implicitly).
func TestStaticTrustedProxiesDefault(t *testing.T) {
	t.Setenv(trustedProxiesEnv, "")
	// initStaticProxies is sync.Once; reset so this test observes the empty env.
	staticProxyOnce = sync.Once{}
	got := StaticTrustedProxies()
	if len(got) != len(cloudflareTrustedProxies) {
		t.Fatalf("default trusted set must be Cloudflare-only, got %d want %d", len(got), len(cloudflareTrustedProxies))
	}
}

// TestStaticTrustedProxiesAppendsEnv verifies valid env entries are appended and
// malformed ones dropped.
func TestStaticTrustedProxiesAppendsEnv(t *testing.T) {
	t.Setenv(trustedProxiesEnv, "172.16.0.0/12, 10.0.0.1 , , not-an-ip")
	staticProxyOnce = sync.Once{}
	got := StaticTrustedProxies()
	want := len(cloudflareTrustedProxies) + 2 // two valid; "" and "not-an-ip" dropped
	if len(got) != want {
		t.Fatalf("expected %d entries, got %d: %v", want, len(got), got)
	}
}
