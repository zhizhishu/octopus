package client

// Plaintext HTTP/1.1 wire-level request-header ordering.
//
// Go's net/http serialises an HTTP/1.1 request header block in its OWN canonical order
// (`Host`, `User-Agent`, `Content-Length` first — they are written outside the header
// map — then every remaining header in ASCII order) and never emits `Connection` at
// all, because keep-alive is implicit in HTTP/1.1. A genuine claude-cli puts a
// different block on the wire (see claudeHTTP1WireHeaderOrder in header_order.go), and
// byte-exactness includes that order.
//
// Go exposes no hook for this, so we rewrite the block at the byte layer: this is a
// net.Conn wrapper, installed on the DIAL of the standard transport, that reorders an
// already-serialised header block on its way to the socket. The transport itself —
// connection pooling, timeouts, redirects, proxy handling, response decoding, TLS —
// is untouched standard library. Request lines, header values, header names that are
// not in the order list, and every body byte pass through unchanged.
//
// Ported from CLIProxyAPI's internal/httpwire.NewOrderedRequestConn (the same
// reference project this repo already borrows CLI identity/header handling from),
// including its chunked-body tracker, so behaviour matches a version already exercised
// in production rather than a fresh invention.
//
// Scope guard — TLS and HTTP/2 must never enter this parser. Both would be corrupted
// by a rewrite, `Connection` is a forbidden header in HTTP/2, and upstream TLS/h2
// shape is decided elsewhere and must stay byte-for-byte untouched. Two mechanisms
// keep them out: (1) only a buffer that BEGINS with an HTTP method token is ever
// treated as a request header block, and (2) the first time a connection presents
// anything else (a TLS ClientHello record, ciphertext) the connection latches to a
// permanent pass-through, so no encrypted stream is ever buffered or scanned again.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

// maxBufferedHTTP1RequestHeader caps how much of a request header block the wrapper
// buffers while waiting for the blank line that terminates it, so a peer that never
// terminates one cannot make us allocate without bound. Same ceiling as the reference.
const maxBufferedHTTP1RequestHeader = 1 << 20

// requestHeaderOrderFunc returns the desired header-name order for one HTTP/1.1
// request. Names are matched case-insensitively and are written to the wire with the
// casing given here. A listed name that is absent from the block is added only when it
// has an explicit synthesis rule (see synthesizedHTTP1HeaderLine); names not listed
// keep their original relative order after the listed ones. Returning nil/empty leaves
// the block byte-for-byte as the standard library wrote it.
type requestHeaderOrderFunc func(method, requestTarget string) []string

// newOrderedRequestConn wraps conn so that request header blocks are rewritten to the
// order returned by order. A nil order or nil conn is returned unchanged.
func newOrderedRequestConn(conn net.Conn, order requestHeaderOrderFunc) net.Conn {
	if conn == nil || order == nil {
		return conn
	}
	return &orderedRequestConn{Conn: conn, order: order}
}

type orderedRequestConn struct {
	net.Conn
	order requestHeaderOrderFunc

	mu sync.Mutex
	// opaque latches once the connection has shown bytes that cannot be an HTTP/1.1
	// request (a TLS ClientHello, h2 ciphertext). From then on every Write is a plain
	// pass-through — see the scope guard in the package comment.
	opaque        bool
	header        []byte
	bodyRemaining int64
	chunked       *chunkedRequestTracker
}

func (c *orderedRequestConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	originalLength := len(p)
	consumed := 0
	remaining := p
	for len(remaining) > 0 {
		if c.opaque {
			written, errWrite := writeAll(c.Conn, remaining)
			consumed += written
			if errWrite != nil {
				return consumed, errWrite
			}
			return originalLength, nil
		}
		if c.bodyRemaining > 0 {
			bodyBytes := min(int64(len(remaining)), c.bodyRemaining)
			written, errWrite := writeAll(c.Conn, remaining[:bodyBytes])
			consumed += written
			c.bodyRemaining -= int64(written)
			if errWrite != nil {
				return consumed, errWrite
			}
			remaining = remaining[bodyBytes:]
			continue
		}
		if c.chunked != nil {
			preview := c.chunked.clone()
			chunkBytes, _, errChunk := preview.consume(remaining)
			if errChunk != nil {
				return consumed, errChunk
			}
			written, errWrite := writeAll(c.Conn, remaining[:chunkBytes])
			consumed += written
			_, completed, errConsume := c.chunked.consume(remaining[:written])
			if errConsume != nil {
				return consumed, errConsume
			}
			if completed {
				c.chunked = nil
			}
			if errWrite != nil {
				return consumed, errWrite
			}
			remaining = remaining[chunkBytes:]
			continue
		}

		if len(c.header) == 0 && c.bodyRemaining == 0 && c.chunked == nil && !isHTTPMethodPrefix(remaining) {
			// Not an HTTP/1.1 request: latch to pass-through and never look at this
			// connection's bytes again.
			c.opaque = true
			written, errWrite := writeAll(c.Conn, remaining)
			consumed += written
			if errWrite != nil {
				return consumed, errWrite
			}
			return originalLength, nil
		}

		previousHeaderLength := len(c.header)
		c.header = append(c.header, remaining...)
		headerEnd := bytes.Index(c.header, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			if len(c.header) > maxBufferedHTTP1RequestHeader {
				return consumed, fmt.Errorf("client: HTTP/1.1 request header exceeds %d bytes", maxBufferedHTTP1RequestHeader)
			}
			return originalLength, nil
		}

		headerEnd += len("\r\n\r\n")
		header := c.header[:headerEnd]
		body := c.header[headerEnd:]
		c.header = nil
		currentHeaderBytes := min(len(remaining), max(0, headerEnd-previousHeaderLength))

		ordered, contentLength, chunked := orderRequestHeader(header, c.order)
		if _, errWrite := writeAll(c.Conn, ordered); errWrite != nil {
			// Every caller byte was accepted into the wrapper before the transformed
			// header write failed. Report the full input count with the terminal
			// connection error so callers never replay an ambiguous partial header.
			return originalLength, errWrite
		}
		consumed += currentHeaderBytes
		remaining = body
		if chunked {
			c.chunked = newChunkedRequestTracker()
			continue
		}
		c.bodyRemaining = contentLength
	}
	return originalLength, nil
}

