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

// net/url 只校验端口是“数字”（validOptionalPort），不校验范围，所以
// http://host:99999999/… 会被 url.Parse 接受。这里钉死：显式端口必须落在
// 1..65535（strconv.Atoi，顺带搞定位数过长的串），主机名必须非空；默认端口与
// 合法显式端口照常工作，且派生结果保留 authority。
func TestCustomChannelURLPortAndHostnameValidation(t *testing.T) {
	valid := []struct {
		name string
		in   string
		want string
	}{
		{"default port", "http://upstream.example/v1/chat/completions", "http://upstream.example/v1/models"},
		{"explicit 80", "http://upstream.example:80/v1/chat/completions", "http://upstream.example:80/v1/models"},
		{"explicit 443", "https://upstream.example:443/v1/chat/completions", "https://upstream.example:443/v1/models"},
		{"explicit 8080", "https://upstream.example:8080/v1/chat/completions", "https://upstream.example:8080/v1/models"},
		{"upper bound 65535", "http://upstream.example:65535/v1/chat/completions", "http://upstream.example:65535/v1/models"},
		{"ipv6 with port", "http://[::1]:8080/v1/chat/completions", "http://[::1]:8080/v1/models"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CustomChannelModelsURL(tc.in)
			if err != nil {
				t.Fatalf("CustomChannelModelsURL(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("CustomChannelModelsURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	invalid := []struct {
		name string
		in   string
	}{
		{"zero port", "http://upstream.example:0/v1/chat/completions"},
		{"out of range port", "http://upstream.example:65536/v1/chat/completions"},
		{"numeric but absurd port", "https://user:secret@upstream.example:99999999/v1/chat/completions"},
		{"port too long for an int", "http://upstream.example:123456789012345678901234567890/v1/chat/completions"},
		{"empty hostname with port", "http://:80/v1/chat/completions"},
		{"empty hostname with userinfo", "http://user:secret@:8080/v1/chat/completions"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CustomChannelModelsURL(tc.in)
			if err == nil {
				t.Fatalf("CustomChannelModelsURL(%q) = %q, want an error", tc.in, got)
			}
			if got != "" {
				t.Fatalf("returned %q alongside an error", got)
			}
			msg := err.Error()
			for _, forbidden := range []string{"secret", "upstream.example", "99999999", "65536", "8080", "1234567890"} {
				if strings.Contains(msg, forbidden) {
					t.Fatalf("error %q echoes the input (%q)", msg, forbidden)
				}
			}
		})
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
		"https://user:secret@upstream.example:0/v1/chat/completions",
		"https://user:secret@upstream.example:65536/v1/chat/completions",
		"https://user:secret@upstream.example:123456789012345678901234567890/v1/chat/completions",
		"https://upstream.example:70000/v1/chat/completions",
		"http://:80/v1/chat/completions",
		"http://user:secret@:8080/v1/chat/completions",
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
