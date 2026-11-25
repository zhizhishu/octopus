package modeltest

import (
	"fmt"
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// customTestEndpointName is the endpoint value that means "test this custom
// channel against its own full URL, byte for byte". It is the only selection that
// is verbatim; every other selection is an explicit protocol test and is derived
// onto same-origin standard paths (see channelForTestEndpoint).
const customTestEndpointName = "custom"

// Only explicit channel tests switch protocols; saved configuration and audit probes keep their contract.
func channelForTestEndpoint(channel *dbmodel.Channel, value string) (*dbmodel.Channel, error) {
	if channel == nil {
		return nil, fmt.Errorf("channel is nil")
	}
	endpoint, err := normalizeEndpoint(value)
	if err != nil {
		return nil, err
	}
	// "custom" is the channel's own URL and only exists for a custom channel.
	// Testing it anywhere else would mean "send a chat body to some other
	// channel's /v1/chat/completions", which is the plain chat test.
	if endpoint.name == customTestEndpointName {
		if channel.Type != outbound.OutboundTypeCustomOpenAIChat {
			return nil, fmt.Errorf("endpoint %q only applies to a custom channel that stores a full request URL", customTestEndpointName)
		}
		return channel, nil
	}

	if channel.Type == outbound.OutboundTypeCustomOpenAIChat {
		return customChannelTestEndpoint(channel, endpoint.name)
	}

	// Baseline behaviour for every ordinary channel: only the four chat protocols
	// remap the channel type. Embeddings / images / decision keep the stored type
	// and are judged by their own gates in the runner — deriving them is a
	// custom-channel (type 6) privilege, because only there is the full URL known.
	channelType, chatProtocol := testEndpointChannelType(endpoint.name)
	if !chatProtocol {
		return channel, nil
	}
	if !outbound.IsChatChannelType(channel.Type) || channel.Type == channelType {
		return channel, nil
	}
	copy := *channel
	copy.Type = channelType
	return &copy, nil
}

// testEndpointChannelType maps a chat-protocol test endpoint to the channel type
// whose outbound adapter speaks that protocol. Only the four chat protocols are
// mapped: an ordinary channel's embeddings/decision tests must keep their stored
// type, exactly as before this feature.
func testEndpointChannelType(name string) (outbound.OutboundType, bool) {
	switch name {
	case "openai_chat":
		return outbound.OutboundTypeOpenAIChat, true
	case "openai_responses":
		return outbound.OutboundTypeOpenAIResponse, true
	case "anthropic_messages":
		return outbound.OutboundTypeAnthropic, true
	case "gemini_generate_content":
		return outbound.OutboundTypeGemini, true
	default:
		return 0, false
	}
}

// customChannelTestEndpoint derives the value handed to the adapter for an
// explicit protocol test on a custom (type-6) channel.
//
// A custom channel stores a full, arbitrary request URL, so every protocol test
// on it has to be rebuilt from the same origin: the stored URL's scheme and host
// (with its query kept, some gateways pin a version there) plus the standard path
// the selected protocol's adapter appends. Nothing of the stored path survives
// except its origin — the operator asked for "the Responses endpoint of THIS host",
// not "this exact chat URL with a Responses body".
//
// The copy never reaches the config: it is a value used for one test round, and
// the original channel (its Type, BaseUrls backing array, capability slice and
// path overrides) is left untouched.
func customChannelTestEndpoint(channel *dbmodel.Channel, endpointName string) (*dbmodel.Channel, error) {
	// The derivation source is the URL this channel really posts chat traffic to:
	// an absolute chat-path override wins over the stored base URL, exactly as in
	// modelTestOutboundBaseURL / VerbatimRequestBaseURL.
	chatURL := strings.TrimSpace(channel.VerbatimRequestBaseURL(false))
	if chatURL == "" {
		return nil, fmt.Errorf("custom channel has no full request URL, so a %s endpoint cannot be derived from it", endpointName)
	}

	base, err := xurl.CustomChannelBaseURL(chatURL)
	if err != nil {
		return nil, fmt.Errorf("cannot derive a %s endpoint from this custom channel: %w", endpointName, err)
	}

	copy := *channel
	// Fresh backing array: rewriting element 0 of the shared slice would mutate
	// the channel the caller holds (and, on a route test, the cached one).
	copy.BaseUrls = []dbmodel.BaseUrl{{URL: base}}
	// Every path override belongs to the stored protocol. Left in place, an
	// absolute override would silently replace the derived URL — the test would
	// report the custom chat URL while claiming to test another endpoint.
	copy.OpenAIChatPath = ""
	copy.OpenAIModelsPath = ""
	copy.OpenAIEmbeddingsPath = ""
	copy.OpenAIVideosPath = ""
	copy.OpenAITasksPath = ""
	copy.OpenAIImagesPath = ""

	switch endpointName {
	case embeddingTestEndpointName:
		// An embedding channel type inherently supports embeddings
		// (model.Channel.SupportsCapability), so the copy needs no capability edit;
		// the stored capability slice is not touched either.
		copy.Type = outbound.OutboundTypeOpenAIEmbedding
	case "systemone":
		copy.Type = outbound.OutboundTypeSystemOne
	case imageGenerationTestEndpointName:
		// Images ride the same chat-compatible type as production: with the stored
		// path overrides cleared, relay.openAIUpstreamURL joins the standard
		// <origin>/v1/images/generations onto the derived base, query included.
		copy.Type = outbound.OutboundTypeOpenAIChat
	default:
		channelType, chatProtocol := testEndpointChannelType(endpointName)
		if !chatProtocol {
			return channel, nil
		}
		copy.Type = channelType
	}
	return &copy, nil
}