// orderRequestHeader rewrites one complete request header block (terminated by the
// blank line) into the requested field order. It also returns how many body bytes
// follow, so the wrapper can pass the body through untouched.
func orderRequestHeader(header []byte, order requestHeaderOrderFunc) ([]byte, int64, bool) {
	lines := bytes.Split(header[:len(header)-len("\r\n\r\n")], []byte("\r\n"))
	if len(lines) == 0 {
		return header, 0, false
	}
	requestParts := strings.SplitN(string(lines[0]), " ", 3)
	if len(requestParts) != 3 {
		return header, requestContentLength(lines[1:]), requestUsesChunkedEncoding(lines[1:])
	}

	desired := order(requestParts[0], requestParts[1])
	if len(desired) == 0 {
		return header, requestContentLength(lines[1:]), requestUsesChunkedEncoding(lines[1:])
	}

	headerLines := lines[1:]
	used := make([]bool, len(headerLines))
	orderedLines := make([][]byte, 0, len(lines))
	orderedLines = append(orderedLines, lines[0])
	for _, name := range desired {
		matched := false
		for index, line := range headerLines {
			if used[index] || !headerLineNamed(line, name) {
				continue
			}
			// Rewrite the name to the requested spelling: the case of a header name is
			// part of the wire bytes, and Go canonicalises what it writes.
			colon := bytes.IndexByte(line, ':')
			if colon > 0 && name != "" {
				rewritten := make([]byte, 0, len(name)+len(line)-colon)
				rewritten = append(rewritten, name...)
				rewritten = append(rewritten, line[colon:]...)
				orderedLines = append(orderedLines, rewritten)
			} else {
				orderedLines = append(orderedLines, line)
			}
			used[index] = true
			matched = true
		}
		if !matched {
			if synthesized := synthesizedHTTP1HeaderLine(name); synthesized != nil {
				orderedLines = append(orderedLines, synthesized)
			}
		}
	}
	for index, line := range headerLines {
		if !used[index] {
			orderedLines = append(orderedLines, line)
		}
	}

	var output bytes.Buffer
	for _, line := range orderedLines {
		output.Write(line)
		output.WriteString("\r\n")
	}
	output.WriteString("\r\n")
	return output.Bytes(), requestContentLength(headerLines), requestUsesChunkedEncoding(headerLines)
}

// synthesizedHTTP1HeaderLine returns the wire line to ADD when name is listed in the
// requested order but absent from the request, or nil when nothing may be synthesized.
//
// Adding a header is deliberately restricted to `Connection`. This is a NEW header, not
// a reordering: a genuine claude-cli's own HTTP stack always emits
// `Connection: keep-alive` on its plaintext leg, while Go's net/http never emits the
// header at all (keep-alive is implicit in HTTP/1.1, and DisableCompression is on so
// Go adds nothing here). Because synthesis is reachable only through an order list that
// names `Connection`, it can never fire for a leg whose table does not ask for it — and
// a connection that already carries a Connection line (Go emits `Connection: close`
// when the caller asked for it) matches that line and is reordered rather than
// overwritten, so a deliberate close is never turned into a keep-alive.
func synthesizedHTTP1HeaderLine(name string) []byte {
	if strings.EqualFold(name, "Connection") {
		return []byte("Connection: keep-alive")
	}
	return nil
}

func headerLineNamed(line []byte, name string) bool {
	colon := bytes.IndexByte(line, ':')
	return colon > 0 && strings.EqualFold(string(line[:colon]), name)
}

func isHTTPTokenChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '!' || b == '#' || b == '$' || b == '%' || b == '&' ||
		b == '\'' || b == '*' || b == '+' || b == '-' || b == '.' ||
		b == '^' || b == '_' || b == '`' || b == '|' || b == '~'
}

