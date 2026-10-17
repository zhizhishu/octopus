package op

import (
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// TestBuiltinPresetsAreThreeUncorrelatedDevices pins the shape of the three built-in
// identities: same CLI product families, three DIFFERENT machines. Every field that
// makes a device distinguishable must differ; every field that identifies the product
// must stay in lockstep (an upstream pairs the codex UA with its Originator).
func TestBuiltinPresetsAreThreeUncorrelatedDevices(t *testing.T) {
	presets := builtinPresets()
	if len(presets) != 3 {
		t.Fatalf("expected three built-in presets, got %d", len(presets))
	}
	seen := map[string]string{}
	for _, p := range presets {
		// Seeds must be distinct so device_id / installation id never collide.
		if prev, dup := seen[p.Seed]; true {
			if dup {
				t.Fatalf("preset %q reuses seed of %q", p.Name, prev)
			}
			seen[p.Seed] = p.Name
		}
		// Every preset must pin all three identity surfaces.
		if p.ClaudeUserAgent == "" || p.CodexUserAgent == "" || p.GenericUA == "" {
			t.Fatalf("preset %q leaves an identity surface empty: %+v", p.Name, p)
		}
		// The codex Originator must be the UA's leading product token — an upstream
		// pairs the two and a mismatch is itself the tell (sub2api #3901).
		uaProduct := p.CodexUserAgent[:strings.Index(p.CodexUserAgent, "/")]
		if p.CodexOriginator != uaProduct {
			t.Fatalf("preset %q: originator %q does not match UA product %q",
				p.Name, p.CodexOriginator, uaProduct)
		}
		// Non-CLI channels must present a real desktop chat client, never a browser:
		// a browser does not call a chat API endpoint directly.
		if !strings.Contains(p.GenericUA, "CherryStudio/") {
			t.Fatalf("preset %q generic UA is not a CherryStudio client: %q", p.Name, p.GenericUA)
		}
	}

	// The macOS preset must be the Mac one, and it must not be the old bare Chrome.
	mac := presets[2]
	if mac.Name != builtinMacOSPresetName {
		t.Fatalf("third preset must be the macOS one, got %q", mac.Name)
	}
	if !strings.Contains(mac.GenericUA, "Macintosh") || !strings.Contains(mac.GenericUA, "CherryStudio/") {
		t.Fatalf("macOS generic UA must be CherryStudio on macOS, got %q", mac.GenericUA)
	}
	if strings.Contains(mac.GenericUA, "Chrome/131") {
		t.Fatalf("macOS generic UA is still the retired bare-Chrome string: %q", mac.GenericUA)
	}
	if !strings.Contains(mac.CodexUserAgent, "Mac OS X") {
		t.Fatalf("macOS codex UA must carry a Mac platform segment, got %q", mac.CodexUserAgent)
	}
	if mac.ClaudeOS != "MacOS" || mac.ClaudeArch != "arm64" {
		t.Fatalf("macOS claude must present MacOS/arm64, got %s/%s", mac.ClaudeOS, mac.ClaudeArch)
	}

	// All three must be distinguishable on every device-ish axis.
	if presets[0].GenericUA == presets[2].GenericUA || presets[1].GenericUA == presets[2].GenericUA {
		t.Fatalf("macOS generic UA must differ from both Linux presets")
	}
	if presets[0].CodexUserAgent == presets[2].CodexUserAgent {
		t.Fatalf("macOS codex UA must differ from the Debian one")
	}
	if presets[1].CodexUserAgent == presets[2].CodexUserAgent {
		t.Fatalf("macOS codex UA must differ from the Ubuntu one")
	}

	// The claude UA carries NO platform token (claude-cli reports platform through
	// X-Stainless-OS/Arch), so all three legitimately share the same claude UA string.
	if presets[0].ClaudeUserAgent != presets[2].ClaudeUserAgent {
		t.Fatalf("claude UA must stay the platform-free captured string across presets")
	}
	_ = model.GenericUAMacOS
}
