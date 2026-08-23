package relay

import (
	"github.com/bestruirui/octopus/internal/fingerprint"
	dbmodel "github.com/bestruirui/octopus/internal/model"
)

type resolvedFingerprint = fingerprint.Resolved

func (ra *relayAttempt) fingerprint() resolvedFingerprint {
	if ra == nil {
		return resolvedFingerprint{}
	}
	return resolveFingerprintForChannel(ra.channel)
}

// resolveFingerprintForChannel resolves the channel's selected profile. It is the
// single source used by both the relayAttempt method and the free raw-protocol
// path so they agree on one device. A nil channel, ProfileID 0, or a dangling id
// all yield the global-default fingerprint.
func resolveFingerprintForChannel(channel *dbmodel.Channel) resolvedFingerprint {
	return fingerprint.Resolve(channel)
}
