package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// errJSONSyntax means the log body is not a JSON value we can walk. Callers
// keep the original text so a non-JSON snapshot is never dropped or rewritten.
var errJSONSyntax = errors.New("json syntax")

// maxJSONWalkDepth caps recursive descent. A deeper document keeps that
// subtree raw so a hostile nest cannot overflow the stack. 256 is far past
// any real request shape.
const maxJSONWalkDepth = 256

// maxLogMIMELen is the longest media type copied into a placeholder.
// The data-URL header up to the first comma is client-controlled.
const maxLogMIMELen = 64

// payloadMarkers are substrings that appear in JSON carrying inline bytes.
// A body without any of them is returned unchanged (no copy, no parse).
//
// "data" matches a JSON string token, not the letters inside "database" or an
// escaped quote inside a text value (those are \").
var payloadMarkers = []string{
	"data:",
	`"inlineData"`,
	`"inline_data"`,
	`"input_audio"`,
	`"file_data"`,
	`"image_url"`,
	`"b64_json"`,
	`"bytesBase64Encoded"`,
	`"base64"`,
	`"data"`,
	// Responses image_generation items carry raw base64 in `result` with no
	// data: prefix and no "data" key, so the key itself has to be a marker.
	`"image_generation"`,
}

// stripInlineBinaryForLog replaces inline binary payloads in a JSON log body
// with a short placeholder. It returns s itself when there is nothing to
// strip or the text is not JSON.
//
// The walk never mutates s. Callers that also hold the outbound []byte must
// pass a copy (string(body) already is one) and must not write the result
// back into that buffer.
//
// Handled shapes (anywhere they are nested, including extra_body and a
// ParamOverride merge):
//   - OpenAI chat image_url object {url, detail} and the string form used by
//     Responses input_image
//   - data: URLs in url / file_url / fileUri / file_uri / image_uri / imageUri
//   - raw base64 in an image_url string when it is at least 64 characters and
//     not an http(s), gs://, or file:// reference
//   - input_audio.data and message-level audio.data (format/voice-only audio
//     config is left untouched)
//   - file.file_data and Responses input_file file_data (data URL or raw base64)
//   - Anthropic image/document source type=base64 (type=url and type=file stay)
//   - Gemini inlineData / inline_data
//   - b64_json and bytesBase64Encoded
//   - Responses image_generation item result
//
// A placeholder is used only when it is not longer than the JSON value it
// replaces, so the stored log never grows. Media types copied into a
// placeholder are capped at 64 characters of [A-Za-z0-9.+/-]; anything else
// is recorded as "unknown".
//
// http(s), gs:// and file:// references are kept. file_id is kept.
//
// Deliberately left alone:
//   - text, tool-argument strings, and any other string that merely mentions a
//     data URL (we do not decode JSON stored inside a string)
//   - encrypted_content, reasoning signatures, thoughtSignature
//   - embedding vectors and the embedding "base64" encoding flag
//   - the response_format value "b64_json" (that is an enum, not a payload)
//   - fields tagged json:"-" on the internal request (ResponsesInputRaw,
//     RawRequest, AnthropicExtraTopLevel, …). They are not part of
//     request_content. When an outbound body actually serializes those bytes,
//     this walk strips the shapes above inside that body.
//   - non-JSON snapshots (multipart is already summarized elsewhere)
func stripInlineBinaryForLog(s string) string {
	return stripInlineBinaryWith(s, nil)
}

// stripInlineBinaryWith 与 stripInlineBinaryForLog 相同；c 非空时可保存的图片
// 会登记进 c，占位符带上内容哈希。c 收到的只是解码后的副本，s 本身不变。
func stripInlineBinaryWith(s string, c *imageCapture) string {
	if s == "" || !looksLikeJSONValue(s) || !mayContainInlinePayload(s) {
		return s
	}
	trimmed := strings.TrimSpace(s)
	repl, changed, err := rewriteJSONValue("", trimmed, mediaHint{capture: c}, 0)
	// Compare against the trimmed text that was actually rewritten. A longer
	// result is discarded so strip(s) is never longer than s, and a second
	// pass cannot depend on whitespace that the first pass already removed.
	if err != nil || !changed || len(repl) > len(trimmed) {
		return s
	}
	return repl
}

