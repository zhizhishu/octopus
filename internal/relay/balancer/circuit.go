package balancer

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/utils/log"
)

// CircuitState 熔断器状态
type CircuitState int

const (
	StateClosed   CircuitState = iota // 正常通行
	StateOpen                         // 熔断中，拒绝所有请求
	StateHalfOpen                     // 半开，仅允许单个试探请求
)

// circuitEntry 单个熔断器条目
type circuitEntry struct {
	State               CircuitState
	ConsecutiveFailures int64
	LastFailureTime     time.Time
	TripCount           int // 累计熔断触发次数（用于指数退避）
	Policy              circuitFailurePolicy
	mu                  sync.Mutex
}

type circuitFailurePolicy struct {
	thresholdFloor   int64
	cooldownBaseCeil int
	cooldownMaxCeil  int
	reason           string
}

// 全局熔断器存储
var globalBreaker sync.Map // key: string -> value: *circuitEntry

// circuitKey 生成熔断器键：channelID:channelKeyID:modelName
func circuitKey(channelID, keyID int, modelName string) string {
	return fmt.Sprintf("%d:%d:%s", channelID, keyID, modelName)
}

func circuitScopedKey(channelID, keyID int, modelName, endpoint, capability string) string {
	key := circuitKey(channelID, keyID, modelName)
	endpoint = strings.TrimSpace(endpoint)
	capability = strings.TrimSpace(capability)
	if endpoint == "" && capability == "" {
		return key
	}
	return fmt.Sprintf("%s:endpoint=%s:capability=%s", key, endpoint, capability)
}

// getOrCreateEntry 获取或创建熔断器条目
func getOrCreateEntry(key string) *circuitEntry {
	if v, ok := globalBreaker.Load(key); ok {
		return v.(*circuitEntry)
	}
	entry := &circuitEntry{State: StateClosed}
	actual, _ := globalBreaker.LoadOrStore(key, entry)
	return actual.(*circuitEntry)
}

// getThreshold 获取熔断阈值配置
func getThreshold() int64 {
	v, err := op.SettingGetInt(model.SettingKeyCircuitBreakerThreshold)
	if err != nil || v <= 0 {
		return 10
	}
	return int64(v)
}

func getThresholdForPolicy(policy circuitFailurePolicy) int64 {
	threshold := getThreshold()
	if policy.thresholdFloor > threshold {
		return policy.thresholdFloor
	}
	return threshold
}

// GetCooldown 获取当前冷却时间（带指数退避）
func GetCooldown(tripCount int) time.Duration {
	return getCooldownForPolicy(tripCount, circuitFailurePolicy{})
}

func getCooldownForPolicy(tripCount int, policy circuitFailurePolicy) time.Duration {
	base, err := op.SettingGetInt(model.SettingKeyCircuitBreakerCooldown)
	if err != nil || base <= 0 {
		base = 30
	}
	if policy.cooldownBaseCeil > 0 && base > policy.cooldownBaseCeil {
		base = policy.cooldownBaseCeil
	}
	maxCooldown, err := op.SettingGetInt(model.SettingKeyCircuitBreakerMaxCooldown)
	if err != nil || maxCooldown <= 0 {
		// 120s (was 600): exponential backoff still works, but a flaky key
		// cannot black out a channel/model for ten minutes. sub2api uses ~10min
		// only for account-level 529 overload quarantine, not per-request breaker.
		maxCooldown = 120
	}
	if policy.cooldownMaxCeil > 0 && maxCooldown > policy.cooldownMaxCeil {
		maxCooldown = policy.cooldownMaxCeil
	}
	// Absolute safety cap even if an admin set an extreme max in settings.
	// Soft runtime cooldown already tops out at 2 minutes; hard breaker should
	// not outlive that by an order of magnitude.
	const absoluteMaxCooldownSeconds = 180
	if maxCooldown > absoluteMaxCooldownSeconds {
		maxCooldown = absoluteMaxCooldownSeconds
	}

	// 指数退避：baseCooldown * 2^(tripCount-1)
	cooldown := base
	if tripCount > 1 {
		shift := tripCount - 1
		if shift > 20 { // 防止溢出
			shift = 20
		}
		cooldown = base << shift
	}
	if cooldown > maxCooldown {
		cooldown = maxCooldown
	}

	return time.Duration(cooldown) * time.Second
}

func failurePolicyForStatus(statusCode int, modelName string) circuitFailurePolicy {
	return failurePolicyForStatusAndCapability(statusCode, modelName, "")
}

