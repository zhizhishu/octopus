package helper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

const channelProxyProbeTimeout = 10 * time.Second

type ChannelProxyInfo struct {
	Used        bool
	Source      string
	Scheme      string
	Host        string
	Description string
}

// ChannelProxyInfoFor returns redacted proxy metadata that matches
// ChannelHttpClient's routing decision.
func ChannelProxyInfoFor(channel *model.Channel) (ChannelProxyInfo, error) {
	if channel == nil {
		return ChannelProxyInfo{}, fmt.Errorf("channel is nil")
	}
	if !channel.Proxy {
		return ChannelProxyInfo{Description: "direct"}, nil
	}

	source := "system"
	proxyURL := ""
	if channel.ChannelProxy != nil && strings.TrimSpace(*channel.ChannelProxy) != "" {
		source = "channel"
		proxyURL = strings.TrimSpace(*channel.ChannelProxy)
	} else {
		value, err := op.SettingGetString(model.SettingKeyProxyURL)
		if err != nil {
			return ChannelProxyInfo{Used: true, Source: source}, err
		}
		proxyURL = strings.TrimSpace(value)
	}
	if proxyURL == "" {
		return ChannelProxyInfo{Used: true, Source: source}, fmt.Errorf("proxy url is empty")
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return ChannelProxyInfo{Used: true, Source: source}, fmt.Errorf("invalid proxy url: %w", err)
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	host := parsed.Host
	if scheme == "" {
		scheme = "proxy"
	}
	if host == "" {
		host = redactedURLForDisplay(proxyURL)
	}
	description := fmt.Sprintf("%s %s proxy %s", source, scheme, host)
	return ChannelProxyInfo{
		Used:        true,
		Source:      source,
		Scheme:      scheme,
		Host:        host,
		Description: description,
	}, nil
}

// CheckChannelProxyConnectivity verifies that the configured HTTP/SOCKS proxy
// can establish a request to the channel base URL. Any HTTP status from the
// target counts as connectivity success; provider/API validity is tested by the
// subsequent model request.
func CheckChannelProxyConnectivity(ctx context.Context, channel *model.Channel) (ChannelProxyInfo, int, error) {
	if channel == nil {
		return ChannelProxyInfo{}, 0, fmt.Errorf("channel is nil")
	}
	if !channel.Proxy {
		return ChannelProxyInfo{}, 0, nil
	}

	info, err := ChannelProxyInfoFor(channel)
	if err != nil {
		return info, 0, err
	}
	client, err := ChannelHttpClient(channel)
	if err != nil {
		return info, 0, err
	}

	target := strings.TrimSpace(channel.GetBaseUrl())
	if target == "" {
		return info, 0, fmt.Errorf("channel has no base url")
	}
	probeURL, err := normalizeProxyProbeURL(target)
	if err != nil {
		return info, 0, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, channelProxyProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, probeURL, nil)
	if err != nil {
		return info, 0, err
	}
	req.Header.Set("User-Agent", "Octopus-Proxy-Check/1.0")
	info.Description = fmt.Sprintf("%s -> %s", info.Description, redactedURLForDisplay(probeURL))

	resp, err := client.Do(req)
	if err != nil {
		return info, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return info, resp.StatusCode, nil
}

func normalizeProxyProbeURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid base url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid base url")
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String(), nil
}

func redactedURLForDisplay(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimSpace(raw)
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String()
}
