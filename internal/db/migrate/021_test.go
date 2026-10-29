package migrate

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new021DB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "octopus.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.Channel{}, &model.ChannelKey{}, &model.Setting{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestMigration021RewritesType6BaseURLsOnce(t *testing.T) {
	db := new021DB(t)

	root := model.Channel{
		Name: "root",
		Type: outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []model.BaseUrl{
			{URL: "https://api.example.com", Delay: 3},
			{URL: "https://backup.example.com/v1/", Delay: 9},
		},
	}
	withV1 := model.Channel{
		Name:     "v1",
		Type:     outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: "https://api.example.com/v1"}},
	}
	already := model.Channel{
		Name:     "already",
		Type:     outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: "https://api.example.com/v1/chat/completions"}},
	}
	query := model.Channel{
		Name: "query",
		Type: outbound.OutboundTypeCustomOpenAIChat,
		BaseUrls: []model.BaseUrl{{
			URL: "https://user:s3cret@api.example.com/v1?api_key=sk-live&foo=bar",
		}},
		OpenAIChatPath: "/chat/completions",
	}
	other := model.Channel{
		Name:     "chat",
		Type:     outbound.OutboundTypeOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: "https://api.example.com"}},
	}
	for _, ch := range []*model.Channel{&root, &withV1, &already, &query, &other} {
		if err := db.Create(ch).Error; err != nil {
			t.Fatalf("seed %s: %v", ch.Name, err)
		}
	}
	secretKey := "sk-do-not-log"
	if err := db.Create(&model.ChannelKey{ChannelID: root.ID, Enabled: true, ChannelKey: secretKey}).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}

	if err := migrateCustomOpenAIChatVerbatimURLs(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got := map[string]model.Channel{}
	var rows []model.Channel
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, row := range rows {
		got[row.Name] = row
	}

	if urls := baseURLStrings(got["root"].BaseUrls); urls[0] != "https://api.example.com/v1/chat/completions" || urls[1] != "https://backup.example.com/v1/chat/completions" {
		t.Fatalf("multiple base urls = %#v", got["root"].BaseUrls)
	}
	if got["root"].BaseUrls[0].Delay != 3 || got["root"].BaseUrls[1].Delay != 9 {
		t.Fatalf("delays were not preserved: %#v", got["root"].BaseUrls)
	}
	if got["v1"].BaseUrls[0].URL != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("v1 base = %q", got["v1"].BaseUrls[0].URL)
	}
	if got["already"].BaseUrls[0].URL != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("chat completions url changed: %q", got["already"].BaseUrls[0].URL)
	}
	wantQuery, err := legacyType6ChatURL("https://user:s3cret@api.example.com/v1?api_key=sk-live&foo=bar", "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if got["query"].BaseUrls[0].URL != wantQuery {
		t.Fatalf("query base = %q, want %q", got["query"].BaseUrls[0].URL, wantQuery)
	}
	if got["query"].OpenAIChatPath != "" {
		t.Fatalf("chat path was not cleared: %q", got["query"].OpenAIChatPath)
	}
	if !strings.Contains(wantQuery, "foo=bar") {
		t.Fatalf("query string dropped: %q", wantQuery)
	}
	if got["chat"].BaseUrls[0].URL != "https://api.example.com" || got["chat"].Type != outbound.OutboundTypeOpenAIChat {
		t.Fatalf("non-type-6 channel was rewritten: %#v", got["chat"])
	}

	var storedKey model.ChannelKey
	if err := db.Where("channel_id = ?", root.ID).First(&storedKey).Error; err != nil {
		t.Fatal(err)
	}
	if storedKey.ChannelKey != secretKey {
		t.Fatalf("channel key changed")
	}

	// Rerun is a no-op, including when a URL is later edited by hand.
	if err := db.Model(&model.Channel{}).Where("id = ?", root.ID).Update("base_urls", `[{"url":"https://hand-edited.example/keep","delay":3}]`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateCustomOpenAIChatVerbatimURLs(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var reread model.Channel
	if err := db.First(&reread, root.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reread.BaseUrls[0].URL != "https://hand-edited.example/keep" {
		t.Fatalf("rerun rewrote a url after the marker was set: %q", reread.BaseUrls[0].URL)
	}
}

func TestMigration021RedactsCredentialsInLogText(t *testing.T) {
	raw := "https://user:s3cret@api.example.com/v1/chat/completions?api_key=sk-live&foo=bar"
	got := redactURLForLog(raw)
	for _, secret := range []string{"s3cret", "sk-live", "user:s3cret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("log text still contains %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "api.example.com/v1/chat/completions") {
		t.Fatalf("redacted log dropped the host or path: %s", got)
	}
}

func TestMigration021NilDB(t *testing.T) {
	if err := migrateCustomOpenAIChatVerbatimURLs(nil); err == nil {
		t.Fatal("nil db must error")
	}
}

func baseURLStrings(values []model.BaseUrl) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.URL
	}
	return out
}