func failurePolicyForStatusAndCapability(statusCode int, modelName, capability string) circuitFailurePolicy {
	normalizedModel := strings.ToLower(strings.TrimSpace(modelName))
	normalizedCapability := strings.ToLower(strings.TrimSpace(capability))
	policy := circuitFailurePolicy{}
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 529:
		policy.thresholdFloor = 10
		policy.cooldownBaseCeil = 30
		policy.cooldownMaxCeil = 30
		policy.reason = "transient_upstream"
	case http.StatusUnauthorized, http.StatusForbidden:
		// Auth / model-permission blips (common on multi-key GLM pools: one key
		// 403 "no access to model", another key succeeds). Do not escalate to
		// multi-minute open; short cool-down is enough for key rotation.
		// Threshold stays at the global default (no floor raise) so a truly dead
		// key still trips after consecutive failures, but cooldown stays short.
		policy.cooldownBaseCeil = 15
		policy.cooldownMaxCeil = 30
		policy.reason = "auth_or_permission"
	}
	if (strings.Contains(normalizedModel, "claude") && strings.Contains(normalizedModel, "[1m]")) ||
		strings.Contains(normalizedCapability, "anthropic_context_1m") {
		policy.thresholdFloor = 10
		policy.cooldownBaseCeil = 30
		policy.cooldownMaxCeil = 30
		if policy.reason == "" {
			policy.reason = "claude_1m"
		}
	}
	return policy
}

// IsTripped 检查通道是否处于熔断状态
// 返回 tripped=true 表示该通道应被跳过，remaining 为剩余冷却时间
func IsTripped(channelID, keyID int, modelName string) (tripped bool, remaining time.Duration) {
	return IsTrippedScoped(channelID, keyID, modelName, "", "")
}

func IsTrippedScoped(channelID, keyID int, modelName, endpoint, capability string) (tripped bool, remaining time.Duration) {
	key := circuitScopedKey(channelID, keyID, modelName, endpoint, capability)
	v, ok := globalBreaker.Load(key)
	if !ok {
		return false, 0 // 无记录，视为 Closed
	}
	entry := v.(*circuitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	switch entry.State {
	case StateClosed:
		return false, 0

	case StateOpen:
		cooldown := getCooldownForPolicy(entry.TripCount, entry.Policy)
		elapsed := time.Since(entry.LastFailureTime)
		if elapsed >= cooldown {
			entry.State = StateHalfOpen
			log.Infof("circuit breaker [%s] Open -> HalfOpen (cooldown %v elapsed)", key, cooldown)
			return false, 0
		}
		// 仍在冷却中
		return true, cooldown - elapsed

	case StateHalfOpen:
		// 已有试探请求在进行中，拒绝其他请求
		return true, 0

	default:
		return false, 0
	}
}

// RecordSuccess 记录成功，重置熔断器状态
func RecordSuccess(channelID, keyID int, modelName string) {
	RecordSuccessScoped(channelID, keyID, modelName, "", "")
}

func RecordSuccessScoped(channelID, keyID int, modelName, endpoint, capability string) {
	key := circuitScopedKey(channelID, keyID, modelName, endpoint, capability)
	v, ok := globalBreaker.Load(key)
	if !ok {
		return
	}
	entry := v.(*circuitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.State == StateHalfOpen {
		log.Infof("circuit breaker [%s] HalfOpen -> Closed (probe succeeded)", key)
	}

	// 重置全部状态
	entry.State = StateClosed
	entry.ConsecutiveFailures = 0
	entry.TripCount = 0
}

// RecordFailure 记录失败，可能触发熔断
func RecordFailure(channelID, keyID int, modelName string) {
	RecordFailureWithStatus(channelID, keyID, modelName, 0)
}

// RecordFailureWithStatus records upstream failures with a gentler policy for
// transient overload/rate-limit errors and long Claude 1M routes.
func RecordFailureWithStatus(channelID, keyID int, modelName string, statusCode int) {
	RecordFailureWithStatusScoped(channelID, keyID, modelName, "", "", statusCode)
}

func RecordFailureWithStatusScoped(channelID, keyID int, modelName, endpoint, capability string, statusCode int) {
	key := circuitScopedKey(channelID, keyID, modelName, endpoint, capability)
	entry := getOrCreateEntry(key)
	policy := failurePolicyForStatusAndCapability(statusCode, modelName, capability)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.LastFailureTime = time.Now()
	entry.Policy = policy

	switch entry.State {
	case StateClosed:
		entry.ConsecutiveFailures++
		threshold := getThresholdForPolicy(policy)
		if entry.ConsecutiveFailures >= threshold {
			entry.State = StateOpen
			entry.TripCount++
			log.Warnf("circuit breaker [%s] Closed -> Open (failures=%d >= threshold=%d, tripCount=%d, cooldown=%v)",
				key, entry.ConsecutiveFailures, threshold, entry.TripCount, getCooldownForPolicy(entry.TripCount, policy))
		}

	case StateHalfOpen:
		// 试探失败，重新进入 Open 状态，TripCount 递增（冷却时间翻倍）
		entry.State = StateOpen
		entry.TripCount++
		entry.ConsecutiveFailures = 0 // 重新开始计数
		log.Warnf("circuit breaker [%s] HalfOpen -> Open (probe failed, tripCount=%d, cooldown=%v)",
			key, entry.TripCount, getCooldownForPolicy(entry.TripCount, policy))

	case StateOpen:
		// 理论上不应该在 Open 状态下接收到失败记录（请求应被拒绝），
		// 但为安全起见仍更新失败时间
	}
}
