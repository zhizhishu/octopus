package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

const relayLogStreamHeartbeatInterval = 15 * time.Second
const relayLogStreamReplayLimit = 200

func init() {
	router.NewGroupRouter("/api/v1/log").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listLog),
		).
		AddRoute(
			router.NewRoute("/count", http.MethodGet).
				Handle(countLog),
		).
		AddRoute(
			router.NewRoute("/export", http.MethodGet).
				Handle(exportLog),
		).
		AddRoute(
			router.NewRoute("/clear", http.MethodDelete).
				Handle(clearLog),
		).
		AddRoute(
			router.NewRoute("/storage", http.MethodGet).
				Handle(getLogStorage),
		).
		AddRoute(
			router.NewRoute("/stream-token", http.MethodGet).
				Handle(getStreamToken),
		)

	router.NewGroupRouter("/api/v1/log").
		AddRoute(
			router.NewRoute("/stream", http.MethodGet).
				Handle(streamLog),
		)
}

// severityFromQuery reads the optional ?severity= filter and validates it against
// the three known buckets; anything else (incl. empty) means "all severities".
func severityFromQuery(c *gin.Context) string {
	switch s := strings.ToLower(strings.TrimSpace(c.Query("severity"))); s {
	case "success", "warn", "error":
		return s
	default:
		return ""
	}
}

// retriedFromQuery reads the optional ?retried= flag ("1"/"true") that narrows to
// requests which took more than one channel attempt (a retry / failover happened).
func retriedFromQuery(c *gin.Context) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query("retried"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// hideModelTestFromQuery reads the optional ?hide_model_test= flag ("1"/"true")
// that excludes channel-test probe rows (endpoint "model_test*") from the list,
// so repeat/capacity test bursts don't drown out real traffic.
func hideModelTestFromQuery(c *gin.Context) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query("hide_model_test"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func listLog(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	startTime, endTime, err := parseLogTimeRange(c)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	scope := logScopeFromContext(c)
	if endpoint := strings.TrimSpace(c.Query("endpoint")); endpoint != "" {
		scope.Endpoint = endpoint
	}
	if middleware.CurrentUserIsAdmin(c) {
		var err error
		scope, err = logScopeFromAdminQuery(c, scope)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
			return
		}
	} else {
		// Defense in depth: non-admins are already reduced to RelayLogUserSummary
		// below, but keep the scope redacted too so the rows loaded from the store
		// carry no IP/content/attempts even if the summary step is ever bypassed.
		scope.Redact = true
	}
	scope.Severity = severityFromQuery(c)
	scope.RetriedOnly = retriedFromQuery(c)
	scope.HideModelTest = hideModelTestFromQuery(c)

	logs, err := op.RelayLogList(c.Request.Context(), startTime, endTime, page, pageSize, &scope)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	if !middleware.CurrentUserIsAdmin(c) {
		summaries := make([]model.RelayLogUserSummary, 0, len(logs))
		for _, log := range logs {
			summaries = append(summaries, op.RelayLogUserSummary(log))
		}
		resp.Success(c, summaries)
		return
	}

	resp.Success(c, logs)
}

func countLog(c *gin.Context) {
	startTime, endTime, err := parseLogTimeRange(c)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	scope := logScopeFromContext(c)
	if endpoint := strings.TrimSpace(c.Query("endpoint")); endpoint != "" {
		scope.Endpoint = endpoint
	}
	if middleware.CurrentUserIsAdmin(c) {
		var err error
		scope, err = logScopeFromAdminQuery(c, scope)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
			return
		}
	} else {
		scope.Redact = false
	}

	scope.RetriedOnly = retriedFromQuery(c)
	scope.HideModelTest = hideModelTestFromQuery(c)

	counts, err := op.RelayLogSeverityCounts(c.Request.Context(), startTime, endTime, &scope)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	resp.Success(c, counts)
}

