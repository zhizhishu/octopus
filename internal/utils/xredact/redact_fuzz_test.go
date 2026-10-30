package xredact

import (
	"strings"
	"testing"
	"time"
)

// urlSecretsFuzzSeeds is the seed corpus for FuzzURLSecrets. go test runs every
// seed once; go test -fuzz mutates outward from here. The inputs cover empty
// text, every credential name, harmless lookalikes, percent-encoding, quoted
// JSON, and the "***" replacement the scrubber writes.
var urlSecretsFuzzSeeds = []string{
	"",
	"dial tcp: connection refused",
	"?",
	"&",
	"=",
	"%",
	"%3",
	"%3F",
	"%3f",
	"%26",
	"%3D",
	"%3d",
	"%ZZ",
	"%25",
	"%%",
	"?apikey=",
	"?apikey=SECRET",
	"?foo=1&apiKey=SECRET",
	"?foo=1&apiKey=SECRET&bar=2",
	"?keyword=keep&monkey=see&tokens=12",
	"?keyword=1&key=SECRET&keywords=2&tokens=4&token=SECRET&token_type=bearer&author=ada&auth=SECRET&authority=x",
	"?apikey=abc%26def",
	"?apikey=abc%26safe%3Dkeep",
	"?apikey=SECRET%23frag",
	"?apikey%253DSECRET",
	"?key=***",
	"?apikey=***&token=***",
	"?key=AIza+Sy%2Fabc==&safe=1",
	"https://user:SECRET@upstream.example/v1?safe=1",
	"https://upstream.example/v1?api-version=2024-10-01&client_id=public&expires=99",
	`Post "https://upstream.example/v1?apikey=SUPERSECRET&model=m": dial tcp: connection refused`,
	`{"url":"https://upstream.example/v1?foo=1&apiKey=SUPERSECRET"}`,
	`{"error":"Post \"https://upstream.example/v1?apikey=SUPERSECRET\": dial"}`,
	"pre %3Fapikey%3DSUPERSECRET%26foo%3D1%26apiKey%3DSUPERSECRET post",
	"?api%5Fkey=SECRET&api%2Dkey=SECRET&x%2Dgoog%2Dapi%2Dkey=SECRET&ok=1",
	"?%6B%65%79=SECRET&tail=1",
	"\x00\xff ?apikey=SECRET&\n",
	strings.Repeat("%26", 64) + "apikey%3DSECRET" + strings.Repeat("a", 128),
	strings.Repeat("?", 32) + "key=" + strings.Repeat("k", 64) + "&safe=1",
}

func init() {
	var b strings.Builder
	for name := range credentialQueryParams {
		b.WriteString("&")
		b.WriteString(name)
		b.WriteString("=SECRET")
	}
	urlSecretsFuzzSeeds = append(urlSecretsFuzzSeeds, "?"+b.String()[1:])
}

// FuzzURLSecrets checks the scrubber on arbitrary strings: it must not panic,
// it must return, and a second pass must be a no-op.
func FuzzURLSecrets(f *testing.F) {
	for _, seed := range urlSecretsFuzzSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		once, twice, panicked, ok := runURLSecretsBounded(in, 5*time.Second)
		if !ok {
			t.Fatalf("URLSecrets did not return within 5s\ninput: %q", in)
		}
		if panicked != nil {
			t.Fatalf("URLSecrets panicked: %v\ninput: %q", panicked, in)
		}
		if once != twice {
			t.Fatalf("URLSecrets is not idempotent\ninput: %q\nonce:  %q\ntwice: %q", in, once, twice)
		}
	})
}

// runURLSecretsBounded runs URLSecrets twice. ok is false when the call does
// not return before the limit; a panic is recovered and reported separately
// so the fuzz target fails instead of crashing the worker.
func runURLSecretsBounded(in string, limit time.Duration) (once, twice string, panicked any, ok bool) {
	type result struct {
		once, twice string
		panicked    any
	}
	ch := make(chan result, 1)
	go func() {
		var out result
		defer func() {
			out.panicked = recover()
			ch <- out
		}()
		out.once = URLSecrets(in)
		out.twice = URLSecrets(out.once)
	}()
	select {
	case out := <-ch:
		return out.once, out.twice, out.panicked, true
	case <-time.After(limit):
		return "", "", nil, false
	}
}
