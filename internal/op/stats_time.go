package op

import (
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// 统计口径时区缓存：按配置的时区名缓存已解析的 *time.Location，
// 仅当 stats_timezone 设置变化时才重新 LoadLocation，避免每次请求解析 zoneinfo。
var (
	statsLocMu   sync.RWMutex
	statsLocName string
	statsLocLoad *time.Location = time.Local
)

// statsLocation 返回统计"当天/小时"划分自然日所用的时区。
// 取 SettingKeyStatsTimezone(IANA 名，如 "America/Los_Angeles")；
// 为空或加载失败时回退到容器本地时区(time.Local)，与旧行为一致。
func statsLocation() *time.Location {
	name := ""
	if v, err := SettingGetString(model.SettingKeyStatsTimezone); err == nil {
		name = strings.TrimSpace(v)
	}

	statsLocMu.RLock()
	if name == statsLocName && statsLocLoad != nil {
		loc := statsLocLoad
		statsLocMu.RUnlock()
		return loc
	}
	statsLocMu.RUnlock()

	loc := time.Local
	if name != "" {
		if l, err := time.LoadLocation(name); err == nil {
			loc = l
		}
	}

	statsLocMu.Lock()
	statsLocName = name
	statsLocLoad = loc
	statsLocMu.Unlock()
	return loc
}

// statsNow 返回统计口径时区下的当前时间，用于计算"今天(20060102)"与小时。
func statsNow() time.Time {
	return time.Now().In(statsLocation())
}
