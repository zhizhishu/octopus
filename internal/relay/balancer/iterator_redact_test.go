package balancer

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// TestIteratorSkipAndEndRedactMsgPinnedMode pins that attempt messages written
// via Skip and AttemptSpan.End never retain raw credential-like text.
func TestIteratorSkipAndEndRedactMsgPinnedMode(t *testing.T) {
	group := model.Group{Items: []model.GroupItem{{ChannelID: 1, ModelName: "m", Priority: 0}}}
	it := NewIteratorWithSessionKey(group, 1, "m", "")
	if !it.Next() {
		t.Fatal("expected first candidate")
	}

	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	hex := "0123456789abcdef0123456789abcdef"
	it.Skip(1, 0, "ch1", "skip reason token "+jwt+" hex "+hex)

	span := it.StartAttempt(1, 0, "ch1")
	span.End(model.AttemptFailed, 401, "upstream said sk-test1234567890abcdefgh "+jwt)

	// pack-1128（用户 11-28 拍板）：attempts.msg 原样落库，不再打码。
	attempts := it.Attempts()
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(attempts))
	}
	if !strings.Contains(attempts[0].Msg, jwt) || !strings.Contains(attempts[0].Msg, hex) {
		t.Fatalf("pack-1128: skip msg must be verbatim: %s", attempts[0].Msg)
	}
	if !strings.Contains(attempts[1].Msg, "sk-test1234567890abcdefgh") || !strings.Contains(attempts[1].Msg, jwt) {
		t.Fatalf("pack-1128: end msg must be verbatim: %s", attempts[1].Msg)
	}
	for i, a := range attempts {
		if strings.Contains(a.Msg, "[redacted]") {
			t.Fatalf("pack-1128: attempt %d msg must not carry redaction markers: %s", i, a.Msg)
		}
	}
}
