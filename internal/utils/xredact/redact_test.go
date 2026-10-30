package xredact

import (
	"strings"
	"testing"
)

const (
	fakeJWT  = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	fakeHex  = "0123456789abcdef0123456789abcdef"
	fakeSK   = "sk-test1234567890abcdefgh"
	fakeBear = "Bearer qW3ertyuiop1234567890"
)

// TestSecretsRedactsJWTPinnedMode pins that raw JWT tokens are redacted.
func TestSecretsRedactsJWTPinnedMode(t *testing.T) {
	got := Secrets("auth failed for " + fakeJWT)
	if strings.Contains(got, fakeJWT) {
		t.Fatalf("JWT leaked: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected [redacted] marker, got %s", got)
	}
}

// TestSecretsRedactsBareHex pins that bare 32+ hex strings are redacted.
func TestSecretsRedactsBareHex(t *testing.T) {
	got := Secrets("key= " + fakeHex + " rejected")
	if strings.Contains(got, fakeHex) {
		t.Fatalf("bare hex leaked: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected [redacted] marker, got %s", got)
	}
}

// TestSecretsRedactsSKAndBearer keeps the legacy forms covered.
func TestSecretsRedactsSKAndBearer(t *testing.T) {
	got := Secrets("Authorization: " + fakeBear + " key " + fakeSK)
	if strings.Contains(got, fakeSK) || strings.Contains(got, "qW3ertyuiop1234567890") {
		t.Fatalf("secret leaked: %s", got)
	}
}

// TestSecretsKeepsShortHex pins that short hex ids are not over-redacted.
func TestSecretsKeepsShortHex(t *testing.T) {
	in := "request id abcdef0123456789 done"
	if got := Secrets(in); got != in {
		t.Fatalf("over-redacted short id: %s", got)
	}
}

