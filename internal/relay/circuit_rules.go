package relay

import (
	"errors"
	"strings"

	"github.com/zhizhishu/octopus/internal/relay/balancer"
)

const circuitRuleEvidenceLimit = 8 << 10

// circuitRuleEvidence is the text operator error rules match keywords against: the error
// message plus, for an upstream error, its error code and (bounded) response body, since
// the telling phrase ("insufficient_quota", "model not found") usually lives in the body.
func circuitRuleEvidence(err error) string {
	if err == nil {
		return ""
	}
	parts := []string{err.Error()}
	var upErr *upstreamError
	if errors.As(err, &upErr) && upErr != nil {
		if upErr.code != "" {
			parts = append(parts, upErr.code)
		}
		body := upErr.body
		if len(body) > circuitRuleEvidenceLimit {
			body = body[:circuitRuleEvidenceLimit]
		}
		if body != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n")
}

// applyCircuitErrorRules runs a counted failure through the operator error rules. Call it
// only where the failure is already charged to channel health, so caller cancels and
// request-shape rejections never trigger a hold.
func applyCircuitErrorRules(channelID, keyID int, modelName string, statusCode int, err error) {
	balancer.ApplyErrorRules(channelID, keyID, modelName, statusCode, circuitRuleEvidence(err))
}
