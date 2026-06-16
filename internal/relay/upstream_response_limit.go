package relay

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/conf"
)

const defaultUpstreamResponseReadMaxBytes int64 = 128 * 1024 * 1024

var errUpstreamResponseBodyTooLarge = errors.New("upstream response body too large")

type upstreamResponseLimitReadCloser struct {
	io.ReadCloser
	remaining int64
}

func limitUpstreamResponseBody(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	maxBytes := currentUpstreamResponseReadMaxBytes()
	if maxBytes <= 0 {
		return
	}
	response.Body = &upstreamResponseLimitReadCloser{
		ReadCloser: response.Body,
		remaining:  maxBytes,
	}
}

func currentUpstreamResponseReadMaxBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(strings.ToUpper(conf.APP_NAME) + "_RELAY_UPSTREAM_RESPONSE_READ_MAX_BYTES"))
	if raw == "" {
		return defaultUpstreamResponseReadMaxBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return defaultUpstreamResponseReadMaxBytes
	}
	return value
}

func (r *upstreamResponseLimitReadCloser) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		var probe [1]byte
		n, err := r.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errUpstreamResponseBodyTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	return n, err
}
