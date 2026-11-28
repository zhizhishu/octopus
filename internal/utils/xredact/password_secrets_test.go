package xredact

import (
	"strings"
	"testing"
)

// pack-1128 E：测试只用假值，绝不取真实日志内容。

func TestPasswordSecretsRedactsJSONPasswordKeys(t *testing.T) {
	in := `{"prompt":"hi","password":"fakepw-111","nested":{"client_secret":"fakesec-222","access_token":"faketok-333"}}`
	got := PasswordSecrets(in)
	for _, leak := range []string{"fakepw-111", "fakesec-222", "faketok-333"} {
		if strings.Contains(got, leak) {
			t.Fatalf("json credential %s leaked: %s", leak, got)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected redacted marker, got %s", got)
	}
	if !strings.Contains(got, `"prompt":"hi"`) {
		t.Fatalf("non-credential key distorted: %s", got)
	}
}

func TestPasswordSecretsKeepsTokenCountKeys(t *testing.T) {
	in := `{"max_tokens":4096,"tokens":5,"token_type":"bearer","input_tokens":12}`
	if got := PasswordSecrets(in); got != in {
		t.Fatalf("count/type keys distorted: %s", got)
	}
}

func TestPasswordSecretsRedactsKeyValuePassword(t *testing.T) {
	got := PasswordSecrets("login failed password=fakepw-444 pwd:fakepw-555 passwd=fakepw-666")
	for _, leak := range []string{"fakepw-444", "fakepw-555", "fakepw-666"} {
		if strings.Contains(got, leak) {
			t.Fatalf("key=value credential %s leaked: %s", leak, got)
		}
	}
}

func TestPasswordSecretsRedactsAuthorizationHeader(t *testing.T) {
	got := PasswordSecrets("Authorization: Bearer fakeheadertoken123456")
	if strings.Contains(got, "fakeheadertoken123456") {
		t.Fatalf("authorization header leaked: %s", got)
	}
}

func TestPasswordSecretsLeavesOrdinaryTextAlone(t *testing.T) {
	in := "user asked about password reset flow and sha256 0123456789abcdef0123456789abcdef stays"
	got := PasswordSecrets(in)
	if !strings.Contains(got, "0123456789abcdef0123456789abcdef") {
		t.Fatalf("bare hex in downstream text must NOT be redacted by PasswordSecrets: %s", got)
	}
	if !strings.Contains(got, "password reset flow") {
		t.Fatalf("plain word password distorted: %s", got)
	}
}

func TestSecretsAlsoRedactsJSONKeys(t *testing.T) {
	got := Secrets(`upstream echoed {"api_key":"fakeup-777"}`)
	if strings.Contains(got, "fakeup-777") {
		t.Fatalf("json api_key leaked in Secrets: %s", got)
	}
}
