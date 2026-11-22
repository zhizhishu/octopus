package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

func init() {
	router.NewGroupRouter("/api/v1/stats").Use(middleware.Auth()).AddRoute(router.NewRoute("/usage-report", http.MethodGet).Handle(getUsageReport)).AddRoute(router.NewRoute("/usage-report.csv", http.MethodGet).Handle(getUsageReport))
}

func getUsageReport(c *gin.Context) {
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, "authentication required")
		return
	}
	rng, err := op.ParseUsageReportRange(c.Query("range"), c.Query("start"), c.Query("end"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	f := op.UsageReportFilter{Group: c.DefaultQuery("group", "user"), Model: c.Query("model")}
	for key, dst := range map[string]*int{"user_id": &f.UserID, "api_key_id": &f.APIKeyID} {
		if key == "user_id" && !actor.IsAdmin() {
			continue
		}
		if raw := c.Query(key); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n <= 0 {
				resp.Error(c, http.StatusBadRequest, key+" must be a positive integer")
				return
			}
			*dst = n
		}
	}
	report, err := op.UsageReportGet(c.Request.Context(), actor, rng, f)
	if err != nil {
		var invalid *op.StatsRangeError
		switch {
		case errors.Is(err, op.ErrUsageReportForbidden):
			resp.Error(c, http.StatusForbidden, err.Error())
		case errors.As(err, &invalid):
			resp.Error(c, http.StatusBadRequest, err.Error())
		default:
			resp.Error(c, http.StatusInternalServerError, "usage report unavailable")
		}
		return
	}
	if c.Request.URL.Path == "/api/v1/stats/usage-report.csv" {
		var buf bytes.Buffer
		if err := op.WriteUsageReportCSV(&buf, report); err != nil {
			resp.Error(c, http.StatusInternalServerError, "CSV export failed")
			return
		}
		c.Header("Content-Disposition", `attachment; filename="usage-report.csv"`)
		c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
		return
	}
	resp.Success(c, report)
}
