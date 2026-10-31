package op

// 日志模式（普通 / 调试）的运行时控制。
//
//   - 普通（默认）：运行日志 warn 级；中继日志照旧。
//   - 调试：运行日志 debug 级；中继日志多记一份调试详情（见 model.RelayLogDebugInfo）。
//
// 切换即时生效（zap 的 AtomicLevel + 设置缓存），不用重启。调试模式默认 24 小时后
// 自动回到普通：开着忘关会让日志变多、变大，这比「排查到一半被关掉」更常见也更伤。
//
// 这里只管「看多少」，不碰任何出站行为：中继侧只读 LogModeDebugActive()。

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

// logModeEnvLevel 是部署时显式指定的运行日志级别。设了它，普通模式就用它；
// 没设才用 warn。调试模式一律 debug。
const logModeEnvLevel = "OCTOPUS_LOG_LEVEL"

const logModeWatchInterval = 30 * time.Second

var (
	logModeApplyMu   sync.Mutex
	logModeWatchOnce sync.Once
	// logModeNow 可在测试里替换。
	logModeNow = time.Now
)

// LogModeDebugActive 报告此刻是否处于调试模式（已到自动关闭时刻的算关闭）。
// 中继热路径每个请求读一次：只读设置缓存，不碰数据库。
func LogModeDebugActive() bool {
	mode, err := SettingGetString(model.SettingKeyLogMode)
	if err != nil || mode != model.LogModeDebug {
		return false
	}
	until := LogDebugExpiresAt()
	return until == 0 || logModeNow().Unix() < until
}

// LogDebugExpiresAt 返回调试模式自动关闭的 Unix 秒；0 = 不会自动关（或没开）。
func LogDebugExpiresAt() int64 {
	since, err := settingGetInt64(model.SettingKeyLogDebugSince)
	if err != nil || since <= 0 {
		return 0
	}
	hours, err := SettingGetInt(model.SettingKeyLogDebugAutoOffHours)
	if err != nil || hours <= 0 {
		return 0
	}
	return since + int64(hours)*3600
}

func settingGetInt64(key model.SettingKey) (int64, error) {
	raw, err := SettingGetString(key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

// LogModeNormalLevel 是普通模式下的运行日志级别：显式设了 OCTOPUS_LOG_LEVEL 用它，否则 warn。
func LogModeNormalLevel() string {
	if raw, ok := os.LookupEnv(logModeEnvLevel); ok {
		level := strings.ToLower(strings.TrimSpace(raw))
		switch level {
		case "debug", "info", "warn", "error", "dpanic", "panic", "fatal":
			return level
		}
	}
	return "warn"
}

// LogModeOnSettingChanged 在管理员改了日志模式相关设置之后调用（设置已写入）。
// 切到调试会把计时从现在重新开始；切回普通清掉计时。随后立即应用运行日志级别。
func LogModeOnSettingChanged(key model.SettingKey) {
	switch key {
	case model.SettingKeyLogMode:
		mode, _ := SettingGetString(model.SettingKeyLogMode)
		since := "0"
		if mode == model.LogModeDebug {
			since = strconv.FormatInt(logModeNow().Unix(), 10)
		}
		if err := SettingSetString(model.SettingKeyLogDebugSince, since); err != nil {
			log.Warnf("log mode: failed to record debug start time: %v", err)
		}
	case model.SettingKeyLogDebugAutoOffHours, model.SettingKeyLogDebugSince:
	default:
		return
	}
	LogModeApply()
}

// LogModeApply 让运行日志级别与设置一致，并处理到点自动关闭。
// 启动时、设置变更后、导入备份后、以及后台每 30 秒各调用一次；幂等。
func LogModeApply() {
	logModeApplyMu.Lock()
	defer logModeApplyMu.Unlock()

	mode, err := SettingGetString(model.SettingKeyLogMode)
	if err != nil {
		return
	}
	if mode == model.LogModeDebug {
		// 导入的备份或直接改库可能只带了 mode=debug、没有开始时间：从现在起算，
		// 否则「自动关闭」永远不会触发。
		if since, sinceErr := settingGetInt64(model.SettingKeyLogDebugSince); sinceErr != nil || since <= 0 {
			_ = SettingSetString(model.SettingKeyLogDebugSince, strconv.FormatInt(logModeNow().Unix(), 10))
		}
		if until := LogDebugExpiresAt(); until > 0 && logModeNow().Unix() >= until {
			if setErr := SettingSetString(model.SettingKeyLogMode, model.LogModeNormal); setErr != nil {
				log.Warnf("log mode: debug expired but switching back failed: %v", setErr)
			} else {
				_ = SettingSetString(model.SettingKeyLogDebugSince, "0")
				mode = model.LogModeNormal
				log.Warnf("log mode: debug auto-off reached, back to normal")
			}
		}
	}

	want := LogModeNormalLevel()
	if mode == model.LogModeDebug {
		want = "debug"
	}
	if log.GetLevel() == want {
		return
	}
	log.SetLevel(want)
	// 用 Warn 打这一行：普通模式下也必须看得到「级别变了」。
	log.Warnf("log mode: %s, runtime log level=%s", mode, want)
}

// LogModeStartWatcher 启动后台检查（只启动一次）：到点自动关闭调试模式。
func LogModeStartWatcher() {
	logModeWatchOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(logModeWatchInterval)
			defer ticker.Stop()
			for range ticker.C {
				LogModeApply()
			}
		}()
	})
}

// relayLogDebugInfoSweepWindow：每轮只看「刚过 3 天」往前 7 天这一段（走 time 索引），
// 不做全表扫描；更老的行早晚会被保留期整行删掉。
const relayLogDebugInfoSweepWindow = 7 * 24 * time.Hour

// relayLogClearExpiredDebugInfo 把超过 RelayLogDebugRetentionDays 的调试详情清成 NULL。
// 只清这一列，日志行本身按原保留期保留。失败只记 debug 日志，下一轮再试。
func relayLogClearExpiredDebugInfo(ctx context.Context) {
	cutoff := logModeNow().Add(-time.Duration(model.RelayLogDebugRetentionDays) * 24 * time.Hour)
	from := cutoff.Add(-relayLogDebugInfoSweepWindow).Unix()
	if err := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).
		Where("time >= ? AND time < ? AND debug_info IS NOT NULL", from, cutoff.Unix()).
		Update("debug_info", gorm.Expr("NULL")).Error; err != nil {
		log.Debugf("relay log debug info sweep failed: %v", err)
	}
}
