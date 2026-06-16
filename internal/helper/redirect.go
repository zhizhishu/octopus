package helper

import (
	"fmt"
	"net/http"
)

// DoPreserveMethodRedirect follows common same-host http -> https redirects
// without changing POST into GET. This is intentionally narrow so local HTTP
// proxies and provider-owned cross-host redirects keep their original behavior.
func DoPreserveMethodRedirect(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		return nil, fmt.Errorf("http client is nil")
	}
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}

	redirectClient := *client
	redirectClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := redirectClient.Do(req)
	if err != nil || resp == nil || !shouldFollowHTTPSUpgrade(resp.StatusCode) {
		return resp, err
	}
	location := resp.Header.Get("Location")
	if location == "" || req.URL == nil {
		return resp, err
	}
	nextURL, parseErr := req.URL.Parse(location)
	if parseErr != nil || nextURL == nil {
		return resp, err
	}
	if req.URL.Scheme != "http" || nextURL.Scheme != "https" || nextURL.Host != req.URL.Host {
		return resp, err
	}
	if req.GetBody == nil {
		return resp, err
	}

	body, bodyErr := req.GetBody()
	if bodyErr != nil {
		return resp, err
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	nextReq := req.Clone(req.Context())
	nextReq.URL = nextURL
	nextReq.Body = body
	nextReq.GetBody = req.GetBody
	nextReq.ContentLength = req.ContentLength
	return redirectClient.Do(nextReq)
}

func shouldFollowHTTPSUpgrade(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}
