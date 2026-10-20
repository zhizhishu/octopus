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

	for i, a := range it.Attempts() {
		for _, secret := range []string{jwt, hex, "sk-test1234567890abcdefgh"} {
			if strings.Contains(a.Msg, secret) {
				t.Fatalf("attempt %d msg leaked secret: %s", i, a.Msg)
			}
		}
		if !strings.Contains(a.Msg, "[redacted]") {
			t.Fatalf("attempt %d msg missing [redacted]: %s", i, a.Msg)
		}
	}
}
