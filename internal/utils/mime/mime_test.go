package mime

import "testing"

func TestFromExtension(t *testing.T) {
	cases := map[string]string{
		"png":     "image/png",
		".png":    "image/png",
		"PNG":     "image/png",
		"jpg":     "image/jpeg",
		"jpeg":    "image/jpeg",
		"webp":    "image/webp",
		"heic":    "image/heic",
		"mp3":     "audio/mpeg",
		"wav":     "audio/wav",
		"flac":    "audio/flac",
		"opus":    "audio/opus",
		"mp4":     "video/mp4",
		"mov":     "video/quicktime",
		"webm":    "video/webm",
		"pdf":     "application/pdf",
		"docx":    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"":        "",
		" ":       "",
		"unknown": "",
	}
	for in, want := range cases {
		if got := FromExtension(in); got != want {
			t.Fatalf("FromExtension(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromURL(t *testing.T) {
	cases := map[string]string{
		"https://h/x.png":            "image/png",
		"https://h/x.JPG":            "image/jpeg",
		"https://h/x.jpeg":           "image/jpeg",
		"https://h/a/b.webp?sig=abc": "image/webp",
		"https://h/x.heic":           "image/heic",
		"https://h/x.pdf":            "application/pdf",
		"https://h/x.mp4":            "video/mp4",
		"https://h/x.mp3":            "audio/mpeg",
		"https://h/x.wav#frag":       "audio/wav",
		"https://h/x.unknownext":     "",
		"https://h/noext":            "",
		"https://a.b/c":              "", // last dot precedes a slash -> no extension
		"":                           "",
		"   ":                        "",
		"data:image/png;base64,AAAA": "image/png",
		"data:application/pdf,foo":   "application/pdf",
		"data:,plain":                "", // RFC 2397 default text/plain treated as unknown
		// FromURL intentionally treats text/plain as "unknown" so a caller can fall
		// back to extension inference; FromDataURL is the API that preserves an
		// explicit text/plain data URL.
		"data:text/plain;base64,AAAA": "",
	}
	for in, want := range cases {
		if got := FromURL(in); got != want {
			t.Fatalf("FromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromDataURL(t *testing.T) {
	cases := map[string]string{
		"data:image/png;base64,AAAA":       "image/png",
		"data:image/png; charset=x,AAAA":   "image/png",
		"data:application/pdf;base64,Zm9v": "application/pdf",
		"data:text/plain;base64,AAAA":      "text/plain",
		"data:,plain":                      "", // default-filled text/plain -> unknown
		"https://h/x.png":                  "", // not a data URL
		"":                                 "",
	}
	for in, want := range cases {
		if got := FromDataURL(in); got != want {
			t.Fatalf("FromDataURL(%q) = %q, want %q", in, got, want)
		}
	}
}
