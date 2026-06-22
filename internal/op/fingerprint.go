package op

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
)

// One uniform upstream device fingerprint per octopus instance.
//
// Requirement: an upstream must NOT see "many devices" just because requests pass
// through octopus — every relayed request (and every channel/model test), no matter
// which downstream user / api-key / client sent it, must present the SAME UA and the
// SAME device identity. Session ids still vary per conversation (a single real
// install legitimately has many sessions); only the DEVICE is unified here.
//
// The seed is a per-deployment random value generated once and persisted, so it is
// stable within an instance but unique across deployments (not a global hard-coded
// constant that would itself fingerprint "this is octopus").

var (
	fingerprintInstanceOnce sync.Once
	fingerprintInstanceID   string
)

// FingerprintInstanceID returns the stable per-deployment seed. Generated and
// persisted on first use; cached for the process lifetime.
func FingerprintInstanceID() string {
	fingerprintInstanceOnce.Do(func() {
		if v, err := SettingGetString(model.SettingKeyFingerprintInstanceID); err == nil {
			if v = strings.TrimSpace(v); v != "" {
				fingerprintInstanceID = v
				return
			}
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			sum := sha256.Sum256([]byte("octopus:fingerprint:instance:fallback-seed"))
			fingerprintInstanceID = hex.EncodeToString(sum[:])
		} else {
			fingerprintInstanceID = hex.EncodeToString(buf)
		}
		_ = SettingSetString(model.SettingKeyFingerprintInstanceID, fingerprintInstanceID)
	})
	return fingerprintInstanceID
}

// ClaudeFingerprintDeviceID returns the 64-hex device id a genuine Claude Code
// install reports — stable per octopus instance, uniform for ALL claude traffic
// regardless of the downstream user/api-key. Both the relay forward path and the
// channel/model test path build it through this one helper.
func ClaudeFingerprintDeviceID() string {
	sum := sha256.Sum256([]byte("octopus:claude:device:" + FingerprintInstanceID()))
	return hex.EncodeToString(sum[:])
}

// CodexInstallationID returns the codex installation UUID — stable per octopus
// instance, uniform for ALL codex traffic. Both relay and the channel/model test
// path build it through this one helper so they are identical.
func CodexInstallationID() string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("octopus:codex:installation:"+FingerprintInstanceID())).String()
}
