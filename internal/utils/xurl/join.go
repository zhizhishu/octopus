package xurl

import (
	"net/url"
	"strings"
)

// JoinPath appends endpoint to baseURL while avoiding duplicated leading API
// version segments such as /v1/v1/messages.
func JoinPath(baseURL string, endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}

	endpointPath := "/" + strings.Trim(strings.TrimSpace(endpoint), "/")
	if endpointPath == "/" {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawPath = ""
		return parsed.String(), nil
	}

	basePath := strings.TrimRight(parsed.Path, "/")
	if samePathSuffix(basePath, endpointPath) {
		parsed.Path = basePath
		parsed.RawPath = ""
		return parsed.String(), nil
	}

	firstEndpointSegment := firstPathSegment(endpointPath)
	if firstEndpointSegment != "" && strings.EqualFold(lastPathSegment(basePath), firstEndpointSegment) {
		endpointPath = strings.TrimPrefix(endpointPath, "/"+firstEndpointSegment)
	}

	if basePath == "" {
		parsed.Path = endpointPath
	} else {
		parsed.Path = basePath + endpointPath
	}
	parsed.RawPath = ""
	return parsed.String(), nil
}

// TrimPathSuffixes removes one known API endpoint suffix from baseURL. It keeps
// provider-specific prefixes, queries, and hosts intact.
func TrimPathSuffixes(baseURL string, suffixes ...string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}

	basePath := "/" + strings.Trim(parsed.Path, "/")
	if basePath == "/" {
		parsed.Path = ""
		parsed.RawPath = ""
		return parsed.String(), nil
	}

	lowerBase := strings.ToLower(basePath)
	for _, suffix := range suffixes {
		suffixPath := "/" + strings.Trim(strings.TrimSpace(suffix), "/")
		if suffixPath == "/" {
			continue
		}
		lowerSuffix := strings.ToLower(suffixPath)
		if lowerBase == lowerSuffix || strings.HasSuffix(lowerBase, lowerSuffix) {
			nextPath := strings.TrimRight(basePath[:len(basePath)-len(suffixPath)], "/")
			if nextPath == "" || nextPath == "/" {
				parsed.Path = ""
			} else {
				parsed.Path = nextPath
			}
			parsed.RawPath = ""
			return parsed.String(), nil
		}
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func JoinAnthropicPath(baseURL string, endpoint string) (string, error) {
	cleanedBaseURL, err := TrimPathSuffixes(
		baseURL,
		"/v1/messages",
		"/messages",
		"/v1/models",
		"/v1",
	)
	if err != nil {
		return "", err
	}
	return JoinPath(cleanedBaseURL, endpoint)
}

func JoinOpenAIPath(baseURL string, endpoint string) (string, error) {
	cleanedBaseURL, err := TrimPathSuffixes(
		baseURL,
		"/v1/chat/completions",
		"/chat/completions",
		"/v1/responses/compact",
		"/responses/compact",
		"/v1/responses",
		"/responses",
		"/v1/completions",
		"/completions",
		"/v1/edits",
		"/edits",
		"/v1/embeddings",
		"/embeddings",
		"/v1/audio/speech",
		"/audio/speech",
		"/v1/audio/transcriptions",
		"/audio/transcriptions",
		"/v1/audio/translations",
		"/audio/translations",
		"/v1/moderations",
		"/moderations",
		"/v1/rerank",
		"/rerank",
		"/v1/models",
		"/models",
		"/v1",
	)
	if err != nil {
		return "", err
	}
	return JoinPath(cleanedBaseURL, endpoint)
}

func JoinCustomOpenAIChatPath(baseURL string, endpoint string) (string, error) {
	if endpointURL, ok, err := ParseEndpointOverride(endpoint); ok || err != nil {
		return endpointURL, err
	}
	if !isChatCompletionsEndpoint(endpoint) || !isChatCompletionsBaseURL(baseURL) {
		return JoinOpenAIPath(baseURL, endpoint)
	}

	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func JoinCustomOpenAIModelsPath(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}

	path := "/" + strings.Trim(parsed.Path, "/")
	lowerPath := strings.ToLower(path)
	for _, suffix := range []string{"/v1/chat/completions", "/chat/completions"} {
		if lowerPath == suffix || strings.HasSuffix(lowerPath, suffix) {
			nextPath := strings.TrimRight(path[:len(path)-len(suffix)], "/")
			if nextPath == "" || nextPath == "/" {
				nextPath = "/v1"
			}
			parsed.Path = nextPath + "/models"
			parsed.RawPath = ""
			return parsed.String(), nil
		}
	}

	return JoinOpenAIPath(baseURL, "/v1/models")
}

func JoinCustomOpenAIModelsOverride(baseURL string, endpoint string) (string, error) {
	if endpointURL, ok, err := ParseEndpointOverride(endpoint); ok || err != nil {
		return endpointURL, err
	}
	return JoinPath(baseURL, endpoint)
}

func ParseEndpointOverride(endpoint string) (string, bool, error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return "", false, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", false, err
	}
	if parsed.IsAbs() && parsed.Host != "" {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawPath = ""
		return parsed.String(), true, nil
	}
	return "", false, nil
}

