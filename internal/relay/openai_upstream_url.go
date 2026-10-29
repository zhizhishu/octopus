package relay

import (
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// openAIUpstreamURL builds the upstream URL for an OpenAI-compatible request.
// Type 6 (custom chat) is verbatim: endpoint is ignored unless it is an
// absolute URL (ParseEndpointOverride). verbatimOverride is the channel's
// path field, checked first so a full URL the user stored there still wins.
func openAIUpstreamURL(channel *model.Channel, endpoint, verbatimOverride string) (string, error) {
	if channel != nil && channel.Type == outbound.OutboundTypeCustomOpenAIChat {
		override := strings.TrimSpace(verbatimOverride)
		if override == "" {
			override = endpoint
		}
		return xurl.VerbatimRequestURL(channel.GetBaseUrl(), override)
	}
	base := ""
	if channel != nil {
		base = channel.GetBaseUrl()
	}
	return xurl.JoinOpenAIPath(base, endpoint)
}

func channelImagesOverride(channel *model.Channel) string {
	if channel == nil {
		return ""
	}
	return channel.OpenAIImagesPath
}

func channelVideosOverride(channel *model.Channel) string {
	if channel == nil {
		return ""
	}
	return channel.OpenAIVideosPath
}
