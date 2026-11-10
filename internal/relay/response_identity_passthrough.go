package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
)

type passthroughIdentityError struct{ cause error }

func (e *passthroughIdentityError) Error() string {
	return "invalid upstream identity response: " + e.cause.Error()
}
func (e *passthroughIdentityError) Unwrap() error { return e.cause }
func isPassthroughIdentityError(err error) bool {
	var identityErr *passthroughIdentityError
	return errors.As(err, &identityErr)
}

type upstreamIdentityContextKey struct{}

// WithOriginalUpstreamIdentity is for in-process administrator probes only.
// HTTP headers never enable this mode.
func WithOriginalUpstreamIdentity(ctx context.Context) context.Context {
	return context.WithValue(ctx, upstreamIdentityContextKey{}, true)
}
func originalUpstreamIdentity(c *gin.Context) bool {
	if _, pinned := imagesChannelTestPinFor(c); pinned {
		return true
	}
	return c != nil && c.Request != nil && c.Request.Context().Value(upstreamIdentityContextKey{}) == true
}

type passthroughIdentityEvidence struct{ Model, Fingerprint string }
type passthroughIdentity struct {
	alias              string
	evidence           *passthroughIdentityEvidence
	mediaEnvelopes     bool
	failureDescription bool
}

const passthroughIdentityKey = "octopus_response_identity"

func bindPassthroughIdentity(c *gin.Context, alias string, evidence *passthroughIdentityEvidence) {
	if evidence != nil {
		*evidence = passthroughIdentityEvidence{}
	}
	path := c.Request.URL.Path
	media := strings.Contains(path, "/images") || strings.Contains(path, "/videos") || strings.Contains(path, "/tasks") || strings.HasPrefix(path, "/mj/")
	c.Set(passthroughIdentityKey, passthroughIdentity{alias: alias, evidence: evidence, mediaEnvelopes: media})
}
func passthroughIdentityFor(c *gin.Context) (passthroughIdentity, bool) {
	v, ok := c.Get(passthroughIdentityKey)
	identity, valid := v.(passthroughIdentity)
	return identity, ok && valid && !originalUpstreamIdentity(c)
}
func identityJSONContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	return ct == "" || ct == "application/json" || strings.HasSuffix(ct, "+json")
}

// The existing cache spills at 16 MiB and caps at 256 MiB by default. Validate
// before publishing bytes; media strings are copied without decoding a JSON tree.
func prepareIdentityBody(body io.ReadCloser, identity passthroughIdentity) (*bodycache.BodyCache, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer body.Close()
		err := copyIdentityJSON(writer, body, identity)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	cached, err := bodycache.New(reader)
	_ = reader.Close()
	if err != nil {
		_ = body.Close()
	}
	parseErr := <-done
	if err != nil {
		return nil, &passthroughIdentityError{cause: err}
	}
	if parseErr != nil {
		_ = cached.Close()
		return nil, &passthroughIdentityError{cause: parseErr}
	}
	return cached, nil
}
func rewriteIdentityJSON(body []byte, identity passthroughIdentity) ([]byte, error) {
	var out bytes.Buffer
	if err := copyIdentityJSON(&out, bytes.NewReader(body), identity); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// readIdentitySSEEvent joins data lines according to SSE rules before rewriting.
// Event names, IDs, retry fields and comments are retained byte for byte.
func readIdentitySSEEvent(br *bufio.Reader, identity passthroughIdentity, stopOnTerminal bool) ([][]byte, error) {
	currentEvent := ""
	var lines [][]byte
	var data [][]byte
	size := 0
	var finalErr error
	for {
		line, err := readLineLimited(br, maxSSEEventSize)
		size += len(line)
		if size > maxSSEEventSize {
			return nil, errors.New("upstream SSE event exceeds limit")
		}
		if len(line) > 0 {
			lines = append(lines, line)
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("event:")) {
				currentEvent = strings.TrimSpace(string(trimmed[6:]))
			}
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				part := trimmed[5:]
				if len(part) > 0 && part[0] == ' ' {
					part = part[1:]
				}
				data = append(data, part)
				if stopOnTerminal {
					joined := bytes.Join(data, []byte("\n"))
					if (json.Valid(joined) || bytes.Equal(bytes.TrimSpace(joined), []byte("[DONE]"))) && responsesSSETerminalPayload(currentEvent, joined) {
						lines = append(lines, []byte("\n"))
						break
					}
				}
			}
			if len(trimmed) == 0 {
				break
			}
		}
		if err != nil {
			finalErr = err
			break
		}
	}
	if len(data) == 0 {
		return lines, finalErr
	}
	payload := bytes.Join(data, []byte("\n"))
	if len(bytes.TrimSpace(payload)) == 0 || bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return lines, finalErr
	}
	rewritten, err := rewriteIdentityJSON(payload, identity)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream SSE JSON: %w", err)
	}
	var out [][]byte
	emitted := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if !emitted {
				out = append(out, append(append([]byte("data: "), rewritten...), '\n'))
				emitted = true
			}
		} else {
			out = append(out, line)
		}
	}
	return out, finalErr
}

