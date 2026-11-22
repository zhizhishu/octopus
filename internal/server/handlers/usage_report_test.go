package handlers

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

func TestUsageReportRoutes(t *testing.T) {
	f := setupDashFixture(t)
	base := "/api/v1/stats/usage-report?range=custom&start=" + f.day(-3) + "&end=" + f.day(0) + "&group=user"
	for _, tc := range []struct {
		token, path string
		code        int
	}{
		{"", base, http.StatusBadRequest},
		{f.adminJWT, base, http.StatusOK},
		{f.userJWT, base + fmt.Sprintf("&user_id=%d", f.bob.ID), http.StatusOK},
		{f.userJWT, base + fmt.Sprintf("&api_key_id=%d", f.keyB.ID), http.StatusForbidden},
		{f.userJWT, base + "&api_key_id=999999", http.StatusForbidden},
		{f.adminJWT, base + "&api_key_id=oops", http.StatusBadRequest},
		{f.adminJWT, base + "&group=oops", http.StatusOK},
		{f.adminJWT, "/api/v1/stats/usage-report?range=custom&start=2020-01-01&end=2026-01-01", http.StatusBadRequest},
		{f.adminJWT, "/api/v1/stats/usage-report?group=oops", http.StatusBadRequest},
	} {
		r := dashGet(f, tc.token, tc.path)
		if r.Code != tc.code {
			t.Fatalf("%s: got %d want %d: %s", tc.path, r.Code, tc.code, r.Body.String())
		}
	}
	r := dashGet(f, f.userJWT, base+fmt.Sprintf("&user_id=%d", f.bob.ID))
	var env dashEnvelope[model.UsageReport]
	if err := json.Unmarshal(r.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Rows) != 1 || env.Data.Rows[0].Name != "alice" || env.Data.Summary.TotalCost != 5.5 {
		t.Fatalf("user scope: %s", r.Body.String())
	}
	for _, row := range env.Data.Rows {
		if row.ID != strconv.Itoa(f.alice.ID) {
			t.Fatalf("foreign user: %+v", row)
		}
	}
	csvRec := dashGet(f, f.userJWT, strings.Replace(base, "usage-report?", "usage-report.csv?", 1))
	if csvRec.Code != http.StatusOK || !strings.HasPrefix(csvRec.Body.String(), "\xef\xbb\xbf") || !strings.Contains(csvRec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("CSV: %d %s", csvRec.Code, csvRec.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(csvRec.Body.String(), "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.0
	for _, row := range rows[1:] {
		if row[0] == "group" {
			v, _ := strconv.ParseFloat(row[11], 64)
			cost += v
		}
	}
	if cost != env.Data.Summary.TotalCost {
		t.Fatalf("JSON vs CSV: %v != %v", cost, env.Data.Summary.TotalCost)
	}
}
