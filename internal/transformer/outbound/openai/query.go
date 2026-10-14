package openai

import (
	"net/url"
	"strings"
)

// inboundQueryDenylist lists query parameters that are protocol-specific to
// non-OpenAI inbound formats. When a request crosses protocols (e.g. an
// Anthropic /v1/messages?beta=true client hitting a Chat channel), these
// parameters must not leak into the outbound URL: a real chat client never
// sends them, so their presence fingerprints the request as converter traffic
// (new-api keeps beta only for same-protocol anthropic outbound; we match
// that behavior for the OpenAI family).
var inboundQueryDenylist = map[string]struct{}{
	"beta": {}, // Anthropic /v1/messages?beta=true switch
}

func mergeInboundQuery(parsedURL *url.URL, requestQuery url.Values) {
	if parsedURL == nil || len(requestQuery) == 0 {
		return
	}
	q := parsedURL.Query()
	for key, values := range requestQuery {
		if _, blocked := inboundQueryDenylist[strings.ToLower(strings.TrimSpace(key))]; blocked {
			continue
		}
		q.Del(key)
		for _, value := range values {
			q.Add(key, value)
		}
	}
	parsedURL.RawQuery = q.Encode()
}
