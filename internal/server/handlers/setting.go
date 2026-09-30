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

// authMethodJWT is the marker middleware.Auth writes into the gin context when the
// request was authenticated by a real admin LOGIN session (a short-lived JWT). The
// only other value it writes is "admin_token" — the long-lived automation credential.
const authMethodJWT = "jwt"

// errAdminLoginRequired is the ONE refusal wording shared by every endpoint that must
// not answer an automation credential (the admin access token) — the secret read and
// the full-site backup. Same sentence everywhere so the answer cannot be used to probe
// which endpoint is guarded.
const errAdminLoginRequired = "请先用管理员账号登录"

// requireAdminLogin reports whether the request may touch site-wide secrets, and writes
// the 403 refusal itself when it may not. Whitelist + fail-closed: ONLY an exact "jwt"
// marker passes, so a missing marker (handler mounted outside middleware.Auth, or a
// future auth kind) is denied instead of silently trusted — the earlier blacklist
// (`== "admin_token"`) let a missing/unknown marker straight through.
//
// Why it exists: the admin access token is meant for a script, and a script credential
// must never be able to read back the credential it is using, nor pull a backup that
// carries every plaintext key of the site (admin token, SMTP password, channel keys).
func requireAdminLogin(c *gin.Context) bool {
	if c.GetString("auth_method") == authMethodJWT {
		return true
	}
	resp.Error(c, http.StatusForbidden, errAdminLoginRequired)
	return false
}

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
//   - only a real logged-in admin session may read it: requireAdminLogin refuses
//     everyone else with 403 (an automation credential must never be able to read
//     back the credential it is using), whitelist + fail-closed;
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
	if !requireAdminLogin(c) {
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

// exportDB streams the whole-site backup. It dumps settings (including the plaintext
// admin access token, SMTP password and channel keys) verbatim, so it is guarded by the
// same admin-LOGIN requirement as getSettingSecret: a script holding the admin access
// token must not be able to download every secret of the site. A logged-in admin is
// unaffected — the backup stays a faithful, unredacted migration package.
func exportDB(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}

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

// importDB merges an uploaded backup, which can overwrite site settings (including the
// admin access token and SMTP password), so it is guarded by the same admin-LOGIN
// requirement as getSettingSecret. The check runs BEFORE the body is read, so a script
// credential cannot even make the server ingest its payload.
func importDB(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}

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
