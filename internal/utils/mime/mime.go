// Package mime provides best-effort, network-free MIME type inference for
// multimodal content (images, audio, video, documents). It is the shared helper
// behind the transformer outbound paths (Gemini fileData, Anthropic
// document/image) so a remote URL whose extension is recognized never reaches an
// upstream with an empty mimeType — several providers reject fileData/document
// blocks that carry a bare URI with no media type.
//
// Design: inference is deliberately layered cheapest-first and stays off the
// network. The hot request path must not block on HEAD/GET downloads, so we
// rely on (in order) explicit data: URL media types and file-extension lookup.
// Unknown inputs yield "" so callers can decide whether to leave the field empty
// rather than guessing a wrong type.
package mime

import (
	"strings"

	"github.com/bestruirui/octopus/internal/utils/xurl"
)

// extToMime maps a lowercase file extension (without the dot) to a MIME type.
// The table is intentionally broad — covering the image/audio/video/document
// formats accepted by Gemini and Anthropic — but only contains extensions we are
// confident about. An unknown extension is not in the table and yields "".
var extToMime = map[string]string{
	// Images
	"png":   "image/png",
	"jpg":   "image/jpeg",
	"jpeg":  "image/jpeg",
	"jfif":  "image/jpeg",
	"pjpeg": "image/jpeg",
	"webp":  "image/webp",
	"gif":   "image/gif",
	"heic":  "image/heic",
	"heif":  "image/heif",
	"bmp":   "image/bmp",
	"tif":   "image/tiff",
	"tiff":  "image/tiff",
	"svg":   "image/svg+xml",
	"ico":   "image/x-icon",

	// Audio
	"mp3":  "audio/mpeg",
	"mpga": "audio/mpeg",
	"m4a":  "audio/mp4",
	"aac":  "audio/aac",
	"wav":  "audio/wav",
	"flac": "audio/flac",
	"ogg":  "audio/ogg",
	"oga":  "audio/ogg",
	"opus": "audio/opus",
	"aiff": "audio/aiff",
	"aif":  "audio/aiff",
	"weba": "audio/webm",

	// Video
	"mp4":  "video/mp4",
	"m4v":  "video/mp4",
	"mov":  "video/quicktime",
	"webm": "video/webm",
	"mpeg": "video/mpeg",
	"mpg":  "video/mpeg",
	"avi":  "video/x-msvideo",
	"wmv":  "video/x-ms-wmv",
	"flv":  "video/x-flv",
	"3gp":  "video/3gpp",
	"mkv":  "video/x-matroska",

	// Documents
	"pdf":      "application/pdf",
	"txt":      "text/plain",
	"md":       "text/markdown",
	"markdown": "text/markdown",
	"csv":      "text/csv",
	"json":     "application/json",
	"xml":      "application/xml",
	"html":     "text/html",
	"htm":      "text/html",
	"rtf":      "application/rtf",
	"doc":      "application/msword",
	"docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":      "application/vnd.ms-excel",
	"xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"ppt":      "application/vnd.ms-powerpoint",
	"pptx":     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// FromExtension returns the MIME type for a file extension (with or without a
// leading dot, case-insensitive). It returns "" for unknown extensions.
func FromExtension(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	if ext == "" {
		return ""
	}
	return extToMime[ext]
}

// FromURL infers a MIME type from a URL's (or path's) file extension. Query and
// fragment components are stripped before the extension is inspected so
// "https://host/a.png?sig=..." still resolves. A "." that belongs to a host or
// path segment rather than a filename (e.g. "https://a.b/c") yields "". Unknown
// extensions yield "" so callers can leave the field empty instead of guessing.
//
// FromURL stays off the network by design; it never downloads the resource.
func FromURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}

	// A data: URL carries its own media type; honor it directly.
	if parsed := xurl.ParseDataURL(rawURL); parsed != nil {
		mt := normalizeMediaType(parsed.MediaType)
		// ParseDataURL defaults a missing media type to "text/plain" (RFC 2397),
		// which is rarely the real type for binary payloads; treat that default as
		// "unknown" so an extension-based caller fallback can take over upstream.
		if mt == "" || mt == "text/plain" {
			return ""
		}
		return mt
	}

	// Strip query/fragment before inspecting the extension.
	path := rawURL
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}

	dot := strings.LastIndex(path, ".")
	if dot < 0 {
		return ""
	}
	// Reject a "." that precedes a slash (i.e. belongs to a host/path segment),
	// e.g. "https://a.b/c" where the last dot is before the final path segment.
	if strings.ContainsAny(path[dot:], "/") {
		return ""
	}
	return FromExtension(path[dot+1:])
}

// FromDataURL returns the media type declared by a data: URL, or "" when the
// input is not a data URL or carries no usable media type. Unlike
// xurl.ParseDataURL it does not substitute the RFC 2397 "text/plain" default for
// a missing media type, so callers can fall back to other inference.
func FromDataURL(rawURL string) string {
	parsed := xurl.ParseDataURL(rawURL)
	if parsed == nil {
		return ""
	}
	mt := normalizeMediaType(parsed.MediaType)
	if mt == "text/plain" {
		// Distinguish an explicit "text/plain" data URL (which keeps its type) from
		// ParseDataURL's default-filled one is not possible here, so be
		// conservative and treat text/plain as a real type only when the raw header
		// actually contained it.
		if strings.Contains(strings.SplitN(rawURL, ",", 2)[0], "text/plain") {
			return "text/plain"
		}
		return ""
	}
	return mt
}

// normalizeMediaType trims a media type and drops any parameters (e.g.
// "image/png; charset=binary" -> "image/png").
func normalizeMediaType(mt string) string {
	mt = strings.TrimSpace(mt)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return strings.ToLower(mt)
}