func JoinGeminiPath(baseURL string, endpoint string) (string, error) {
	cleanedBaseURL, err := trimGeminiGenerateContentSuffix(baseURL)
	if err != nil {
		return "", err
	}
	cleanedBaseURL, err = TrimPathSuffixes(
		cleanedBaseURL,
		"/v1beta/models",
		"/v1/models",
		"/models",
		"/v1beta",
		"/v1",
	)
	if err != nil {
		return "", err
	}
	return JoinPath(cleanedBaseURL, endpoint)
}

func samePathSuffix(basePath string, endpointPath string) bool {
	if basePath == "" || endpointPath == "" {
		return false
	}
	basePath = "/" + strings.Trim(basePath, "/")
	endpointPath = "/" + strings.Trim(endpointPath, "/")
	return strings.EqualFold(basePath, endpointPath) || strings.HasSuffix(strings.ToLower(basePath), strings.ToLower(endpointPath))
}

func lastPathSegment(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func firstPathSegment(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	return parts[0]
}

func isChatCompletionsEndpoint(endpoint string) bool {
	path := "/" + strings.Trim(strings.TrimSpace(endpoint), "/")
	return strings.EqualFold(path, "/v1/chat/completions") || strings.EqualFold(path, "/chat/completions")
}

func isChatCompletionsBaseURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	path := "/" + strings.Trim(parsed.Path, "/")
	lowerPath := strings.ToLower(path)
	return lowerPath == "/v1/chat/completions" ||
		strings.HasSuffix(lowerPath, "/v1/chat/completions") ||
		lowerPath == "/chat/completions" ||
		strings.HasSuffix(lowerPath, "/chat/completions")
}

func trimGeminiGenerateContentSuffix(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}

	path := "/" + strings.Trim(parsed.Path, "/")
	if path == "/" {
		parsed.Path = ""
		parsed.RawPath = ""
		return parsed.String(), nil
	}

	lowerPath := strings.ToLower(path)
	if !strings.Contains(lowerPath, ":generatecontent") && !strings.Contains(lowerPath, ":streamgeneratecontent") {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawPath = ""
		return parsed.String(), nil
	}

	for _, marker := range []string{"/v1beta/models/", "/v1/models/", "/models/"} {
		if idx := strings.LastIndex(lowerPath, marker); idx >= 0 {
			nextPath := strings.TrimRight(path[:idx], "/")
			if nextPath == "" || nextPath == "/" {
				parsed.Path = ""
			} else {
				parsed.Path = nextPath
			}
			parsed.RawPath = ""
			return parsed.String(), nil
		}
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}
