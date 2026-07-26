package server

import "testing"

// TestBuildTrustedProxiesDefaultsToCloudflareOnly verifies the safe default:
// with the env unset, the trusted set is exactly the built-in Cloudflare ranges,
// so trust is never widened implicitly.
func TestBuildTrustedProxiesDefaultsToCloudflareOnly(t *testing.T) {
	t.Setenv(trustedProxiesEnv, "")
	got := buildTrustedProxies()
	if len(got) != len(cloudflareTrustedProxies) {
		t.Fatalf("unset env must yield only cloudflare ranges, got %d want %d", len(got), len(cloudflareTrustedProxies))
	}
}

// TestBuildTrustedProxiesAppendsValidEntries verifies that valid IP/CIDR entries
// are appended after the Cloudflare ranges while blanks and malformed entries are
// dropped (not silently promoted to a trusted proxy).
func TestBuildTrustedProxiesAppendsValidEntries(t *testing.T) {
	t.Setenv(trustedProxiesEnv, "172.16.0.0/12, 10.0.0.1 , , not-an-ip, 192.168.0.0/16")
	got := buildTrustedProxies()

	want := len(cloudflareTrustedProxies) + 3 // three valid entries; "" and "not-an-ip" skipped
	if len(got) != want {
		t.Fatalf("expected %d trusted proxies, got %d: %v", want, len(got), got)
	}

	extras := got[len(cloudflareTrustedProxies):]
	expectExtras := []string{"172.16.0.0/12", "10.0.0.1", "192.168.0.0/16"}
	for i, e := range expectExtras {
		if extras[i] != e {
			t.Fatalf("extra[%d]=%q want %q", i, extras[i], e)
		}
	}
}