// Visit protocol envelopes only; arbitrary messages/tools/signatures are values.
type identityJSONCopier struct {
	r          *bufio.Reader
	w          *bufio.Writer
	identity   passthroughIdentity
	errorDepth int
}

func copyIdentityJSON(dst io.Writer, src io.Reader, identity passthroughIdentity) error {
	p := identityJSONCopier{r: bufio.NewReaderSize(src, 32*1024), w: bufio.NewWriterSize(dst, 32*1024), identity: identity}
	if err := p.value(true, 0); err != nil {
		return err
	}
	if err := p.space(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if _, err := p.r.Peek(1); !errors.Is(err, io.EOF) {
		return errors.New("trailing upstream JSON")
	}
	return p.w.Flush()
}
func (p *identityJSONCopier) space() error {
	for {
		b, err := p.r.Peek(1)
		if err != nil {
			return err
		}
		if !bytes.ContainsAny(b, " \t\r\n") {
			return nil
		}
		_, _ = p.r.ReadByte()
	}
}
func (p *identityJSONCopier) stringToken(capture bool) ([]byte, error) {
	var raw []byte
	put := func(b byte) error {
		if capture {
			if len(raw) >= 64*1024 {
				return errors.New("upstream identity/key exceeds limit")
			}
			raw = append(raw, b)
			return nil
		}
		return p.w.WriteByte(b)
	}
	b, err := p.r.ReadByte()
	if err != nil || b != '"' {
		return nil, errors.New("invalid JSON string")
	}
	if err := put(b); err != nil {
		return nil, err
	}
	for {
		b, err = p.r.ReadByte()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		if b < 0x20 {
			return nil, errors.New("invalid JSON string control")
		}
		if err := put(b); err != nil {
			return nil, err
		}
		if b == '"' {
			return raw, nil
		}
		if b != '\\' {
			continue
		}
		b, err = p.r.ReadByte()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		if !bytes.ContainsRune([]byte(`"\/bfnrtu`), rune(b)) {
			return nil, errors.New("invalid JSON escape")
		}
		if err := put(b); err != nil {
			return nil, err
		}
		if b == 'u' {
			for i := 0; i < 4; i++ {
				b, err = p.r.ReadByte()
				if err != nil || !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')) {
					return nil, errors.New("invalid JSON unicode escape")
				}
				if err := put(b); err != nil {
					return nil, err
				}
			}
		}
	}
}

// Match encoding/json's maximum container depth, including non-identity values.
const identityJSONMaxDepth = 10000

