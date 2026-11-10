package op

import (
	"strconv"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
)

func relayLogUserProjection(scope *model.RelayLogScope) bool {
	return scope != nil && scope.UserProjection
}

// RelayLogExportSummary keeps admin light exports compatible without exposing
// their routing identities in an authenticated user's summary.
func RelayLogExportSummary(row model.RelayLog, userProjection bool) model.RelayLogUserSummary {
	summary := RelayLogUserSummary(row)
	if !userProjection {
		summary.ActualModelName = row.ActualModelName
		summary.BillingModel = row.BillingModel
	}
	return summary
}

// Only the matching copy is changed; persisted rows and billing snapshots remain intact.
func relayLogUserSearchProjection(row model.RelayLog) model.RelayLog {
	row.ActualModelName = row.RequestModelName
	row.UpstreamResponseModel = ""
	row.ChannelId = 0
	row.ChannelName = ""
	row.UserName = ""
	row.Error = ""
	return row
}

func relayLogUserSearchCond(trimmed string) (string, []any) {
	columns := []string{"request_api_key_name", "request_model_name", "request_endpoint", "request_path", "session_key", "error_code", "request_user_agent"}
	conditions := make([]string, 0, len(columns)+1)
	args := make([]any, 0, len(columns)+1)
	if id, err := strconv.ParseInt(trimmed, 10, 64); err == nil && id > 0 {
		conditions = append(conditions, "id = ?")
		args = append(args, id)
	}
	pattern := "%" + escapeLogEndpointLike(strings.ToLower(trimmed)) + "%"
	for _, column := range columns {
		conditions = append(conditions, "LOWER("+column+") LIKE ? ESCAPE '\\'")
		args = append(args, pattern)
	}
	return strings.Join(conditions, " OR "), args
}

// RelayLogMatchesScope is shared with live SSE so cache and stream filters agree.
func RelayLogMatchesScope(row model.RelayLog, scope *model.RelayLogScope) bool {
	return relayLogMatchScope(row, scope)
}