// isHTTPMethodPrefix reports whether the buffer starts with an HTTP method token
// followed by a space — i.e. whether it could be the start of an HTTP/1.1 request
// line. Anything else (notably a TLS record: first byte 0x16, not a token char) is
// passed through.
func isHTTPMethodPrefix(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for i, b := range p {
		if b == ' ' && i > 0 {
			return true
		}
		if !isHTTPTokenChar(b) {
			return false
		}
	}
	return true
}

func requestContentLength(lines [][]byte) int64 {
	for _, line := range lines {
		if !headerLineNamed(line, "Content-Length") {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		value := strings.TrimSpace(string(line[colon+1:]))
		length, errParse := strconv.ParseInt(value, 10, 64)
		if errParse == nil && length > 0 {
			return length
		}
		return 0
	}
	return 0
}

func requestUsesChunkedEncoding(lines [][]byte) bool {
	for _, line := range lines {
		if !headerLineNamed(line, "Transfer-Encoding") {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		for _, encoding := range strings.Split(string(line[colon+1:]), ",") {
			if strings.EqualFold(strings.TrimSpace(encoding), "chunked") {
				return true
			}
		}
	}
	return false
}

// chunkedRequestTracker follows one chunked request body so the wrapper knows exactly
// where the body ends and the next request's header block begins on a reused
// connection. Request bodies here are not rewritten — the tracker only measures.
type chunkedRequestTracker struct {
	state         uint8
	line          []byte
	dataRemaining int64
	crlfPosition  int
	trailers      []byte
}

const (
	chunkedReadingSize uint8 = iota
	chunkedReadingData
	chunkedReadingDataCRLF
	chunkedReadingTrailers
)

func newChunkedRequestTracker() *chunkedRequestTracker {
	return &chunkedRequestTracker{state: chunkedReadingSize}
}

func (tracker *chunkedRequestTracker) clone() *chunkedRequestTracker {
	cloned := *tracker
	cloned.line = append([]byte(nil), tracker.line...)
	cloned.trailers = append([]byte(nil), tracker.trailers...)
	return &cloned
}

func (tracker *chunkedRequestTracker) consume(data []byte) (consumed int, completed bool, err error) {
	for consumed < len(data) {
		switch tracker.state {
		case chunkedReadingSize:
			tracker.line = append(tracker.line, data[consumed])
			consumed++
			if len(tracker.line) > maxBufferedHTTP1RequestHeader {
				return consumed, false, fmt.Errorf("client: chunk size line exceeds %d bytes", maxBufferedHTTP1RequestHeader)
			}
			if len(tracker.line) < 2 || !bytes.Equal(tracker.line[len(tracker.line)-2:], []byte("\r\n")) {
				continue
			}
			sizeText := strings.TrimSpace(string(tracker.line[:len(tracker.line)-2]))
			if extension := strings.IndexByte(sizeText, ';'); extension >= 0 {
				sizeText = strings.TrimSpace(sizeText[:extension])
			}
			size, errParse := strconv.ParseInt(sizeText, 16, 64)
			if errParse != nil || size < 0 {
				return consumed, false, fmt.Errorf("client: invalid chunk size %q", sizeText)
			}
			tracker.line = tracker.line[:0]
			if size == 0 {
				tracker.state = chunkedReadingTrailers
				continue
			}
			tracker.dataRemaining = size
			tracker.state = chunkedReadingData
		case chunkedReadingData:
			chunkBytes := min(int64(len(data)-consumed), tracker.dataRemaining)
			consumed += int(chunkBytes)
			tracker.dataRemaining -= chunkBytes
			if tracker.dataRemaining == 0 {
				tracker.crlfPosition = 0
				tracker.state = chunkedReadingDataCRLF
			}
		case chunkedReadingDataCRLF:
			want := []byte("\r\n")
			if data[consumed] != want[tracker.crlfPosition] {
				return consumed, false, fmt.Errorf("client: chunk data is missing CRLF terminator")
			}
			consumed++
			tracker.crlfPosition++
			if tracker.crlfPosition == len(want) {
				tracker.state = chunkedReadingSize
			}
		case chunkedReadingTrailers:
			tracker.trailers = append(tracker.trailers, data[consumed])
			consumed++
			if len(tracker.trailers) > maxBufferedHTTP1RequestHeader {
				return consumed, false, fmt.Errorf("client: chunk trailers exceed %d bytes", maxBufferedHTTP1RequestHeader)
			}
			if bytes.Equal(tracker.trailers, []byte("\r\n")) ||
				(len(tracker.trailers) >= 4 && bytes.Equal(tracker.trailers[len(tracker.trailers)-4:], []byte("\r\n\r\n"))) {
				return consumed, true, nil
			}
		default:
			return consumed, false, fmt.Errorf("client: invalid chunk parser state %d", tracker.state)
		}
	}
	return consumed, false, nil
}

func writeAll(writer io.Writer, data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		written, errWrite := writer.Write(data)
		total += written
		if errWrite != nil {
			return total, errWrite
		}
		if written <= 0 {
			return total, io.ErrShortWrite
		}
		data = data[written:]
	}
	return total, nil
}