// requestContentForLog is the request_content stored on a relay log.
//
// ParamOverride keeps the historical maps.Copy semantics: a top-level key in
// the override replaces that key, a null value is stored as JSON null (it does
// not delete the key — deletion is only the outbound ApplyParamOverride
// contract). Marshal / merge failures fall back to the previous successful
// JSON, then inline bytes are stripped and redactLogBody still runs.
func requestContentForLog(req *transformerModel.InternalLLMRequest, paramOverride string) string {
	if req == nil {
		return ""
	}
	reqJSON, jsonErr := json.Marshal(req)
	content := string(reqJSON)
	if jsonErr == nil && paramOverride != "" {
		var reqMap map[string]any
		if err := json.Unmarshal(reqJSON, &reqMap); err == nil {
			var override map[string]any
			if err := json.Unmarshal([]byte(paramOverride), &override); err == nil {
				maps.Copy(reqMap, override)
				if finalJSON, err := json.Marshal(reqMap); err == nil {
					content = string(finalJSON)
				}
			}
		}
	}
	return redactLogBody(stripInlineBinaryForLog(content))
}

type mediaHint struct {
	mime      string
	blockType string
	// capture 非空时，可保存的内联图片换成带 id 的占位符并暂存字节
	// （只有真实出站请求体这一路会传；request_content 永远是 nil）。
	capture *imageCapture
}

type fieldSpan struct {
	key    string
	keyRaw string
	valRaw string
}

type js struct {
	s string
	i int
	n int
}

func looksLikeJSONValue(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			return s[i] == '{' || s[i] == '['
		}
	}
	return false
}

