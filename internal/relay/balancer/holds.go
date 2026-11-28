package balancer

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// A hold is a cooldown with a fixed end, set by an operator error rule rather than by the
// consecutive-failure breaker. It lives beside the breaker so the routing check, the admin
// list, reset and Retry-After all see one picture.
//
// Hold keys reuse the breaker layout channelID:keyID:model with two wildcards:
//   - channel:key:model  scope model   (this model on this key)
//   - channel:key:*      scope key     (every model on this key)
//   - channel:0:*        scope channel (every key and model)
const holdAnyModel = "*"

type holdEntry struct {
	until time.Time
	rule  string
	// endpoint/capability 是触发这条 hold 的流量作用域（给探测调度用的元数据）。
	// 留空表示"不知道这道请求是什么形状"：老口径 hold 与通配 hold 都留空，探测调度
	// 因此不会去探它们（见 circuit_probe.go 的 classifyProbeScope）。
	// 它们**不参与**任何 hold 匹配/路由判定：IsTrippedScoped / holdRemaining 一律
	// 只看 channel:key:model（或通配），不受这里影响。
	endpoint   string
	capability string
	// writeSeq 同 circuitEntry.writeSeq：hold 被重新写过（新建/延长）就取一个新的全局
	// 版本号，让拿在手上的"准入时那一条 hold"能被不可变地认出来。
	writeSeq uint64
}

var globalHolds sync.Map // key: string -> holdEntry

func holdKey(channelID, keyID int, modelName string) string {
	return fmt.Sprintf("%d:%d:%s", channelID, keyID, modelName)
}

// ApplyHold holds the scope until now+cooldown. A concurrent or repeated hold only ever
// extends the end, never shortens it.
//
// 这是**老口径**入口：它不知道触发这条 hold 的请求是什么形状，所以记下的 hold 行没有
// 端点作用域元数据，熔断探测调度不会去探它。想让它可探就用 ApplyHoldScoped。
func ApplyHold(channelID, keyID int, modelName, scope string, cooldown time.Duration, rule string) {
	ApplyHoldScoped(channelID, keyID, modelName, scope, cooldown, rule, "", "")
}

// ApplyHoldScoped 与 ApplyHold 同义，额外带上触发这次 hold 的端点作用域（endpoint /
// capability，与熔断键同一套口径）。带上作用域后，熔断探测调度才把这条 hold 行当成
// "普通文本作用域可探"；留空仍是老口径（未知作用域、不探）。通配作用域的 hold
// （key/channel 级）无论有没有元数据都不参与探测。
func ApplyHoldScoped(channelID, keyID int, modelName, scope string, cooldown time.Duration, rule, endpoint, capability string) {
	if cooldown <= 0 {
		return
	}
	var key string
	switch scope {
	case model.CircuitRuleScopeModel:
		key = holdKey(channelID, keyID, modelName)
	case model.CircuitRuleScopeKey:
		key = holdKey(channelID, keyID, holdAnyModel)
	case model.CircuitRuleScopeChannel:
		key = holdKey(channelID, 0, holdAnyModel)
	default:
		return
	}
	entry := holdEntry{
		until:      time.Now().Add(cooldown),
		rule:       rule,
		endpoint:   strings.TrimSpace(endpoint),
		capability: strings.TrimSpace(capability),
		writeSeq:   nextCircuitRevision(),
	}
	// tuple 级提交锁：hold 延长与探测重置（重置会清掉同一条 hold）必须串行，
	// 否则重置的"复核 → 清理"之间能漏进一次延长，把新 hold 一并抹掉。
	unlock := lockCircuitTuple(channelID, keyID, modelName)
	defer unlock()
	for {
		current, loaded := globalHolds.LoadOrStore(key, entry)
		if !loaded {
			break
		}
		existing := current.(holdEntry)
		if !entry.until.After(existing.until) {
			break
		}
		// 延长也是一次写：取一个新版本号，拿在手上的旧 hold 就不再是这一条。
		entry.writeSeq = nextCircuitRevision()
		if globalHolds.CompareAndSwap(key, existing, entry) {
			break
		}
	}
	log.Warnf("circuit hold [%s] scope=%s rule=%q for %v", key, scope, rule, cooldown)
}

// holdRemaining returns the longest active hold covering channel/key/model.
func holdRemaining(channelID, keyID int, modelName string) (time.Duration, string) {
	now := time.Now()
	var longest time.Duration
	var rule string
	for _, key := range []string{
		holdKey(channelID, keyID, modelName),
		holdKey(channelID, keyID, holdAnyModel),
		holdKey(channelID, 0, holdAnyModel),
	} {
		value, ok := globalHolds.Load(key)
		if !ok {
			continue
		}
		entry := value.(holdEntry)
		left := entry.until.Sub(now)
		if left <= 0 {
			globalHolds.CompareAndDelete(key, entry)
			continue
		}
		if left > longest {
			longest = left
			rule = entry.rule
		}
	}
	return longest, rule
}

