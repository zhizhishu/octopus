package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
)

func TestLogUserProjectionHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "logs.db"), false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatal(err)
	}
	testDB := db.GetDB()
	if err := op.RelayLogClear(context.Background(), nil); err != nil {
		t.Fatalf("reset temporary test log cache: %v", err)
	}
	t.Cleanup(func() {
		if db.GetDB() != testDB {
			t.Error("handler test database was replaced before cleanup")
			return
		}
		if err := op.RelayLogClear(context.Background(), nil); err != nil {
			t.Errorf("clear temporary test logs: %v", err)
		}
	})
	yes := true
	row := model.RelayLog{ID: 9101, Time: 9101, UserID: 17, RequestModelName: "A", ActualModelName: "B", UpstreamResponseModel: "C", BillingModel: "B", ChannelName: "hidden-channel", ChannelId: 992, Cost: 0.42, UpstreamModelMismatch: &yes}
	if err := db.GetDB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	index := model.RelayLogSearchIndex{ID: row.ID, RequestModelName: row.RequestModelName, ActualModelName: row.ActualModelName, UpstreamResponseModel: row.UpstreamResponseModel, ChannelName: row.ChannelName, ChannelId: row.ChannelId}
	if err := db.GetDB().Create(&index).Error; err != nil {
		t.Fatal(err)
	}
	for _, admin := range []bool{false, true} {
		role := model.UserRoleUser
		if admin {
			role = model.UserRoleAdmin
		}
		call := func(handler gin.HandlerFunc, query string) *httptest.ResponseRecorder {
			if db.GetDB() != testDB {
				t.Fatal("handler test database was replaced")
			}
			params, err := url.ParseQuery(strings.TrimPrefix(query, "?"))
			if err != nil {
				t.Fatal(err)
			}
			params.Set("user_id", "17")
			params.Set("start_time", "9101")
			params.Set("end_time", "9101")
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/log/test?"+params.Encode(), nil)
			c.Params = gin.Params{{Key: "id", Value: "9101"}}
			callerID := 17
			if admin {
				callerID = 18
			}
			middleware.SetCurrentUser(c, model.User{ID: callerID, Role: role})
			handler(c)
			if r.Code != http.StatusOK {
				t.Fatalf("admin=%v status=%d body=%s", admin, r.Code, r.Body.String())
			}
			return r
		}
		for _, handler := range []gin.HandlerFunc{listLog, getLog, exportLog} {
			r := call(handler, "?user_projection=false&user_id=17")
			actual := "A"
			if admin {
				actual = "B"
			}
			for _, field := range []string{"actual_model_name", "billing_model"} {
				if !strings.Contains(r.Body.String(), `"`+field+`":"`+actual+`"`) {
					t.Fatalf("admin=%v field=%s body=%s", admin, field, r.Body.String())
				}
			}
		}
		for _, query := range []string{"?search=B", "?search=C", "?search=hidden-channel", "?search=992", "?model=B"} {
			r := call(listLog, query+"&user_projection=false")
			var list struct {
				Data []json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			want := 0
			if admin {
				want = 1
			}
			if len(list.Data) != want {
				t.Fatalf("admin=%v query=%s list=%s", admin, query, r.Body.String())
			}
			r = call(countLog, query)
			var count struct {
				Data model.RelayLogSeverityCounts `json:"data"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &count); err != nil {
				t.Fatal(err)
			}
			if count.Data.Total != int64(want) {
				t.Fatalf("admin=%v query=%s count=%s", admin, query, r.Body.String())
			}
		}
		r := call(getStreamToken, "?search=B&user_projection=false")
		var minted struct {
			Data struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &minted); err != nil {
			t.Fatal(err)
		}
		token, ok := op.RelayLogStreamTokenVerify(minted.Data.Token)
		if !ok || token.IsAdmin != admin {
			t.Fatal("stream token role differs from authenticated caller")
		}
		op.RelayLogStreamTokenRevoke(minted.Data.Token)
		scope := model.RelayLogScope{UserID: token.UserID, Search: token.Search, UserProjection: !token.IsAdmin}
		if relayLogMatchesScope(row, &scope) != admin {
			t.Fatalf("live filter role=%v", admin)
		}
		r = httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		if !writeScopedLogStreamData(c, row, !admin) {
			t.Fatal("stream write failed")
		}
		actual := "A"
		if admin {
			actual = "B"
		}
		if !strings.Contains(r.Body.String(), `"actual_model_name":"`+actual+`"`) {
			t.Fatalf("SSE projection: %s", r.Body.String())
		}
	}
}

func TestLogUserProjectionRequestState(t *testing.T) {
	state := &relay.RequestState{Model: "A", TargetModel: "B", TargetChannel: "hidden-channel", Error: "C", Attempts: []relay.AttemptSnapshot{{UpstreamModel: "B", ChannelName: "hidden-channel"}}}
	user := relay.RedactRequestState(state, false)
	if user.Model != "A" || user.TargetModel != "" || user.TargetChannel != "" || user.Error != "" || len(user.Attempts) != 0 {
		t.Fatalf("user state: %+v", user)
	}
	admin := relay.RedactRequestState(state, true)
	if admin.TargetModel != "B" || len(admin.Attempts) != 1 || state.TargetModel != "B" {
		t.Fatal("state source/admin identity changed")
	}
}
