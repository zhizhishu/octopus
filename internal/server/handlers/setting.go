package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/task"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/setting").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(getSettingList),
		).
		AddRoute(
			router.NewRoute("/set", http.MethodPost).
				Use(middleware.RequireJSON()).
				Handle(setSetting),
		).
		AddRoute(
			router.NewRoute("/secret", http.MethodGet).
				Handle(getSettingSecret),
		).
		AddRoute(
			router.NewRoute("/export", http.MethodGet).
				Handle(exportDB),
		).
		AddRoute(
			router.NewRoute("/import", http.MethodPost).
				Handle(importDB),
		)
}

func getSettingList(c *gin.Context) {
	settings, err := op.SettingList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range settings {
		if model.IsSecretSettingKey(settings[i].Key) && settings[i].Value != "" {
			settings[i].Value = model.SettingSecretMaskValue
		}
		if settings[i].Key == model.SettingKeyProxyURL {
			settings[i].Value = model.RedactProxyURLPassword(settings[i].Value)
		}
	}
	resp.Success(c, settings)
}

// getSettingSecret returns the PLAINTEXT of the ONE secret the Settings page must be
// able to show and copy: the admin access token. Guard rails that keep this from
// becoming a generic "read any secret" endpoint:
//   - `key` must be SettingKeyAdminToken (anything else is 400);
//   - a request authenticated BY the admin token itself gets 403 — an automation
//     credential must never be able to export the credential it is using, only a
//     real logged-in admin session may read it;
//   - a token supplied by the <APP>_ADMIN_TOKEN env var is NOT exported (source
//     "env" with an empty value); the page only tells the operator to manage it in
//     the deployment config;
//   - Cache-Control: no-store so the plaintext never lands in a browser/proxy cache;
//   - the value is never logged, never put in an error string, never fmt-printed.
func getSettingSecret(c *gin.Context) {
	key := c.Query("key")
	if key != string(model.SettingKeyAdminToken) {
		resp.Error(c, http.StatusBadRequest, "unsupported setting key")
		return
	}
	if c.GetString("auth_method") == "admin_token" {
		resp.Error(c, http.StatusForbidden, "admin access token cannot read secrets; sign in as an admin")
		return
	}

	c.Header("Cache-Control", "no-store")

	value, err := op.SettingGetString(model.SettingKeyAdminToken)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to read setting")
		return
	}
	value = strings.TrimSpace(value)

	source := "none"
	switch {
	case value != "":
		source = "setting"
	case strings.TrimSpace(os.Getenv(strings.ToUpper(conf.APP_NAME)+"_ADMIN_TOKEN")) != "":
		// Same env fallback VerifyAdminAccessToken uses. The value stays unexported.
		source = "env"
		value = ""
	}

	resp.Success(c, gin.H{"key": key, "value": value, "source": source})
}

func setSetting(c *gin.Context) {
	var setting model.Setting
	if err := c.ShouldBindJSON(&setting); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if model.IsSecretSettingKey(setting.Key) && setting.Value == model.SettingSecretMaskValue {
		resp.Success(c, setting)
		return
	}
	if setting.Key == model.SettingKeyProxyURL {
		// Restore the real password when the admin saves a round-tripped
		// (password-redacted) proxy URL, so editing host/user does not wipe it.
		stored, _ := op.SettingGetString(model.SettingKeyProxyURL)
		setting.Value = model.MergeProxyURLPassword(setting.Value, stored)
	}
	if err := setting.Validate(); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.SettingSetString(setting.Key, setting.Value); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	switch setting.Key {
	case model.SettingKeyModelInfoUpdateInterval:
		hours, err := strconv.Atoi(setting.Value)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		task.Update(string(setting.Key), time.Duration(hours)*time.Hour)
	case model.SettingKeySyncLLMInterval:
		hours, err := strconv.Atoi(setting.Value)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		task.Update(string(setting.Key), time.Duration(hours)*time.Hour)
	case model.SettingKeyRelayLogKeepPeriod, model.SettingKeyRelayLogMaxStorageGB:
		if err := op.RelayLogSaveDBTask(c.Request.Context()); err != nil {
			resp.Error(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	resp.Success(c, setting)
}

func exportDB(c *gin.Context) {
	includeLogs, _ := strconv.ParseBool(c.DefaultQuery("include_logs", "false"))
	includeStats, _ := strconv.ParseBool(c.DefaultQuery("include_stats", "false"))

	dump, err := op.DBExportAll(c.Request.Context(), includeLogs, includeStats)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.Header("Content-Type", "application/json")
	c.Header("Content-Disposition", "attachment; filename=\"octopus-export-"+time.Now().Format("20060102150405")+".json\"")
	c.JSON(http.StatusOK, dump)
}

func importDB(c *gin.Context) {
	var dump model.DBDump

	contentType := c.GetHeader("Content-Type")
	if strings.Contains(contentType, "multipart/form-data") {
		fh, err := c.FormFile("file")
		if err != nil {
			resp.Error(c, http.StatusBadRequest, "missing upload file field 'file'")
			return
		}
		f, err := fh.Open()
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		defer f.Close()
		body, err := io.ReadAll(f)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := decodeDBDump(body, &dump); err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := decodeDBDump(body, &dump); err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
	}

	result, err := op.DBImportIncremental(c.Request.Context(), &dump)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	_ = op.InitCache()

	resp.Success(c, result)
}

func decodeDBDump(body []byte, dump *model.DBDump) error {
	if dump == nil {
		return json.Unmarshal(body, &struct{}{})
	}

	if err := json.Unmarshal(body, dump); err != nil {
		return err
	}

	if dump.Version == 0 &&
		len(dump.Channels) == 0 &&
		len(dump.Settings) == 0 &&
		len(dump.APIKeys) == 0 &&
		len(dump.LLMInfos) == 0 &&
		len(dump.RelayLogs) == 0 &&
		len(dump.StatsDaily) == 0 &&
		len(dump.StatsHourly) == 0 &&
		len(dump.StatsTotal) == 0 &&
		len(dump.StatsChannel) == 0 &&
		len(dump.StatsModel) == 0 &&
		len(dump.StatsAPIKey) == 0 {
		var wrapper struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Data) > 0 {
			return json.Unmarshal(wrapper.Data, dump)
		}
	}

	return nil
}
