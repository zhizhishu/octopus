package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
	"gorm.io/gorm"
)

// settingKeyCustomChatVerbatim marks the one-time rewrite of type-6 base URLs.
// The migration record (version 21) is a second guard; this row makes the
// rewrite itself idempotent inside the same transaction as the URL updates.
const settingKeyCustomChatVerbatim model.SettingKey = "migrate_custom_openai_chat_verbatim_urls_v1"

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 21,
		Up:      migrateCustomOpenAIChatVerbatimURLs,
	})
}

// migrateCustomOpenAIChatVerbatimURLs rewrites every type-6 base URL to the
// chat URL the old JoinCustomOpenAIChatPath logic would have produced.
//
// Type 6 is now verbatim: the stored URL is the request URL. Without this
// rewrite, a base of https://host/v1 would start hitting /v1 instead of
// /v1/chat/completions. A URL that already ends in /chat/completions is left
// unchanged (the old join trims one trailing slash and then stops).
//
// OpenAIChatPath is folded in with the same old two-step join the chat relay
// used to do, then cleared, so a later verbatim send does not apply it twice.
// Channel keys are never read or written. Other channel types are untouched.
//
// Non-chat raw endpoints on a type-6 channel cannot be detected from the
// stored URL alone. After the rewrite every request kind goes to the old chat
// URL. The startup warning lists the channel ids so the owner can check.
func migrateCustomOpenAIChatVerbatimURLs(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var marker model.Setting
		err := tx.Where("key = ?", settingKeyCustomChatVerbatim).Take(&marker).Error
		if err == nil && marker.Value == "done" {
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("read verbatim-url marker: %w", err)
		}

		var channels []model.Channel
		if err := tx.Where("type = ?", outbound.OutboundTypeCustomOpenAIChat).Find(&channels).Error; err != nil {
			return fmt.Errorf("list type-6 channels: %w", err)
		}

		ids := make([]int, 0, len(channels))
		for i := range channels {
			ch := &channels[i]
			ids = append(ids, ch.ID)
			changed, err := rewriteType6ChannelURLs(tx, ch)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
		}
		if len(ids) > 0 {
			log.Warnf("migrate 021: 自定义渠道（type 6）编号 %v 已改成按旧的聊天地址原样发送。这次迁移看不出它们有没有被拿去打过聊天以外的接口（图片、嵌入、systemone 等），请人工核对。", ids)
		}

		marker = model.Setting{Key: settingKeyCustomChatVerbatim, Value: "done"}
		if err := tx.Save(&marker).Error; err != nil {
			return fmt.Errorf("write verbatim-url marker: %w", err)
		}
		return nil
	})
}

func rewriteType6ChannelURLs(tx *gorm.DB, ch *model.Channel) (bool, error) {
	if ch == nil {
		return false, nil
	}
	next := make([]model.BaseUrl, len(ch.BaseUrls))
	copy(next, ch.BaseUrls)
	changed := false
	for i, bu := range next {
		raw := strings.TrimSpace(bu.URL)
		if raw == "" {
			continue
		}
		rewritten, err := legacyType6ChatURL(raw, ch.OpenAIChatPath)
		if err != nil {
			return false, fmt.Errorf("channel %d base url: %w", ch.ID, err)
		}
		if rewritten == bu.URL {
			continue
		}
		log.Warnf("migrate 021: channel %d base url %s -> %s", ch.ID, redactURLForLog(bu.URL), redactURLForLog(rewritten))
		next[i].URL = rewritten
		changed = true
	}
	path := strings.TrimSpace(ch.OpenAIChatPath)
	updates := map[string]any{}
	if changed {
		raw, err := jsonMarshalBaseURLs(next)
		if err != nil {
			return false, err
		}
		updates["base_urls"] = raw
	}
	if path != "" {
		log.Warnf("migrate 021: channel %d cleared openai_chat_path %s (folded into the base url)", ch.ID, redactURLForLog(path))
		updates["open_ai_chat_path"] = ""
		changed = true
	}
	if !changed {
		return false, nil
	}
	if err := tx.Table("channels").Where("id = ?", ch.ID).Updates(updates).Error; err != nil {
		return false, fmt.Errorf("update channel %d: %w", ch.ID, err)
	}
	return true, nil
}

// legacyType6ChatURL is the URL a chat request used to hit:
// GetOpenAIChatBaseUrl (join OpenAIChatPath when set) then
// JoinCustomOpenAIChatPath(that, "/v1/chat/completions").
func legacyType6ChatURL(baseURL, chatPath string) (string, error) {
	stage := strings.TrimSpace(baseURL)
	if path := strings.TrimSpace(chatPath); path != "" {
		next, err := xurl.LegacyJoinCustomOpenAIChatPath(stage, path)
		if err != nil {
			return "", err
		}
		stage = next
	}
	return xurl.LegacyJoinCustomOpenAIChatPath(stage, "/v1/chat/completions")
}

func jsonMarshalBaseURLs(values []model.BaseUrl) (string, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func redactURLForLog(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if parsed, err := url.Parse(trimmed); err == nil && parsed != nil && parsed.User != nil {
		parsed.User = url.User("redacted")
		trimmed = parsed.String()
	}
	return xredact.Secrets(xredact.URLSecrets(trimmed))
}
