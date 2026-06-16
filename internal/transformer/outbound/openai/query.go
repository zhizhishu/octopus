package openai

import "net/url"

func mergeInboundQuery(parsedURL *url.URL, requestQuery url.Values) {
	if parsedURL == nil || len(requestQuery) == 0 {
		return
	}
	q := parsedURL.Query()
	for key, values := range requestQuery {
		q.Del(key)
		for _, value := range values {
			q.Add(key, value)
		}
	}
	parsedURL.RawQuery = q.Encode()
}
