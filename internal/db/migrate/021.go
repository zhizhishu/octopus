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
// Relative embeddings, images, videos, and tasks paths are rewritten to the
// absolute URL the old join produced, using the original base (not the new
// chat URL). Empty fields stay empty. An empty tasks path is not a skip: poll
// and content fall back to the videos prefix. Channels with an empty
// embeddings, images, or videos field are listed at startup: those endpoints
// are skipped, because a type-6 channel must not post non-chat traffic at the
// chat URL. Raw protocols have no path field and are not served on type 6.
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

		var noEmbeddings, noImages, noVideos []int
		for i := range channels {
			ch := &channels[i]
			if strings.TrimSpace(ch.OpenAIEmbeddingsPath) == "" {
				noEmbeddings = append(noEmbeddings, ch.ID)
			}
			if strings.TrimSpace(ch.OpenAIImagesPath) == "" {
				noImages = append(noImages, ch.ID)
			}
			if strings.TrimSpace(ch.OpenAIVideosPath) == "" {
				noVideos = append(noVideos, ch.ID)
			}
			if _, err := rewriteType6ChannelURLs(tx, ch); err != nil {
				return err
			}
		}
		if len(noEmbeddings) > 0 || len(noImages) > 0 || len(noVideos) > 0 {
			log.Warnf("migrate 021: type-6 channels with no embeddings/images/videos path will skip that endpoint (no full URL). embeddings %v images %v videos %v. Raw endpoints such as systemone and rerank are not served on type 6.", noEmbeddings, noImages, noVideos)
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
	originalBases := append([]model.BaseUrl(nil), ch.BaseUrls...)
	bestBase := (&model.Channel{BaseUrls: originalBases}).GetBaseUrl()
	next := append([]model.BaseUrl(nil), originalBases...)
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
	updates := map[string]any{}
	if changed {
		raw, err := jsonMarshalBaseURLs(next)
		if err != nil {
			return false, err
		}
		updates["base_urls"] = raw
	}
	if path := strings.TrimSpace(ch.OpenAIChatPath); path != "" {
		log.Warnf("migrate 021: channel %d cleared openai_chat_path %s (folded into the base url)", ch.ID, redactURLForLog(path))
		updates["open_ai_chat_path"] = ""
		changed = true
	}
	for _, field := range []struct {
		column string
		value  string
		join   func(base, path string) (string, error)
	}{
		{"open_ai_embeddings_path", ch.OpenAIEmbeddingsPath, legacyType6EmbeddingsURL},
		{"open_ai_images_path", ch.OpenAIImagesPath, legacyType6MediaPrefix},
		{"open_ai_videos_path", ch.OpenAIVideosPath, legacyType6MediaPrefix},
		// Poll and content prefer this prefix over the videos path when a task id
		// is present. A relative value has to become absolute or type 6 refuses
		// the request instead of posting it at the chat URL.
		{"open_ai_tasks_path", ch.OpenAITasksPath, legacyType6MediaPrefix},
	} {
		rewritten, rewrite, err := legacyAbsolutePathField(bestBase, field.value, field.join)
		if err != nil {
			return false, fmt.Errorf("channel %d %s: %w", ch.ID, field.column, err)
		}
		if !rewrite {
			continue
		}
		log.Warnf("migrate 021: channel %d %s %s -> %s", ch.ID, field.column, redactURLForLog(field.value), redactURLForLog(rewritten))
		updates[field.column] = rewritten
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

// legacyAbsolutePathField converts a non-empty relative path into the absolute
// URL the old join produced. An empty field stays empty. An absolute field is
// already the stored override and is left alone, so a second pass is a no-op.
func legacyAbsolutePathField(base, path string, join func(base, path string) (string, error)) (string, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false, nil
	}
	if _, ok, err := xurl.ParseEndpointOverride(path); err != nil {
		return "", false, err
	} else if ok {
		return path, false, nil
	}
	if strings.TrimSpace(base) == "" {
		return "", false, nil
	}
	abs, err := join(base, path)
	if err != nil {
		return "", false, err
	}
	if abs == path {
		return path, false, nil
	}
	return abs, true, nil
}

func legacyType6EmbeddingsURL(base, path string) (string, error) {
	stage, err := xurl.JoinCustomOpenAIEmbeddingsPath(base, path)
	if err != nil {
		return "", err
	}
	return xurl.JoinCustomOpenAIEmbeddingsPath(stage, "/v1/embeddings")
}

func legacyType6MediaPrefix(base, path string) (string, error) {
	return xurl.JoinOpenAIPath(base, strings.TrimRight(strings.TrimSpace(path), "/"))
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
