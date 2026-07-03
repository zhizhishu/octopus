package relay

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// unwrapResponseEncoding replaces resp.Body with a decompressing reader when the
// upstream returned a Content-Encoding, so the SSE reader always sees plain text.
//
// octopus advertises the genuine claude-cli Accept-Encoding (gzip, deflate, br,
// zstd). Because that request header is set manually, Go's http.Transport does NOT
// auto-decompress the response, so the relay must. In practice Anthropic/AnyRouter
// do not compress text/event-stream bodies, so this is a safety net that no-ops on
// the common (absent / identity) path; it exists so a compression-enabling upstream
// cannot break the SSE parser.
func unwrapResponseEncoding(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "", "identity":
		return nil
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		resp.Body = &decompressBody{r: gr, underlying: resp.Body}
	case "deflate":
		resp.Body = &decompressBody{r: flate.NewReader(resp.Body), underlying: resp.Body}
	case "br":
		resp.Body = &decompressBody{r: brotli.NewReader(resp.Body), underlying: resp.Body}
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return err
		}
		resp.Body = &decompressBody{r: zr.IOReadCloser(), underlying: resp.Body}
	default:
		// Unknown/unsupported encoding: leave the body untouched so the SSE reader
		// surfaces a clear error rather than silently mangling the stream.
		return nil
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// decompressBody adapts a decompressing reader back into an io.ReadCloser, closing
// both the decompressor (when it is itself a Closer) and the underlying body.
type decompressBody struct {
	r          io.Reader
	underlying io.Closer
}

func (d *decompressBody) Read(p []byte) (int, error) { return d.r.Read(p) }

func (d *decompressBody) Close() error {
	if c, ok := d.r.(io.Closer); ok {
		_ = c.Close()
	}
	return d.underlying.Close()
}
