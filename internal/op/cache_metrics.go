package op

import "github.com/bestruirui/octopus/internal/model"

func relayLogCacheRateBase(relayLog model.RelayLog) int64 {
	reportedBase := int64(relayLog.CacheInputTokens)
	cacheTokenSum := int64(relayLog.CacheHitTokens + relayLog.CacheWriteTokens)
	inputTokens := int64(relayLog.InputTokens)

	if reportedBase > 0 {
		return maxInt64(reportedBase, cacheTokenSum)
	}
	if cacheTokenSum > 0 {
		return maxInt64(inputTokens, cacheTokenSum)
	}
	return inputTokens
}

func relayLogEffectiveInputTokens(relayLog model.RelayLog) int64 {
	return maxInt64(int64(relayLog.InputTokens), relayLogCacheRateBase(relayLog))
}
