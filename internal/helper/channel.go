package helper

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/bestruirui/octopus/internal/client"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/log"
)

// ChannelHttpClient returns the HTTP client for one channel, with no per-request
// TLS face. Callers that carry a face on the request context must use
// ChannelHttpClientContext so the shape is honoured.
func ChannelHttpClient(channel *model.Channel) (*http.Client, error) {
	return ChannelHttpClientContext(context.Background(), channel)
}

// ChannelHttpClientContext returns the HTTP client for one channel, honouring a
// per-request TLS face carried on ctx by client.WithTLSFace.
//
// The selection rule is driven by INTENT, not by transport:
//
//	A channel that has committed to a CLI identity (a cloak shape, or a channel type
//	that IS that protocol) must present that identity's ClientHello no matter how the
//	request reaches the upstream — direct or through a proxy. The proxy is a means of
//	transport; silently dropping the face because one is configured would put a
//	"codex-shaped" request on the wire with a stock Go handshake underneath, which is
//	more identifying than not shaping at all: every other layer already claims codex.
//
//	So: face present → uTLS, with the dial routed through the proxy when the channel
//	has one. Face absent → exactly the previous behaviour (system proxy, channel
//	proxy, or direct), byte for byte.
//
// The uTLS global switch still applies to face-less traffic only; it exists to let an
// operator opt into a Chrome face for channels that never asked for a CLI identity.
func ChannelHttpClientContext(ctx context.Context, channel *model.Channel) (*http.Client, error) {
	if channel == nil {
		return nil, errors.New("channel is nil")
	}

	face := client.TLSFaceFromContext(ctx)
	log.Infof("shape diag: channel=%d type=%d face=%q proxy=%v", channel.ID, channel.Type, face, channel.Proxy)
	if face != "" {
		proxyURL, err := channelProxyURL(channel)
		if err != nil {
			return nil, err
		}
		return client.GetHTTPClientUTLSForProxy(proxyURL)
	}

	if !channel.Proxy {
		// Opt-in uTLS (Chrome JA3) applies only to direct upstream calls; proxied
		// channels keep the standard transport (uTLS-over-proxy is a follow-up). The
		// setting is default-off and must pass the relay shape re-verification first.
		if upstreamUTLSFingerprintEnabled() {
			return client.GetHTTPClientUTLSDirect()
		}
		return client.GetHTTPClientSystemProxy(false)
	} else if channel.ChannelProxy == nil || strings.TrimSpace(*channel.ChannelProxy) == "" {
		return client.GetHTTPClientSystemProxy(true)
	} else {
		return client.GetHTTPClientCustomProxy(strings.TrimSpace(*channel.ChannelProxy))
	}
}

// channelProxyURL resolves the proxy a channel should use, or nil for a direct
// connection. It mirrors the precedence ChannelHttpClientContext applies for
// face-less traffic so a shaped channel reaches the upstream over the same path
// it would have taken unshaped — only the TLS handshake on top of it changes.
func channelProxyURL(channel *model.Channel) (*url.URL, error) {
	if channel == nil {
		return nil, errors.New("channel is nil")
	}
	if !channel.Proxy {
		// A channel that is not using a proxy stays direct even though the system
		// proxy setting exists; the per-channel switch is the operator's decision.
		return nil, nil
	}
	raw := ""
	if channel.ChannelProxy != nil {
		raw = strings.TrimSpace(*channel.ChannelProxy)
	}
	if raw == "" {
		// No per-channel proxy: fall back to the instance-wide proxy setting, the
		// same value GetHTTPClientSystemProxy(true) would have used.
		systemProxy, err := op.SettingGetString(model.SettingKeyProxyURL)
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(systemProxy)
		if raw == "" {
			// The channel asked for a proxy but none is configured. Rather than fail
			// the request outright (the unshaped path errors here too, but a shaped
			// request should still reach the upstream), go direct and say so loudly:
			// the shape is preserved, which is the property this change protects.
			log.Warnf("channel %d uses a proxy but no proxy URL is configured; shaped request going direct", channel.ID)
			return nil, nil
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

func upstreamUTLSFingerprintEnabled() bool {
	enabled, err := op.SettingGetBool(model.SettingKeyUpstreamUTLSFingerprint)
	if err != nil {
		return false
	}
	return enabled
}

func ChannelBaseUrlDelayUpdate(channel *model.Channel, ctx context.Context) {
	if channel == nil {
		return
	}
	newBaseUrls := make([]model.BaseUrl, 0, len(channel.BaseUrls))
	for _, baseUrl := range channel.BaseUrls {
		if baseUrl.URL == "" {
			continue
		}
		httpClient, err := ChannelHttpClient(channel)
		if err != nil {
			log.Warnf("failed to get http client (channel=%d): %v", channel.ID, err)
			continue
		}
		delay, err := GetUrlDelay(httpClient, baseUrl.URL, ctx)
		if err != nil {
			log.Warnf("failed to get url delay (channel=%d): %v", channel.ID, err)
			continue
		}
		newBaseUrls = append(newBaseUrls, model.BaseUrl{
			URL:   baseUrl.URL,
			Delay: delay,
		})
	}
	if len(newBaseUrls) > 0 {
		op.ChannelBaseUrlUpdate(channel.ID, newBaseUrls)
	}
}