func exportLog(c *gin.Context) {
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "5000"))
	if pageSize < 1 || pageSize > 20000 {
		pageSize = 5000
	}

	startTime, endTime, err := parseLogTimeRange(c)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	scope := logScopeFromContext(c)
	if endpoint := strings.TrimSpace(c.Query("endpoint")); endpoint != "" {
		scope.Endpoint = endpoint
	}
	isAdmin := middleware.CurrentUserIsAdmin(c)
	if isAdmin {
		var err error
		scope, err = logScopeFromAdminQuery(c, scope)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
			return
		}
	} else {
		scope.Redact = true
	}

	// Opt-in portable NDJSON/JSONL mode. The default (json array) behavior is
	// unchanged. NDJSON is line-delimited so each record parses independently,
	// which makes exports easy to back up, append to, and move across devices /
	// redeploys. For admins it additionally carries the FULL request/response
	// content; non-admins always get the redacted summary regardless of format.
	switch strings.ToLower(strings.TrimSpace(c.Query("format"))) {
	case "ndjson", "jsonl":
		// Admins may opt into full request/response content via include_content=1
		// (default keeps the lighter summary to match the existing array export).
		userSummary := true
		if isAdmin {
			switch strings.ToLower(strings.TrimSpace(c.Query("include_content"))) {
			case "1", "true", "yes", "full":
				userSummary = false
			}
		}
		filename := fmt.Sprintf("octopus-logs-%s.ndjson", time.Now().Format("20060102-150405"))
		c.Header("Content-Type", "application/x-ndjson; charset=utf-8")
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
		if err := op.RelayLogStreamExportNDJSON(c.Request.Context(), c.Writer, startTime, endTime, pageSize, &scope, userSummary); err != nil {
			// Headers may already be sent; a partial chunked download cannot be
			// turned back into a JSON API error envelope.
			_, _ = c.Writer.Write([]byte("\n"))
			return
		}
		return
	}

	filename := fmt.Sprintf("octopus-logs-%s.json", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	if err := streamRelayLogExport(c, startTime, endTime, pageSize, &scope, true); err != nil {
		// Headers may already be sent. Log-side export errors cannot be turned into
		// a normal JSON API envelope once chunked download has started.
		_, _ = c.Writer.Write([]byte("\n"))
		return
	}
}

func streamRelayLogExport(c *gin.Context, startTime, endTime *int, pageSize int, scope *model.RelayLogScope, userSummary bool) error {
	_, _ = c.Writer.Write([]byte("["))
	first := true
	encoder := json.NewEncoder(c.Writer)
	for page := 1; ; page++ {
		logs, err := op.RelayLogList(c.Request.Context(), startTime, endTime, page, pageSize, scope)
		if err != nil {
			return err
		}
		if len(logs) == 0 {
			break
		}
		for _, relayLog := range logs {
			if !first {
				_, _ = c.Writer.Write([]byte(","))
			}
			first = false
			if userSummary {
				if err := encoder.Encode(op.RelayLogUserSummary(relayLog)); err != nil {
					return err
				}
			} else if err := encoder.Encode(relayLog); err != nil {
				return err
			}
		}
		c.Writer.Flush()
		if len(logs) < pageSize {
			break
		}
	}
	_, _ = c.Writer.Write([]byte("]"))
	c.Writer.Flush()
	return nil
}

func clearLog(c *gin.Context) {
	scope := logScopeFromContext(c)
	if middleware.CurrentUserIsAdmin(c) {
		scope = model.RelayLogScope{}
	}
	if err := op.RelayLogClear(c.Request.Context(), &scope); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func getLogStorage(c *gin.Context) {
	scope := logScopeFromContext(c)
	if middleware.CurrentUserIsAdmin(c) {
		scope = model.RelayLogScope{}
	}
	storage, err := op.RelayLogStorageInfo(c.Request.Context(), &scope)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, storage)
}

func getStreamToken(c *gin.Context) {
	startTime, endTime, err := parseLogTimeRange(c)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	scope := logScopeFromContext(c)
	if endpoint := strings.TrimSpace(c.Query("endpoint")); endpoint != "" {
		scope.Endpoint = endpoint
	}
	if middleware.CurrentUserIsAdmin(c) {
		scope, err = logScopeFromAdminQuery(c, scope)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
			return
		}
	}
	token, err := op.RelayLogStreamTokenCreateWithTimeRange(scope, middleware.CurrentUserIsAdmin(c), startTime, endTime)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"token": token})
}

