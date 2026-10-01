package op

import (
	"context"
	"fmt"
	"time"
)

const cacheOperationTimeout = 60 * time.Second

func InitCache() error {
	ctx, cancel := context.WithTimeout(context.Background(), cacheOperationTimeout)
	defer cancel()
	return initCache(ctx)
}

// initCache loads settings first and under the settings write lock, then the caches
// that do not read the settings cache. Callers that already hold settingWriteMu must
// call refreshRuntimeCaches directly instead of this.
func initCache(ctx context.Context) error {
	if err := settingRefreshCache(ctx); err != nil {
		return fmt.Errorf("setting refresh cache error: %v", err)
	}
	return refreshRuntimeCaches(ctx)
}

// refreshRuntimeCaches reloads every cache except settings. None of these read the
// settings cache, so they run OUTSIDE settingWriteMu and never extend the window in
// which a concurrent settings save has to wait.
func refreshRuntimeCaches(ctx context.Context) error {
	if err := userRefreshCache(ctx); err != nil {
		return fmt.Errorf("user refresh cache error: %v", err)
	}
	if err := channelRefreshCache(ctx); err != nil {
		return fmt.Errorf("channel refresh cache error: %v", err)
	}
	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		return fmt.Errorf("fingerprint profile refresh cache error: %v", err)
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		return fmt.Errorf("access plan refresh cache error: %v", err)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		return fmt.Errorf("api key refresh cache error: %v", err)
	}
	if err := llmRefreshCache(ctx); err != nil {
		return fmt.Errorf("llm refresh cache error: %v", err)
	}
	if err := statsRefreshCache(ctx); err != nil {
		return fmt.Errorf("stats refresh cache error: %v", err)
	}
	return nil
}

func SaveCache() error {
	ctx, cancel := context.WithTimeout(context.Background(), cacheOperationTimeout)
	defer cancel()
	if err := StatsSaveDB(ctx); err != nil {
		return err
	}
	if err := ChannelKeySaveDB(ctx); err != nil {
		return err
	}
	if err := RelayLogSaveDBTask(ctx); err != nil {
		return err
	}
	// relay IP 审计改成周期批量落库后(见 UserRecordRelayIP), 优雅退出时把待落库的
	// last_relay_ip/at 余量刷进 DB, 否则最多丢一个周期(1 分钟)的审计更新。
	if err := UserRelayIPSaveDB(ctx); err != nil {
		return err
	}
	return nil
}
