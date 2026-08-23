package modeltest

import (
	"github.com/bestruirui/octopus/internal/fingerprint"
	dbmodel "github.com/bestruirui/octopus/internal/model"
)

type resolvedFingerprint = fingerprint.Resolved

func resolveFingerprint(channel *dbmodel.Channel) resolvedFingerprint {
	return fingerprint.Resolve(channel)
}
