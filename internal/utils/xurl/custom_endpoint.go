package xurl

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// Custom (type 6) channel URL derivation.
//
// A custom channel stores the *final* request URL (for example
// https://upstream.example/api/paas/v4/chat/completions). Everything derived from
// it therefore only needs the same origin: the scheme and host of the stored URL
// plus a standard path, so one custom channel can be asked whether it also serves
// /models, /v1/embeddings, /v1/images, and so on.
//
// Two properties are load-bearing here:
//
//  1. Nothing is derived by editing the raw string. The URL is parsed and rebuilt,
//     so an existing query survives untouched (some gateways require
//     ?api-version=… on every path) and a path can never be double-joined.
//  2. Errors never echo the input. A custom channel URL is operator supplied and
//     may carry credentials (userinfo, or a key in the query). These messages can
//     reach an API response, a log line, or the UI, so they say what is wrong
//     without ever containing the value.
//
// Userinfo is deliberately dropped from every derived URL: the channel key
// travels in the Authorization header, and copying credentials into a second URL
// is not needed for the request to work.
const (
	customChannelChatCompletionsSuffix = "/chat/completions"
	customChannelModelsLeaf            = "models"
	customChannelFallbackModelsPath    = "/v1/models"
)

var (
	errCustomChannelURLEmpty   = errors.New("custom channel URL is empty")
	errCustomChannelURLInvalid = errors.New("custom channel URL is not a valid absolute URL")
	errCustomChannelURLNoHTTP  = errors.New("custom channel URL must start with http:// or https://")
	errCustomChannelURLNoHost  = errors.New("custom channel URL must include a host")
	errCustomChannelURLBadPort = errors.New("custom channel URL port must be a number between 1 and 65535")
)

// CustomChannelBaseURL returns the same-origin root of a custom channel URL with
// the original query preserved ("https://upstream.example?api-version=1").
//
// This is what a derived protocol test hands to an outbound adapter as its base
// URL: the adapter appends its own standard path (/v1/chat/completions,
// /v1/responses, /v1/messages, /v1beta/models/<model>:generateContent,
// /v1/images/generations, …) and the query rides along, so a gateway that pins a
// version in the query keeps working.
func CustomChannelBaseURL(rawURL string) (string, error) {
	parsed, err := parseCustomChannelURL(rawURL)
	if err != nil {
		return "", err
	}
	return sameOriginURL(parsed, "", parsed.RawQuery), nil
}

// CustomChannelModelsURL derives the model-list URL for a custom channel.
//
// A recognised /chat/completions tail is replaced by /models, keeping whatever
// version prefix the provider uses:
//
//	https://upstream.example/v1/chat/completions             -> https://upstream.example/v1/models
//	https://upstream.example/api/paas/v4/chat/completions    -> https://upstream.example/api/paas/v4/models
//
// A URL that is already a model-list URL is returned unchanged (so a refresh is
// idempotent), and anything else — no recognised tail, or a bare
// /chat/completions with no version prefix to keep — falls back to
// CustomChannelModelsFallbackURL. The query is preserved in every case.
func CustomChannelModelsURL(rawURL string) (string, error) {
	parsed, err := parseCustomChannelURL(rawURL)
	if err != nil {
		return "", err
	}
	path := customChannelPath(parsed)
	lowerPath := strings.ToLower(path)
	if strings.HasSuffix(lowerPath, "/"+customChannelModelsLeaf) {
		return sameOriginURL(parsed, path, parsed.RawQuery), nil
	}
	if strings.HasSuffix(lowerPath, customChannelChatCompletionsSuffix) {
		dir := strings.TrimRight(path[:len(path)-len(customChannelChatCompletionsSuffix)], "/")
		if dir != "" {
			return sameOriginURL(parsed, dir+"/"+customChannelModelsLeaf, parsed.RawQuery), nil
		}
	}
	return CustomChannelModelsFallbackURL(rawURL)
}

// CustomChannelModelsFallbackURL is the same-origin standard model list
// ("<origin>/v1/models"). It is the second and last candidate a custom channel's
// model sync tries, so a provider that keeps its list on the plain OpenAI path
// still works when the sibling derivation misses.
func CustomChannelModelsFallbackURL(rawURL string) (string, error) {
	parsed, err := parseCustomChannelURL(rawURL)
	if err != nil {
		return "", err
	}
	return sameOriginURL(parsed, customChannelFallbackModelsPath, parsed.RawQuery), nil
}

// parseCustomChannelURL parses raw and requires an absolute http(s) URL with a
// non-empty hostname and, when a port is written, a port in 1..65535.
//
// net/url only checks that a port is *numeric* (validOptionalPort), so it happily
// accepts http://host:99999999/… — a value no dialer can honour. The range is
// checked here with strconv.Atoi, which also catches digit strings too long for an
// int. url.Parse's and Atoi's own error text echoes the input, so neither is ever
// propagated: every error below is a fixed message.
func parseCustomChannelURL(rawURL string) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, errCustomChannelURLEmpty
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, errCustomChannelURLInvalid
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return nil, errCustomChannelURLNoHTTP
	}
	// Host can be ":80" (empty hostname, valid port); that is not an origin.
	if parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errCustomChannelURLNoHost
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, errCustomChannelURLBadPort
		}
	}
	return parsed, nil
}

// customChannelPath normalises the parsed path to "/a/b" form ("" for the root),
// with trailing slashes removed so a copy-pasted URL with a slash behaves like one
// without.
func customChannelPath(parsed *url.URL) string {
	path := "/" + strings.Trim(parsed.Path, "/")
	if path == "/" {
		return ""
	}
	return path
}

// sameOriginURL rebuilds the URL from scheme + host only, which is what drops
// userinfo. An empty path yields "scheme://host" (net/url omits the slash).
func sameOriginURL(parsed *url.URL, path string, rawQuery string) string {
	return (&url.URL{
		Scheme:   strings.ToLower(parsed.Scheme),
		Host:     parsed.Host,
		Path:     path,
		RawQuery: rawQuery,
	}).String()
}