func (p *identityJSONCopier) value(envelope bool, depth int) error {
	if err := p.space(); err != nil {
		return err
	}
	peek, err := p.r.Peek(1)
	if err != nil {
		return err
	}
	if depth >= identityJSONMaxDepth && (peek[0] == '{' || peek[0] == '[') {
		return errors.New("upstream JSON nesting exceeds limit")
	}
	switch peek[0] {
	case '{':
		_, _ = p.r.ReadByte()
		if err := p.w.WriteByte('{'); err != nil {
			return err
		}
		first, emitted := true, false
		for {
			if err := p.space(); err != nil {
				return err
			}
			b, err := p.r.Peek(1)
			if err != nil {
				return err
			}
			if b[0] == '}' {
				_, _ = p.r.ReadByte()
				return p.w.WriteByte('}')
			}
			if !first {
				if b[0] != ',' {
					return errors.New("invalid JSON object separator")
				}
				_, _ = p.r.ReadByte()
				if err := p.space(); err != nil {
					return err
				}
			}
			first = false
			key, err := p.stringToken(true)
			if err != nil {
				return err
			}
			var name string
			if err := json.Unmarshal(key, &name); err != nil {
				return err
			}
			if err := p.space(); err != nil {
				return err
			}
			b0, err := p.r.ReadByte()
			if err != nil || b0 != ':' {
				return errors.New("invalid JSON object colon")
			}
			publicError := p.identity.mediaEnvelopes && ((envelope && (name == "fail_reason" || name == "failReason")) || (p.errorDepth > 0 && depth == p.errorDepth && name == "message") || (depth == 0 && name == "description" && p.identity.failureDescription))
			if publicError {
				if err := p.space(); err != nil {
					return err
				}
				var raw []byte
				peek, err := p.r.Peek(1)
				if err != nil {
					return err
				}
				if peek[0] == 'n' {
					raw = make([]byte, 4)
					if _, err := io.ReadFull(p.r, raw); err != nil || string(raw) != "null" {
						return errors.New("invalid nullable task error")
					}
				} else {
					raw, err = p.stringToken(true)
					if err != nil {
						return err
					}
				}
				var message string
				if err := json.Unmarshal(raw, &message); err != nil {
					return err
				}
				if message != "" {
					raw = []byte(`"task failed"`)
				}
				if emitted {
					if err := p.w.WriteByte(','); err != nil {
						return err
					}
				}
				emitted = true
				if _, err := p.w.Write(key); err != nil {
					return err
				}
				if err := p.w.WriteByte(':'); err != nil {
					return err
				}
				if _, err := p.w.Write(raw); err != nil {
					return err
				}
				continue
			}
			identityField := envelope && (name == "model" || name == "modelVersion" || name == "system_fingerprint")
			if identityField {
				if err := p.space(); err != nil {
					return err
				}
				var raw []byte
				peek, err := p.r.Peek(1)
				if err != nil {
					return err
				}
				if peek[0] == 'n' {
					raw = make([]byte, 4)
					if _, err := io.ReadFull(p.r, raw); err != nil || string(raw) != "null" {
						return errors.New("invalid nullable identity")
					}
				} else {
					raw, err = p.stringToken(true)
					if err != nil {
						return err
					}
				}
				var observed string
				if err := json.Unmarshal(raw, &observed); err != nil {
					return err
				}
				if e := p.identity.evidence; e != nil {
					if name == "system_fingerprint" {
						if e.Fingerprint == "" {
							e.Fingerprint = observed
						}
					} else if e.Model == "" {
						e.Model = observed
					}
				}
				if name == "system_fingerprint" || p.identity.alias == "" {
					continue
				}
				if emitted {
					if err := p.w.WriteByte(','); err != nil {
						return err
					}
				}
				emitted = true
				if _, err := p.w.Write(key); err != nil {
					return err
				}
				if err := p.w.WriteByte(':'); err != nil {
					return err
				}
				alias, _ := json.Marshal(p.identity.alias)
				if _, err := p.w.Write(alias); err != nil {
					return err
				}
				continue
			}
			if emitted {
				if err := p.w.WriteByte(','); err != nil {
					return err
				}
			}
			emitted = true
			if _, err := p.w.Write(key); err != nil {
				return err
			}
			if err := p.w.WriteByte(':'); err != nil {
				return err
			}
			child := depth == 0 && (name == "response" || name == "message" || name == "session" || (p.identity.mediaEnvelopes && (name == "result" || name == "data")))
			previousErrorDepth := p.errorDepth
			if depth == 0 && name == "error" && p.identity.mediaEnvelopes {
				p.errorDepth = depth + 1
			}
			if err := p.value(child, depth+1); err != nil {
				return err
			}
			p.errorDepth = previousErrorDepth
		}
	case '[':
		_, _ = p.r.ReadByte()
		if err := p.w.WriteByte('['); err != nil {
			return err
		}
		first := true
		for {
			if err := p.space(); err != nil {
				return err
			}
			b, err := p.r.Peek(1)
			if err != nil {
				return err
			}
			if b[0] == ']' {
				_, _ = p.r.ReadByte()
				return p.w.WriteByte(']')
			}
			if !first {
				if b[0] != ',' {
					return errors.New("invalid JSON array separator")
				}
				_, _ = p.r.ReadByte()
				if err := p.w.WriteByte(','); err != nil {
					return err
				}
			}
			first = false
			if err := p.value(envelope, depth+1); err != nil {
				return err
			}
		}
	case '"':
		_, err := p.stringToken(false)
		return err
	default:
		var raw []byte
		for {
			b, err := p.r.Peek(1)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if bytes.ContainsAny(b, ",]} \r\n\t") {
				break
			}
			if len(raw) >= 64*1024 {
				return errors.New("upstream JSON literal exceeds limit")
			}
			x, _ := p.r.ReadByte()
			raw = append(raw, x)
		}
		if !json.Valid(raw) {
			return errors.New("invalid upstream JSON literal")
		}
		_, err := p.w.Write(raw)
		return err
	}
}
