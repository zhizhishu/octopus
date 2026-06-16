package op

import (
	"strings"

	"github.com/bestruirui/octopus/internal/model"
)

func relayLogClientEmptyRequest(relayLog model.RelayLog) bool {
	return strings.EqualFold(strings.TrimSpace(relayLog.ErrorCode), model.RelayLogErrorCodeClientEmptyRequest) ||
		strings.Contains(strings.ToLower(relayLog.ErrorStrategy), model.RelayLogErrorStrategyLocalValidationPart)
}

func relayLogExcludedFromModelTelemetry(relayLog model.RelayLog) bool {
	if relayLogClientEmptyRequest(relayLog) {
		return true
	}
	strategy := strings.ToLower(relayLog.ErrorStrategy)
	code := strings.ToLower(strings.TrimSpace(relayLog.ErrorCode))
	if strings.Contains(strategy, "breaker_counted=false") {
		return true
	}
	if strings.Contains(strategy, "local_route_selection") {
		return true
	}
	return code == "octopus_client_canceled" || code == "octopus_client_timeout" || code == "octopus_channel_circuit_open"
}
