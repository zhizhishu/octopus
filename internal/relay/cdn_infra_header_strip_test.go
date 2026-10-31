package relay

import (
	"net/http"
	"testing"
)

// CDN / 负载均衡加在入站请求上的头（CF-Ray、CDN-Loop、CF-Visitor ...）不是客户端发的，
// 真 CLI 也从来不发，出站一律不能带。客户端自己的普通头照常转发。
func TestCDNInfraHeadersAreNotForwarded(t *testing.T) {
	for _, h := range []string{"CF-Ray", "CDN-Loop", "CF-Visitor", "CF-Worker", "CF-EW-Via", "CF-Connecting-IPv6", "CF-Pseudo-IPv4",
		"X-Forwarded-Scheme", "X-Forwarded-Ssl", "X-Original-Forwarded-For", "X-Real-Port", "X-Scheme", "X-Amzn-Trace-Id"} {
		for _, name := range []string{h, http.CanonicalHeaderKey(h), " " + h + " "} {
			for _, cli := range []bool{false, true} {
				if shouldForwardClientHeaderForCaller(name, cli) {
					t.Fatalf("%q must not be forwarded upstream (cliShaped=%v)", name, cli)
				}
			}
		}
	}
	for _, h := range []string{"Anthropic-Beta", "Originator", "X-Region", "Traceparent"} {
		if !shouldForwardClientHeader(h) {
			t.Fatalf("%s should still be forwarded", h)
		}
	}
}
