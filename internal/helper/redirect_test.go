package helper

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDoPreserveMethodRedirectFollowsHTTPSToSameHost(t *testing.T) {
	calls := 0
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				if req.Method != http.MethodPost {
					t.Fatalf("first request method = %s, want POST", req.Method)
				}
				return &http.Response{
					StatusCode: http.StatusMovedPermanently,
					Header: http.Header{
						"Location": []string{"https://cpa.example/v1/messages?beta=true"},
					},
					Body:    io.NopCloser(strings.NewReader("redirect")),
					Request: req,
				}, nil
			}

			if req.Method != http.MethodPost {
				t.Fatalf("redirect request method = %s, want POST", req.Method)
			}
			if req.URL.String() != "https://cpa.example/v1/messages?beta=true" {
				t.Fatalf("redirect URL = %q", req.URL.String())
			}
			body, _ := io.ReadAll(req.Body)
			if string(body) != `{"ping":true}` {
				t.Fatalf("redirect body = %q", string(body))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
	}

	req, err := http.NewRequest(http.MethodPost, "http://cpa.example/v1/messages?beta=true", bytes.NewReader([]byte(`{"ping":true}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := DoPreserveMethodRedirect(client, req)
	if err != nil {
		t.Fatalf("DoPreserveMethodRedirect returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}