// TestURLSecretsRedactsQueryCredentials pins that credential-carrying URL query
// parameters (the Gemini ?key=<channel key> shape) are replaced with ***, while
// non-sensitive parameters, the path, and surrounding text stay intact.
func TestURLSecretsRedactsQueryCredentials(t *testing.T) {
	in := `Post "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=AIzaSecret123&api_key=k2&api-key=k3&token=t4&access_token=t5&sig=s6&signature=s7&safe=keep": dial tcp: connection refused`
	want := `Post "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=***&api_key=***&api-key=***&token=***&access_token=***&sig=***&signature=***&safe=keep": dial tcp: connection refused`
	if got := URLSecrets(in); got != want {
		t.Fatalf("URLSecrets mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestSecretsCoversURLQueryCredentials pins that the existing Secrets scrubber
// (used for per-attempt audit messages) inherits the URL query rule, so a Gemini
// key in an echoed request URL cannot ride through that path either.
func TestSecretsCoversURLQueryCredentials(t *testing.T) {
	got := Secrets(`Post "https://x/v1?alt=sse&key=AIzaSecret123": boom`)
	if strings.Contains(got, "AIzaSecret123") {
		t.Fatalf("Secrets leaked URL query key: %s", got)
	}
	if !strings.Contains(got, "alt=sse&key=***") {
		t.Fatalf("expected key=*** with alt=sse preserved, got: %s", got)
	}
}

// TestURLSecretsTable pins credential query redaction at the start, middle, and
// end of a query, inside quoted JSON, and in percent-encoded form. Harmless
// names that share a prefix or a word with a credential stay byte-for-byte.
func TestURLSecretsTable(t *testing.T) {
	const secret = "SUPERSECRET"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "no query",
			in:   "dial tcp: connection refused",
			want: "dial tcp: connection refused",
		},
		{
			name: "apikey at start of query",
			in:   `Post "https://upstream.example/v1?apikey=` + secret + `&model=m": dial tcp: connection refused`,
			want: `Post "https://upstream.example/v1?apikey=***&model=m": dial tcp: connection refused`,
		},
		{
			name: "apiKey in the middle",
			in:   `Post "https://upstream.example/v1?foo=1&apiKey=` + secret + `&bar=2": dial tcp: connection refused`,
			want: `Post "https://upstream.example/v1?foo=1&apiKey=***&bar=2": dial tcp: connection refused`,
		},
		{
			name: "apiKey at end of query",
			in:   `Post "https://upstream.example/v1?foo=1&apiKey=` + secret + `": dial tcp: connection refused`,
			want: `Post "https://upstream.example/v1?foo=1&apiKey=***": dial tcp: connection refused`,
		},
		{
			name: "both casings in one url",
			in:   `https://upstream.example/v1?apikey=` + secret + `&x=1&apiKey=` + secret,
			want: `https://upstream.example/v1?apikey=***&x=1&apiKey=***`,
		},
		{
			name: "quoted json string",
			in:   `{"error":"upstream https://upstream.example/v1?foo=1&apikey=` + secret + `&bar=2 failed"}`,
			want: `{"error":"upstream https://upstream.example/v1?foo=1&apikey=***&bar=2 failed"}`,
		},
		{
			name: "json string param at end",
			in:   `{"url":"https://upstream.example/v1?apiKey=` + secret + `"}`,
			want: `{"url":"https://upstream.example/v1?apiKey=***"}`,
		},
		{
			name: "escaped quotes around a go error embedded in json",
			in:   `{"error":"Post \"https://upstream.example/v1?apikey=` + secret + `\": dial"}`,
			want: `{"error":"Post \"https://upstream.example/v1?apikey=***\": dial"}`,
		},
		{
			name: "percent-encoded separators, middle and end",
			in:   `pre %3Fapikey%3D` + secret + `%26foo%3D1%26apiKey%3D` + secret + ` post`,
			want: `pre %3Fapikey%3D***%26foo%3D1%26apiKey%3D*** post`,
		},
		{
			name: "lowercase hex escapes",
			in:   `https%3a%2f%2fupstream.example%2fv1%3fapi_key%3d` + secret + `%26safe%3dkeep`,
			want: `https%3a%2f%2fupstream.example%2fv1%3fapi_key%3d***%26safe%3dkeep`,
		},
		{
			name: "encoded underscore and hyphen in the name",
			in:   `?api%5Fkey=` + secret + `&api%2Dkey=` + secret + `&x%2Dgoog%2Dapi%2Dkey=` + secret + `&client%5Fsecret=` + secret + `&ok=1`,
			want: `?api%5Fkey=***&api%2Dkey=***&x%2Dgoog%2Dapi%2Dkey=***&client%5Fsecret=***&ok=1`,
		},
		{
			name: "fully percent-encoded name",
			in:   `?%6B%65%79=` + secret + `&tail=1`,
			want: `?%6B%65%79=***&tail=1`,
		},
		{
			name: "transform request error shape",
			in:   `failed to parse gemini url: parse "https://upstream.example/v1beta/models/m:generateContent?apikey=` + secret + `": invalid URL`,
			want: `failed to parse gemini url: parse "https://upstream.example/v1beta/models/m:generateContent?apikey=***": invalid URL`,
		},
		{
			name: "prefix lookalikes stay",
			in:   `?keyword=1&key=` + secret + `&keywords=2&tokens=4&token=` + secret + `&token_type=bearer&author=ada&auth=` + secret + `&authority=x`,
			want: `?keyword=1&key=***&keywords=2&tokens=4&token=***&token_type=bearer&author=ada&auth=***&authority=x`,
		},
		{
			name: "harmless names are untouched",
			in:   "https://upstream.example/search?keyword=token&monkey=secret&tokens=12&max_tokens=100&author=ada&authority=host&authentication=none&auth_type=basic&api-version=2024-10-01&api_version=1&key_id=kid&token_type=bearer&token_count=3&client_id=public&signature_method=HMAC-SHA256&sig_alg=rs256&code=en&state=xyz&session=chat&sid=1&expires=99&user_password_hint=nope&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20231029T000000Z&X-Amz-SignedHeaders=host&X-Goog-Expires=3600&X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Date=20231029T000000Z&X-Goog-SignedHeaders=host&GoogleAccessId=id&OSSAccessKeyId=id&Key-Pair-Id=APK",
			want: "https://upstream.example/search?keyword=token&monkey=secret&tokens=12&max_tokens=100&author=ada&authority=host&authentication=none&auth_type=basic&api-version=2024-10-01&api_version=1&key_id=kid&token_type=bearer&token_count=3&client_id=public&signature_method=HMAC-SHA256&sig_alg=rs256&code=en&state=xyz&session=chat&sid=1&expires=99&user_password_hint=nope&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20231029T000000Z&X-Amz-SignedHeaders=host&X-Goog-Expires=3600&X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Date=20231029T000000Z&X-Goog-SignedHeaders=host&GoogleAccessId=id&OSSAccessKeyId=id&Key-Pair-Id=APK",
		},
		{
			name: "empty credential value stays empty",
			in:   "?apikey=&safe=1",
			want: "?apikey=&safe=1",
		},
		{
			name: "value may contain encoded equals and plus",
			in:   "?key=AIza+Sy%2Fabc==&safe=1",
			want: "?key=***&safe=1",
		},
		{
			name: "encoded amp inside the secret is still redacted when it is not a parameter",
			in:   "?apikey=abc%26def",
			want: "?apikey=***",
		},
		{
			name: "encoded amp that introduces the next parameter stops the value",
			in:   "?apikey=abc%26safe%3Dkeep",
			want: "?apikey=***%26safe%3Dkeep",
		},
		{
			name: "encoded fragment stops the value",
			in:   "?apikey=" + secret + "%23frag",
			want: "?apikey=***%23frag",
		},
		{
			name: "double-encoded equals is not a separator",
			in:   "?apikey%253D" + secret,
			want: "?apikey%253D" + secret,
		},
		{
			name: "newline ends the value",
			in:   "?apikey=" + secret + "\nnext=1",
			want: "?apikey=***\nnext=1",
		},
		{
			name: "single quotes",
			in:   `'https://upstream.example/v1?password=` + secret + `'`,
			want: `'https://upstream.example/v1?password=***'`,
		},
		{
			name: "userinfo is not a query parameter",
			in:   "https://user:" + secret + "@upstream.example/v1?safe=1",
			want: "https://user:" + secret + "@upstream.example/v1?safe=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := URLSecrets(tt.in)
			if got != tt.want {
				t.Fatalf("URLSecrets mismatch:\n got: %s\nwant: %s", got, tt.want)
			}
			if got2 := URLSecrets(got); got2 != got {
				t.Fatalf("URLSecrets not idempotent:\n once: %s\n twice: %s", got, got2)
			}
		})
	}
}

