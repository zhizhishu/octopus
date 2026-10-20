package helper

import (
	"context"
	"net/http"
	"testing"

	"github.com/zhizhishu/octopus/internal/client"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// TestShapedChannelKeepsUTLSThroughProxy is the regression test for the rule that
// decided this change: a channel that presents a CLI identity must keep that
// identity's ClientHello no matter how it reaches the upstream. The previous
// selection short-circuited on "channel uses a proxy", so a shaped channel silently
// fell back to a stock Go handshake — the one combination that is more identifying
// than not shaping at all, because the headers still claim to be the CLI.
func TestShapedChannelKeepsUTLSThroughProxy(t *testing.T) {
	const proxyURL = "http://127.0.0.1:18080"

	cases := []struct {
		name    string
		channel *dbmodel.Channel
		face    string
	}{
		{
			name: "codex face, channel proxy",
			channel: &dbmodel.Channel{
				ID:           1,
				Type:         outbound.OutboundTypeOpenAIResponse,
				Proxy:        true,
				ChannelProxy: strPtr(proxyURL),
			},
			face: "codex",
		},
		{
			name: "claude face, channel proxy",
			channel: &dbmodel.Channel{
				ID:           2,
				Type:         outbound.OutboundTypeAnthropic,
				Proxy:        true,
				ChannelProxy: strPtr(proxyURL),
			},
			face: "claude",
		},
		{
			name: "codex face, direct channel stays on the direct uTLS client",
			channel: &dbmodel.Channel{
				ID:   3,
				Type: outbound.OutboundTypeOpenAIResponse,
			},
			face: "codex",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := client.WithTLSFace(context.Background(), tc.face)
			httpClient, err := ChannelHttpClientContext(ctx, tc.channel)
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			if httpClient == nil || httpClient.Transport == nil {
				t.Fatal("no transport")
			}
			// The uTLS transport is the only one that presents a CLI ClientHello. Its
			// concrete type is the contract here: everything else means a stock
			// handshake, which is what this change exists to prevent.
			if got := transportKind(httpClient); got != "utls" {
				t.Fatalf("shaped channel must use the uTLS transport, got %q", got)
			}
		})
	}
}

// TestUnshapedChannelKeepsPriorSelection is the zero-regression half: without a
// face, client selection must be exactly what it was before this change.
func TestUnshapedChannelKeepsPriorSelection(t *testing.T) {
	cases := []struct {
		name        string
		channel     *dbmodel.Channel
		wantNotUTLS bool
	}{
		{
			name: "generic chat channel with a proxy stays on the proxy transport",
			channel: &dbmodel.Channel{
				ID:           10,
				Type:         outbound.OutboundTypeOpenAIChat,
				Proxy:        true,
				ChannelProxy: strPtr("http://127.0.0.1:18080"),
			},
			wantNotUTLS: true,
		},
		{
			name: "gemini channel with a proxy stays on the proxy transport",
			channel: &dbmodel.Channel{
				ID:           11,
				Type:         outbound.OutboundTypeGemini,
				Proxy:        true,
				ChannelProxy: strPtr("http://127.0.0.1:18080"),
			},
			wantNotUTLS: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpClient, err := ChannelHttpClientContext(context.Background(), tc.channel)
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			isUTLS := transportKind(httpClient) == "utls"
			if tc.wantNotUTLS && isUTLS {
				t.Fatal("face-less channel must not switch to the uTLS transport")
			}
		})
	}
}

// TestChannelProxyURLPrecedence pins the proxy resolution a shaped channel uses, so
// tunnelling cannot silently change which proxy an operator configured.
func TestChannelProxyURLPrecedence(t *testing.T) {
	specific := "http://127.0.0.1:18081"

	got, err := channelProxyURL(&dbmodel.Channel{
		Proxy:        true,
		ChannelProxy: strPtr(specific),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil || got.String() != specific {
		t.Fatalf("channel proxy must win, got %v", got)
	}

	// A channel that is not using a proxy stays direct even if a channel proxy is
	// configured: the per-channel switch is the operator's decision.
	got, err = channelProxyURL(&dbmodel.Channel{
		Proxy:        false,
		ChannelProxy: strPtr(specific),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("non-proxy channel must resolve to direct, got %v", got)
	}
}

func strPtr(s string) *string { return &s }

func transportKind(c *http.Client) string { return client.TransportKind(c) }
