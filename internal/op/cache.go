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
	if err := settingRefreshCache(ctx); err != nil {
		return fmt.Errorf("setting refresh cache error: %v", err)
	}
	if err := userRefreshCache(ctx); err != nil {
		return fmt.Errorf("user refresh cache error: %v", err)
	}
	if err := channelRefreshCache(ctx); err != nil {
		return fmt.Errorf("channel refresh cache error: %v", err)
	}
	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		return fmt.Errorf("fingerprint profile refresh cache error: %v", err)
	}
	if err := groupRefreshCache(ctx); err != nil {
		return fmt.Errorf("group refresh cache error: %v", err)
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
	return nil
}
