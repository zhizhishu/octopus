package modeltest

import (
	"github.com/zhizhishu/octopus/internal/fingerprint"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
)

type resolvedFingerprint = fingerprint.Resolved

func resolveFingerprint(channel *dbmodel.Channel) resolvedFingerprint {
	return fingerprint.Resolve(channel)
}