func streamLog(c *gin.Context) {
	token := c.Query("token")
	tokenScope, ok := op.RelayLogStreamTokenVerify(token)
	if token == "" || !ok {
		resp.Error(c, http.StatusUnauthorized, "invalid stream token")
		return
	}

	op.RelayLogStreamTokenRevoke(token)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	logChan := op.RelayLogSubscribe()
	defer op.RelayLogUnsubscribe(logChan)

	ctx := c.Request.Context()
	heartbeat := time.NewTicker(relayLogStreamHeartbeatInterval)
	defer heartbeat.Stop()

	if !writeLogStreamComment(c, "connected") {
		return
	}

	scope := model.RelayLogScope{UserID: tokenScope.UserID, APIKeyID: tokenScope.APIKeyID, Endpoint: tokenScope.Endpoint, Redact: !tokenScope.IsAdmin}
	var startTime, endTime *int
	if tokenScope.HasTimeRange {
		startTime = &tokenScope.StartTime
		endTime = &tokenScope.EndTime
	}
	if sinceID, err := strconv.ParseInt(strings.TrimSpace(c.Query("since_id")), 10, 64); err == nil && sinceID > 0 {
		logs, listErr := op.RelayLogListSinceRange(ctx, sinceID, relayLogStreamReplayLimit, &scope, startTime, endTime)
		if listErr == nil {
			for _, log := range logs {
				if !writeScopedLogStreamData(c, log, scope.Redact) {
					return
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if !writeLogStreamComment(c, "ping") {
				return
			}
		case log, ok := <-logChan:
			if !ok {
				return
			}
			if !relayLogMatchesScope(log, &scope) || !relayLogMatchesTimeRange(log, startTime, endTime) {
				continue
			}
			if !writeScopedLogStreamData(c, log, scope.Redact) {
				return
			}
		}
	}
}

func parseLogTimeRange(c *gin.Context) (*int, *int, error) {
	startTimeStr := strings.TrimSpace(c.Query("start_time"))
	endTimeStr := strings.TrimSpace(c.Query("end_time"))
	if startTimeStr == "" && endTimeStr == "" {
		return nil, nil, nil
	}
	if startTimeStr == "" || endTimeStr == "" {
		return nil, nil, fmt.Errorf("start_time and end_time must be provided together")
	}
	startTime, err := strconv.Atoi(startTimeStr)
	if err != nil {
		return nil, nil, err
	}
	endTime, err := strconv.Atoi(endTimeStr)
	if err != nil {
		return nil, nil, err
	}
	return &startTime, &endTime, nil
}

func writeScopedLogStreamData(c *gin.Context, log model.RelayLog, redact bool) bool {
	var payload any = log
	if redact {
		payload = op.RelayLogUserSummary(log)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return true
	}
	return writeLogStreamData(c, data)
}

func writeLogStreamComment(c *gin.Context, comment string) bool {
	if _, err := c.Writer.Write([]byte(fmt.Sprintf(": %s\n\n", comment))); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

func writeLogStreamData(c *gin.Context, data []byte) bool {
	if _, err := c.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", data))); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

func logScopeFromContext(c *gin.Context) model.RelayLogScope {
	if middleware.CurrentUserIsAdmin(c) {
		return model.RelayLogScope{}
	}
	return model.RelayLogScope{UserID: middleware.CurrentUserID(c), Redact: true}
}

func logScopeFromAdminQuery(c *gin.Context, scope model.RelayLogScope) (model.RelayLogScope, error) {
	if userIDStr := c.Query("user_id"); userIDStr != "" {
		userID, err := strconv.Atoi(userIDStr)
		if err != nil {
			return scope, err
		}
		scope.UserID = userID
	}
	if apiKeyIDStr := c.Query("api_key_id"); apiKeyIDStr != "" {
		apiKeyID, err := strconv.Atoi(apiKeyIDStr)
		if err != nil {
			return scope, err
		}
		scope.APIKeyID = apiKeyID
	}
	if endpoint := strings.TrimSpace(c.Query("endpoint")); endpoint != "" {
		scope.Endpoint = endpoint
	}
	return scope, nil
}

func relayLogMatchesScope(log model.RelayLog, scope *model.RelayLogScope) bool {
	if scope == nil {
		return true
	}
	if scope.UserID > 0 && log.UserID != scope.UserID {
		return false
	}
	if scope.APIKeyID > 0 && log.APIKeyID != scope.APIKeyID {
		return false
	}
	if scope.Endpoint != "" && log.RequestEndpoint != scope.Endpoint && !strings.HasPrefix(log.RequestEndpoint, scope.Endpoint+"_") {
		// Family match, kept in sync with op.relayLogEndpointMatches / relayLogApplyScope.
		return false
	}
	return true
}

func relayLogMatchesTimeRange(log model.RelayLog, startTime, endTime *int) bool {
	if startTime == nil || endTime == nil {
		return true
	}
	return log.Time >= int64(*startTime) && log.Time <= int64(*endTime)
}