func mayContainInlinePayload(s string) bool {
	for _, marker := range payloadMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func rewriteJSONValue(key, raw string, hint mediaHint, depth int) (string, bool, error) {
	if depth > maxJSONWalkDepth {
		return "", false, nil
	}
	p := js{s: raw, n: len(raw)}
	p.skipSpace()
	if p.i >= p.n {
		return "", false, errJSONSyntax
	}
	switch p.s[p.i] {
	case '{':
		switch key {
		case "audio", "input_audio":
			return p.rewriteAudioObject(hint.capture)
		case "inlineData", "inline_data":
			return p.rewriteInlineObject(hint.capture)
		case "source":
			return p.rewriteSourceObject(hint)
		default:
			return p.rewriteObject(hint, depth)
		}
	case '[':
		return p.rewriteArray(hint, depth)
	case '"':
		switch key {
		case "image_url":
			if repl, ok := replaceImageURLString(raw, hint.capture); ok {
				return repl, true, nil
			}
		case "url", "file_url", "fileUri", "file_uri", "image_uri", "imageUri":
			if repl, ok := replaceReferenceOrDataURL(raw, hint.capture); ok {
				return repl, true, nil
			}
		case "file_data":
			if repl, ok := replaceFileDataString(raw, hint.capture); ok {
				return repl, true, nil
			}
		case "b64_json", "bytesBase64Encoded":
			if repl, ok := replaceDeclaredBlob(raw, "image", hint.mime, hint.capture); ok {
				return repl, true, nil
			}
		case "result":
			if hint.blockType == "image_generation" {
				if repl, ok := replaceDeclaredBlob(raw, "image", hint.mime, hint.capture); ok {
					return repl, true, nil
				}
			}
		}
		return "", false, nil
	default:
		return "", false, nil
	}
}

func (p *js) rewriteObject(hint mediaHint, depth int) (string, bool, error) {
	fields, err := p.splitObject()
	if err != nil {
		return "", false, err
	}
	child := hint
	if t, ok := unquoteField(fields, "type"); ok && t != "" {
		child.blockType = t
	}
	if m := mimeFromFields(fields); m != "" {
		child.mime = m
	}
	repl := make(map[int]string, 1)
	for i, f := range fields {
		next, changed, err := rewriteJSONValue(f.key, f.valRaw, child, depth+1)
		if err != nil {
			return "", false, err
		}
		if acceptReplacement(f.valRaw, next, changed) {
			repl[i] = next
		}
	}
	if len(repl) == 0 {
		return "", false, nil
	}
	return rebuildObject(fields, repl), true, nil
}

func (p *js) rewriteArray(hint mediaHint, depth int) (string, bool, error) {
	if p.i >= p.n || p.s[p.i] != '[' {
		return "", false, errJSONSyntax
	}
	p.i++
	var raws []string
	repl := make(map[int]string)
	for {
		p.skipSpace()
		if p.i >= p.n {
			return "", false, errJSONSyntax
		}
		if p.s[p.i] == ']' {
			p.i++
			break
		}
		if len(raws) > 0 {
			if p.s[p.i] != ',' {
				return "", false, errJSONSyntax
			}
			p.i++
			p.skipSpace()
			if p.i < p.n && p.s[p.i] == ']' {
				return "", false, errJSONSyntax
			}
		}
		valStart := p.i
		if err := p.skipValue(); err != nil {
			return "", false, err
		}
		rawVal := p.s[valStart:p.i]
		next, changed, err := rewriteJSONValue("", rawVal, hint, depth+1)
		if err != nil {
			return "", false, err
		}
		if acceptReplacement(rawVal, next, changed) {
			repl[len(raws)] = next
		}
		raws = append(raws, rawVal)
	}
	if len(repl) == 0 {
		return "", false, nil
	}
	var b strings.Builder
	n := 2
	for i, raw := range raws {
		if r, ok := repl[i]; ok {
			n += len(r)
		} else {
			n += len(raw)
		}
		n++
	}
	b.Grow(n)
	b.WriteByte('[')
	for i, raw := range raws {
		if i > 0 {
			b.WriteByte(',')
		}
		if r, ok := repl[i]; ok {
			b.WriteString(r)
		} else {
			b.WriteString(raw)
		}
	}
	b.WriteByte(']')
	return b.String(), true, nil
}

func (p *js) rewriteAudioObject(c *imageCapture) (string, bool, error) {
	fields, err := p.splitObject()
	if err != nil {
		return "", false, err
	}
	format, _ := unquoteField(fields, "format")
	return replaceDataFields(fields, "audio", audioMime(format), c)
}

func (p *js) rewriteInlineObject(c *imageCapture) (string, bool, error) {
	fields, err := p.splitObject()
	if err != nil {
		return "", false, err
	}
	mime := mimeFromFields(fields)
	kind := kindFromMime(mime)
	if strings.TrimSpace(mime) == "" {
		kind = "image"
	}
	return replaceDataFields(fields, kind, mimeOrUnknown(mime), c)
}

func (p *js) rewriteSourceObject(hint mediaHint) (string, bool, error) {
	fields, err := p.splitObject()
	if err != nil {
		return "", false, err
	}
	typ, _ := unquoteField(fields, "type")
	mime := mimeFromFields(fields)
	repl := make(map[int]string)
	if typ != "base64" {
		for i, f := range fields {
			if f.key != "url" && f.key != "file_url" {
				continue
			}
			if next, ok := replaceReferenceOrDataURL(f.valRaw, hint.capture); ok {
				repl[i] = next
			}
		}
		if len(repl) == 0 {
			return "", false, nil
		}
		return rebuildObject(fields, repl), true, nil
	}
	kind := kindFromBlock(hint.blockType, mime)
	mimeLabel := mimeOrUnknown(mime)
	for i, f := range fields {
		if f.key != "data" || !jsonStringCarriesPayload(f.valRaw) {
			continue
		}
		repl[i] = hint.capture.placeholder(kind, mimeLabel, payloadSizeStr(f.valRaw), f.valRaw)
	}
	if len(repl) == 0 {
		return "", false, nil
	}
	return rebuildObject(fields, repl), true, nil
}

func replaceDataFields(fields []fieldSpan, kind, mime string, c *imageCapture) (string, bool, error) {
	repl := make(map[int]string)
	for i, f := range fields {
		if f.key != "data" || !jsonStringCarriesPayload(f.valRaw) {
			continue
		}
		repl[i] = c.placeholder(kind, mimeOrUnknown(mime), payloadSizeStr(f.valRaw), f.valRaw)
	}
	if len(repl) == 0 {
		return "", false, nil
	}
	return rebuildObject(fields, repl), true, nil
}

func jsonStringCarriesPayload(raw string) bool {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw == `""` {
		return false
	}
	// A placeholder is not a payload. Replacing it again would change the
	// recorded size and strip(strip(x)) would not equal strip(x).
	return !isOmissionPlaceholder(raw)
}

func isOmissionPlaceholder(raw string) bool {
	inner, ok := jsonStringBodyStr(raw)
	if !ok {
		s, decoded := unquoteSimple(raw)
		if !decoded {
			return false
		}
		inner = s
	}
	if len(inner) < 12 || inner[0] != '[' || inner[len(inner)-1] != ']' {
		return false
	}
	return strings.Contains(inner, " omitted: ") || isSavedImagePlaceholder(inner)
}

// acceptReplacement reports whether next can stand in for raw without making
// the log larger than the value it replaces.
func acceptReplacement(raw, next string, changed bool) bool {
	if !changed {
		return false
	}
	return len(next) <= len(strings.TrimSpace(raw))
}

func (p *js) splitObject() ([]fieldSpan, error) {
	if p.i >= p.n || p.s[p.i] != '{' {
		return nil, errJSONSyntax
	}
	p.i++
	var fields []fieldSpan
	for {
		p.skipSpace()
		if p.i >= p.n {
			return nil, errJSONSyntax
		}
		if p.s[p.i] == '}' {
			p.i++
			return fields, nil
		}
		if len(fields) > 0 {
			if p.s[p.i] != ',' {
				return nil, errJSONSyntax
			}
			p.i++
			p.skipSpace()
			if p.i < p.n && p.s[p.i] == '}' {
				return nil, errJSONSyntax
			}
		}
		keyStart := p.i
		key, err := p.readString()
		if err != nil {
			return nil, err
		}
		keyEnd := p.i
		p.skipSpace()
		if p.i >= p.n || p.s[p.i] != ':' {
			return nil, errJSONSyntax
		}
		p.i++
		p.skipSpace()
		valStart := p.i
		if err := p.skipValue(); err != nil {
			return nil, err
		}
		fields = append(fields, fieldSpan{
			key:    key,
			keyRaw: p.s[keyStart:keyEnd],
			valRaw: p.s[valStart:p.i],
		})
	}
}

func rebuildObject(fields []fieldSpan, repl map[int]string) string {
	n := 2
	for i, f := range fields {
		n += len(f.keyRaw) + 1
		if r, ok := repl[i]; ok {
			n += len(r)
		} else {
			n += len(f.valRaw)
		}
		if i > 0 {
			n++
		}
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(f.keyRaw)
		b.WriteByte(':')
		if r, ok := repl[i]; ok {
			b.WriteString(r)
		} else {
			b.WriteString(f.valRaw)
		}
	}
	b.WriteByte('}')
	return b.String()
}

func (p *js) skipSpace() {
	for p.i < p.n {
		switch p.s[p.i] {
		case ' ', '\n', '\r', '\t':
			p.i++
		default:
			return
		}
	}
}

func (p *js) skipValue() error {
	p.skipSpace()
	if p.i >= p.n {
		return errJSONSyntax
	}
	switch p.s[p.i] {
	case '{':
		return p.skipContainer('{', '}')
	case '[':
		return p.skipContainer('[', ']')
	case '"':
		return p.skipString()
	default:
		return p.skipLiteral()
	}
}

func (p *js) skipContainer(open, close byte) error {
	if p.i >= p.n || p.s[p.i] != open {
		return errJSONSyntax
	}
	p.i++
	depth := 1
	inStr := false
	esc := false
	for p.i < p.n && depth > 0 {
		c := p.s[p.i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			p.i++
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
		}
		p.i++
	}
	if depth != 0 {
		return errJSONSyntax
	}
	return nil
}

func (p *js) skipString() error {
	if p.i >= p.n || p.s[p.i] != '"' {
		return errJSONSyntax
	}
	p.i++
	for p.i < p.n {
		c := p.s[p.i]
		switch c {
		case '\\':
			p.i++
			if p.i >= p.n {
				return errJSONSyntax
			}
			if p.s[p.i] == 'u' {
				if p.i+4 >= p.n {
					return errJSONSyntax
				}
				p.i += 5
				continue
			}
			p.i++
		case '"':
			p.i++
			return nil
		default:
			p.i++
		}
	}
	return errJSONSyntax
}

func (p *js) skipLiteral() error {
	start := p.i
	for p.i < p.n {
		switch p.s[p.i] {
		case ',', '}', ']', ' ', '\n', '\r', '\t':
			if p.i == start {
				return errJSONSyntax
			}
			return nil
		default:
			p.i++
		}
	}
	if p.i == start {
		return errJSONSyntax
	}
	return nil
}

func (p *js) readString() (string, error) {
	start := p.i
	if err := p.skipString(); err != nil {
		return "", err
	}
	raw := p.s[start:p.i]
	if strings.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1], nil
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return "", err
	}
	return s, nil
}

