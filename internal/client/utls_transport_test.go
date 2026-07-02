package client

import (
	"net/http"
	"testing"
)

type recordingRoundTripper struct{ called bool }

func (r *recordingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.called = true
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

// Plain http:// must bypass the uTLS/JA3 path entirely and use the fallback
// transport — the fingerprint only applies to HTTPS.
func TestUTLSRoundTripperDelegatesPlainHTTP(t *testing.T) {
	fb := &recordingRoundTripper{}
	rt := newUTLSRoundTripper(nil, fb) // dialContext is never reached on the http:// path
	req, err := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("plain http roundtrip failed: %v", err)
	}
	if !fb.called {
		t.Fatalf("plain http:// must be delegated to the fallback transport")
	}
}

// GetHTTPClientUTLSDirect builds a cached client whose transport is the uTLS
// round tripper, so the opt-in path is wired end to end.
func TestGetHTTPClientUTLSDirectCaches(t *testing.T) {
	c1, err := GetHTTPClientUTLSDirect()
	if err != nil || c1 == nil {
		t.Fatalf("uTLS direct client build failed: c=%v err=%v", c1, err)
	}
	c2, err := GetHTTPClientUTLSDirect()
	if err != nil {
		t.Fatalf("second uTLS direct client build failed: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("uTLS direct client must be cached (same instance)")
	}
	if _, ok := c1.Transport.(*utlsRoundTripper); !ok {
		t.Fatalf("uTLS direct client transport must be *utlsRoundTripper, got %T", c1.Transport)
	}
}
