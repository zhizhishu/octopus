package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// Shipping a new default is not enough: a settings row written by the previous seed
// wins over the new constant forever, so already-installed deployments would keep the
// behaviour the new default exists to fix (900s of silence before the relay acts, and
// no absolute pre-content deadline at all). These tests pin the convergence for the
// exact legacy defaults, and — just as important — that an operator's own value is
// never rewritten.

func TestSettingRefreshCacheUpgradesLegacyStreamDataTimeout900(t *testing.T) {
	ctx := setupSettingTest(t)

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyRelayStreamDataTimeoutSec,
		Value: model.LegacyDefaultRelayStreamDataIntervalTimeoutSeconds900,
	}).Error; err != nil {
		t.Fatalf("seed previous stream timeout default: %v", err)
	}
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
	cached, err := SettingGetString(model.SettingKeyRelayStreamDataTimeoutSec)
	if err != nil {
		t.Fatalf("get cached setting: %v", err)
	}
	if cached != model.DefaultRelayStreamDataIntervalTimeoutSeconds {
		t.Fatalf("previous default %q must converge to %q, got %q",
			model.LegacyDefaultRelayStreamDataIntervalTimeoutSeconds900,
			model.DefaultRelayStreamDataIntervalTimeoutSeconds, cached)
	}
}

func TestSettingRefreshCacheUpgradesDisabledFirstTokenDefault(t *testing.T) {
	ctx := setupSettingTest(t)

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyFirstTokenTimeOutDefault,
		Value: model.LegacyDefaultFirstTokenTimeOutSeconds,
	}).Error; err != nil {
		t.Fatalf("seed legacy first token default: %v", err)
	}
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
	cached, err := SettingGetString(model.SettingKeyFirstTokenTimeOutDefault)
	if err != nil {
		t.Fatalf("get cached setting: %v", err)
	}
	if cached != model.DefaultFirstTokenTimeOutSeconds {
		t.Fatalf("the shipped %q (disabled) default must converge to %q, got %q",
			model.LegacyDefaultFirstTokenTimeOutSeconds, model.DefaultFirstTokenTimeOutSeconds, cached)
	}
}

func TestSettingRefreshCacheKeepsOperatorFirstTokenValue(t *testing.T) {
	ctx := setupSettingTest(t)
	const operatorValue = "45"

	if err := db.GetDB().WithContext(ctx).Create(&model.Setting{
		Key:   model.SettingKeyFirstTokenTimeOutDefault,
		Value: operatorValue,
	}).Error; err != nil {
		t.Fatalf("seed operator first token default: %v", err)
	}
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
	cached, err := SettingGetString(model.SettingKeyFirstTokenTimeOutDefault)
	if err != nil {
		t.Fatalf("get cached setting: %v", err)
	}
	if cached != operatorValue {
		t.Fatalf("an operator's own value must never be rewritten, got %q", cached)
	}
}