func unquoteField(fields []fieldSpan, key string) (string, bool) {
	for _, f := range fields {
		if f.key == key {
			return unquoteSimple(f.valRaw)
		}
	}
	return "", false
}

func unquoteSimple(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	if strings.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1], true
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return "", false
	}
	return s, true
}

func mimeFromFields(fields []fieldSpan) string {
	for _, key := range []string{"mimeType", "mime_type", "media_type", "mediaType"} {
		if s, ok := unquoteField(fields, key); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func jsonStringBodyStr(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	inner := raw[1 : len(raw)-1]
	if strings.IndexByte(inner, '\\') >= 0 {
		return "", false
	}
	return inner, true
}

func isKeepReference(raw string) bool {
	inner, ok := jsonStringBodyStr(raw)
	if !ok {
		s, ok := unquoteSimple(raw)
		if !ok {
			return false
		}
		inner = s
	}
	return strings.HasPrefix(inner, "https://") ||
		strings.HasPrefix(inner, "http://") ||
		strings.HasPrefix(inner, "gs://") ||
		strings.HasPrefix(inner, "file://")
}

func dataURLInfoStr(raw string) (mime string, size int, ok bool) {
	inner, ok := jsonStringBodyStr(raw)
	if !ok {
		s, decoded := unquoteSimple(raw)
		if !decoded || !strings.HasPrefix(s, "data:") {
			return "", 0, false
		}
		inner = s
	}
	if !strings.HasPrefix(inner, "data:") {
		return "", 0, false
	}
	comma := strings.IndexByte(inner, ',')
	if comma < 0 {
		return "", 0, false
	}
	header := inner[len("data:"):comma]
	data := inner[comma+1:]
	mimePart := header
	params := ""
	if semi := strings.IndexByte(header, ';'); semi >= 0 {
		mimePart = header[:semi]
		params = header[semi+1:]
	}
	mimePart = strings.TrimSpace(mimePart)
	isB64 := false
	if params != "" {
		for _, part := range strings.Split(params, ";") {
			if strings.TrimSpace(part) == "base64" {
				isB64 = true
				break
			}
		}
	}
	if isB64 {
		size = decodedBase64SizeStr(data)
	} else {
		size = len(data)
	}
	return mimePart, size, true
}

func payloadSizeStr(raw string) int {
	if _, size, ok := dataURLInfoStr(raw); ok {
		return size
	}
	inner, ok := jsonStringBodyStr(raw)
	if !ok {
		s, decoded := unquoteSimple(raw)
		if !decoded {
			return 0
		}
		if isBase64Str(s) {
			return decodedBase64SizeStr(s)
		}
		return len(s)
	}
	if isBase64Str(inner) {
		return decodedBase64SizeStr(inner)
	}
	return len(inner)
}

func decodedBase64SizeStr(s string) int {
	if s == "" {
		return 0
	}
	pad := 0
	if s[len(s)-1] == '=' {
		pad++
		if len(s) > 1 && s[len(s)-2] == '=' {
			pad++
		}
	}
	n := len(s) - pad
	if n < 0 {
		return 0
	}
	return n * 3 / 4
}

func isBase64Str(s string) bool {
	if s == "" {
		return false
	}
	end := len(s)
	if s[end-1] == '=' {
		end--
		if end > 0 && s[end-1] == '=' {
			end--
		}
		if strings.IndexByte(s[:end], '=') >= 0 {
			return false
		}
	}
	if end == 0 {
		return false
	}
	for i := 0; i < end; i++ {
		if !isB64Char(s[i]) {
			return false
		}
	}
	return true
}

func isB64Char(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/'
}

func replaceImageURLString(raw string, c *imageCapture) (string, bool) {
	raw = strings.TrimSpace(raw)
	if isKeepReference(raw) {
		return "", false
	}
	if mime, size, ok := dataURLInfoStr(raw); ok {
		return c.placeholder(kindForImageURL(mime), mimeOrUnknown(mime), size, raw), true
	}
	inner, ok := jsonStringBodyStr(raw)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(inner, "data:") {
		return placeholderJSON("image", "unknown", len(inner)), true
	}
	// Short non-URL tokens (file ids, "auto") are not payloads. Real inline
	// base64 without a data: prefix is much longer than 64 characters.
	if len(inner) >= 64 && isBase64Str(inner) {
		return c.placeholder("image", "unknown", decodedBase64SizeStr(inner), raw), true
	}
	return "", false
}

func replaceReferenceOrDataURL(raw string, c *imageCapture) (string, bool) {
	raw = strings.TrimSpace(raw)
	if isKeepReference(raw) {
		return "", false
	}
	if mime, size, ok := dataURLInfoStr(raw); ok {
		return c.placeholder(kindForImageURL(mime), mimeOrUnknown(mime), size, raw), true
	}
	inner, ok := jsonStringBodyStr(raw)
	if ok && strings.HasPrefix(inner, "data:") {
		return placeholderJSON("file", "unknown", len(inner)), true
	}
	return "", false
}

func replaceFileDataString(raw string, c *imageCapture) (string, bool) {
	raw = strings.TrimSpace(raw)
	if isKeepReference(raw) {
		return "", false
	}
	if mime, size, ok := dataURLInfoStr(raw); ok {
		kind := "file"
		if mime != "" {
			kind = kindFromMime(mime)
		}
		return c.placeholder(kind, mimeOrUnknown(mime), size, raw), true
	}
	inner, ok := jsonStringBodyStr(raw)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(inner, "data:") {
		return placeholderJSON("file", "unknown", len(inner)), true
	}
	if len(inner) >= 4 && isBase64Str(inner) {
		return placeholderJSON("file", "unknown", decodedBase64SizeStr(inner)), true
	}
	return "", false
}

func replaceDeclaredBlob(raw, kind, mime string, c *imageCapture) (string, bool) {
	raw = strings.TrimSpace(raw)
	if isKeepReference(raw) || !jsonStringCarriesPayload(raw) {
		return "", false
	}
	if m, size, ok := dataURLInfoStr(raw); ok {
		if m != "" {
			mime = m
			kind = kindForImageURL(m)
		}
		return c.placeholder(kind, mimeOrUnknown(mime), size, raw), true
	}
	if kind == "" {
		kind = "image"
	}
	return c.placeholder(kind, mimeOrUnknown(mime), payloadSizeStr(raw), raw), true
}

func kindFromMime(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	case strings.HasPrefix(m, "video/"):
		return "video"
	default:
		return "file"
	}
}

func kindForImageURL(mime string) string {
	k := kindFromMime(mime)
	if k == "audio" || k == "video" {
		return k
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "application/") {
		return "file"
	}
	return "image"
}

func kindFromBlock(blockType, mime string) string {
	if strings.TrimSpace(mime) != "" {
		return kindFromMime(mime)
	}
	switch blockType {
	case "image", "image_url", "input_image":
		return "image"
	case "audio", "input_audio":
		return "audio"
	default:
		return "file"
	}
}

func audioMime(format string) string {
	format = strings.TrimSpace(format)
	if format == "" {
		return "unknown"
	}
	if strings.Contains(format, "/") {
		return format
	}
	return "audio/" + strings.ToLower(format)
}

func mimeOrUnknown(mime string) string {
	return sanitizeLogMIME(mime)
}

// sanitizeLogMIME keeps a media type that is safe to store. The data-URL
// type is whatever the client put before the first comma, and mimeType /
// media_type / audio format are client-controlled too. Only a short token
// is copied into the placeholder; everything else becomes "unknown".
func sanitizeLogMIME(mime string) string {
	mime = strings.TrimSpace(mime)
	if mime == "" || len(mime) > maxLogMIMELen {
		return "unknown"
	}
	for i := 0; i < len(mime); i++ {
		c := mime[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '+' || c == '/' || c == '-':
		default:
			return "unknown"
		}
	}
	return mime
}

func formatByteSize(n int) string {
	if n < 0 {
		n = 0
	}
	const (
		kb = 1024
		mb = 1024 * 1024
		gb = 1024 * 1024 * 1024
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%dB", n)
	case n < mb:
		return fmt.Sprintf("%dKB", n/kb)
	case n < gb:
		return fmt.Sprintf("%dMB", n/mb)
	default:
		return fmt.Sprintf("%dGB", n/gb)
	}
}

func placeholderJSON(kind, mime string, size int) string {
	if kind == "" {
		kind = "file"
	}
	mime = sanitizeLogMIME(mime)
	s := fmt.Sprintf("[%s omitted: %s, %s]", kind, mime, formatByteSize(size))
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
