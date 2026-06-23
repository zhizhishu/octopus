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

// ClaudeFingerprintDeviceID returns the 64-hex device id for the GLOBAL default
// fingerprint — stable per octopus instance, uniform for ALL claude traffic that
// does not select a profile. Equivalent to ClaudeFingerprintDeviceIDForSeed with
// the per-instance seed, so the no-profile (ProfileID 0) path is byte-for-byte
// unchanged from before profiles existed.
func ClaudeFingerprintDeviceID() string {
	return ClaudeFingerprintDeviceIDForSeed(FingerprintInstanceID())
}

// ClaudeFingerprintDeviceIDForSeed derives the 64-hex claude device id from an
// arbitrary seed, so each fingerprint profile gets its own unrelated device id.
// An empty seed falls back to the global per-instance seed (== global default).
func ClaudeFingerprintDeviceIDForSeed(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		seed = FingerprintInstanceID()
	}
	sum := sha256.Sum256([]byte("octopus:claude:device:" + seed))
	return hex.EncodeToString(sum[:])
}

// CodexInstallationID returns the codex installation UUID for the GLOBAL default
// fingerprint — stable per octopus instance, uniform for ALL codex traffic that
// does not select a profile. Equivalent to CodexInstallationIDForSeed with the
// per-instance seed, so the no-profile path is byte-for-byte unchanged.
func CodexInstallationID() string {
	return CodexInstallationIDForSeed(FingerprintInstanceID())
}

// CodexInstallationIDForSeed derives the codex installation UUID from an
// arbitrary seed so each profile gets its own unrelated install id. An empty
// seed falls back to the global per-instance seed (== global default).
func CodexInstallationIDForSeed(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		seed = FingerprintInstanceID()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("octopus:codex:installation:"+seed)).String()
}