// TestURLSecretsRedactsEveryCredentialName walks the documented name list so a
// newly added parameter cannot ship without a start, middle, end, and JSON case.
func TestURLSecretsRedactsEveryCredentialName(t *testing.T) {
	for name := range credentialQueryParams {
		t.Run(name, func(t *testing.T) {
			secret := "SECRET-" + name
			cases := []string{
				"?" + name + "=" + secret,
				"?safe=1&" + name + "=" + secret + "&tail=ok",
				"?safe=1&" + name + "=" + secret,
				`{"u":"https://upstream.example/v1?` + name + "=" + secret + `"}`,
			}
			for _, in := range cases {
				got := URLSecrets(in)
				if strings.Contains(got, secret) {
					t.Fatalf("leaked %q\n in:  %s\n got: %s", secret, in, got)
				}
				if !strings.Contains(got, name+"=***") {
					t.Fatalf("missing %s=***\n in:  %s\n got: %s", name, in, got)
				}
				if strings.Contains(in, "safe=1") && !strings.Contains(got, "safe=1") {
					t.Fatalf("safe param eaten: %s", got)
				}
				if strings.Contains(in, "tail=ok") && !strings.Contains(got, "tail=ok") {
					t.Fatalf("tail param eaten: %s", got)
				}
			}
		})
	}
}

// TestSecretsKeepsHarmlessQueryCounts pins that the broader scrubber, which
// calls URLSecrets first, still does not eat tokens= counts or keyword=.
func TestSecretsKeepsHarmlessQueryCounts(t *testing.T) {
	got := Secrets(`Post "https://upstream.example/v1?keyword=hello&monkey=secret&tokens=3&apikey=SUPERSECRET": boom`)
	if strings.Contains(got, "SUPERSECRET") {
		t.Fatalf("Secrets leaked apikey: %s", got)
	}
	for _, want := range []string{"keyword=hello", "monkey=secret", "tokens=3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Secrets over-redacted %q: %s", want, got)
		}
	}
}
