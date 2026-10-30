package relay

import (
	"errors"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// CustomChannelNoFullURLReason is why a type-6 channel is skipped for a
// non-chat endpoint. Chat uses the base URL as-is. Embeddings, images, and
// videos run only when their own path field is an absolute URL. Raw protocols
// have no such field, so they are always skipped.
const CustomChannelNoFullURLReason = "custom channel has no full URL for this endpoint"

var errCustomChannelNoFullURL = errors.New(CustomChannelNoFullURLReason)

func customChannelAbsoluteURL(raw string) (string, bool) {
	abs, ok, err := xurl.ParseEndpointOverride(raw)
	if err != nil || !ok {
		return "", false
	}
	return abs, true
}

// openAIUpstreamURL builds the upstream URL for an OpenAI-compatible request.
//
// Type 6 is verbatim for chat only. For images and videos the caller passes
// the path from GetOpenAIImagesPath / videoUpstreamPathForChannel. When the
// channel's path field is an absolute URL, that helper already replaced the
// /v1/images or /v1/videos prefix and appended the operation suffix, so the
// endpoint string is itself absolute and is used as the request URL. A missing
// or relative field does not fall back to the chat URL.
func openAIUpstreamURL(channel *model.Channel, endpoint string) (string, error) {
	if channel != nil && channel.Type == outbound.OutboundTypeCustomOpenAIChat {
		if abs, ok, err := xurl.ParseEndpointOverride(endpoint); ok || err != nil {
			return abs, err
		}
		return "", errCustomChannelNoFullURL
	}
	base := ""
	if channel != nil {
		base = channel.GetBaseUrl()
	}
	return xurl.JoinOpenAIPath(base, endpoint)
}