// clearHolds removes holds narrowed like ResetScoped. A key or model reset keeps wider
// holds (a key reset keeps the channel-wide hold; a model reset keeps key-wide holds),
// since those were not about that key or model.
func clearHolds(channelID, keyID int, modelName string) int {
	cleared := 0
	globalHolds.Range(func(key, _ any) bool {
		parsed, ok := parseCircuitKey(key.(string))
		if !ok || parsed.channelID != channelID {
			return true
		}
		if keyID > 0 && parsed.keyID != keyID {
			return true
		}
		if modelName != "" && parsed.model != modelName {
			return true
		}
		globalHolds.Delete(key)
		cleared++
		return true
	})
	return cleared
}

// listHolds returns the channel's active holds as admin-list rows.
func listHolds(channelID int) []CircuitEntryView {
	now := time.Now()
	views := make([]CircuitEntryView, 0)
	globalHolds.Range(func(key, value any) bool {
		parsed, ok := parseCircuitKey(key.(string))
		if !ok || parsed.channelID != channelID {
			return true
		}
		entry := value.(holdEntry)
		left := entry.until.Sub(now)
		if left <= 0 {
			return true
		}
		views = append(views, CircuitEntryView{
			KeyID:            parsed.keyID,
			Model:            parsed.model,
			Endpoint:         entry.endpoint,
			Capability:       entry.capability,
			State:            "held",
			RemainingSeconds: int((left + time.Second - 1) / time.Second),
			Reason:           entry.rule,
		})
		return true
	})
	return views
}

type compiledCircuitRules struct {
	raw   string
	rules []model.CircuitErrorRule
}

var circuitRulesCache sync.Map // single key "rules" -> compiledCircuitRules

// circuitRulesSource reads the raw rule setting; tests swap it to avoid the settings DB.
var circuitRulesSource = func() (string, error) {
	return op.SettingGetString(model.SettingKeyCircuitBreakerErrorRules)
}

// CurrentCircuitRules returns the operator-configured error rules used by the
// unified relay error judge. Keep currentCircuitRules private for the legacy hold
// path; this wrapper is read-only and shares the same compiled cache.
func CurrentCircuitRules() []model.CircuitErrorRule {
	return currentCircuitRules()
}

func currentCircuitRules() []model.CircuitErrorRule {
	raw, err := circuitRulesSource()
	if err != nil {
		return nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	if cached, ok := circuitRulesCache.Load("rules"); ok {
		compiled := cached.(compiledCircuitRules)
		if compiled.raw == raw {
			return compiled.rules
		}
	}
	rules, err := model.ParseCircuitErrorRules(raw)
	if err != nil {
		log.Warnf("ignoring invalid circuit error rules: %v", err)
		rules = nil
	}
	circuitRulesCache.Store("rules", compiledCircuitRules{raw: raw, rules: rules})
	return rules
}

// ApplyErrorRules checks a counted failure against the operator rules and holds the first
// matching rule's scope. It returns the matched rule name, or "" when none matched.
//
// 老口径入口：hold 行不带端点作用域元数据，因此熔断探测调度不会去探它。
func ApplyErrorRules(channelID, keyID int, modelName string, statusCode int, errText string) string {
	return ApplyErrorRulesScoped(channelID, keyID, modelName, statusCode, errText, "", "")
}

// ApplyErrorRulesScoped 与 ApplyErrorRules 同义，额外把触发这次失败的作用域（endpoint /
// capability，与熔断键同一套口径）交给 hold：带上之后这条 hold 行在熔断探测调度那边是
// "普通文本作用域可探"，而不是老口径的未知作用域。留空则与 ApplyErrorRules 完全一致。
func ApplyErrorRulesScoped(channelID, keyID int, modelName string, statusCode int, errText, endpoint, capability string) string {
	rules := currentCircuitRules()
	if len(rules) == 0 {
		return ""
	}
	lower := strings.ToLower(errText)
	for _, rule := range rules {
		if !rule.Match(statusCode, lower) {
			continue
		}
		ApplyHoldScoped(channelID, keyID, modelName, rule.Scope, time.Duration(rule.CooldownSeconds)*time.Second, rule.Name, endpoint, capability)
		return rule.Name
	}
	return ""
}
