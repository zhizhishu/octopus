package xurl

import (
	"strings"
	"testing"
)

// Neutral hosts only (upstream.example): these tests must not encode any real
// provider's hostname or path beyond the generic version-prefix shapes.
func TestCustomChannelModelsURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"version prefix kept", "https://upstream.example/api/paas/v4/chat/completions", "https://upstream.example/api/paas/v4/models"},
		{"standard v1", "https://upstream.example/v1/chat/completions", "https://upstream.example/v1/models"},
		{"query preserved", "https://upstream.example/api/paas/v4/chat/completions?api-version=2024-10-21", "https://upstream.example/api/paas/v4/models?api-version=2024-10-21"},
		{"trailing slash on tail", "https://upstream.example/v1/chat/completions/", "https://upstream.example/v1/models"},
		{"uppercase tail", "https://upstream.example/v1/Chat/Completions", "https://upstream.example/v1/models"},
		{"bare tail falls back to origin", "https://upstream.example/chat/completions", "https://upstream.example/v1/models"},
		{"unrecognised tail falls back to origin", "https://upstream.example/fixed/chat", "https://upstream.example/v1/models"},
		{"unrecognised tail keeps query", "https://upstream.example/fixed/chat?api-version=1", "https://upstream.example/v1/models?api-version=1"},
		{"already a models url", "https://upstream.example/api/paas/v4/models", "https://upstream.example/api/paas/v4/models"},
		{"already a models url with query", "https://upstream.example/v1/models?version=2", "https://upstream.example/v1/models?version=2"},
		{"root url", "https://upstream.example", "https://upstream.example/v1/models"},
		{"root url with query", "https://upstream.example/?x=1", "https://upstream.example/v1/models?x=1"},
		{"userinfo dropped", "https://user:secret@upstream.example/v1/chat/completions", "https://upstream.example/v1/models"},
		{"userinfo and query together", "https://user:secret@upstream.example/api/paas/v4/chat/completions?api-version=1&x=2", "https://upstream.example/api/paas/v4/models?api-version=1&x=2"},
		{"escaped path segment is re-encoded, not sliced", "https://upstream.example/team%20a/v1/chat/completions", "https://upstream.example/team%20a/v1/models"},
		{"escaped query value is kept verbatim", "https://upstream.example/v1/chat/completions?api-version=1%2F2", "https://upstream.example/v1/models?api-version=1%2F2"},
		{"port kept", "http://upstream.example:8080/v1/chat/completions", "http://upstream.example:8080/v1/models"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CustomChannelModelsURL(tt.in)
			if err != nil {
				t.Fatalf("CustomChannelModelsURL(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("CustomChannelModelsURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCustomChannelModelsFallbackURL(t *testing.T) {
	got, err := CustomChannelModelsFallbackURL("https://upstream.example/api/paas/v4/chat/completions?api-version=1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://upstream.example/v1/models?api-version=1"; got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
}

func TestCustomChannelBaseURLPreservesQueryButNotUserinfoOrPath(t *testing.T) {
	const in = "https://user:secret@upstream.example/api/paas/v4/chat/completions?api-version=1"

	base, err := CustomChannelBaseURL(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://upstream.example?api-version=1"; base != want {
		t.Fatalf("base = %q, want %q", base, want)
	}
	if strings.Contains(base, "secret") || strings.Contains(base, "user") {
		t.Fatalf("base kept userinfo: %q", base)
	}
}

func TestCustomChannelURLErrorsNeverEchoTheInput(t *testing.T) {
	for _, in := range []string{
		"",
		"  ",
		"/v1/chat/completions",
		"upstream.example/v1/chat/completions",
		"ftp://upstream.example/v1/chat/completions",
		"https:///v1/chat/completions",
		"https://user:secret@upstream.example:99999999/v1/chat/completions",
	} {
		for name, call := range map[string]func(string) (string, error){
			"models":   CustomChannelModelsURL,
			"fallback": CustomChannelModelsFallbackURL,
			"base":     CustomChannelBaseURL,
		} {
			got, err := call(in)
			if err == nil {
				t.Fatalf("%s(%q) = %q, want an error", name, in, got)
			}
			if got != "" {
				t.Fatalf("%s(%q) returned %q alongside an error", name, in, got)
			}
			if msg := err.Error(); strings.Contains(msg, "secret") || strings.Contains(msg, "upstream.example") {
				t.Fatalf("%s(%q) error echoes the input: %q", name, in, msg)
			}
		}
	}
}
